package mcpadapter

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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
}

// TLSFiles names a certificate and key, re-read when either file changes.
type TLSFiles struct {
	CertFile, KeyFile string
}

type principalKey struct{}

// NewHTTPHandler serves MCP at /mcp, statelessly, for the principal each request's
// bearer token names, and health endpoints at /healthz and /readyz.
func NewHTTPHandler(e *engine.Engine, opt HTTPOptions) http.Handler {
	log := opt.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := newServer(e, principalFromRequest)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, Logger: log,
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

	mux := http.NewServeMux()
	mux.Handle("/mcp", http.NewCrossOriginProtection().Handler(authenticate(opt.Authenticate, log, carry(h))))
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
func authenticate(fn Authenticate, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			unauthorized(w)
			return
		}
		p, exp, err := fn(r.Context(), strings.TrimSpace(token))
		switch {
		case errors.Is(err, identity.ErrUnauthenticated):
			log.Warn("authentication rejected", "remote", r.RemoteAddr, "reason", err.Error())
			unauthorized(w)
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

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="opsward"`)
	http.Error(w, "invalid or missing bearer token", http.StatusUnauthorized)
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

// ServeHTTP serves NewHTTPHandler on ln until ctx is done, then stops accepting and
// lets calls in flight finish for up to 30 seconds.
func ServeHTTP(ctx context.Context, ln net.Listener, e *engine.Engine, opt HTTPOptions) error {
	srv := &http.Server{
		Handler:           NewHTTPHandler(e, opt),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	if opt.TLS != nil {
		c := &certFiles{TLSFiles: *opt.TLS}
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
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("mcp http: shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("mcp http: %w", err)
	}
	return nil
}

// certFiles loads a key pair again whenever either file's modification time changes,
// so a rotated certificate is served without a restart.
type certFiles struct {
	TLSFiles
	mu       sync.Mutex
	cert     *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	certSize int64
	keySize  int64
}

func (c *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	ci, err := os.Stat(c.CertFile)
	if err != nil {
		return nil, fmt.Errorf("tls certificate: %w", err)
	}
	ki, err := os.Stat(c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls key: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && ci.ModTime().Equal(c.certMod) && ki.ModTime().Equal(c.keyMod) && ci.Size() == c.certSize && ki.Size() == c.keySize {
		return c.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		if c.cert != nil {
			// A pair caught mid-rotation: keep serving the previous one.
			return c.cert, nil
		}
		return nil, fmt.Errorf("tls key pair: %w", err)
	}
	c.cert, c.certMod, c.keyMod, c.certSize, c.keySize = &cert, ci.ModTime(), ki.ModTime(), ci.Size(), ki.Size()
	return c.cert, nil
}
