package console

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/identity"
	"github.com/quyendv/opsward/internal/identity/oidctest"
	"github.com/quyendv/opsward/internal/policy"
	"github.com/quyendv/opsward/internal/store"
	"github.com/quyendv/opsward/internal/store/sqlite"
)

// call is one tool call the console made.
type call struct {
	p    core.Principal
	tool string
	args map[string]any
}

// fakeEngine records calls and answers with canned data or an error per tool.
type fakeEngine struct {
	mu    sync.Mutex
	calls []call
	data  map[string]any
	errs  map[string]error
}

func (f *fakeEngine) Invoke(_ context.Context, p core.Principal, name string, raw json.RawMessage) (*engine.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	f.calls = append(f.calls, call{p, name, args})
	if err := f.errs[name]; err != nil {
		return nil, err
	}
	return &engine.Result{Data: f.data[name]}, nil
}

func (f *fakeEngine) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type env struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	st     store.Store
	auth   *identity.Authenticator
	eng    *fakeEngine
	is     *oidctest.Issuer
	now    time.Time
}

const clientID = "opsward-console"

func newEnv(t *testing.T, opt Options) *env {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{t: t, st: st, eng: &fakeEngine{data: map[string]any{"list_approvals": []any{}}, errs: map[string]error{}}, is: oidctest.New(t), now: time.Now()}
	e.auth = identity.New(identity.Options{Store: st, Groups: map[string][]string{"platform": {"alice@example.com"}}, OIDC: &identity.OIDCOptions{
		Issuer: e.is.URL, Audience: "https://opsward.example.com/mcp", SubjectClaim: "email", GroupsClaim: "groups", GroupsPrefix: "oidc:",
	}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.auth.Discover(ctx)
	for range 200 {
		if _, _, ok := e.auth.Endpoints(); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	opt.Engine, opt.Identity, opt.Sessions = e.eng, e.auth, st
	if opt.SessionTTL == 0 {
		opt.SessionTTL = 8 * time.Hour
	}
	opt.Now = func() time.Time { return e.now }
	h, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	// TLS, as browsers keep __Host- cookies only over HTTPS.
	e.srv = httptest.NewTLSServer(h)
	t.Cleanup(e.srv.Close)
	if opt.OIDC != nil {
		opt.OIDC.RedirectURL = e.srv.URL + "/console/callback"
	}
	jar, _ := cookiejar.New(nil)
	e.client = e.srv.Client()
	e.client.Jar = jar
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return e
}

// reply is a response, read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func (e *env) do(method, path string, form url.Values, header ...string) reply {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, Body: string(b)}
}

func (e *env) token(subject string, groups ...string) (string, store.APIToken) {
	e.t.Helper()
	tok, rec, err := e.auth.CreateToken(context.Background(), identity.TokenRequest{Subject: subject, Groups: groups, TTL: time.Hour, MaxTTL: time.Hour})
	if err != nil {
		e.t.Fatal(err)
	}
	return tok, rec
}

func (e *env) signInWithToken(subject string, groups ...string) store.APIToken {
	e.t.Helper()
	tok, rec := e.token(subject, groups...)
	resp := e.do("POST", "/console/login/token", url.Values{"token": {tok}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/console/approvals" {
		e.t.Fatalf("sign-in: %d %s", resp.StatusCode, resp.Body)
	}
	return rec
}

func TestEveryResponseCarriesTheBrowserProtections(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	resp := e.do("GET", "/console/login", nil)
	h := resp.Header
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", h)
	}
	if body := resp.Body; strings.Contains(body, "<script") {
		t.Fatal("the console serves script")
	}
}

func TestPagesNeedASession(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	for _, path := range []string{"/console/approvals", "/console/audit", "/console/you"} {
		resp := e.do("GET", path, nil)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/console/login" {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if resp := e.do("POST", "/console/approvals/decide", url.Values{"id": {"x"}, "decision": {"approve"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("decide without a session: %d", resp.StatusCode)
	}
	if len(e.eng.calls) != 0 {
		t.Fatalf("calls without a session: %+v", e.eng.calls)
	}
}

func TestSignInWithAnAPIToken(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	rec := e.signInWithToken("alice@example.com", "team-a")
	u, _ := url.Parse(e.srv.URL)
	cookies := e.client.Jar.Cookies(u)
	if len(cookies) != 1 || cookies[0].Name != "__Host-opsward-session" {
		t.Fatalf("cookies = %v", cookies)
	}
	// The store keeps the identifier's hash, never the identifier.
	sum := sha256.Sum256([]byte(cookies[0].Value))
	s, ok, err := e.st.Session(context.Background(), sum[:])
	if err != nil || !ok || s.Subject != "alice@example.com" || s.Credential != "token:"+rec.ID || len(s.Groups) != 0 {
		t.Fatalf("session = %+v, %v, %v", s, ok, err)
	}
	if !s.ExpiresAt.Equal(rec.ExpiresAt) {
		t.Fatalf("the session ends %v, after its token (%v)", s.ExpiresAt, rec.ExpiresAt)
	}
	if resp := e.do("GET", "/console/approvals", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approvals: %d", resp.StatusCode)
	}
	c := e.eng.last()
	if c.tool != "list_approvals" || c.p.Subject != "alice@example.com" || c.p.Adapter != "console" ||
		c.p.Credential != "token:"+rec.ID || !slices.Equal(c.p.Groups, []string{"platform", "team-a"}) {
		t.Fatalf("call = %+v", c)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	tok, _ := e.token("alice@example.com")
	resp := e.do("POST", "/console/login/token", url.Values{"token": {tok}})
	raw := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"__Host-opsward-session=", "Path=/", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("Set-Cookie %q lacks %s", raw, want)
		}
	}
	if strings.Contains(raw, "Domain=") {
		t.Fatalf("Set-Cookie %q names a domain", raw)
	}
}

func TestSignInRefusals(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	for _, tok := range []string{"not-a-token", "opw_aaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		resp := e.do("POST", "/console/login/token", url.Values{"token": {tok}})
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Set-Cookie") != "" {
			t.Fatalf("%s: %d, Set-Cookie %q", tok, resp.StatusCode, resp.Header.Get("Set-Cookie"))
		}
	}
	off := newEnv(t, Options{})
	tok, _ := off.token("alice@example.com")
	if resp := off.do("POST", "/console/login/token", url.Values{"token": {tok}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("API token sign-in while disabled: %d", resp.StatusCode)
	}
}

// Revoking the token ends the sessions started with it at their next request.
func TestRevokingTheTokenEndsTheSession(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	rec := e.signInWithToken("alice@example.com")
	if _, err := e.st.RevokeAPIToken(context.Background(), rec.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp := e.do("GET", "/console/approvals", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/console/login" {
		t.Fatalf("after revocation: %d", resp.StatusCode)
	}
	if len(e.eng.calls) != 0 {
		t.Fatal("a call was made for a revoked token")
	}
}

func TestSessionsExpire(t *testing.T) {
	e := newEnv(t, Options{APITokens: true, SessionTTL: time.Minute})
	e.signInWithToken("alice@example.com")
	e.now = e.now.Add(time.Minute)
	if resp := e.do("GET", "/console/approvals", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("an expired session: %d", resp.StatusCode)
	}
}

func TestSignOutEndsTheSession(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	e.signInWithToken("alice@example.com")
	u, _ := url.Parse(e.srv.URL)
	value := e.client.Jar.Cookies(u)[0].Value
	if resp := e.do("POST", "/console/logout", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign out: %d", resp.StatusCode)
	}
	sum := sha256.Sum256([]byte(value))
	if _, ok, _ := e.st.Session(context.Background(), sum[:]); ok {
		t.Fatal("the session is still stored")
	}
	// The old cookie, replayed, opens nothing.
	e.client.Jar.SetCookies(u, []*http.Cookie{{Name: "__Host-opsward-session", Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}})
	if resp := e.do("GET", "/console/approvals", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a replayed cookie: %d", resp.StatusCode)
	}
}

func TestCrossSiteRequestsAreRefused(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	e.signInWithToken("alice@example.com")
	resp := e.do("POST", "/console/approvals/decide", url.Values{"id": {"a1"}, "decision": {"approve"}}, "Sec-Fetch-Site", "cross-site")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-site decision: %d", resp.StatusCode)
	}
	resp = e.do("POST", "/console/approvals/decide", url.Values{"id": {"a1"}, "decision": {"approve"}}, "Origin", "https://evil.example.com")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a decision from another origin: %d", resp.StatusCode)
	}
	if len(e.eng.calls) != 0 {
		t.Fatalf("a cross-site decision reached the engine: %+v", e.eng.calls)
	}
}

func TestDecideCallsTheTool(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	e.signInWithToken("bob@example.com")
	resp := e.do("POST", "/console/approvals/decide", url.Values{"id": {"a1"}, "decision": {"approve"}, "reason": {" looks right "}}, "Sec-Fetch-Site", "same-origin")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/console/approvals?") {
		t.Fatalf("decide: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	c := e.eng.last()
	if c.tool != "decide_approval" || c.args["id"] != "a1" || c.args["decision"] != "approve" || c.args["reason"] != "looks right" || c.p.Adapter != "console" {
		t.Fatalf("call = %+v", c)
	}
	e.eng.errs["decide_approval"] = &engine.DeniedError{Decision: core.Decision{Effect: core.EffectDeny, Rule: "no-self"}, Reason: "an approver cannot decide their own request"}
	resp = e.do("POST", "/console/approvals/decide", url.Values{"id": {"a1"}, "decision": {"approve"}}, "Sec-Fetch-Site", "same-origin")
	if body := resp.Body; resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "an approver cannot decide their own request") {
		t.Fatalf("a denied decision: %d %s", resp.StatusCode, body)
	}
	// Other failures are not shown: they may say more than the person needs.
	e.eng.errs["decide_approval"] = errors.New("database at 10.0.0.1 is down")
	resp = e.do("POST", "/console/approvals/decide", url.Values{"id": {"a1"}, "decision": {"approve"}}, "Sec-Fetch-Site", "same-origin")
	if body := resp.Body; strings.Contains(body, "10.0.0.1") || !strings.Contains(body, "Opsward&#39;s log") {
		t.Fatalf("a failed decision: %s", body)
	}
}

func TestApprovalsAreEscaped(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	e.eng.data["list_approvals"] = []any{map[string]any{
		"id": "a1", "status": "pending", "requester": "carol", "tool": "scale_workload", "canDecide": true,
		"scope": map[string]any{"environment": "staging", "namespace": "team-a", "kind": "Deployment", "name": "api"},
		"diff":  "spec.replicas: 3 -> 5 <script>alert(1)</script>",
	}}
	e.signInWithToken("bob@example.com")
	body := e.do("GET", "/console/approvals", nil).Body
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("the diff was not escaped:\n%s", body)
	}
	if !strings.Contains(body, `value="approve"`) {
		t.Fatal("a request the person may decide has no form")
	}
}

func TestAuditSearchesThroughTheTool(t *testing.T) {
	e := newEnv(t, Options{APITokens: true})
	e.eng.data["search_audit"] = map[string]any{
		"records": []any{map[string]any{
			"id": "r1", "time": "2026-10-17T08:30:00Z", "phase": "outcome", "tool": "restart_workload",
			"principal": map[string]any{"subject": "carol", "adapter": "mcp-http"},
			"scope":     map[string]any{"environment": "staging", "namespace": "team-a"},
			"outcome":   map[string]any{"status": "ok"},
		}},
		"next": "CURSOR",
	}
	e.signInWithToken("alice@example.com")
	body := e.do("GET", "/console/audit?since=2026-10-17T08:00&tool=restart_workload&status=", nil).Body
	c := e.eng.last()
	if c.tool != "search_audit" || c.args["since"] != "2026-10-17T08:00:00Z" || c.args["tool"] != "restart_workload" || c.args["status"] != nil {
		t.Fatalf("call = %+v", c)
	}
	if !strings.Contains(body, "carol") || !strings.Contains(body, "before=CURSOR") {
		t.Fatalf("page:\n%s", body)
	}
	if resp := e.do("GET", "/console/audit?since=yesterday", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad time: %d", resp.StatusCode)
	}
}

func TestYouShowsWhatPolicySaysAboutThePerson(t *testing.T) {
	var asked core.Principal
	e := newEnv(t, Options{APITokens: true, Naming: func(p core.Principal) ([]policy.BindingView, []policy.BindingView) {
		asked = p
		return []policy.BindingView{{Binding: policy.Binding{Name: "team-a-views", Role: "viewer"}}}, nil
	}})
	e.signInWithToken("alice@example.com")
	body := e.do("GET", "/console/you", nil).Body
	if asked.Subject != "alice@example.com" || !strings.Contains(body, "team-a-views") || !strings.Contains(body, "platform") {
		t.Fatalf("asked %+v; page:\n%s", asked, body)
	}
}

func TestSignInWithTheIssuer(t *testing.T) {
	o := &OIDC{ClientID: clientID, ClientSecret: "s3cret", Scopes: []string{"groups"}}
	e := newEnv(t, Options{OIDC: o})
	resp := e.do("GET", "/console/login/oidc", nil)
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || err != nil || loc.Path != "/auth" {
		t.Fatalf("start: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	q := loc.Query()
	if q.Get("client_id") != clientID || q.Get("response_type") != "code" || q.Get("scope") != "openid groups" ||
		q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != e.srv.URL+"/console/callback" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatalf("authorization request: %v", q)
	}
	var form url.Values
	e.is.Token = func(f url.Values) (int, map[string]any) {
		form = f
		claims := e.is.Claims(clientID, map[string]any{"nonce": q.Get("nonce")})
		return http.StatusOK, map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": e.is.Sign(claims)}
	}
	resp = e.do("GET", "/console/callback?"+url.Values{"code": {"c-1"}, "state": {q.Get("state")}}.Encode(), nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/console/approvals" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Body)
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if form.Get("code") != "c-1" || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Fatalf("token request: %v", form)
	}
	if resp := e.do("GET", "/console/approvals", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approvals: %d", resp.StatusCode)
	}
	c := e.eng.last()
	if c.p.Subject != "alice@example.com" || c.p.Credential != "oidc" || !slices.Equal(c.p.Groups, []string{"oidc:admins", "oidc:team-a", "platform"}) {
		t.Fatalf("principal = %+v", c.p)
	}
}

func TestCallbackRefusals(t *testing.T) {
	o := &OIDC{ClientID: clientID}
	e := newEnv(t, Options{OIDC: o})
	// No sign-in was started in this browser.
	if resp := e.do("GET", "/console/callback?code=c&state=s", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without a sign-in: %d", resp.StatusCode)
	}
	start := func() url.Values {
		loc, _ := url.Parse(e.do("GET", "/console/login/oidc", nil).Header.Get("Location"))
		return loc.Query()
	}
	start()
	if resp := e.do("GET", "/console/callback?code=c&state=other", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("another state: %d", resp.StatusCode)
	}
	// An ID token for another sign-in's nonce.
	q := start()
	e.is.Token = func(url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": e.is.Sign(e.is.Claims(clientID, map[string]any{"nonce": "replayed"}))}
	}
	if resp := e.do("GET", "/console/callback?"+url.Values{"code": {"c"}, "state": {q.Get("state")}}.Encode(), nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another nonce: %d", resp.StatusCode)
	}
	// The state is used once.
	q = start()
	e.is.Token = func(url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": e.is.Sign(e.is.Claims(clientID, map[string]any{"nonce": q.Get("nonce")}))}
	}
	cb := "/console/callback?" + url.Values{"code": {"c"}, "state": {q.Get("state")}}.Encode()
	if resp := e.do("GET", cb, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d", resp.StatusCode)
	}
	if resp := e.do("GET", cb, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the same callback again: %d", resp.StatusCode)
	}
	if len(e.eng.calls) != 0 {
		t.Fatal("the console called a tool while signing in")
	}
}
