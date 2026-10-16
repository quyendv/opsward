// Package engine runs every tool call through one pipeline: identify, resolve,
// validate, authorize, run, redact, audit. See docs/design/0001, "Request lifecycle".
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/redact"
)

// Profile names used when the authorizing binding names none.
const (
	ProfileReader   = core.ProfileReader
	ProfileOperator = core.ProfileOperator
)

// Tool is a named operation with typed arguments and a declared risk class.
type Tool struct {
	Name        string
	Description string
	Risk        core.Risk
	InputSchema *jsonschema.Schema
	// Args are the top-level argument names, for policy linting.
	Args []string
	// Global tools are not bound to an environment, such as knowledge search: the
	// engine skips scope validation and label lookup, and authorizes them with an empty
	// scope.
	Global bool
	// List tools are authorized per item of their result (docs/design/0006): the call
	// must be reachable before anything is read, and each item is kept only when policy
	// allows the tool on the item's scope. They implement RunList, not Run.
	List bool
	// ClusterWide tools read across namespaces when the call names none. Such a call is
	// refused in an environment limited to some namespaces.
	ClusterWide bool
	// Prepare decodes and validates arguments without touching any backend.
	Prepare func(args json.RawMessage) (*Prepared, error)
}

// Prepared is a validated call, ready to authorize and run.
type Prepared struct {
	Scope core.Scope
	// Args is the validated argument object, as policy conditions and audit see it.
	Args map[string]any
	// Resolve, if set, reads the target's labels with Opsward's reader profile so that
	// policy can match on them. It runs after the scope has been validated.
	Resolve func(ctx context.Context) (map[string]string, error)
	// Run performs the call with the named profile.
	Run func(ctx context.Context, profile string) (any, error)
	// Preview, if set, reports what a write would change, as a dry run with the named
	// profile: shown to approvers and recorded in the audit (docs/design/0007).
	Preview func(ctx context.Context, profile string) (*core.Change, error)
	// RunList performs a List tool's call with the named profile.
	RunList func(ctx context.Context, profile string) ([]Item, error)
	// Wrap, if set, shapes a List tool's kept items into its result; by default the
	// result is the list of their data.
	Wrap func(kept []any) any
}

// Item is one element of a listing, with the scope policy decides it on.
type Item struct {
	Scope core.Scope
	Data  any
}

// Reacher is implemented by an Authorizer that can tell through which profiles a
// principal could be allowed a tool anywhere within a scope
// (policy.Policy.ReachableProfiles). Listings need it; without it they are denied.
type Reacher interface {
	ReachableProfiles(core.Request) []string
}

// Authorizer decides calls. policy.Policy implements it.
type Authorizer interface {
	Decide(core.Request) core.Decision
}

// LabelUser is implemented by an Authorizer that can tell whether it ever looks at
// target labels. When it does not, the engine skips reading them, so a denied call
// costs no backend request. An Authorizer without it always gets labels.
type LabelUser interface {
	NeedsLabels() bool
}

// Annotator attaches knowledge notes to a tool's redacted result.
type Annotator interface {
	Annotate(req core.Request, result any) []core.Note
}

// Result is a successful call's redacted output and the notes attached to it.
type Result struct {
	Data  any
	Notes []core.Note
}

// Auditor records calls. audit.Writer implements it.
type Auditor interface {
	Write(ctx context.Context, r audit.Record) (audit.Record, error)
}

// Environment is what the engine needs to validate a scope.
type Environment struct {
	Name       string
	Namespaces []string
}

// Options configures an Engine.
type Options struct {
	Tools        []Tool
	Environments []Environment
	Authorizer   Authorizer
	Auditor      Auditor
	Annotator    Annotator // optional
	// Approvals stores approval requests; without it, calls that need approval are
	// refused (local mode).
	Approvals   Approvals
	ApprovalTTL time.Duration
	Logger      *slog.Logger
	Now         func() time.Time
}

// Engine is the single authorization point. It is safe for concurrent use.
type Engine struct {
	approvals   Approvals
	approvalTTL time.Duration
	tools       map[string]Tool
	state       atomic.Pointer[state]
	audit       Auditor
	log         *slog.Logger
	now         func() time.Time
}

// state is what a configuration reload replaces. A call reads it once, so it runs
// entirely under the configuration it started with.
type state struct {
	envs  map[string]Environment
	authz Authorizer
	ann   Annotator
}

// Errors returned by Invoke. Tool failures are returned wrapped as they are.
var (
	ErrUnidentified     = errors.New("no principal: the caller is not identified")
	ErrUnknownTool      = errors.New("unknown tool")
	ErrInvalidArguments = errors.New("invalid arguments")
	ErrAuditUnavailable = errors.New("audit unavailable: the call was not performed or its result is withheld")
)

// DeniedError reports a call that policy did not allow.
type DeniedError struct {
	Decision core.Decision
}

func (e *DeniedError) Error() string {
	if e.Decision.Effect == core.EffectApprove {
		return fmt.Sprintf("requires approval (rule %s), and approvals need a second person: they are available in remote mode only", e.Decision.Rule)
	}
	return fmt.Sprintf("denied by policy (rule %s)", e.Decision.Rule)
}

// redactedError is what Invoke returns for a failed call: its text is redacted,
// because adapters show it to the agent, while Unwrap keeps errors.Is and errors.As
// working on the original chain.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// New builds an engine. Tool names must be unique.
func New(opt Options) (*Engine, error) {
	if opt.Authorizer == nil || opt.Auditor == nil {
		return nil, errors.New("engine: Authorizer and Auditor are required")
	}
	e := &Engine{approvals: opt.Approvals, approvalTTL: opt.ApprovalTTL, tools: map[string]Tool{}, audit: opt.Auditor, log: opt.Logger, now: opt.Now}
	if e.log == nil {
		e.log = slog.Default()
	}
	if e.now == nil {
		e.now = time.Now
	}
	for _, t := range opt.Tools {
		if _, dup := e.tools[t.Name]; dup {
			return nil, fmt.Errorf("engine: duplicate tool %q", t.Name)
		}
		if !t.Risk.Valid() {
			return nil, fmt.Errorf("engine: tool %q: invalid risk %q", t.Name, t.Risk)
		}
		e.tools[t.Name] = t
	}
	e.Update(opt.Environments, opt.Authorizer, opt.Annotator)
	return e, nil
}

// Update replaces the environments, the authorizer and the annotator in one step.
// Calls already running finish under the previous ones.
func (e *Engine) Update(envs []Environment, authz Authorizer, ann Annotator) {
	s := &state{envs: map[string]Environment{}, authz: authz, ann: ann}
	for _, env := range envs {
		s.envs[env.Name] = env
	}
	e.state.Store(s)
}

// Tools returns the registered tools sorted by name.
func (e *Engine) Tools() []Tool {
	out := make([]Tool, 0, len(e.tools))
	for _, t := range e.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Invoke runs one call through the pipeline and returns its redacted result. Every
// path through it, including denials and failures, writes an audit record.
func (e *Engine) Invoke(ctx context.Context, p core.Principal, name string, args json.RawMessage) (*Result, error) {
	st := e.state.Load()
	start := e.now()
	// A client that cancels or disconnects must not erase the record of what it
	// asked for; the audit writer's own timeout bounds these writes.
	actx := context.WithoutCancel(ctx)
	rec := audit.Record{Phase: audit.PhaseOutcome, Principal: p, Tool: name}
	finish := func(status string, cause error) error {
		outcome := &audit.Outcome{Status: status, DurationMs: e.now().Sub(start).Milliseconds()}
		if rec.Outcome != nil {
			outcome.Listed = rec.Outcome.Listed
		}
		rec.Outcome = outcome
		var out error
		if cause != nil {
			out = &redactedError{msg: redact.String(cause.Error()), cause: cause}
			rec.Outcome.Error = out.Error()
		}
		if _, err := e.audit.Write(actx, rec); err != nil {
			e.log.Error("audit write failed", "tool", name, "error", err)
			return errors.Join(out, ErrAuditUnavailable)
		}
		return out
	}

	if p.Subject == "" {
		return nil, finish(audit.StatusDenied, ErrUnidentified)
	}
	tool, ok := e.tools[name]
	if !ok {
		return nil, finish(audit.StatusUnknownTool, fmt.Errorf("%w: %q", ErrUnknownTool, name))
	}
	rec.Risk = tool.Risk

	prep, err := tool.Prepare(args)
	if err != nil {
		return nil, finish(audit.StatusInvalid, fmt.Errorf("%w: %w", ErrInvalidArguments, err))
	}
	rec.Scope = prep.Scope
	rec.Args = redactArgs(prep.Args)
	if !tool.Global {
		if err := st.validateScope(prep.Scope); err != nil {
			return nil, finish(audit.StatusInvalid, fmt.Errorf("%w: %w", ErrInvalidArguments, err))
		}
		if tool.ClusterWide && prep.Scope.Namespace == "" && !st.unlimited(prep.Scope.Environment) {
			// The patterns are not echoed: this is answered before authorization.
			return nil, finish(audit.StatusInvalid, fmt.Errorf("%w: environment %q is limited to some namespaces; name one",
				ErrInvalidArguments, prep.Scope.Environment))
		}
	}
	if tool.List {
		return e.list(ctx, st, tool, prep, &rec, finish, core.Request{Principal: p, Tool: name, Risk: tool.Risk, Scope: prep.Scope, Args: prep.Args, Now: start})
	}

	scope := prep.Scope
	if lu, ok := st.authz.(LabelUser); !tool.Global && prep.Resolve != nil && (!ok || lu.NeedsLabels()) {
		// A failed lookup is not reported to the caller before authorization, so that
		// an unauthorized caller cannot probe which objects exist. Policy is told the
		// labels are unknown, so app-scoped deny and approve bindings still apply; an
		// authorized call reports the error from Run.
		labels, err := prep.Resolve(ctx)
		if err != nil {
			e.log.Debug("resolving target labels failed", "tool", name, "error", err)
		}
		scope.Labels, scope.LabelsKnown = labels, err == nil
	}

	req := core.Request{Principal: p, Tool: name, Risk: tool.Risk, Scope: scope, Args: prep.Args, Now: start}
	dec := st.authz.Decide(req)
	if dec.Effect == core.EffectAllow && tool.Risk == core.RiskDestructive {
		// Policy validation already rejects this; the engine refuses it too, so no
		// Authorizer can let a destructive call through without approval.
		dec.Effect = core.EffectApprove
	}
	rec.Decision = &dec
	profile := dec.Profile
	if profile == "" {
		profile = ProfileOperator
		if tool.Risk == core.RiskRead {
			profile = ProfileReader
		}
	}
	ctx = core.WithPrincipal(ctx, p)
	switch dec.Effect {
	case core.EffectAllow:
	case core.EffectApprove:
		if e.approvals == nil {
			return nil, finish(audit.StatusDenied, &DeniedError{Decision: dec})
		}
		id, _ := prep.Args[ApprovalArg].(string)
		if id == "" {
			return e.requestApproval(ctx, p, name, prep, &rec, &dec, profile, start, finish)
		}
		change, err := e.useApproval(ctx, p, name, prep, id, profile)
		if err != nil {
			return nil, finish(audit.StatusDenied, err)
		}
		dec.Approval, rec.Change = id, change
	case core.EffectDeny:
		return nil, finish(audit.StatusDenied, &DeniedError{Decision: dec})
	default:
		return nil, finish(audit.StatusDenied, &DeniedError{Decision: dec})
	}

	if tool.Risk != core.RiskRead && prep.Preview != nil && rec.Change == nil {
		// Every write is a dry run first, and the diff is part of its record.
		change, err := prep.Preview(ctx, profile)
		if errors.Is(err, core.ErrInvalidInput) {
			return nil, finish(audit.StatusInvalid, err)
		}
		if err != nil {
			return nil, finish(audit.StatusError, err)
		}
		rec.Change = change
	}

	if tool.Risk != core.RiskRead {
		intent := rec
		intent.Phase = audit.PhaseIntent
		if _, err := e.audit.Write(actx, intent); err != nil {
			e.log.Error("audit intent write failed; refusing the call", "tool", name, "error", err)
			return nil, finish(audit.StatusError, ErrAuditUnavailable)
		}
	}

	out, err := prep.Run(ctx, profile)
	if errors.Is(err, core.ErrInvalidInput) {
		return nil, finish(audit.StatusInvalid, err)
	}
	if err != nil {
		return nil, finish(audit.StatusError, err)
	}
	data, err := normalize(out)
	if err != nil {
		return nil, finish(audit.StatusError, err)
	}
	if err := finish(audit.StatusOK, nil); err != nil {
		return nil, err
	}
	res := &Result{Data: data}
	if st.ann != nil {
		res.Notes = st.ann.Annotate(req, data)
	}
	return res, nil
}

// list runs a List tool (docs/design/0006): once per profile through which the call is
// reachable, keeping each item only when policy allows it through the profile it was read
// with.
func (e *Engine) list(ctx context.Context, st *state, tool Tool, prep *Prepared, rec *audit.Record, finish func(string, error) error, req core.Request) (*Result, error) {
	deny := func() (*Result, error) {
		dec := core.Decision{Effect: core.EffectDeny, Rule: core.DefaultDenyRule}
		rec.Decision = &dec
		return nil, finish(audit.StatusDenied, &DeniedError{Decision: dec})
	}
	reacher, ok := st.authz.(Reacher)
	if !ok {
		return deny()
	}
	// A global listing (environments) reads only configuration, so it needs no profile
	// and no reachable scope before running; its items are still decided one by one.
	profiles := []string{ProfileReader}
	if !tool.Global {
		if profiles = reacher.ReachableProfiles(req); len(profiles) == 0 {
			return deny()
		}
	}
	rec.Decision = &core.Decision{Effect: core.EffectAllow, Rule: core.PerItemRule}
	if prep.RunList == nil {
		return nil, finish(audit.StatusError, fmt.Errorf("tool %s lists but has no RunList", tool.Name))
	}
	kept := []any{}
	seen := map[string]bool{}
	read, listed := 0, false
	var failed error
	for _, profile := range profiles {
		items, err := prep.RunList(ctx, profile)
		if errors.Is(err, core.ErrInvalidInput) {
			return nil, finish(audit.StatusInvalid, err)
		}
		if err != nil {
			// A narrower profile may not list what the reader can; the others still count.
			failed = errors.Join(failed, fmt.Errorf("profile %s: %w", profile, err))
			continue
		}
		listed = true
		read += len(items)
		for _, it := range items {
			key := it.Scope.Environment + "/" + it.Scope.Namespace + "/" + it.Scope.Kind + "/" + it.Scope.Name
			// Defence in depth: the tool filters by the environment, and so does the engine.
			if seen[key] || (!tool.Global && st.validateScope(it.Scope) != nil) {
				continue
			}
			ireq := req
			ireq.Scope = it.Scope
			var allowed bool
			switch {
			case tool.Global:
				allowed = len(reacher.ReachableProfiles(ireq)) > 0
			case it.Scope.Name != "":
				d := st.authz.Decide(ireq)
				p := d.Profile
				if p == "" {
					p = ProfileReader
				}
				allowed = d.Effect == core.EffectAllow && p == profile
			default:
				allowed = slices.Contains(reacher.ReachableProfiles(ireq), profile)
			}
			if allowed {
				seen[key] = true
				kept = append(kept, it.Data)
			}
		}
	}
	if !listed {
		return nil, finish(audit.StatusError, failed)
	}
	if failed != nil {
		e.log.Warn("a listing failed with some profiles; their items are left out", "tool", tool.Name, "error", failed)
	}
	rec.Outcome = &audit.Outcome{Listed: &audit.Listed{Read: read, Kept: len(kept)}}
	// Listings carry no knowledge notes: notes describe a target, and a listing has none.
	var out any = kept
	if prep.Wrap != nil {
		out = prep.Wrap(kept)
	}
	data, err := normalize(out)
	if err != nil {
		return nil, finish(audit.StatusError, err)
	}
	if err := finish(audit.StatusOK, nil); err != nil {
		return nil, err
	}
	return &Result{Data: data}, nil
}

// unlimited reports whether an environment spans every namespace.
func (s *state) unlimited(env string) bool {
	return slices.Contains(s.envs[env].Namespaces, "*")
}

func (s *state) validateScope(sc core.Scope) error {
	env, ok := s.envs[sc.Environment]
	if !ok {
		return fmt.Errorf("unknown environment %q", sc.Environment)
	}
	if sc.Namespace == "" {
		return nil
	}
	for _, pat := range env.Namespaces {
		if ok, _ := path.Match(pat, sc.Namespace); ok {
			return nil
		}
	}
	return fmt.Errorf("namespace %q is not part of environment %q", sc.Namespace, sc.Environment)
}

// normalize turns a tool's typed result into plain JSON values and redacts them.
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	return redact.Value(out), nil
}

func redactArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out, _ := redact.Value(args).(map[string]any)
	return out
}
