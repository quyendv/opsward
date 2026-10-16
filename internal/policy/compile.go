package policy

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"

	"github.com/quyendv/opsward/internal/core"
)

// celCostLimit matches the per-call limit Kubernetes applies to admission policy CEL.
const celCostLimit = 1_000_000

var (
	tzlessTimeAccessor = regexp.MustCompile(`\.get(FullYear|Month|Date|DayOfMonth|DayOfYear|DayOfWeek|Hours|Minutes|Seconds|Milliseconds)\(\s*\)`)
	argsField          = regexp.MustCompile(`\bargs\.([A-Za-z_][A-Za-z0-9_]*)`)
)

// Compile validates cfg against cat and returns a ready policy. It reports every problem
// it finds, not only the first. Warnings describe configuration that is valid but
// probably not what was meant.
func Compile(cfg Config, cat Catalog, log *slog.Logger) (*Policy, []string, error) {
	if log == nil {
		log = slog.Default()
	}
	env, err := newCELEnv()
	if err != nil {
		return nil, nil, err
	}

	var errs []error
	var warnings []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	for _, s := range cfg.Sources {
		if s != "file" {
			fail("policy.sources: %q is not available in this version", s)
		}
	}
	appLabel := cfg.AppLabel
	if appLabel == "" {
		appLabel = DefaultAppLabel
	}

	for name, role := range cfg.Roles {
		for _, r := range role.Risk {
			if !r.Valid() {
				fail("role %q: unknown risk %q", name, r)
			}
		}
		for _, t := range role.Tools {
			if _, ok := cat.Tools[t]; !ok {
				warnings = append(warnings, fmt.Sprintf("role %q: tool %q is not registered", name, t))
			}
		}
	}

	if len(cfg.Bindings) == 0 {
		warnings = append(warnings, "policy has no bindings: every call will be denied")
	}

	p := &Policy{appLabel: appLabel, log: log}
	seen := map[string]bool{}
	for i, b := range cfg.Bindings {
		where := fmt.Sprintf("binding %d", i)
		if b.Name == "" {
			fail("%s: name is required", where)
		} else {
			where = fmt.Sprintf("binding %q", b.Name)
			if seen[b.Name] {
				fail("%s: duplicate name", where)
			}
			seen[b.Name] = true
		}
		if b.Effect == "" {
			b.Effect = core.EffectAllow
		}
		if !b.Effect.Valid() {
			fail("%s: unknown effect %q", where, b.Effect)
		}
		role, ok := cfg.Roles[b.Role]
		if !ok {
			fail("%s: unknown role %q", where, b.Role)
		}
		if b.Effect == core.EffectAllow && grantsDestructive(role, cat) {
			fail("%s: role %q contains destructive tools, which can never be allowed without approval", where, b.Role)
		}
		if b.Effect == core.EffectApprove && len(b.Approvers.Users) == 0 && len(b.Approvers.Groups) == 0 {
			fail("%s: effect approve needs approvers", where)
		}
		for _, list := range [][]string{b.Scope.Environments, b.Scope.Namespaces, b.Scope.Workloads, b.Scope.Apps} {
			for _, pat := range list {
				if _, err := path.Match(pat, ""); err != nil {
					fail("%s: invalid pattern %q", where, pat)
				}
			}
		}
		for _, e := range b.Scope.Environments {
			if !strings.ContainsAny(e, "*?[") {
				if _, ok := cat.Environments[e]; !ok {
					fail("%s: unknown environment %q", where, e)
				}
			}
		}
		for envName, profiles := range cat.Environments {
			if !anyGlob(b.Scope.Environments, envName) {
				continue
			}
			if b.Profile != "" && !slices.Contains(profiles, b.Profile) {
				fail("%s: profile %q is not declared for environment %q", where, b.Profile, envName)
			}
			if b.Profile == "" {
				for _, need := range defaultProfiles(role, cat) {
					if !slices.Contains(profiles, need) {
						fail("%s: needs profile %q in environment %q, or set the binding's profile field", where, need, envName)
					}
				}
			}
			if len(b.Scope.Apps) > 0 && !slices.Contains(profiles, core.ProfileReader) {
				warnings = append(warnings, fmt.Sprintf(
					"%s: apps labels are read with the %q profile, which environment %q does not declare; they will always be unknown",
					where, core.ProfileReader, envName))
			}
		}

		c := compiled{Binding: b, role: role}
		if b.When != "" {
			prg, err := compileWhen(env, b.When)
			if err != nil {
				fail("%s: when: %w", where, err)
			}
			c.when = prg
			warnings = append(warnings, lintWhen(where, b.When, role, cat)...)
		}
		p.bindings = append(p.bindings, c)
	}

	if len(errs) > 0 {
		return nil, warnings, errors.Join(errs...)
	}
	return p, warnings, nil
}

// defaultProfiles returns the profiles the role's registered tools run with when a
// binding names none.
func defaultProfiles(r Role, cat Catalog) []string {
	var out []string
	for name, t := range cat.Tools {
		for _, p := range profilesFor(r, name, t, "") {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// profilesFor returns the profiles calls to the tool run with under a binding of role r
// that names profile (or none): a call runs at the tool's risk or a lower one it declares,
// and a binding without a profile uses the default one for that risk. Global tools use no
// profile.
func profilesFor(r Role, name string, t ToolInfo, profile string) []string {
	if t.Global {
		return nil
	}
	var out []string
	for _, risk := range append([]core.Risk{t.Risk}, t.Risks...) {
		if !r.contains(name, risk) {
			continue
		}
		p := profile
		if p == "" {
			p = core.ProfileOperator
			if risk == core.RiskRead {
				p = core.ProfileReader
			}
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func grantsDestructive(r Role, cat Catalog) bool {
	if slices.Contains(r.Risk, core.RiskDestructive) {
		return true
	}
	for _, t := range r.Tools {
		if cat.Tools[t].Risk == core.RiskDestructive {
			return true
		}
	}
	return false
}

func newCELEnv() (*cel.Env, error) {
	env, err := cel.NewEnv(
		cel.Variable("principal", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("tool", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("scope", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("args", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("now", cel.TimestampType),
	)
	if err != nil {
		return nil, fmt.Errorf("policy: CEL environment: %w", err)
	}
	return env, nil
}

func compileWhen(env *cel.Env, expr string) (cel.Program, error) {
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("must evaluate to bool, not %s", ast.OutputType())
	}
	prg, err := env.Program(ast, cel.CostLimit(celCostLimit))
	if err != nil {
		return nil, err
	}
	return prg, nil
}

func lintWhen(where, expr string, role Role, cat Catalog) []string {
	var out []string
	if tzlessTimeAccessor.MatchString(expr) {
		out = append(out, fmt.Sprintf("%s: when: a time accessor without a time zone evaluates in UTC; name the zone, e.g. getHours(\"UTC\")", where))
	}
	known := map[string]bool{}
	for name, t := range cat.Tools {
		if role.contains(name, t.Risk) {
			for _, a := range t.Args {
				known[a] = true
			}
		}
	}
	for _, m := range argsField.FindAllStringSubmatch(expr, -1) {
		if !known[m[1]] {
			out = append(out, fmt.Sprintf("%s: when: no tool in this role has an argument %q", where, m[1]))
		}
	}
	return out
}

// conditionHolds evaluates a binding's condition. A deny or approve binding whose
// condition fails to evaluate applies; an allow binding whose condition fails does not.
// Either way the stricter outcome wins.
func (p *Policy) conditionHolds(b *compiled, req core.Request) bool {
	if b.when == nil {
		return true
	}
	groups := req.Principal.Groups
	if groups == nil {
		groups = []string{}
	}
	args := req.Args
	if args == nil {
		args = map[string]any{}
	}
	out, _, err := b.when.Eval(map[string]any{
		"principal": map[string]any{"subject": req.Principal.Subject, "groups": groups, "adapter": req.Principal.Adapter},
		"tool":      map[string]any{"name": req.Tool, "risk": string(req.Risk)},
		"scope": map[string]any{
			"environment": req.Scope.Environment, "namespace": req.Scope.Namespace,
			"kind": req.Scope.Kind, "name": req.Scope.Name, "labels": labelsOrEmpty(req.Scope.Labels),
		},
		"args": args,
		"now":  req.Now,
	})
	if err != nil {
		p.log.Warn("policy condition failed to evaluate", "binding", b.Name, "error", err)
		return b.Effect != core.EffectAllow
	}
	v, ok := out.Value().(bool)
	if !ok {
		p.log.Warn("policy condition did not return a bool", "binding", b.Name)
		return b.Effect != core.EffectAllow
	}
	return v
}

func labelsOrEmpty(l map[string]string) map[string]string {
	if l == nil {
		return map[string]string{}
	}
	return l
}
