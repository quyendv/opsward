// Package source finds the repository files that declare a workload: through the Argo CD
// Application that manages it, an annotation on it, or a static map (docs/design/0008);
// and that Application, with its status (docs/design/0010).
package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// ErrUnresolved is returned when no resolver knows the workload's source.
var ErrUnresolved = errors.New("source not resolved")

// ErrNoApplication is returned by an application reader for an Application that does not
// exist.
var ErrNoApplication = errors.New("no such Argo CD Application")

// Workload is what resolution needs to know about the target.
type Workload struct {
	Environment, Namespace, Kind, Name string
	Labels, Annotations                map[string]string
}

// Location is part of a repository: a directory, explicit files, or both.
type Location struct {
	Repo     string   `json:"repo"`     // as written in the source: a URL, or forge:owner/name
	Revision string   `json:"revision"` // "" or HEAD: the default branch
	Path     string   `json:"path,omitempty"`
	Files    []string `json:"files,omitempty"`
}

// Contains reports whether p, a path in the location's repository, is part of it. A
// location whose path is "." is the whole repository.
func (l Location) Contains(p string) bool {
	if p == "." || !inRepo(p) {
		return false
	}
	if slices.Contains(l.Files, p) {
		return true
	}
	switch {
	case l.Path == ".":
		return true
	case l.Path != "" && inRepo(l.Path):
		return strings.HasPrefix(p, l.Path+"/")
	}
	return false
}

// inRepo reports whether p is a clean path inside a repository: relative, without "."
// or ".." segments or empty ones. Paths reach forge API URLs, where a ".." segment could
// leave the repository.
func inRepo(p string) bool {
	if p == "." {
		return true
	}
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return false
		}
	}
	return true
}

// ValidRevision reports whether rev can name a branch, tag or commit, following the rules
// of git check-ref-format; it rules out what could change a forge API URL.
func ValidRevision(rev string) bool {
	if rev == "" {
		return true
	}
	if strings.HasPrefix(rev, "/") || strings.HasPrefix(rev, "-") || strings.HasSuffix(rev, "/") ||
		strings.HasSuffix(rev, ".") || strings.HasSuffix(rev, ".lock") ||
		strings.Contains(rev, "..") || strings.Contains(rev, "//") || strings.Contains(rev, "@{") ||
		strings.ContainsAny(rev, " ~^:?*[\\%#") {
		return false
	}
	for _, r := range rev {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(rev, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// check normalizes the location's path ("./apps/" is "apps") and refuses paths that
// leave the repository and revisions that are not refs.
func (l *Location) check() error {
	if l.Path != "" {
		l.Path = path.Clean(l.Path)
		if !inRepo(l.Path) {
			return fmt.Errorf("path %q is not inside the repository", l.Path)
		}
	}
	for i, f := range l.Files {
		l.Files[i] = path.Clean(f)
		if l.Files[i] == "." || !inRepo(l.Files[i]) {
			return fmt.Errorf("file %q is not inside the repository", f)
		}
	}
	if !ValidRevision(l.Revision) {
		return fmt.Errorf("revision %q is not a valid ref", l.Revision)
	}
	return nil
}

// Source is where a workload is declared.
type Source struct {
	Resolver    string     `json:"resolver"` // argocd, annotation or static
	Application string     `json:"application,omitempty"`
	Locations   []Location `json:"locations"`
}

// Static maps one workload to a source; an empty Kind matches any kind.
type Static struct {
	Environment, Namespace, Kind, Name string
	Source                             string // <repo>//<path>[@<revision>]
}

// Options configures resolution.
type Options struct {
	AnnotationKeys []string
	Static         []Static
	// Applications reads an Argo CD Application; nil when there is no Argo CD.
	Applications         func(ctx context.Context, namespace, name string) (*Application, error)
	ApplicationNamespace string
	// ApplicationNamespaces are the other namespaces Applications may be read from,
	// named <namespace>_<name>; any other is refused.
	ApplicationNamespaces []string
	// LabelTracking: Argo CD tracks resources by the app.kubernetes.io/instance label,
	// which is read only then: charts set it too.
	LabelTracking bool
}

// Managed is the Argo CD Application that manages a workload.
type Managed struct {
	Namespace, Name string
	App             *Application
}

// Managing returns the Argo CD Application that manages the workload, or nil when no
// Application tracks it (or, with label tracking, the label names none). A tracking id
// that names another workload, an Application in a namespace not listed, or one deploying
// elsewhere is ErrUnresolved: the workload cannot borrow another Application.
func Managing(ctx context.Context, w Workload, opt Options) (*Managed, error) {
	if opt.Applications == nil {
		return nil, nil
	}
	ns, name, byAnnotation, err := application(w, opt.ApplicationNamespace, opt.LabelTracking)
	if err != nil || name == "" {
		return nil, err
	}
	if ns != opt.ApplicationNamespace && !slices.Contains(opt.ApplicationNamespaces, ns) {
		return nil, fmt.Errorf("%w: Argo CD Application %s/%s is in a namespace providers.delivery.applicationNamespaces does not list",
			ErrUnresolved, ns, name)
	}
	app, err := opt.Applications(ctx, ns, name)
	switch {
	case errors.Is(err, ErrNoApplication) && !byAnnotation:
		// The label is also a chart convention: nothing manages the workload.
		return nil, nil
	case errors.Is(err, ErrNoApplication):
		return nil, fmt.Errorf("%w: the workload names Argo CD Application %s/%s, which does not exist", ErrUnresolved, ns, name)
	case err != nil:
		return nil, fmt.Errorf("reading Argo CD Application %s/%s: %w", ns, name, err)
	case app.Spec.Destination.Namespace != w.Namespace:
		// Argo CD would not let this Application manage the workload either.
		return nil, fmt.Errorf("%w: Argo CD Application %s/%s deploys to namespace %s, not %s",
			ErrUnresolved, ns, name, app.Spec.Destination.Namespace, w.Namespace)
	}
	return &Managed{Namespace: ns, Name: name, App: app}, nil
}

// Resolve tries Argo CD, then the annotations, then the static map.
func Resolve(ctx context.Context, w Workload, opt Options) (*Source, error) {
	m, err := Managing(ctx, w, opt)
	if err != nil {
		return nil, err
	}
	if m != nil {
		locs := m.App.locations()
		for i := range locs {
			if err := locs[i].check(); err != nil {
				return nil, fmt.Errorf("argo CD Application %s/%s: %w", m.Namespace, m.Name, err)
			}
		}
		return &Source{Resolver: "argocd", Application: m.Namespace + "/" + m.Name, Locations: locs}, nil
	}
	for _, k := range opt.AnnotationKeys {
		if v := w.Annotations[k]; v != "" {
			l, err := ParseLocation(v)
			if err != nil {
				return nil, fmt.Errorf("annotation %s: %w", k, err)
			}
			return &Source{Resolver: "annotation", Locations: []Location{l}}, nil
		}
	}
	for _, s := range opt.Static {
		if s.Environment == w.Environment && s.Namespace == w.Namespace && s.Name == w.Name && (s.Kind == "" || s.Kind == w.Kind) {
			l, err := ParseLocation(s.Source)
			if err != nil {
				return nil, fmt.Errorf("static source: %w", err)
			}
			return &Source{Resolver: "static", Locations: []Location{l}}, nil
		}
	}
	return nil, fmt.Errorf("%w: no Argo CD Application, annotation or static entry names %s %s/%s", ErrUnresolved, w.Kind, w.Namespace, w.Name)
}

// application names the Argo CD Application that tracks the workload, by annotation or,
// with label tracking, by label; name is empty when neither does. "<namespace>_<name>"
// names an Application outside the default namespace. A tracking id must name the
// workload itself (<app>:<group>/<kind>:<namespace>/<name>), as Argo CD checks, so that
// it cannot borrow another Application's source.
func application(w Workload, defaultNS string, labels bool) (ns, name string, byAnnotation bool, err error) {
	if id := w.Annotations["argocd.argoproj.io/tracking-id"]; id != "" {
		parts := strings.SplitN(id, ":", 3)
		if len(parts) != 3 || parts[1] != "apps/"+w.Kind || parts[2] != w.Namespace+"/"+w.Name {
			return "", "", false, fmt.Errorf("%w: tracking id %q does not name %s %s/%s", ErrUnresolved, id, w.Kind, w.Namespace, w.Name)
		}
		name, byAnnotation = parts[0], true
	} else if v := w.Labels["app.kubernetes.io/instance"]; v != "" && labels {
		name = v
	}
	if name == "" {
		return "", "", false, nil
	}
	if n, rest, found := strings.Cut(name, "_"); found {
		return n, rest, byAnnotation, nil
	}
	return defaultNS, name, byAnnotation, nil
}

// ParseLocation reads <repo>//<path>[@<revision>]; the path must stay inside the
// repository and the revision must be a valid ref.
func ParseLocation(s string) (Location, error) {
	i := strings.LastIndex(s, "//")
	if i <= 0 || strings.HasSuffix(s[:i], ":") {
		return Location{}, fmt.Errorf("%q: want <repository>//<path>[@<revision>]", s)
	}
	repo, rest := s[:i], s[i+2:]
	p, rev, _ := strings.Cut(rest, "@")
	if p == "" {
		return Location{}, fmt.Errorf("%q: no path", s)
	}
	l := Location{Repo: repo, Revision: rev, Path: p}
	if err := l.check(); err != nil {
		return Location{}, fmt.Errorf("%q: %w", s, err)
	}
	return l, nil
}

// Application is the part of an Argo CD Application that says where it comes from.
type Application struct {
	Spec struct {
		Source      *ApplicationSource  `json:"source"`
		Sources     []ApplicationSource `json:"sources"`
		Destination struct {
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
	Status ApplicationStatus `json:"status"`
}

// ApplicationStatus is what Argo CD reports about an Application (docs/design/0010).
type ApplicationStatus struct {
	Sync struct {
		Status   string `json:"status"`
		Revision string `json:"revision"`
	} `json:"sync"`
	Health struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"health"`
	OperationState *struct {
		Phase      string `json:"phase"`
		Message    string `json:"message"`
		StartedAt  string `json:"startedAt"`
		FinishedAt string `json:"finishedAt"`
		SyncResult *struct {
			Revision string `json:"revision"`
		} `json:"syncResult"`
	} `json:"operationState"`
	Conditions []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"conditions"`
	Resources []struct {
		Group     string `json:"group"`
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Status    string `json:"status"`
		Health    *struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"health"`
	} `json:"resources"`
	History []struct {
		Revision   string `json:"revision"`
		DeployedAt string `json:"deployedAt"`
	} `json:"history"`
}

// TargetRevision is the revision the Application's first source targets.
func (a *Application) TargetRevision() string {
	if a.Spec.Source != nil {
		return a.Spec.Source.TargetRevision
	}
	if len(a.Spec.Sources) > 0 {
		return a.Spec.Sources[0].TargetRevision
	}
	return ""
}

// ApplicationSource is one source of an Application.
type ApplicationSource struct {
	RepoURL        string `json:"repoURL"`
	Path           string `json:"path"`
	TargetRevision string `json:"targetRevision"`
	Chart          string `json:"chart"`
	Ref            string `json:"ref"`
	Helm           *struct {
		ValueFiles []string `json:"valueFiles"`
	} `json:"helm"`
}

// locations turns an Application's sources into repository locations. A chart from a
// Helm repository is not one; value files it reads from a Git source through $ref are.
func (a *Application) locations() []Location {
	sources := a.Spec.Sources
	if a.Spec.Source != nil {
		sources = append([]ApplicationSource{*a.Spec.Source}, sources...)
	}
	refs := map[string]ApplicationSource{}
	for _, s := range sources {
		if s.Ref != "" {
			refs[s.Ref] = s
		}
	}
	var out []Location
	add := func(repo, rev, dir, file string) {
		for i := range out {
			if out[i].Repo == repo && out[i].Revision == rev {
				if dir != "" && out[i].Path == "" {
					out[i].Path = dir
				}
				if file != "" && !slices.Contains(out[i].Files, file) {
					out[i].Files = append(out[i].Files, file)
				}
				return
			}
		}
		l := Location{Repo: repo, Revision: rev, Path: dir}
		if file != "" {
			l.Files = []string{file}
		}
		out = append(out, l)
	}
	for _, s := range sources {
		if s.Path != "" {
			add(s.RepoURL, s.TargetRevision, s.Path, "")
		}
		if s.Helm == nil {
			continue
		}
		for _, vf := range s.Helm.ValueFiles {
			if strings.HasPrefix(vf, "$") {
				name, rest, _ := strings.Cut(strings.TrimPrefix(vf, "$"), "/")
				if r, ok := refs[name]; ok && rest != "" {
					add(r.RepoURL, r.TargetRevision, "", path.Clean(rest))
				}
				continue
			}
			if s.Path != "" {
				add(s.RepoURL, s.TargetRevision, "", path.Join(s.Path, vf))
			}
		}
	}
	return out
}

// Repository identifies a repository: by forge name (forge:owner/name) or by host and
// path, from a URL.
type Repository struct {
	Forge string // set for forge:owner/name
	Host  string // set for URLs
	Path  string // owner/name, or group/subgroup/name
}

// ParseRepository reads an HTTPS or SSH URL, an scp-like address, or forge:owner/name.
func ParseRepository(s string) (Repository, error) {
	trim := func(p string) string { return strings.TrimSuffix(strings.Trim(p, "/"), ".git") }
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" {
			return Repository{}, fmt.Errorf("repository %q: not a URL", s)
		}
		return Repository{Host: u.Hostname(), Path: trim(u.Path)}, nil
	case strings.Contains(s, "@") && strings.Contains(s, ":"):
		hostPart, p, _ := strings.Cut(s, ":")
		_, host, _ := strings.Cut(hostPart, "@")
		return Repository{Host: host, Path: trim(p)}, nil
	case strings.Contains(s, ":"):
		forge, p, _ := strings.Cut(s, ":")
		return Repository{Forge: forge, Path: trim(p)}, nil
	}
	return Repository{}, fmt.Errorf("repository %q: want a URL or forge:owner/name", s)
}
