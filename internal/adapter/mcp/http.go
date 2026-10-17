package mcpadapter

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/identity"
)

// AdapterHTTP identifies calls that arrived over MCP over HTTP.
const AdapterHTTP = "mcp-http"

// Limits from docs/design/0005.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 2 * time.Minute
	idleTimeout       = 2 * time.Minute
	shutdownTimeout   = 30 * time.Second
	// callTimeout ends a call early enough that its error still fits in the response.
	callTimeout = writeTimeout - 10*time.Second
	// outcomeGrace is how long cancelled calls get to write their outcome records.
	outcomeGrace = 5 * time.Second
	maxBodyBytes = 4 << 20
)

// Authenticate returns the principal a bearer token names and when its credential
// expires. An error that wraps identity.ErrUnauthenticated rejects the token; any other
// error means authentication could not be decided.
type Authenticate func(ctx context.Context, token string) (core.Principal, time.Time, error)

// HTTPOptions configures MCP over HTTP.
type HTTPOptions struct {
	Authenticate Authenticate
	// Ready reports whether the process can serve calls, for /readyz.
	Ready  func(context.Context) error
	TLS    *TLSFiles
	Logger *slog.Logger
	// CallTimeout bounds each tool call; default a little under the write timeout.
	CallTimeout time.Duration
	// ShutdownTimeout is how long calls in flight may finish once serving stops;
	// default 30 seconds.
	ShutdownTimeout time.Duration
	// Routes are other endpoints, such as the alert webhook (docs/design/0012), served
	// for the principal of the request's bearer token like /mcp.
	Routes map[string]PrincipalHandler
	// Handlers are endpoints that authenticate requests themselves, such as the console
	// (docs/design/0015), served as they are.
	Handlers map[string]http.Handler
	// Resource, when set, is advertised as RFC 9728 protected resource metadata and in
	// every 401 challenge, so that OAuth clients can find the authorization server.
	Resource *Resource
}

// PrincipalHandler serves a request for an authenticated principal.
type PrincipalHandler func(w http.ResponseWriter, r *http.Request, p core.Principal)

// Resource describes the MCP endpoint as an OAuth protected resource.
type Resource struct {
	URL                  string // the MCP endpoint as clients reach it
	AuthorizationServers []string
	Scopes               []string
}

// TLSFiles names a certificate and key, re-read when either file changes.
type TLSFiles struct {
	CertFile, KeyFile string
}

type principalKey struct{}

// NewHTTPHandler serves MCP at /mcp, statelessly, for the principal each request's
// bearer token names, and health endpoints at /healthz and /readyz.
func NewHTTPHandler(e *engine.Engine, opt HTTPOptions) http.Handler {
	return newHTTPHandler(e, opt, context.Background())
}

// newHTTPHandler is NewHTTPHandler whose calls also end when stop does.
func newHTTPHandler(e *engine.Engine, opt HTTPOptions, stop context.Context) http.Handler {
	log := logger(opt.Logger)
	timeout := opt.CallTimeout
	if timeout <= 0 {
		timeout = callTimeout
	}
	srv := newServer(e, principalFromRequest, func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		release := context.AfterFunc(stop, cancel)
		return ctx, func() { release(); cancel() }
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, Logger: log,
		MaxRequestBodyBytes: maxBodyBytes, PropagateRequestCancellation: true,
	})
	// authenticate decides; the SDK's middleware then carries the result into each
	// call's RequestExtra, which is the one path that reaches tool handlers.
	carry := auth.RequireBearerToken(func(_ context.Context, _ string, r *http.Request) (*auth.TokenInfo, error) {
		a, ok := r.Context().Value(principalKey{}).(authenticated)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: a.p.Subject, Expiration: a.expires, Extra: map[string]any{"principal": a.p}}, nil
	}, nil)

	challenge := `Bearer realm="opsward"`
	mux := http.NewServeMux()
	if r := opt.Resource; r != nil {
		meta := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource: r.URL, AuthorizationServers: r.AuthorizationServers, ScopesSupported: r.Scopes,
			BearerMethodsSupported: []string{"header"},
		})
		mux.Handle("/.well-known/oauth-protected-resource", meta)
		mux.Handle("/.well-known/oauth-protected-resource/mcp", meta)
		// Where RFC 9728 puts it for a resource behind a path, such as a shared host.
		if u, err := url.Parse(r.URL); err == nil && u.Path != "/mcp" && u.Path != "" {
			mux.Handle("/.well-known/oauth-protected-resource"+u.Path, meta)
		}
		challenge = fmt.Sprintf("Bearer resource_metadata=%q", metadataURL(r.URL))
		if len(r.Scopes) > 0 {
			challenge += fmt.Sprintf(", scope=%q", strings.Join(r.Scopes, " "))
		}
	}
	mux.Handle("/mcp", http.NewCrossOriginProtection().Handler(authenticate(opt.Authenticate, challenge, log, singleMessage(carry(h)))))
	for pattern, h := range opt.Routes {
		mux.Handle(pattern, authenticate(opt.Authenticate, challenge, log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a, _ := r.Context().Value(principalKey{}).(authenticated)
			h(w, r, a.p)
		})))
	}
	for pattern, h := range opt.Handlers {
		mux.Handle(pattern, h)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if opt.Ready != nil {
			if err := opt.Ready(r.Context()); err != nil {
				log.Warn("not ready", "error", err)
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

type authenticated struct {
	p       core.Principal
	expires time.Time
}

// authenticate resolves the bearer token. Why a token was rejected is logged, never
// sent: the client learns only that it was.
func authenticate(fn Authenticate, challenge string, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			unauthorized(w, challenge)
			return
		}
		p, exp, err := fn(r.Context(), strings.TrimSpace(token))
		switch {
		case err != nil && r.Context().Err() != nil:
			log.Debug("client went away during authentication", "remote", r.RemoteAddr)
			return
		case errors.Is(err, identity.ErrUnauthenticated):
			log.Warn("authentication rejected", "remote", r.RemoteAddr, "reason", err.Error())
			unauthorized(w, challenge)
			return
		case err != nil:
			log.Error("authentication failed", "remote", r.RemoteAddr, "error", err)
			http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		p.Adapter = AdapterHTTP
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, authenticated{p, exp})))
	})
}

// singleMessage rejects JSON-RPC batches, which the oldest protocol version allows: no
// client sends them, and one request would otherwise carry thousands of calls.
func singleMessage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "cannot read request body", http.StatusBadRequest)
			return
		}
		if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '[' {
			http.Error(w, "JSON-RPC batches are not supported", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l
}

func unauthorized(w http.ResponseWriter, challenge string) {
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, "invalid or missing bearer token", http.StatusUnauthorized)
}

// metadataURL is where RFC 9728 places a resource's metadata: the well-known path
// inserted between the host and the resource's path.
func metadataURL(resource string) string {
	u, err := url.Parse(resource)
	if err != nil {
		return ""
	}
	u.Path = "/.well-known/oauth-protected-resource" + u.Path
	return u.String()
}

// principalFromRequest fails closed: a call without an authenticated principal is
// never run.
func principalFromRequest(req *mcp.CallToolRequest) (core.Principal, error) {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		if p, ok := req.Extra.TokenInfo.Extra["principal"].(core.Principal); ok && p.Subject != "" {
			return p, nil
		}
	}
	return core.Principal{}, errors.New("no authenticated principal")
}

// ServeHTTP serves NewHTTPHandler on ln until ctx is done, then stops accepting and lets
// calls in flight finish for ShutdownTimeout. Calls still running then are cancelled and
// get a few seconds to write their outcome records before connections are closed.
func ServeHTTP(ctx context.Context, ln net.Listener, e *engine.Engine, opt HTTPOptions) error {
	log := logger(opt.Logger)
	stop, stopCalls := context.WithCancel(context.WithoutCancel(ctx))
	defer stopCalls()
	srv := &http.Server{
		Handler:           newHTTPHandler(e, opt, stop),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	if opt.TLS != nil {
		c := &certFiles{TLSFiles: *opt.TLS, log: log}
		if _, err := c.get(nil); err != nil {
			return fmt.Errorf("mcp http: %w", err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.get}
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("mcp http: %w", err)
	case <-ctx.Done():
	}
	grace := opt.ShutdownTimeout
	if grace <= 0 {
		grace = shutdownTimeout
	}
	if err := shutdown(ctx, srv, grace); err != nil {
		log.Warn("calls still running after the shutdown grace period are cancelled", "grace", grace)
		stopCalls()
		if err := shutdown(ctx, srv, outcomeGrace); err != nil {
			log.Warn("closing connections of calls that did not end in time")
			_ = srv.Close()
		}
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("mcp http: %w", err)
	}
	return nil
}

func shutdown(ctx context.Context, srv *http.Server, d time.Duration) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
	defer cancel()
	return srv.Shutdown(sctx)
}

// stamp identifies a version of the certificate and key files.
type stamp struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
}

// certFiles loads a key pair again whenever either file changes, so a rotated
// certificate is served without a restart. A pair that fails to load is logged once and
// not retried until the files change again; meanwhile the previous pair is served.
type certFiles struct {
	TLSFiles
	log    *slog.Logger
	mu     sync.Mutex
	cert   *tls.Certificate
	loaded stamp
	failed stamp
}

func (c *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	ci, cerr := os.Stat(c.CertFile)
	ki, kerr := os.Stat(c.KeyFile)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := errors.Join(cerr, kerr); err != nil {
		if c.cert != nil {
			return c.cert, nil // files caught mid-rotation
		}
		return nil, fmt.Errorf("tls key pair: %w", err)
	}
	now := stamp{ci.ModTime(), ki.ModTime(), ci.Size(), ki.Size()}
	if c.cert != nil && (now == c.loaded || now == c.failed) {
		return c.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		if c.cert == nil {
			return nil, fmt.Errorf("tls key pair: %w", err)
		}
		c.failed = now
		logger(c.log).Error("the TLS certificate files changed but cannot be loaded; the previous certificate is still served", "error", err)
		return c.cert, nil
	}
	c.cert, c.loaded = &cert, now
	return c.cert, nil
}
