// Command opsward is a guarded gateway that lets AI agents operate infrastructure
// within the permissions an operator grants.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	mcpadapter "github.com/quyendv/opsward/internal/adapter/mcp"
	"github.com/quyendv/opsward/internal/app"
	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/version"
)

// configCheckInterval is how often serve checks its configuration file (docs/design/0003).
const configCheckInterval = 10 * time.Second

const usage = `usage: opsward <command> [flags]

commands:
  serve --config FILE          serve MCP: over stdio in local mode, over HTTP in remote mode
  validate --config FILE       check a configuration file and its policy
  audit verify --config FILE   check the stored audit chain
  token create|list|revoke     manage API tokens (opsward token for details)
  rbac render --config FILE    print the Kubernetes RBAC each profile needs
  version                      print version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "validate":
		err = validate(args[1:], stdout)
	case "serve":
		err = serve(ctx, args[1:], stderr)
	case "audit":
		if len(args) < 2 || args[1] != "verify" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		err = verify(ctx, args[2:], stdout)
	case "token":
		err = token(ctx, args[1:], stdout, stderr)
	case "rbac":
		err = rbac(args[1:], stdout)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	var u usageError
	switch {
	case errors.As(err, &u):
		fmt.Fprint(stderr, u)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "opsward: %v\n", err)
		return 1
	}
	return 0
}

func loadConfig(name string, args []string) (*config.Config, error) {
	cfg, _, _, err := loadConfigFile(name, args)
	return cfg, err
}

// loadConfigFile reads the configuration once and returns it with its path and the
// hash of the bytes that were parsed, which serve hands to the config watcher.
func loadConfigFile(name string, args []string) (*config.Config, string, string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		return nil, "", "", err
	}
	if *path == "" {
		return nil, "", "", errors.New("--config is required")
	}
	b, err := os.ReadFile(*path)
	if err != nil {
		return nil, "", "", fmt.Errorf("config: %w", err)
	}
	cfg, err := config.Parse(b)
	return cfg, *path, app.ContentHash(b), err
}

func validate(args []string, stdout io.Writer) error {
	cfg, err := loadConfig("validate", args)
	if err != nil {
		return err
	}
	warnings, err := app.Check(cfg)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	fmt.Fprintln(stdout, "configuration is valid")
	return nil
}

func serve(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, path, sum, err := loadConfigFile("serve", args)
	if err != nil {
		return err
	}
	remote := cfg.Identity.Mode == config.ModeRemote
	if !remote && cfg.Audit.Stream.Type == "stdout" {
		return errors.New("audit.stream.type stdout would corrupt MCP over stdio; use stderr or file")
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	a, err := app.Open(ctx, cfg, app.Options{Logger: log, Stderr: stderr, ConfigPath: path, ConfigHash: sum})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	for _, w := range a.Warnings {
		log.Warn("configuration", "warning", w)
	}
	go a.RunRetention(ctx)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go a.Watch(ctx, configCheckInterval, hup)

	if !remote {
		log.Info("serving MCP over stdio", "version", version.Version, "tools", len(a.Engine.Tools()))
		return mcpadapter.ServeStdio(ctx, a.Engine, a.Principal)
	}
	h := cfg.Server.HTTP
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", h.Address)
	if err != nil {
		return err
	}
	opt := mcpadapter.HTTPOptions{Authenticate: a.Identity.Authenticate, Ready: a.Ready, Logger: log}
	if h.TLS != nil {
		opt.TLS = &mcpadapter.TLSFiles{CertFile: h.TLS.CertFile, KeyFile: h.TLS.KeyFile}
	}
	if o := cfg.Identity.OIDC; o != nil {
		opt.Resource = &mcpadapter.Resource{URL: cfg.ResourceURL(), AuthorizationServers: []string{o.Issuer}, Scopes: o.Scopes}
		go a.Identity.Discover(ctx)
	}
	alertsDone := make(chan struct{})
	if a.Alerts != nil {
		go func() { a.Alerts.Run(ctx); close(alertsDone) }()
		opt.Routes = map[string]mcpadapter.PrincipalHandler{"/alerts/alertmanager": a.Alerts.Handle}
		log.Info("receiving alerts", "path", "/alerts/alertmanager", "principal", cfg.Alerts.Principal)
	} else {
		close(alertsDone)
	}
	log.Info("serving MCP over HTTP", "version", version.Version, "tools", len(a.Engine.Tools()), "address", ln.Addr().String(), "tls", h.TLS != nil)
	err = mcpadapter.ServeHTTP(ctx, ln, a.Engine, opt)
	// Investigations under way finish, and write their audit records, before the store
	// closes.
	<-alertsDone
	return err
}

func verify(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig("audit verify", args)
	if err != nil {
		return err
	}
	// Opening would create a missing database, and an empty one verifies trivially.
	if cfg.Storage.Type == config.StorageSQLite {
		if _, err := os.Stat(cfg.Storage.Path); err != nil {
			return fmt.Errorf("no database at %s: %w", cfg.Storage.Path, err)
		}
	}
	st, err := app.OpenStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	writers, err := st.AuditWriters(ctx)
	if err != nil {
		return err
	}
	if len(writers) == 0 {
		return errors.New("no audit records in the store")
	}
	for _, w := range writers {
		records, err := audit.Load(ctx, st, w)
		if err != nil {
			return err
		}
		if err := audit.Verify(records); err != nil {
			return fmt.Errorf("audit chain %q: %w", w, err)
		}
		fmt.Fprintf(stdout, "audit chain %q: %d records verified\n", w, len(records))
	}
	return nil
}
