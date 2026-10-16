// Package source finds the repository files that declare a workload: through the Argo CD
// Application that manages it, an annotation on it, or a static map. docs/design/0008.
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

// Contains reports whether p, a path in the location's repository, is part of it.
func (l Location) Contains(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	if slices.Contains(l.Files, p) {
		return true
	}
	return l.Path != "" && strings.HasPrefix(p, strings.TrimSuffix(l.Path, "/")+"/")
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
}

// Resolve tries Argo CD, then the annotations, then the static map.
func Resolve(ctx context.Context, w Workload, opt Options) (*Source, error) {
	if opt.Applications != nil {
		if ns, name, byAnnotation, ok := application(w, opt.ApplicationNamespace); ok {
			app, err := opt.Applications(ctx, ns, name)
			switch {
			case errors.Is(err, ErrNoApplication) && !byAnnotation:
				// The label is also a chart convention: try the other resolvers.
			case errors.Is(err, ErrNoApplication):
				return nil, fmt.Errorf("%w: the workload names Argo CD Application %s/%s, which does not exist", ErrUnresolved, ns, name)
			case err != nil:
				return nil, fmt.Errorf("reading Argo CD Application %s/%s: %w", ns, name, err)
			default:
				return &Source{Resolver: "argocd", Application: ns + "/" + name, Locations: app.locations()}, nil
			}
		}
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
// with label tracking, by label. "<namespace>_<name>" names an Application outside the
// default namespace.
func application(w Workload, defaultNS string) (ns, name string, byAnnotation, ok bool) {
	if id := w.Annotations["argocd.argoproj.io/tracking-id"]; id != "" {
		name, _, _ = strings.Cut(id, ":")
		byAnnotation = true
	} else if v := w.Labels["app.kubernetes.io/instance"]; v != "" {
		name = v
	}
	if name == "" {
		return "", "", false, false
	}
	if n, rest, found := strings.Cut(name, "_"); found {
		return n, rest, byAnnotation, true
	}
	return defaultNS, name, byAnnotation, true
}

// ParseLocation reads <repo>//<path>[@<revision>].
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
	return Location{Repo: repo, Revision: rev, Path: strings.TrimSuffix(p, "/")}, nil
}

// Application is the part of an Argo CD Application that says where it comes from.
type Application struct {
	Spec struct {
		Source  *ApplicationSource  `json:"source"`
		Sources []ApplicationSource `json:"sources"`
	} `json:"spec"`
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
