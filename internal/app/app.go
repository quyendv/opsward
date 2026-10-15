// Package app assembles an engine from configuration.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/policy"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
	"github.com/quyendv/opsward/internal/store"
	"github.com/quyendv/opsward/internal/store/sqlite"
	"github.com/quyendv/opsward/internal/tool"
)

// Options are the parts of an App that do not come from the configuration file.
type Options struct {
	Logger *slog.Logger
	// Stderr and Stdout are the process streams an audit stream may name.
	Stderr, Stdout io.Writer
	// ClientFactory replaces kubeconfig-based clients, for tests.
	ClientFactory kubernetes.ClientFactory
}

// App is a running Opsward: an engine and the resources behind it.
type App struct {
	Engine    *engine.Engine
	Principal core.Principal
	Audit     *audit.Writer
	Store     store.Store
	Warnings  []string

	retention time.Duration
	log       *slog.Logger
	closers   []io.Closer
}

// Check validates everything that can be validated without opening the store or
// reaching a backend, and returns policy warnings.
func Check(cfg *config.Config) ([]string, error) {
	tools, err := buildTools(workloadSource(cfg, nil))
	if err != nil {
		return nil, err
	}
	_, warnings, err := policy.Compile(cfg.Policy, catalog(cfg, tools), slog.New(slog.DiscardHandler))
	return warnings, err
}

// Open builds the App: store, audit writer, providers, tools, policy and engine.
func Open(ctx context.Context, cfg *config.Config, opt Options) (*App, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	a := &App{retention: cfg.Storage.AuditRetention.Std(), log: opt.Logger}

	tools, err := buildTools(workloadSource(cfg, opt.ClientFactory))
	if err != nil {
		return nil, err
	}
	pol, warnings, err := policy.Compile(cfg.Policy, catalog(cfg, tools), opt.Logger)
	if err != nil {
		return nil, err
	}
	a.Warnings = warnings

	st, err := sqlite.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return nil, err
	}
	a.Store = st
	a.closers = append(a.closers, st)

	stream, err := a.openStream(cfg.Audit.Stream, opt)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	w, err := audit.NewWriter(ctx, audit.Options{
		WriterID: cfg.Audit.Writer, Store: st, Stream: stream, Timeout: cfg.Audit.Timeout.Std(),
	})
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.Audit = w

	envs := make([]engine.Environment, 0, len(cfg.Environments))
	for _, e := range cfg.Environments {
		envs = append(envs, engine.Environment{Name: e.Name, Namespaces: e.Namespaces})
	}
	a.Engine, err = engine.New(engine.Options{
		Tools: tools, Environments: envs, Authorizer: pol, Auditor: w, Logger: opt.Logger,
	})
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.Principal = core.Principal{Subject: cfg.Identity.Principal.Subject, Groups: cfg.Identity.Principal.Groups}
	return a, nil
}

func (a *App) openStream(s config.Stream, opt Options) (io.Writer, error) {
	switch s.Type {
	case "stdout":
		return orDefault(opt.Stdout, os.Stdout), nil
	case "file":
		f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("audit stream: %w", err)
		}
		a.closers = append(a.closers, f)
		return f, nil
	default:
		return orDefault(opt.Stderr, os.Stderr), nil
	}
}

func orDefault(w, def io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return def
}

// RunRetention prunes audit records now and then daily, until ctx is done.
func (a *App) RunRetention(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if n, err := a.Audit.Prune(ctx, a.retention); err != nil {
			a.log.Error("audit retention failed", "error", err)
		} else if n > 0 {
			a.log.Info("audit retention pruned records", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Close releases the store and any stream file.
func (a *App) Close() error {
	var errs []error
	for _, c := range slices.Backward(a.closers) {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

func workloadSource(cfg *config.Config, f kubernetes.ClientFactory) capability.WorkloadSource {
	envCluster := map[string]string{}
	for _, e := range cfg.Environments {
		envCluster[e.Name] = e.Cluster
	}
	profiles := map[string]map[string]kubernetes.Profile{}
	for name, cl := range cfg.Providers.Kubernetes.Clusters {
		profiles[name] = map[string]kubernetes.Profile{}
		for pn, p := range cl.Profiles {
			profiles[name][pn] = kubernetes.Profile{Kubeconfig: p.Kubeconfig, Context: p.Context}
		}
	}
	return kubernetes.NewSource(envCluster, profiles, f)
}

func buildTools(src capability.WorkloadSource) ([]engine.Tool, error) {
	status, err := tool.WorkloadStatus(src)
	if err != nil {
		return nil, fmt.Errorf("tool get_workload_status: %w", err)
	}
	return []engine.Tool{status}, nil
}

func catalog(cfg *config.Config, tools []engine.Tool) policy.Catalog {
	cat := policy.Catalog{Tools: map[string]policy.ToolInfo{}, Environments: map[string][]string{}}
	for _, t := range tools {
		cat.Tools[t.Name] = policy.ToolInfo{Risk: t.Risk, Args: t.Args}
	}
	for _, e := range cfg.Environments {
		cat.Environments[e.Name] = cfg.Profiles(e)
	}
	return cat
}
