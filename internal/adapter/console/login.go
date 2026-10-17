package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"golang.org/x/oauth2"

	"github.com/quyendv/opsward/internal/identity"
)

type loginData struct {
	OIDC      bool
	APITokens bool
}

func (c *console) loginPage(w http.ResponseWriter, r *http.Request) {
	c.renderLogin(w, http.StatusOK, "")
}

func (c *console) renderLogin(w http.ResponseWriter, status int, msg string) {
	c.render(w, status, "login", page{Title: "Sign in", Error: msg, Data: loginData{OIDC: c.opt.OIDC != nil, APITokens: c.opt.APITokens}})
}

// pending is a sign-in at the issuer, kept in a short-lived cookie until it comes back.
type pending struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

func (c *console) oauth() (*oauth2.Config, bool) {
	o := c.opt.OIDC
	if o == nil {
		return nil, false
	}
	authURL, tokenURL, ok := c.opt.Identity.Endpoints()
	if !ok {
		return nil, false
	}
	return &oauth2.Config{
		ClientID: o.ClientID, ClientSecret: o.ClientSecret, RedirectURL: o.RedirectURL,
		Endpoint: oauth2.Endpoint{AuthURL: authURL, TokenURL: tokenURL},
		Scopes:   append([]string{"openid"}, o.Scopes...),
	}, true
}

func (c *console) startOIDC(w http.ResponseWriter, r *http.Request) {
	conf, ok := c.oauth()
	if !ok {
		if c.opt.OIDC == nil {
			http.NotFound(w, r)
			return
		}
		c.renderLogin(w, http.StatusServiceUnavailable, "The identity provider has not been reached yet; try again in a moment.")
		return
	}
	p := pending{State: randomValue(), Nonce: randomValue(), Verifier: oauth2.GenerateVerifier()}
	b, _ := json.Marshal(p)
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: base64.RawURLEncoding.EncodeToString(b), Path: "/", MaxAge: int(loginTTL.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, conf.AuthCodeURL(p.State, oauth2.S256ChallengeOption(p.Verifier), oauth2.SetAuthURLParam("nonce", p.Nonce)), http.StatusSeeOther)
}

func (c *console) callback(w http.ResponseWriter, r *http.Request) {
	conf, ok := c.oauth()
	if !ok {
		http.NotFound(w, r)
		return
	}
	var p pending
	ck, err := r.Cookie(loginCookie)
	if err == nil {
		var b []byte
		if b, err = base64.RawURLEncoding.DecodeString(ck.Value); err == nil {
			err = json.Unmarshal(b, &p)
		}
	}
	clearCookie(w, loginCookie)
	q := r.URL.Query()
	switch {
	case err != nil || p.State == "":
		c.renderLogin(w, http.StatusBadRequest, "The sign-in expired or was started elsewhere; sign in again.")
		return
	case q.Get("state") != p.State:
		c.renderLogin(w, http.StatusBadRequest, "The sign-in does not match the one this browser started; sign in again.")
		return
	case q.Get("error") != "":
		c.opt.Logger.Warn("console: the issuer refused the sign-in", "error", q.Get("error"), "description", q.Get("error_description"))
		c.renderLogin(w, http.StatusUnauthorized, "The identity provider refused the sign-in.")
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, c.opt.OIDC.HTTPClient)
	tok, err := conf.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		c.opt.Logger.Warn("console: exchanging the code failed", "error", err)
		c.renderLogin(w, http.StatusBadGateway, "The identity provider did not complete the sign-in; try again.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		c.opt.Logger.Warn("console: the issuer returned no ID token")
		c.renderLogin(w, http.StatusBadGateway, "The identity provider returned no ID token.")
		return
	}
	id, err := c.opt.Identity.VerifyIDToken(r.Context(), raw, c.opt.OIDC.ClientID, p.Nonce)
	if err != nil {
		c.refused(w, err)
		return
	}
	c.signIn(w, r, id)
}

func (c *console) tokenLogin(w http.ResponseWriter, r *http.Request) {
	if !c.opt.APITokens {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if !strings.HasPrefix(token, identity.TokenPrefix) {
		c.renderLogin(w, http.StatusUnauthorized, "That is not an API token.")
		return
	}
	p, expires, err := c.opt.Identity.Authenticate(r.Context(), token)
	if err != nil {
		c.refused(w, err)
		return
	}
	// The token's groups are read again on every request, so a session keeps none.
	c.signIn(w, r, identity.Identity{Subject: p.Subject, Credential: p.Credential, Expires: expires})
}

// refused tells the person their credential was not accepted, without why: the reason
// goes to the log.
func (c *console) refused(w http.ResponseWriter, err error) {
	if errors.Is(err, identity.ErrUnauthenticated) {
		c.opt.Logger.Info("console: sign-in refused", "reason", err)
		c.renderLogin(w, http.StatusUnauthorized, "The credential was not accepted.")
		return
	}
	c.opt.Logger.Error("console: sign-in failed", "error", err)
	c.renderLogin(w, http.StatusServiceUnavailable, "Signing in failed; try again in a moment.")
}

func (c *console) signIn(w http.ResponseWriter, r *http.Request, id identity.Identity) {
	if err := c.startSession(w, r, id); err != nil {
		c.opt.Logger.Error("console: starting a session failed", "error", err)
		c.renderLogin(w, http.StatusInternalServerError, "Signing in failed; try again in a moment.")
		return
	}
	http.Redirect(w, r, "/console/approvals", http.StatusSeeOther)
}
