package port

import (
	"context"
	"time"
)

// Version control over generated workspaces.
//
// Declared here rather than imported from infrastructure so the workspace use
// case depends on the capability, not on git. That keeps the dependency rule
// intact and means a future backend — a hosted VCS, or an in-memory one for
// tests — is a substitution rather than a rewrite.

// Commit describes one revision of a generated project.
type Commit struct {
	SHA     string    `json:"sha"`
	Short   string    `json:"short"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	When    time.Time `json:"when"`
	Files   []string  `json:"files,omitempty"`
	Added   int       `json:"added"`
	Removed int       `json:"removed"`
}

// VCSStatus summarises a working tree.
type VCSStatus struct {
	Branch    string   `json:"branch"`
	Head      string   `json:"head"`
	Clean     bool     `json:"clean"`
	Modified  []string `json:"modified,omitempty"`
	Untracked []string `json:"untracked,omitempty"`
	Deleted   []string `json:"deleted,omitempty"`
}

// VersionControl manages history for one workspace.
type VersionControl interface {
	Snapshot(ctx context.Context, message, author string) (string, error)
	Status(ctx context.Context) (*VCSStatus, error)
	Log(ctx context.Context, limit int) ([]Commit, error)
	Diff(ctx context.Context, ref string) (string, error)
	Reset(ctx context.Context, ref string) error
	Branch(ctx context.Context, name string) error
}

// VersionControlFactory opens version control for a workspace path.
//
// A factory rather than an instance because a workspace is chosen per request:
// the use case serves many projects and each has its own repository.
type VersionControlFactory interface {
	Open(ctx context.Context, workspace string) (VersionControl, error)
	Available() bool
}
