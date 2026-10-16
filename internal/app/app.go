// Package app assembles an engine from configuration.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/identity"
	"github.com/quyendv/opsward/internal/knowledge"
	"github.com/quyendv/opsward/internal/policy"
	"github.com/quyendv/opsward/internal/provider/httpapi"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
	"github.com/quyendv/opsward/internal/store"
	"github.com/quyendv/opsward/internal/store/postgres"
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
	// ConfigPath is the file the configuration was loaded from; Watch reloads it.
	ConfigPath string
	// ConfigHash is ContentHash of the bytes the configuration was parsed from. Without
	// it Open hashes the file again, and a change written in between would be missed.
	ConfigHash string
}

// App is a running Opsward: an engine and the resources behind it.
type App struct {
	Engine    *engine.Engine
	Principal core.Principal
	Audit     *audit.Writer
	Store     store.Store
	// Identity authenticates bearer tokens in remote mode.
	Identity *identity.Authenticator
	Warnings []string

	retention time.Duration
	log       *slog.Logger
	closers   []io.Closer

	// What a reload replaces. mu serializes reloads and guards loadedHash, the hash of
	// the configuration content last loaded or rejected, which Watch compares against.
	mu         sync.Mutex
	configPath string
	loadedHash string
	cfg        *config.Config
	source     *kubernetes.Source
	lib        *knowledge.Library
	metrics    *metricsRoutes // nil without any metrics backend
	logs       *logsRoutes    // nil without any logs backend
	changes    *changeRoutes  // nil without any forge
	envs       environments
	tools      []engine.Tool
}

// derived is everything a configuration determines that a reload replaces. It is built
// completely before anything is swapped, so a rejected configuration changes nothing.
type derived struct {
	policy     *policy.Policy
	pack       *knowledge.Pack
	envs       []engine.Environment
	envCluster map[string]string
	profiles   map[string]map[string]kubernetes.Profile
	// clusterValues maps each environment to its value of a backend's cluster label.
	clusterValues map[string]string
	envInfo       []tool.EnvironmentInfo
	// metricsPaths and logsPaths map each environment with a backend to its path.
	metricsPaths, logsPaths map[string]string
	warnings                []string
}

// environments is what list_environments reads; a reload replaces it.
type environments struct {
	p atomic.Pointer[[]tool.EnvironmentInfo]
}

func (e *environments) Environments() []tool.EnvironmentInfo {
	if p := e.p.Load(); p != nil {
		return *p
	}
	return nil
}

func derive(cfg *config.Config, tools []engine.Tool, log *slog.Logger) (*derived, error) {
	d := &derived{
		envCluster: map[string]string{}, profiles: map[string]map[string]kubernetes.Profile{}, clusterValues: map[string]string{},
		metricsPaths: map[string]string{}, logsPaths: map[string]string{},
	}
	pol, pw, perr := policy.Compile(cfg.Policy, catalog(cfg, tools), log)
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	pack, kw, kerr := knowledge.Load(knowledge.Options{
		Builtin: cfg.Knowledge.BuiltinEnabled(), Paths: cfg.Knowledge.Paths, Tools: names,
	})
	if err := errors.Join(perr, kerr); err != nil {
		return nil, err
	}
	d.policy, d.pack = pol, pack
	d.warnings = append(append(cfg.Warnings(), pw...), kw...)
	for _, e := range cfg.Environments {
		d.envs = append(d.envs, engine.Environment{Name: e.Name, Namespaces: e.Namespaces})
		d.envCluster[e.Name] = e.Cluster
		d.clusterValues[e.Name] = e.ClusterLabelValue
		mp, lp := cfg.MetricsBackendPath(e.Name), cfg.LogsBackendPath(e.Name)
		if mp != "" {
			d.metricsPaths[e.Name] = mp
		}
		if lp != "" {
			d.logsPaths[e.Name] = lp
		}
		d.envInfo = append(d.envInfo, tool.EnvironmentInfo{
			Name: e.Name, Cluster: e.Cluster, Namespaces: e.Namespaces, Metrics: mp != "", Logs: lp != "",
		})
	}
	for name, cl := range cfg.Providers.Kubernetes.Clusters {
		d.profiles[name] = map[string]kubernetes.Profile{}
		for pn, p := range cl.Profiles {
			d.profiles[name][pn] = kubernetes.Profile{Kubeconfig: p.Kubeconfig, Context: p.Context}
		}
	}
	return d, nil
}

// Check validates everything that can be validated without opening the store or
// reaching a backend, and returns warnings.
func Check(cfg *config.Config) ([]string, error) {
	tools, err := checkTools(cfg)
	if err != nil {
		return nil, err
	}
	d, err := derive(cfg, tools, slog.New(slog.DiscardHandler))
	if err != nil {
		return nil, err
	}
	return d.warnings, nil
}

// checkTools registers the tools a configuration enables, as Open does, without
// building a backend client.
func checkTools(cfg *config.Config) ([]engine.Tool, error) {
	// Placeholders stand in for the backends: they register the same tools without
	// reading credential or CA files, which may exist only where Opsward runs.
	var ms capability.MetricsSource
	var ls capability.LogsSource
	if len(cfg.MetricsBackends()) > 0 {
		ms = placeholder{}
	}
	if len(cfg.LogsBackends()) > 0 {
		ls = placeholder{}
	}
	var approvals tool.ApprovalStore
	if cfg.Identity.Mode == config.ModeRemote {
		approvals = noApprovals{}
	}
	var changes tool.ChangeConfig
	if len(cfg.Providers.Forges) > 0 {
		changes = &changeRoutes{}
	}
	return buildTools(kubernetes.NewSource(nil, nil, nil), knowledge.NewLibrary(nil), ms, ls, &environments{}, approvals, changes)
}

// Open builds the App: store, audit writer, providers, tools, policy, knowledge and engine.
func Open(ctx context.Context, cfg *config.Config, opt Options) (*App, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	a := &App{
		retention: cfg.Storage.AuditRetention.Std(), log: opt.Logger, cfg: cfg,
		source: kubernetes.NewSource(nil, nil, opt.ClientFactory), lib: knowledge.NewLibrary(nil),
		configPath: opt.ConfigPath,
	}
	switch {
	case opt.ConfigHash != "":
		a.loadedHash = opt.ConfigHash
	case opt.ConfigPath != "":
		a.loadedHash = fileHash(opt.ConfigPath)
	}
	var err error
	// Interfaces are set only for configured backends: a nil *prometheus.Source in a
	// capability.MetricsSource would be a non-nil interface.
	var ms capability.MetricsSource
	var ls capability.LogsSource
	if a.metrics, a.logs, err = openBackends(cfg); err != nil {
		return nil, err
	}
	if a.metrics != nil {
		ms = a.metrics
	}
	if a.logs != nil {
		ls = a.logs
	}
	st, w, closers, err := openAudit(ctx, cfg, opt)
	if err != nil {
		return nil, err
	}
	a.Store, a.Audit, a.closers = st, w, closers
	a.Identity = identity.New(identity.Options{Store: st, Groups: cfg.Identity.Groups, Logger: opt.Logger, OIDC: oidcOptions(cfg)})

	// Approvals need a second person: they exist in remote mode only.
	var approvals engine.Approvals
	var approvalTools tool.ApprovalStore
	if cfg.Identity.Mode == config.ModeRemote {
		approvals, approvalTools = st, st
	}
	// As for the backends: a nil *changeRoutes must not become a non-nil interface.
	var changes tool.ChangeConfig
	if a.changes, err = openChanges(cfg, a.source); err != nil {
		_ = a.Close()
		return nil, err
	}
	if a.changes != nil {
		changes = a.changes
	}
	if a.tools, err = buildTools(a.source, a.lib, ms, ls, &a.envs, approvalTools, changes); err != nil {
		_ = a.Close()
		return nil, err
	}
	d, err := derive(cfg, a.tools, opt.Logger)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.apply(d)
	a.Warnings = d.warnings

	a.Engine, err = engine.New(engine.Options{
		Tools: a.tools, Environments: d.envs, Authorizer: d.policy, Auditor: w, Annotator: a.lib, Logger: opt.Logger,
		Approvals: approvals, ApprovalTTL: cfg.Approvals.TTL.Std(),
	})
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.Principal = core.Principal{Subject: cfg.Identity.Principal.Subject, Groups: cfg.Identity.Principal.Groups}
	return a, nil
}

// ReloadFile loads the configuration at path and applies it with Reload. The content
// is read once, so the hash Watch compares against is that of what was parsed.
func (a *App) ReloadFile(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own configuration file
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	a.mu.Lock()
	a.loadedHash = hash(b)
	a.mu.Unlock()
	next, err := config.Parse(b)
	if err != nil {
		return err
	}
	return a.Reload(next)
}

// Reload applies a new configuration: policy, environments, providers, knowledge and
// internal groups. The rest of identity, server, storage and audit cannot change
// without a restart. On any error nothing
// changes and the previous configuration stays in force.
func (a *App) Reload(next *config.Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.cfg.RestartRequired(next); len(r) > 0 {
		return fmt.Errorf("restart required to change %s", strings.Join(r, ", "))
	}
	d, err := derive(next, a.tools, a.log)
	if err != nil {
		return err
	}
	a.apply(d)
	// Policy and membership are swapped one after the other, not together: a request in
	// between is decided by the new policy with the groups its token was resolved with,
	// which a request authenticated just before the reload sees anyway.
	a.Engine.Update(d.envs, d.policy, a.lib)
	if a.changes != nil {
		a.changes.update(next)
	}
	a.Identity.SetGroups(next.Identity.Groups)
	a.cfg, a.Warnings = next, d.warnings
	return nil
}

// apply hands a derived configuration to the providers and the knowledge library.
func (a *App) apply(d *derived) {
	a.source.Update(d.envCluster, d.profiles)
	a.envs.p.Store(&d.envInfo)
	a.lib.Set(d.pack)
	if a.metrics != nil {
		a.metrics.update(d.metricsPaths, d.clusterValues)
	}
	if a.logs != nil {
		a.logs.update(d.logsPaths, d.clusterValues)
	}
}

func httpOptions(b *config.Backend) httpapi.Options {
	o := httpapi.Options{URL: b.URL, Tenant: b.Tenant, BearerTokenFile: b.Auth.BearerTokenFile, CAFile: b.CAFile}
	if b.Auth.Basic != nil {
		o.BasicUsername, o.BasicPasswordFile = b.Auth.Basic.Username, b.Auth.Basic.PasswordFile
	}
	return o
}

// placeholder registers backend tools for Check without building a client.
type placeholder struct{}

func (placeholder) Metrics(string) (capability.Metrics, error) {
	return nil, errors.New("not available while checking")
}

func (placeholder) Logs(string) (capability.Logs, error) {
	return nil, errors.New("not available while checking")
}

// Watch reloads the configuration file (Options.ConfigPath) when its content differs
// from what was last loaded, checked every interval, and whenever hup receives a
// signal. A rejected content is not retried until it changes. It returns when ctx is
// done.
func (a *App) Watch(ctx context.Context, interval time.Duration, hup <-chan os.Signal) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			a.reload(a.configPath, "signal")
		case <-t.C:
			a.mu.Lock()
			last := a.loadedHash
			a.mu.Unlock()
			if h := fileHash(a.configPath); h != "" && h != last {
				a.reload(a.configPath, "file changed")
			}
		}
	}
}

func (a *App) reload(path, trigger string) {
	if err := a.ReloadFile(path); err != nil {
		a.log.Error("configuration reload rejected; the previous configuration stays in force", "trigger", trigger, "error", err)
		return
	}
	a.log.Info("configuration reloaded", "trigger", trigger)
	for _, w := range a.Warnings {
		a.log.Warn("configuration", "warning", w)
	}
}

// fileHash returns the hash of the file's content, or "" if it cannot be read.
func fileHash(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own configuration file
	if err != nil {
		return ""
	}
	return hash(b)
}

// ContentHash identifies configuration content for Watch.
func ContentHash(b []byte) string { return hash(b) }

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// openAudit opens the store and an audit writer on it. The closers release both, in
// reverse order.
func openAudit(ctx context.Context, cfg *config.Config, opt Options) (store.Store, *audit.Writer, []io.Closer, error) {
	st, err := OpenStore(ctx, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	closers := []io.Closer{st}
	closeAll := func() {
		for _, c := range slices.Backward(closers) {
			_ = c.Close()
		}
	}
	stream, err := openStream(cfg.Audit.Stream, opt, &closers)
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	w, err := audit.NewWriter(ctx, audit.Options{
		WriterID: cfg.Audit.Writer, Store: st, Stream: stream, Timeout: cfg.Audit.Timeout.Std(),
	})
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	return st, w, closers, nil
}

// OpenStore opens the configured store, applying pending migrations.
func OpenStore(ctx context.Context, cfg *config.Config) (store.Store, error) {
	if cfg.Storage.Type == config.StoragePostgres {
		dsn, err := cfg.Storage.DSN.Resolve("storage.dsn")
		if err != nil {
			return nil, err
		}
		return postgres.Open(ctx, dsn)
	}
	return sqlite.Open(ctx, cfg.Storage.Path)
}

func openStream(s config.Stream, opt Options, closers *[]io.Closer) (io.Writer, error) {
	switch s.Type {
	case "stdout":
		return orDefault(opt.Stdout, os.Stdout), nil
	case "file":
		f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("audit stream: %w", err)
		}
		*closers = append(*closers, f)
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

// approvalKeep is how long approval requests stay in the store after they expire.
const approvalKeep = 7 * 24 * time.Hour

// RunRetention prunes audit records and expired approval requests now and then daily,
// until ctx is done.
func (a *App) RunRetention(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if n, err := a.Audit.Prune(ctx, a.retention); err != nil {
			a.log.Error("audit retention failed", "error", err)
		} else if n > 0 {
			a.log.Info("audit retention pruned records", "count", n)
		}
		// The audit keeps every request and decision; the store keeps the requests a
		// week past expiry, for list_approvals.
		if n, err := a.Store.PruneApprovals(ctx, time.Now().Add(-approvalKeep)); err != nil {
			a.log.Error("pruning approval requests failed", "error", err)
		} else if n > 0 {
			a.log.Info("pruned expired approval requests", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func oidcOptions(cfg *config.Config) *identity.OIDCOptions {
	o := cfg.Identity.OIDC
	if o == nil {
		return nil
	}
	return &identity.OIDCOptions{
		Issuer: o.Issuer, Audience: o.Audience, SubjectClaim: o.SubjectClaim, GroupsClaim: o.GroupsClaim, GroupsPrefix: o.GroupsPrefixOrDefault(),
	}
}

// Ready reports whether the store answers, for a readiness probe.
func (a *App) Ready(ctx context.Context) error { return a.Store.Ping(ctx) }

// Close releases the store and any stream file.
func (a *App) Close() error {
	var errs []error
	for _, c := range slices.Backward(a.closers) {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// buildTools registers the tools whose backends are configured (docs/design/0001,
// "Registration follows capability"). ms and ls are nil when not configured.
func buildTools(src capability.WorkloadSource, lib *knowledge.Library, ms capability.MetricsSource, ls capability.LogsSource, envs tool.Environments, approvals tool.ApprovalStore, changes tool.ChangeConfig) ([]engine.Tool, error) {
	builders := []func() (engine.Tool, error){
		func() (engine.Tool, error) { return tool.RestartWorkload(src) },
		func() (engine.Tool, error) { return tool.ScaleWorkload(src) },
		func() (engine.Tool, error) { return tool.ListEnvironments(envs) },
		func() (engine.Tool, error) { return tool.ListNamespaces(envs, src) },
		func() (engine.Tool, error) { return tool.ListWorkloads(envs, src) },
		func() (engine.Tool, error) { return tool.WorkloadStatus(src) },
		func() (engine.Tool, error) { return tool.WorkloadEvents(src) },
		func() (engine.Tool, error) { return tool.WorkloadLogs(src) },
		func() (engine.Tool, error) { return tool.DiagnoseWorkload(src, ms) },
		func() (engine.Tool, error) { return tool.SearchKnowledge(lib) },
		func() (engine.Tool, error) { return tool.GetKnowledge(lib) },
	}
	if ms != nil {
		builders = append(builders,
			func() (engine.Tool, error) { return tool.GetWorkloadMetrics(src, ms) },
			func() (engine.Tool, error) { return tool.QueryMetrics(ms) },
		)
	}
	if ls != nil {
		builders = append(builders, func() (engine.Tool, error) { return tool.QueryLogs(ls) })
	}
	if changes != nil {
		builders = append(builders,
			func() (engine.Tool, error) { return tool.ResolveSource(src, changes) },
			func() (engine.Tool, error) { return tool.ProposeChange(src, changes) },
		)
	}
	if approvals != nil {
		builders = append(builders,
			func() (engine.Tool, error) { return tool.ListApprovals(approvals) },
			func() (engine.Tool, error) { return tool.DecideApproval(approvals) },
		)
	}
	tools := make([]engine.Tool, 0, len(builders))
	for _, b := range builders {
		t, err := b()
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", t.Name, err)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func catalog(cfg *config.Config, tools []engine.Tool) policy.Catalog {
	cat := policy.Catalog{Tools: map[string]policy.ToolInfo{}, Environments: map[string][]string{}}
	for _, t := range tools {
		cat.Tools[t.Name] = policy.ToolInfo{Risk: t.Risk, Risks: t.Risks, Args: t.Args, Global: t.Global}
	}
	for _, e := range cfg.Environments {
		cat.Environments[e.Name] = cfg.Profiles(e)
	}
	return cat
}

// noApprovals registers the approval tools for Check without a store.
type noApprovals struct{}

func (noApprovals) Approval(context.Context, string) (store.Approval, bool, error) {
	return store.Approval{}, false, errors.New("not available while checking")
}

func (noApprovals) ListApprovals(context.Context, time.Time, int) ([]store.Approval, error) {
	return nil, errors.New("not available while checking")
}

func (noApprovals) WithdrawApproval(context.Context, string, string, []string, string, time.Time) (bool, error) {
	return false, errors.New("not available while checking")
}

func (noApprovals) DecideApproval(context.Context, string, string, string, []string, string, time.Time) (bool, error) {
	return false, errors.New("not available while checking")
}
