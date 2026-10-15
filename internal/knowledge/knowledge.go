// Package knowledge loads knowledge packs: Markdown documents that agents can search and
// that attach themselves to tool results when a call matches them. See docs/design/0003.
package knowledge

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"

	"cel.dev/cel-go/cel"
	"go.yaml.in/yaml/v3"

	"github.com/quyendv/opsward/internal/core"
)

//go:embed builtin/*.md
var builtin embed.FS

// celCostLimit matches the per-call limit used for policy conditions.
const celCostLimit = 1_000_000

// maxDocBytes bounds one knowledge file.
const maxDocBytes = 256 << 10

// annotateBudget is the CEL cost all documents' conditions may spend on one call, so
// that many documents over a large result cannot add unbounded work to every call.
var annotateBudget uint64 = 5_000_000

var (
	validID       = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	severityOrder = map[string]int{"critical": 0, "warning": 1, "info": 2}
)

// Doc is one knowledge document.
type Doc struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	Body     string   `json:"body"`
	Tools    []string `json:"-"`
	When     string   `json:"-"`

	when cel.Program
	// Tokens of the title (with the id), summary and body, computed once at load.
	titleT, summaryT, bodyT []string
}

type frontmatter struct {
	ID        string `yaml:"id"`
	Title     string `yaml:"title"`
	Severity  string `yaml:"severity"`
	Summary   string `yaml:"summary"`
	AppliesTo *struct {
		Tools []string `yaml:"tools"`
		When  string   `yaml:"when"`
	} `yaml:"applies-to"`
}

// errNoFrontmatter marks Markdown that is not a knowledge document, such as a README in
// a pack repository. Load skips such files with a warning.
var errNoFrontmatter = errors.New("must start with a --- frontmatter block")

// parse reads one document: YAML frontmatter between "---" lines, then Markdown.
func parse(name string, b []byte) (Doc, error) {
	b = bytes.TrimPrefix(b, []byte("\ufeff"))
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(b, []byte("---\n"))
	if !ok {
		return Doc{}, fmt.Errorf("%s: %w", name, errNoFrontmatter)
	}
	head, body, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		if head, ok = bytes.CutSuffix(rest, []byte("\n---")); !ok {
			return Doc{}, fmt.Errorf("%s: frontmatter block is not closed with ---", name)
		}
	}
	var fm frontmatter
	dec := yaml.NewDecoder(bytes.NewReader(head))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil {
		return Doc{}, fmt.Errorf("%s: frontmatter: %w", name, err)
	}
	d := Doc{ID: fm.ID, Title: fm.Title, Severity: fm.Severity, Summary: fm.Summary, Body: strings.TrimSpace(string(body))}
	switch {
	case d.ID == "":
		return Doc{}, fmt.Errorf("%s: id is required", name)
	case !validID.MatchString(d.ID):
		return Doc{}, fmt.Errorf("%s: id %q must be lowercase letters, digits and dashes", name, d.ID)
	case d.Title == "":
		return Doc{}, fmt.Errorf("%s: title is required", name)
	}
	if d.Severity == "" {
		d.Severity = "info"
	}
	if _, ok := severityOrder[d.Severity]; !ok {
		return Doc{}, fmt.Errorf("%s: severity %q must be info, warning or critical", name, d.Severity)
	}
	if d.Summary == "" {
		d.Summary = firstParagraph(d.Body)
	}
	if fm.AppliesTo != nil {
		if len(fm.AppliesTo.Tools) == 0 {
			return Doc{}, fmt.Errorf("%s: applies-to.tools is required when applies-to is set", name)
		}
		d.Tools, d.When = fm.AppliesTo.Tools, fm.AppliesTo.When
	}
	return d, nil
}

// firstParagraph returns the body's first paragraph that is not a heading.
func firstParagraph(body string) string {
	for para := range strings.SplitSeq(strings.TrimSpace(body), "\n\n") {
		para = strings.TrimSpace(para)
		if para != "" && !strings.HasPrefix(para, "#") {
			return strings.Join(strings.Fields(para), " ")
		}
	}
	return ""
}

// Options selects where documents come from.
type Options struct {
	Builtin bool     // the generic pack embedded in the binary
	Paths   []string // directories, read recursively for *.md
	Tools   []string // registered tool names, to warn about applies-to entries that never match
}

// Pack is a loaded, validated set of documents. It is immutable and safe for
// concurrent use.
type Pack struct {
	docs []Doc
	byID map[string]int
}

// Load reads and validates every document. It reports problems as one error.
func Load(opt Options) (*Pack, []string, error) {
	env, err := cel.NewEnv(
		cel.Variable("tool", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("scope", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("args", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("result", cel.DynType),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("knowledge: CEL environment: %w", err)
	}

	type file struct {
		name string
		body []byte
	}
	var files []file
	var errs []error
	if opt.Builtin {
		names, _ := fs.Glob(builtin, "builtin/*.md")
		for _, n := range names {
			b, err := builtin.ReadFile(n)
			if err != nil {
				return nil, nil, err
			}
			files = append(files, file{"builtin:" + strings.TrimPrefix(n, "builtin/"), b})
		}
	}
	var warnings []string
	for _, configured := range opt.Paths {
		// A root that is itself a symlink (git-sync's "current") is followed; WalkDir
		// would otherwise stop at it.
		root, err := filepath.EvalSymlinks(configured)
		if err != nil {
			errs = append(errs, fmt.Errorf("knowledge path %q: %w", configured, err))
			continue
		}
		before := len(files)
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// Hidden entries are skipped: a ConfigMap volume keeps its real files in a
			// hidden "..<timestamp>" directory behind "..data", and the visible names are
			// symlinks into it, so walking both would load every document twice.
			if path != root && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
				return nil
			}
			if info, err := d.Info(); err == nil && info.Size() > maxDocBytes {
				errs = append(errs, fmt.Errorf("knowledge: %s: larger than %d bytes", path, maxDocBytes))
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // G304: reading the operator's configured knowledge paths is the point
			if err != nil {
				return err
			}
			files = append(files, file{path, b})
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("knowledge: %w", err))
		} else if len(files) == before {
			warnings = append(warnings, fmt.Sprintf("knowledge path %q: no knowledge documents (*.md) found", configured))
		}
	}

	p := &Pack{byID: map[string]int{}}
	for _, f := range files {
		d, err := parse(f.name, f.body)
		if errors.Is(err, errNoFrontmatter) {
			warnings = append(warnings, fmt.Sprintf("knowledge: %s: no frontmatter, skipped", f.name))
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("knowledge: %w", err))
			continue
		}
		if _, dup := p.byID[d.ID]; dup {
			errs = append(errs, fmt.Errorf("knowledge: %s: duplicate id %q", f.name, d.ID))
			continue
		}
		if d.When != "" {
			ast, iss := env.Compile(d.When)
			if iss.Err() != nil {
				errs = append(errs, fmt.Errorf("knowledge: %s: when: %w", f.name, iss.Err()))
				continue
			}
			if ast.OutputType() != cel.BoolType {
				errs = append(errs, fmt.Errorf("knowledge: %s: when must evaluate to bool, not %s", f.name, ast.OutputType()))
				continue
			}
			if d.when, err = env.Program(ast, cel.CostLimit(celCostLimit), cel.EvalOptions(cel.OptTrackCost)); err != nil {
				errs = append(errs, fmt.Errorf("knowledge: %s: when: %w", f.name, err))
				continue
			}
		}
		for _, t := range d.Tools {
			if opt.Tools != nil && !slices.Contains(opt.Tools, t) {
				warnings = append(warnings, fmt.Sprintf("knowledge: %s: applies-to tool %q is not registered", f.name, t))
			}
		}
		d.titleT, d.summaryT, d.bodyT = tokens(d.Title+" "+d.ID), tokens(d.Summary), tokens(d.Body)
		p.byID[d.ID] = len(p.docs)
		p.docs = append(p.docs, d)
	}
	if len(errs) > 0 {
		return nil, warnings, errors.Join(errs...)
	}
	return p, warnings, nil
}

// Get returns a document by id.
func (p *Pack) Get(id string) (Doc, bool) {
	i, ok := p.byID[id]
	if !ok {
		return Doc{}, false
	}
	return p.docs[i], true
}

// Annotate implements engine.Annotator: the notes of every document whose applies-to
// matches, most severe first. A condition that fails to evaluate does not attach. Once
// the conditions have spent annotateBudget, the remaining conditional documents are
// skipped; documents without a condition still attach.
func (p *Pack) Annotate(req core.Request, result any) []core.Note {
	var notes []core.Note
	var spent uint64
	for i := range p.docs {
		d := &p.docs[i]
		if !slices.Contains(d.Tools, req.Tool) {
			continue
		}
		if d.when != nil {
			if spent >= annotateBudget {
				continue
			}
			args := req.Args
			if args == nil {
				args = map[string]any{}
			}
			out, details, err := d.when.Eval(map[string]any{
				"tool":   map[string]any{"name": req.Tool, "risk": string(req.Risk)},
				"scope":  map[string]any{"environment": req.Scope.Environment, "namespace": req.Scope.Namespace, "kind": req.Scope.Kind, "name": req.Scope.Name},
				"args":   args,
				"result": result,
			})
			if details != nil && details.ActualCost() != nil {
				spent += *details.ActualCost()
			}
			if err != nil {
				continue
			}
			if ok, _ := out.Value().(bool); !ok {
				continue
			}
		}
		notes = append(notes, core.Note{ID: d.ID, Title: d.Title, Severity: d.Severity, Summary: d.Summary})
	}
	sort.SliceStable(notes, func(i, j int) bool { return severityOrder[notes[i].Severity] < severityOrder[notes[j].Severity] })
	return notes
}

// Hit is a search result.
type Hit struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// Search ranks documents by query terms in the title, summary and body.
func (p *Pack) Search(query string, limit int) []Hit {
	terms := tokens(query)
	type scored struct {
		doc   *Doc
		score int
	}
	var ranked []scored
	for i := range p.docs {
		d := &p.docs[i]
		score := 0
		for _, t := range terms {
			score += 3*count(d.titleT, t) + 2*count(d.summaryT, t) + count(d.bodyT, t)
		}
		if score > 0 {
			ranked = append(ranked, scored{d, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].doc.ID < ranked[j].doc.ID
	})
	hits := []Hit{}
	for _, r := range ranked {
		if limit > 0 && len(hits) == limit {
			break
		}
		hits = append(hits, Hit{ID: r.doc.ID, Title: r.doc.Title, Severity: r.doc.Severity, Summary: r.doc.Summary})
	}
	return hits
}

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])|([A-Z])([A-Z][a-z])`)

// tokens splits text into lower-case word stems of two or more characters, so that
// "OOMKilled" finds "oom killed", "scheduling" finds "schedule" and "images" "image".
func tokens(s string) []string {
	s = camelBoundary.ReplaceAllString(s, "${1}${3} ${2}${4}")
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := words[:0]
	for _, w := range words {
		if w = stem(w); len(w) >= 2 {
			out = append(out, w)
		}
	}
	return out
}

// stem removes one inflection suffix and then a final "e", the same way for every word.
func stem(w string) string {
	for _, suf := range []string{"ing", "es", "ed", "s"} {
		if len(w) > len(suf)+2 && strings.HasSuffix(w, suf) && (suf != "s" || !strings.HasSuffix(w, "ss")) {
			w = strings.TrimSuffix(w, suf)
			break
		}
	}
	if len(w) > 3 && strings.HasSuffix(w, "e") {
		w = strings.TrimSuffix(w, "e")
	}
	return w
}

func count(words []string, t string) int {
	n := 0
	for _, w := range words {
		if w == t {
			n++
		}
	}
	return n
}

// Library holds the current pack and lets a configuration reload replace it.
type Library struct {
	p atomic.Pointer[Pack]
}

// NewLibrary returns a library holding p.
func NewLibrary(p *Pack) *Library {
	l := &Library{}
	l.Set(p)
	return l
}

// Set replaces the pack.
func (l *Library) Set(p *Pack) { l.p.Store(p) }

// Pack returns the current pack.
func (l *Library) Pack() *Pack { return l.p.Load() }

// Annotate implements engine.Annotator with the current pack.
func (l *Library) Annotate(req core.Request, result any) []core.Note {
	return l.Pack().Annotate(req, result)
}
