package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/pmezard/go-difflib/difflib"

	"github.com/quyendv/opsward/internal/capability"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/engine"
	"github.com/quyendv/opsward/internal/redact"
	"github.com/quyendv/opsward/internal/source"
)

// Limits of one proposal and of what resolve_source returns (docs/design/0008).
const (
	maxProposalFiles = 10
	maxEdits         = 20
	maxFileBytes     = 1 << 20
	maxReadFiles     = 10
	maxShownBytes    = 256 << 10
	maxListedShown   = 500
)

// Change strategies.
const (
	strategySuggest     = "suggest"
	strategyPullRequest = "pull-request"
)

// Changes is what the change tools read from the configuration.
type Changes struct {
	Strategies map[string]string // environment -> strategy
	Resolve    source.Options
	Forges     []ForgeRoute
}

// ForgeRoute is one configured forge and the repositories it may reach.
type ForgeRoute struct {
	Name         string
	Hosts        []string
	Repositories []string
	Forge        capability.Forge
}

// ChangeConfig hands out the current Changes; a reload replaces them.
type ChangeConfig interface {
	Changes() *Changes
}

// repository finds the forge for a repository as a source names it, and the
// repository's path on that forge. A repository no forge lists is refused.
func (c *Changes) repository(repo string) (capability.Forge, string, error) {
	r, err := source.ParseRepository(repo)
	if err != nil {
		return nil, "", err
	}
	for _, f := range c.Forges {
		if (r.Forge != "" && r.Forge == f.Name) || (r.Host != "" && slices.Contains(f.Hosts, r.Host)) {
			if !slices.Contains(f.Repositories, r.Path) {
				return nil, "", fmt.Errorf("repository %s is not listed for forge %s", r.Path, f.Name)
			}
			return f.Forge, r.Path, nil
		}
	}
	return nil, "", fmt.Errorf("no forge serves repository %s", repo)
}

// resolve finds the workload's source with the call's profile.
func (c *Changes) resolve(ctx context.Context, w capability.Workloads, v workloadInput) (*source.Source, error) {
	ref := capability.WorkloadRef{Namespace: v.Namespace, Kind: v.Kind, Name: v.Name}
	m, err := w.WorkloadMetadata(ctx, ref)
	if err != nil {
		return nil, err
	}
	return source.Resolve(ctx, source.Workload{
		Environment: v.Environment, Namespace: v.Namespace, Kind: v.Kind, Name: v.Name,
		Labels: m.Labels, Annotations: m.Annotations,
	}, c.Resolve)
}

// revision turns a location's revision into a ref the forge reads: HEAD and "" are
// the default branch.
func revision(ctx context.Context, f capability.Forge, repo, rev string) (string, error) {
	if rev != "" && rev != "HEAD" {
		return rev, nil
	}
	return f.DefaultBranch(ctx, repo)
}

type resolveInput struct {
	workloadInput
	Files []string `json:"files,omitempty" jsonschema:"paths of files in the source to return the content of, at most 10"`
}

// SourceView is resolve_source's answer.
type SourceView struct {
	Resolver    string         `json:"resolver"`
	Application string         `json:"application,omitempty"`
	Strategy    string         `json:"strategy"` // how propose_change acts in this environment
	Locations   []LocationView `json:"locations"`
	Contents    []FileContent  `json:"contents,omitempty"`
}

// LocationView is one location of a source, with its files.
type LocationView struct {
	Repo           string   `json:"repo"`
	Revision       string   `json:"revision"`
	Path           string   `json:"path,omitempty"`
	Files          []string `json:"files,omitempty"`
	FilesTruncated bool     `json:"filesTruncated,omitempty"`
	// Error says why the location cannot be read, such as a repository no forge lists.
	Error string `json:"error,omitempty"`
}

// FileContent is one file of the source.
type FileContent struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ResolveSource is resolve_source.
func ResolveSource(src capability.WorkloadSource, cfg ChangeConfig) (engine.Tool, error) {
	in, err := newInput[resolveInput](func(s *jsonschema.Schema) {
		refineWorkloadInput(s)
		s.Properties["files"].MaxItems = jsonschema.Ptr(maxReadFiles)
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "resolve_source",
		Description: "Find the repository files that declare a workload (through its Argo CD Application, an annotation, " +
			"or the configuration), list them, and return the content of the files asked for. Read before propose_change.",
		Risk: core.RiskRead, InputSchema: in.schema, Args: in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			return workloadPrepared(src, v.workloadInput, obj, func(ctx context.Context, w capability.Workloads, _ capability.WorkloadRef) (any, error) {
				return resolveSource(ctx, cfg.Changes(), w, v)
			}), nil
		},
	}, nil
}

func resolveSource(ctx context.Context, c *Changes, w capability.Workloads, v resolveInput) (*SourceView, error) {
	s, err := c.resolve(ctx, w, v.workloadInput)
	if err != nil {
		return nil, err
	}
	out := &SourceView{Resolver: s.Resolver, Application: s.Application, Strategy: c.Strategies[v.Environment]}
	type readable struct {
		loc       source.Location
		forge     capability.Forge
		repo, ref string
	}
	var reachable []readable
	for _, l := range s.Locations {
		lv := LocationView{Repo: l.Repo, Revision: l.Revision, Path: l.Path}
		f, repo, err := c.repository(l.Repo)
		if err == nil {
			lv.Revision, err = revision(ctx, f, repo, l.Revision)
		}
		if err == nil && l.Path != "" {
			lv.Files, lv.FilesTruncated, err = f.ListFiles(ctx, repo, lv.Revision, l.Path)
		}
		if err != nil {
			lv.Error = err.Error()
			out.Locations = append(out.Locations, lv)
			continue
		}
		for _, file := range l.Files {
			if !slices.Contains(lv.Files, file) {
				lv.Files = append(lv.Files, file)
			}
		}
		if len(lv.Files) > maxListedShown {
			lv.Files, lv.FilesTruncated = lv.Files[:maxListedShown], true
		}
		out.Locations = append(out.Locations, lv)
		reachable = append(reachable, readable{loc: l, forge: f, repo: repo, ref: lv.Revision})
	}
	for _, p := range v.Files {
		i := slices.IndexFunc(reachable, func(r readable) bool { return r.loc.Contains(p) })
		if i < 0 {
			return nil, fmt.Errorf("%w: %s is outside the workload's source", core.ErrInvalidInput, p)
		}
		r := reachable[i]
		b, found, err := r.forge.ReadFile(ctx, r.repo, r.ref, p)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: %s does not exist at %s", core.ErrInvalidInput, p, r.ref)
		}
		fc := FileContent{Path: p, Content: string(b)}
		if len(b) > maxShownBytes {
			fc.Content, fc.Truncated = strings.ToValidUTF8(string(b[:maxShownBytes]), ""), true
		}
		out.Contents = append(out.Contents, fc)
	}
	return out, nil
}

type proposeInput struct {
	workloadInput
	Title       string `json:"title" jsonschema:"one line: what the change does"`
	Description string `json:"description,omitempty" jsonschema:"why the change is made"`
	Edits       []Edit `json:"edits" jsonschema:"text replacements in files of the workload's source"`
}

// Edit replaces Old, which must occur exactly once in the file, with New.
type Edit struct {
	Path string `json:"path" jsonschema:"path of a file in the workload's source, as resolve_source lists it"`
	Old  string `json:"old" jsonschema:"text to replace; must occur exactly once in the file"`
	New  string `json:"new" jsonschema:"replacement text"`
}

// ProposeChange is propose_change. Its risk is write; under the suggest strategy a call
// lowers it to read.
func ProposeChange(src capability.WorkloadSource, cfg ChangeConfig) (engine.Tool, error) {
	in, err := newInput[proposeInput](func(s *jsonschema.Schema) {
		refineWorkloadInput(s)
		s.Properties["title"].MinLength = jsonschema.Ptr(1)
		s.Properties["title"].MaxLength = jsonschema.Ptr(200)
		s.Properties["description"].MaxLength = jsonschema.Ptr(4000)
		s.Properties["edits"].MinItems = jsonschema.Ptr(1)
		s.Properties["edits"].MaxItems = jsonschema.Ptr(maxEdits)
		s.Properties["edits"].Items.Properties["old"].MinLength = jsonschema.Ptr(1)
	})
	if err != nil {
		return engine.Tool{}, err
	}
	return engine.Tool{
		Name: "propose_change",
		Description: "Propose edits to the files that declare a workload (see resolve_source). Depending on the environment " +
			"this returns the diff only (suggest) or opens a pull request (pull-request); nothing is applied to the cluster.",
		Risk: core.RiskWrite, InputSchema: in.schema, Args: in.names,
		Prepare: func(raw json.RawMessage) (*engine.Prepared, error) {
			v, obj, err := in.decode(raw)
			if err != nil {
				return nil, err
			}
			c := cfg.Changes()
			strategy := c.Strategies[v.Environment]
			p := workloadPrepared(src, v.workloadInput, obj, func(ctx context.Context, w capability.Workloads, _ capability.WorkloadRef) (any, error) {
				return propose(ctx, c, w, v, strategy, true)
			})
			if strategy == strategyPullRequest {
				p.Preview = func(ctx context.Context, profile string) (*core.Change, error) {
					w, err := src.Workloads(v.Environment, profile)
					if err != nil {
						return nil, err
					}
					return propose(ctx, c, w, v, strategy, false)
				}
			} else {
				p.Risk = core.RiskRead
			}
			return p, nil
		},
	}, nil
}

// commitSHA is a full commit id: a pull request needs a branch to target.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// propose computes the change and, under pull-request with apply set, opens it.
func propose(ctx context.Context, c *Changes, w capability.Workloads, v proposeInput, strategy string, apply bool) (*core.Change, error) {
	s, err := c.resolve(ctx, w, v.workloadInput)
	if errors.Is(err, source.ErrUnresolved) {
		return &core.Change{Strategy: strategySuggest, Note: err.Error() + "; nothing was proposed. Make the change in its source by hand, " +
			"or have the workload annotated with its source."}, nil
	}
	if err != nil {
		return nil, err
	}
	byPath := map[string][]Edit{}
	var paths []string
	var loc *source.Location
	for _, e := range v.Edits {
		i := slices.IndexFunc(s.Locations, func(l source.Location) bool { return l.Contains(e.Path) })
		if i < 0 {
			return nil, fmt.Errorf("%w: %s is outside the workload's source", core.ErrInvalidInput, e.Path)
		}
		l := s.Locations[i]
		if loc == nil {
			loc = &l
		} else if l.Repo != loc.Repo || l.Revision != loc.Revision {
			return nil, fmt.Errorf("%w: edits span repositories %s and %s; propose one at a time", core.ErrInvalidInput, loc.Repo, l.Repo)
		}
		if _, ok := byPath[e.Path]; !ok {
			paths = append(paths, e.Path)
		}
		byPath[e.Path] = append(byPath[e.Path], e)
	}
	if len(paths) > maxProposalFiles {
		return nil, fmt.Errorf("%w: %d files; at most %d per proposal", core.ErrInvalidInput, len(paths), maxProposalFiles)
	}
	f, repo, err := c.repository(loc.Repo)
	if err != nil {
		return nil, err
	}
	base, err := revision(ctx, f, repo, loc.Revision)
	if err != nil {
		return nil, err
	}
	if strategy == strategyPullRequest && commitSHA.MatchString(base) {
		return nil, fmt.Errorf("%w: the source is pinned to commit %s; a pull request needs a branch", core.ErrInvalidInput, base)
	}
	files := map[string][]byte{}
	var diff strings.Builder
	for _, p := range paths {
		before, found, err := f.ReadFile(ctx, repo, base, p)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: %s does not exist at %s", core.ErrInvalidInput, p, base)
		}
		if len(before) > maxFileBytes {
			return nil, fmt.Errorf("%w: %s is larger than %d bytes", core.ErrInvalidInput, p, maxFileBytes)
		}
		after := string(before)
		for _, e := range byPath[p] {
			switch n := strings.Count(after, e.Old); n {
			case 1:
				after = strings.Replace(after, e.Old, e.New, 1)
			case 0:
				return nil, fmt.Errorf("%w: %s: the old text was not found at %s", core.ErrInvalidInput, p, base)
			default:
				return nil, fmt.Errorf("%w: %s: the old text occurs %d times; include more context so it occurs once", core.ErrInvalidInput, p, n)
			}
		}
		if after == string(before) {
			return nil, fmt.Errorf("%w: %s: the edit changes nothing", core.ErrInvalidInput, p)
		}
		if !utf8.ValidString(after) {
			return nil, fmt.Errorf("%w: %s is not UTF-8 text", core.ErrInvalidInput, p)
		}
		files[p] = []byte(after)
		d, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: difflib.SplitLines(string(before)), B: difflib.SplitLines(after),
			FromFile: "a/" + p, ToFile: "b/" + p, Context: 3,
		})
		if err != nil {
			return nil, err
		}
		diff.WriteString(d)
	}
	ch := &core.Change{Strategy: strategy, Diff: redact.String(diff.String())}
	if strategy != strategyPullRequest || !apply {
		return ch, nil
	}
	id, ok := core.AuditIDFrom(ctx)
	if !ok {
		return nil, errors.New("propose_change: no audit record to refer to")
	}
	who := "unknown"
	if p, ok := core.PrincipalFrom(ctx); ok {
		who = p.Subject
	}
	trailer := fmt.Sprintf("Proposed through Opsward by %s for %s %s/%s in %s.\nAudit record: %s",
		who, v.Kind, v.Namespace, v.Name, v.Environment, id)
	body := trailer
	if v.Description != "" {
		body = v.Description + "\n\n---\n" + trailer
	}
	ch.URL, err = f.Propose(ctx, repo, capability.Proposal{
		Base: base, Branch: "opsward/" + id, Title: v.Title, Body: body,
		Message: v.Title + "\n\n" + body, Files: files,
	})
	if err != nil {
		return nil, err
	}
	return ch, nil
}
