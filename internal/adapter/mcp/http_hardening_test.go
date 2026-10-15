package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
)

// logBuffer collects log output safely across goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestHTTPRejectsBatchesAndOversizedBodies(t *testing.T) {
	srv := httptest.NewServer(NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{Authenticate: tokens(map[string]string{"ta": "a"})}))
	defer srv.Close()
	auth := map[string]string{"Authorization": "Bearer ta"}
	if status, body := postBody(t, srv.URL+"/mcp", auth, ` [{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`); status != http.StatusBadRequest || !strings.Contains(body, "batch") {
		t.Fatalf("batch = %d %q", status, body)
	}
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"` + strings.Repeat("a", 4<<20) + `"}}`
	if status, _ := postBody(t, srv.URL+"/mcp", auth, big); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d", status)
	}
	if status, body := post(t, srv.URL+"/mcp", auth); status != http.StatusOK {
		t.Fatalf("single request = %d %q", status, body)
	}
}

// A client that goes away while its token is checked is not an authentication failure.
func TestHTTPClientGoneDuringAuthenticationIsNotAnError(t *testing.T) {
	var logs logBuffer
	h := NewHTTPHandler(httpEngine(t, &nopAuditor{}), HTTPOptions{
		Authenticate: func(ctx context.Context, _ string) (core.Principal, time.Time, error) {
			return core.Principal{}, time.Time{}, ctx.Err()
		},
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer x")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("logged as an error: %s", logs.String())
	}
}

// blockTool runs until its context ends, and reports that it did.
func blockTool(started chan<- struct{}, ended chan<- error) engine.Tool {
	return engine.Tool{
		Name: "block", Description: "block", Risk: core.RiskRead,
		InputSchema: &jsonschema.Schema{Type: "object"},
		Prepare: func(json.RawMessage) (*engine.Prepared, error) {
			return &engine.Prepared{
				Scope: core.Scope{Environment: "staging"}, Args: map[string]any{},
				Run: func(ctx context.Context, _ string) (any, error) {
					started <- struct{}{}
					<-ctx.Done()
					ended <- ctx.Err()
					return nil, ctx.Err()
				},
			}, nil
		},
	}
}

func blockEngine(t *testing.T, aud *nopAuditor, started chan struct{}, ended chan error) *engine.Engine {
	t.Helper()
	e, err := engine.New(engine.Options{
		Tools:        []engine.Tool{blockTool(started, ended)},
		Environments: []engine.Environment{{Name: "staging", Namespaces: []string{"*"}}},
		Authorizer:   authorizerFunc(allowAll), Auditor: aud,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A call that outlives its deadline ends as an error the client can read, before the
// server's write timeout would cut the response.
func TestHTTPCallDeadline(t *testing.T) {
	started, ended := make(chan struct{}, 1), make(chan error, 1)
	srv := httptest.NewServer(NewHTTPHandler(blockEngine(t, &nopAuditor{}, started, ended), HTTPOptions{
		Authenticate: tokens(map[string]string{"ta": "a"}), CallTimeout: 100 * time.Millisecond,
	}))
	defer srv.Close()
	cs := httpClient(t, srv.URL+"/mcp", "ta")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "block", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "deadline") {
		t.Fatalf("call = %+v, %v", res, err)
	}
}

// When calls outlast the drain, shutdown cancels them, lets their outcomes be audited,
// and returns without an error.
func TestServeHTTPCancelsCallsThatOutlastTheDrain(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, ended := make(chan struct{}, 1), make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeHTTP(ctx, ln, blockEngine(t, &nopAuditor{}, started, ended), HTTPOptions{
			Authenticate: tokens(map[string]string{"ta": "a"}), ShutdownTimeout: 100 * time.Millisecond,
		})
	}()
	cs := httpClient(t, "http://"+ln.Addr().String()+"/mcp", "ta")
	go func() {
		_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "block", Arguments: map[string]any{}})
	}()
	<-started
	cancel()
	// Well under outcomeGrace: the call is cancelled rather than waited out.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP = %v", err)
		}
	case <-time.After(outcomeGrace / 2):
		t.Fatal("ServeHTTP did not return in time")
	}
	select {
	case <-ended:
	default:
		t.Fatal("the call was not cancelled")
	}
}

func TestCertReloadFailureIsLoggedOnceAndTheCachedPairServes(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, dir, "first")
	var logs logBuffer
	c := &certFiles{TLSFiles: TLSFiles{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}, log: slog.New(slog.NewJSONHandler(&logs, nil))}
	first, err := c.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A key that does not match the certificate.
	other := t.TempDir()
	writeCert(t, other, "other")
	key, _ := os.ReadFile(filepath.Join(other, "tls.key"))                          //nolint:gosec // G304: the test's own t.TempDir
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), key, 0o600); err != nil { //nolint:gosec // G703: as above
		t.Fatal(err)
	}
	for range 5 {
		got, err := c.get(nil)
		if err != nil || got != first {
			t.Fatalf("with a broken pair: %v, %v", got, err)
		}
	}
	if n := strings.Count(logs.String(), `"level":"ERROR"`); n != 1 {
		t.Fatalf("logged %d times, want once: %s", n, logs.String())
	}
	for _, f := range []string{"tls.crt", "tls.key"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := c.get(nil); err != nil || got != first {
		t.Fatalf("with the files gone: %v, %v", got, err)
	}
}
