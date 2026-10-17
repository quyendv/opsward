package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func remoteConfig(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "o.db")
	return writeConfig(t, "identity: {mode: remote, apiTokens: {maxTTL: 30d}}\nserver: {http: {address: 127.0.0.1:0}}\nstorage: {path: "+db+"}\naudit: {stream: {type: file, path: "+db+".audit}}\n")
}

func TestTokenLifecycle(t *testing.T) {
	cfg := remoteConfig(t)
	code, out, errOut := runCLI("token", "create", "--config", cfg, "--subject", "ci@example.com", "--group", "deployers", "--group", "team-a", "--ttl", "7d", "--description", "release pipeline")
	if code != 0 {
		t.Fatalf("create: code %d, stderr %q", code, errOut)
	}
	token := strings.TrimSpace(out)
	if !strings.HasPrefix(token, "opw_") || strings.Count(out, "\n") != 1 {
		t.Fatalf("stdout must be the token alone, got %q", out)
	}
	id := strings.SplitN(strings.TrimPrefix(token, "opw_"), "_", 2)[0]
	if !strings.Contains(errOut, id) || !strings.Contains(errOut, "shown once") {
		t.Fatalf("stderr %q should name the token and warn that it is shown once", errOut)
	}

	code, out, _ = runCLI("token", "list", "--config", cfg)
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "ci@example.com") || !strings.Contains(out, "deployers,team-a") ||
		!strings.Contains(out, "active") || !strings.Contains(out, "release pipeline") {
		t.Fatalf("list: code %d, stdout %q", code, out)
	}
	if strings.Contains(out, strings.TrimPrefix(token, "opw_"+id+"_")) {
		t.Fatal("list shows the secret")
	}

	if code, _, errOut = runCLI("token", "revoke", "--config", cfg, id); code != 0 {
		t.Fatalf("revoke: code %d, stderr %q", code, errOut)
	}
	if _, out, _ = runCLI("token", "list", "--config", cfg); !strings.Contains(out, "revoked") {
		t.Fatalf("after revoke, list = %q", out)
	}
	if code, out, errOut = runCLI("audit", "verify", "--config", cfg); code != 0 || !strings.Contains(out, "4 records verified") {
		t.Fatalf("audit verify: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestTokenRejects(t *testing.T) {
	cfg := remoteConfig(t)
	local := writeConfig(t, "identity: {principal: {subject: me}}\nstorage: {path: "+filepath.Join(t.TempDir(), "o.db")+"}\n")
	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"no subject":     {[]string{"token", "create", "--config", cfg}, "--subject is required"},
		"over max ttl":   {[]string{"token", "create", "--config", cfg, "--subject", "a", "--ttl", "60d"}, "maxTTL"},
		"bad ttl":        {[]string{"token", "create", "--config", cfg, "--subject", "a", "--ttl", "soon"}, "invalid duration"},
		"local mode":     {[]string{"token", "list", "--config", local}, "remote mode"},
		"unknown token":  {[]string{"token", "revoke", "--config", cfg, "aaaaaaaaaaaa"}, `no API token "aaaaaaaaaaaa"`},
		"revoke no id":   {[]string{"token", "revoke", "--config", cfg}, "usage: opsward token revoke"},
		"unknown action": {[]string{"token", "rotate", "--config", cfg}, "usage:"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := runCLI(tt.args...)
			if code == 0 || !strings.Contains(errOut, tt.want) {
				t.Fatalf("code %d, stderr %q, want %q", code, errOut, tt.want)
			}
		})
	}
}

// serve in remote mode answers on its address and stops with its context.
func TestServeRemote(t *testing.T) {
	db := filepath.Join(t.TempDir(), "o.db")
	addr := freeAddr(t)
	cfg := writeConfig(t, "identity: {mode: remote}\nserver: {http: {address: "+addr+"}}\nstorage: {path: "+db+"}\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		var stderr bytes.Buffer
		done <- run(ctx, []string{"serve", "--config", cfg}, io.Discard, &stderr)
	}()
	var resp *http.Response
	var err error
	for range 100 {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/readyz", nil)
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d", resp.StatusCode)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/mcp", strings.NewReader("{}"))
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /mcp = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exited %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// Revoking by the whole token never prints its secret.
func TestTokenRevokeByTheWholeTokenHidesTheSecret(t *testing.T) {
	cfg := remoteConfig(t)
	code, out, errOut := runCLI("token", "create", "--config", cfg, "--subject", "ci@example.com", "--ttl", "1d")
	if code != 0 {
		t.Fatalf("create: code %d, stderr %q", code, errOut)
	}
	token := strings.TrimSpace(out)
	id := strings.SplitN(strings.TrimPrefix(token, "opw_"), "_", 2)[0]
	secret := strings.TrimPrefix(token, "opw_"+id+"_")
	code, out, errOut = runCLI("token", "revoke", "--config", cfg, token)
	if code != 0 {
		t.Fatalf("revoke: code %d, stderr %q", code, errOut)
	}
	if strings.Contains(out+errOut, secret) || !strings.Contains(errOut, id) {
		t.Fatalf("stdout %q, stderr %q: want the ID, never the secret", out, errOut)
	}
}
