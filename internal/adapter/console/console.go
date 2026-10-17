// Package console serves the web console: approvals, audit search and what policy says
// about the signed-in person, each read or decision a tool call through the engine.
// See docs/design/0015.
package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/identity"
	"github.com/quyendv/opsward/internal/policy"
	"github.com/quyendv/opsward/internal/store"
)

// Adapter identifies calls made from the console, for policy and audit.
const Adapter = "console"

const (
	sessionCookie = "__Host-opsward-session"
	loginCookie   = "__Host-opsward-login"
	// loginTTL bounds a sign-in at the issuer, from leaving to coming back.
	loginTTL      = 10 * time.Minute
	maxFormBytes  = 64 << 10
	issuerTimeout = 10 * time.Second
)

// Invoker runs a tool call through the engine.
type Invoker interface {
	Invoke(ctx context.Context, p core.Principal, name string, args json.RawMessage) (*engine.Result, error)
}

// Identity authenticates sign-ins and restores sessions' principals.
type Identity interface {
	Authenticate(ctx context.Context, token string) (core.Principal, time.Time, error)
	Principal(ctx context.Context, id identity.Identity) (core.Principal, error)
	VerifyIDToken(ctx context.Context, raw, clientID, nonce string) (identity.Identity, error)
	Endpoints() (authURL, tokenURL string, ok bool)
}

// Sessions stores console sessions.
type Sessions interface {
	CreateSession(ctx context.Context, s store.Session) error
	Session(ctx context.Context, idHash []byte) (store.Session, bool, error)
	DeleteSession(ctx context.Context, idHash []byte) error
}

// Naming returns the bindings that name a principal, and those that name it an approver.
type Naming func(core.Principal) (grants, approves []policy.BindingView)

// OIDC is the console's client at the issuer.
type OIDC struct {
	ClientID     string
	ClientSecret string // empty: a public client
	Scopes       []string
	RedirectURL  string
	HTTPClient   *http.Client // default: a client with a timeout
}

// Options configures the console.
type Options struct {
	Engine     Invoker
	Identity   Identity
	Sessions   Sessions
	Naming     Naming
	OIDC       *OIDC // nil: no sign-in with the issuer
	APITokens  bool  // sign in with an API token
	SessionTTL time.Duration
	Now        func() time.Time
	Logger     *slog.Logger
}

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type console struct {
	opt   Options
	pages map[string]*template.Template
}

// New returns the console's handler, to be served at /console/.
func New(opt Options) (http.Handler, error) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if o := opt.OIDC; o != nil && o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: issuerTimeout}
	}
	c := &console{opt: opt, pages: map[string]*template.Template{}}
	funcs := template.FuncMap{"json": prettyJSON, "time": formatTime}
	for _, name := range []string{"login", "approvals", "audit", "you"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("console: template %s: %w", name, err)
		}
		c.pages[name] = t
	}
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /console/static/", http.StripPrefix("/console/static/", cacheable(http.FileServerFS(static))))
	mux.HandleFunc("GET /console/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/approvals", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /console/login", c.loginPage)
	mux.HandleFunc("GET /console/login/oidc", c.startOIDC)
	mux.HandleFunc("GET /console/callback", c.callback)
	mux.HandleFunc("POST /console/login/token", c.tokenLogin)
	mux.HandleFunc("POST /console/logout", c.logout)
	mux.Handle("GET /console/approvals", c.signedIn(c.approvals))
	mux.Handle("POST /console/approvals/decide", c.signedIn(c.decide))
	mux.Handle("GET /console/audit", c.signedIn(c.audit))
	mux.Handle("GET /console/you", c.signedIn(c.you))
	// Browsers mark a request sent from another site; one that changes something is refused.
	return headers(http.NewCrossOriginProtection().Handler(limitBody(mux))), nil
}

// headers sets what every console response carries.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func cacheable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		next.ServeHTTP(w, r)
	})
}

// page is what every template sees.
type page struct {
	Title     string
	Nav       string
	Principal *core.Principal
	Session   *store.Session
	Error     string
	Notice    string
	Data      any
}

func (c *console) render(w http.ResponseWriter, status int, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.pages[name].Execute(w, p); err != nil {
		c.opt.Logger.Error("console: rendering failed", "page", name, "error", err)
	}
}

// signedInHandler serves a request for the session's principal.
type signedInHandler func(w http.ResponseWriter, r *http.Request, s *store.Session, p core.Principal)

// signedIn resolves the session cookie to a principal, or sends the browser to sign in.
func (c *console) signedIn(h signedInHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, p, err := c.session(r)
		if err != nil {
			if !errors.Is(err, errNoSession) {
				c.opt.Logger.Warn("console: session refused", "error", err)
			}
			clearCookie(w, sessionCookie)
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/console/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in again", http.StatusUnauthorized)
			return
		}
		h(w, r, s, p)
	})
}

var errNoSession = errors.New("no session")

func (c *console) session(r *http.Request) (*store.Session, core.Principal, error) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return nil, core.Principal{}, errNoSession
	}
	hash := hashID(ck.Value)
	s, ok, err := c.opt.Sessions.Session(r.Context(), hash)
	if err != nil {
		return nil, core.Principal{}, err
	}
	if !ok || !c.opt.Now().Before(s.ExpiresAt) {
		return nil, core.Principal{}, errNoSession
	}
	p, err := c.opt.Identity.Principal(r.Context(), identity.Identity{Subject: s.Subject, Groups: s.Groups, Credential: s.Credential})
	if err != nil {
		// A revoked or expired API token ends the session.
		_ = c.opt.Sessions.DeleteSession(context.WithoutCancel(r.Context()), hash)
		return nil, core.Principal{}, err
	}
	p.Adapter = Adapter
	return &s, p, nil
}

// startSession stores a session for id and sets its cookie.
func (c *console) startSession(w http.ResponseWriter, r *http.Request, id identity.Identity) error {
	now := c.opt.Now()
	expires := now.Add(c.opt.SessionTTL)
	// A session never outlives the API token it was started with.
	if id.Credential != "oidc" && !id.Expires.IsZero() && id.Expires.Before(expires) {
		expires = id.Expires
	}
	value := rand.Text() + rand.Text()
	s := store.Session{
		IDHash: hashID(value), Subject: id.Subject, Groups: id.Groups, Credential: id.Credential,
		CreatedAt: now, ExpiresAt: expires,
	}
	if err := c.opt.Sessions.CreateSession(r.Context(), s); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", Expires: expires,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	c.opt.Logger.Info("console: signed in", "subject", id.Subject, "credential", id.Credential, "expires", expires)
	return nil
}

func hashID(v string) []byte {
	sum := sha256.Sum256([]byte(v))
	return sum[:]
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (c *console) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil && ck.Value != "" {
		if err := c.opt.Sessions.DeleteSession(r.Context(), hashID(ck.Value)); err != nil {
			c.opt.Logger.Error("console: ending a session failed", "error", err)
			http.Error(w, "signing out failed; try again", http.StatusInternalServerError)
			return
		}
	}
	clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/console/login", http.StatusSeeOther)
}

// call runs a tool through the engine and decodes its result into out.
func (c *console) call(r *http.Request, p core.Principal, tool string, args any, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	res, err := c.opt.Engine.Invoke(r.Context(), p, tool, raw)
	if err != nil {
		return err
	}
	b, err := json.Marshal(res.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// message is what a person is told about a failed call: a denial or an invalid argument
// says why, which the engine has already redacted; anything else is logged.
func (c *console) message(tool string, err error) string {
	var denied *engine.DeniedError
	if errors.As(err, &denied) || errors.Is(err, engine.ErrInvalidArguments) || errors.Is(err, core.ErrInvalidInput) {
		return err.Error()
	}
	c.opt.Logger.Error("console: call failed", "tool", tool, "error", err)
	return "The call failed; the error is in Opsward's log."
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// randomValue is an unguessable value for state, nonce and the PKCE verifier.
func randomValue() string { return rand.Text() + rand.Text() }
