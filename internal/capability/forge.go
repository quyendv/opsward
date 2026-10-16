package capability

import "context"

// Forge reads files from and proposes changes to repositories on a Git hosting service
// (docs/design/0008). Repositories are "owner/name" (a GitLab project path).
type Forge interface {
	// DefaultBranch returns the repository's default branch.
	DefaultBranch(ctx context.Context, repo string) (string, error)
	// ResolveRef returns the commit a branch, tag or commit points to; branch reports
	// whether ref is a branch, which a proposal needs as its base.
	ResolveRef(ctx context.Context, repo, ref string) (commit string, branch bool, err error)
	// ReadFile returns a file's content at ref; found is false when it does not exist.
	ReadFile(ctx context.Context, repo, ref, path string) (content []byte, found bool, err error)
	// ListFiles returns the paths of the files under dir at ref, recursively and sorted,
	// at most MaxListedFiles of them; truncated reports that there were more.
	ListFiles(ctx context.Context, repo, ref, dir string) (files []string, truncated bool, err error)
	// Propose creates Branch from Base with one commit replacing Files, and opens a pull
	// (merge) request into Base. It returns the request's web URL.
	Propose(ctx context.Context, repo string, p Proposal) (string, error)
}

// MaxListedFiles bounds Forge.ListFiles.
const MaxListedFiles = 2000

// Proposal is one change to propose.
type Proposal struct {
	Base string // branch the change is proposed into
	// BaseCommit is the commit of Base the files were read at: the change is built on
	// it, so a commit to Base since then cannot be reverted by the proposal.
	BaseCommit string
	Branch     string            // new branch holding the change
	Title      string            // of the pull request, and the commit's subject
	Body       string            // of the pull request
	Message    string            // the commit message
	Files      map[string][]byte // path -> new content; every file exists at BaseCommit
}
