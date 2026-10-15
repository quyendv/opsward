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
	"os"
	"os/signal"
	"syscall"

	mcpadapter "github.com/quyendv/opsward/internal/adapter/mcp"
	"github.com/quyendv/opsward/internal/app"
	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/store/sqlite"
	"github.com/quyendv/opsward/internal/version"
)

const usage = `usage: opsward <command> [flags]

commands:
  serve --config FILE          serve MCP over stdio
  validate --config FILE       check a configuration file and its policy
  audit verify --config FILE   check the stored audit chain
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
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "opsward: %v\n", err)
		return 1
	}
	return 0
}

func loadConfig(name string, args []string) (*config.Config, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *path == "" {
		return nil, errors.New("--config is required")
	}
	return config.Load(*path)
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
	cfg, err := loadConfig("serve", args)
	if err != nil {
		return err
	}
	if cfg.Audit.Stream.Type == "stdout" {
		return errors.New("audit.stream.type stdout would corrupt MCP over stdio; use stderr or file")
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	a, err := app.Open(ctx, cfg, app.Options{Logger: log, Stderr: stderr})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	for _, w := range a.Warnings {
		log.Warn("policy", "warning", w)
	}
	go a.RunRetention(ctx)

	log.Info("serving MCP over stdio", "version", version.Version, "tools", len(a.Engine.Tools()))
	return mcpadapter.ServeStdio(ctx, a.Engine, a.Principal)
}

func verify(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig("audit verify", args)
	if err != nil {
		return err
	}
	// Opening would create a missing database, and an empty one verifies trivially.
	if _, err := os.Stat(cfg.Storage.Path); err != nil {
		return fmt.Errorf("no database at %s: %w", cfg.Storage.Path, err)
	}
	st, err := sqlite.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	writers, err := st.AuditWriters(ctx)
	if err != nil {
		return err
	}
	if len(writers) == 0 {
		return fmt.Errorf("no audit records in %s", cfg.Storage.Path)
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
