// Package vcs provides version control over generated workspaces.
//
// Why this exists: the factory writes and rewrites a user's project. Without
// version control an agent's mistake is unrecoverable, a user cannot see what
// changed, and "the AI broke my code" has no remedy. Git makes every agent
// action reversible and inspectable with tools the user already knows.
//
// Why shelling out to git rather than a Go library: the generated repository is
// for the user, not for us. It must be a completely ordinary git repository
// they can open in any editor, push anywhere, and inspect with any tool. A pure
// Go implementation is an extra dependency, a compatibility risk, and buys
// nothing here — the operations needed are init, add, commit, log, diff and
// reset, all of which are stable git plumbing.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Commit and Status are the port's types, re-exported so this package reads
// naturally while satisfying the interface the inner layers own.
type Commit = port.Commit

// Status summarises the working tree.
type Status = port.VCSStatus

// Repository is a git working tree.
type Repository struct {
	root string
	git  string
	// timeout bounds any single git invocation. A hung git process would
	// otherwise stall a build indefinitely.
	timeout time.Duration
}

// Available reports whether git is installed.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// Open prepares a repository handle for a workspace, initialising the
// repository if it does not exist yet.
func Open(ctx context.Context, root string) (*Repository, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, domain.Unavailable("git_unavailable",
			"git is not installed; version control of generated projects is disabled")
	}

	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	repo := &Repository{root: absolute, git: git, timeout: 30 * time.Second}

	if _, err := repo.run(ctx, "rev-parse", "--git-dir"); err != nil {
		if err := repo.initialise(ctx); err != nil {
			return nil, err
		}
	}
	return repo, nil
}

// Root returns the working tree path.
func (r *Repository) Root() string { return r.root }

// initialise creates a repository with settings that make generated history
// usable rather than merely present.
func (r *Repository) initialise(ctx context.Context) error {
	if _, err := r.run(ctx, "init", "--initial-branch=main"); err != nil {
		// Older git does not support --initial-branch.
		if _, retry := r.run(ctx, "init"); retry != nil {
			return fmt.Errorf("initialise repository: %w", retry)
		}
	}

	// A local identity avoids failing on machines with no global git config,
	// which is common in containers and CI.
	if _, err := r.run(ctx, "config", "user.name", "Genesis AI Factory"); err != nil {
		return err
	}
	if _, err := r.run(ctx, "config", "user.email", "factory@genesis.local"); err != nil {
		return err
	}
	// Generated projects are text; normalising line endings prevents a Windows
	// checkout from showing every line as modified.
	_, _ = r.run(ctx, "config", "core.autocrlf", "false")
	return nil
}

// run executes a git command in the repository.
func (r *Repository) run(ctx context.Context, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.git, args...)
	cmd.Dir = r.root
	// A deterministic environment keeps output parseable regardless of the
	// user's locale, pager or hook configuration.
	cmd.Env = append(cmd.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"LC_ALL=C",
	)

	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return text, fmt.Errorf("git %s timed out", args[0])
		}
		return text, fmt.Errorf("git %s: %s", args[0], truncate(text, 300))
	}
	return text, nil
}

// Snapshot commits every change in the working tree.
//
// This is the primitive that makes agent work reversible: a snapshot before
// each phase means any subsequent damage is exactly one `git reset` away.
// Returning an empty SHA for a no-op is deliberate — an empty commit per phase
// would bury the real changes in noise.
func (r *Repository) Snapshot(ctx context.Context, message string, author string) (string, error) {
	if _, err := r.run(ctx, "add", "-A"); err != nil {
		return "", err
	}

	// --quiet exits non-zero when there is something to commit, which is how
	// git reports "dirty" without a separate parse.
	if _, err := r.run(ctx, "diff", "--cached", "--quiet"); err == nil {
		return "", nil
	}

	args := []string{"commit", "-m", message}
	if author != "" {
		args = append(args, "--author", fmt.Sprintf("%s <agent@genesis.local>", author))
	}
	if _, err := r.run(ctx, args...); err != nil {
		return "", err
	}
	return r.Head(ctx)
}

// Head returns the current commit SHA, or empty for a repository with no
// commits yet.
func (r *Repository) Head(ctx context.Context) (string, error) {
	sha, err := r.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", nil
	}
	return sha, nil
}

// Status reports the state of the working tree.
func (r *Repository) Status(ctx context.Context) (*Status, error) {
	branch, _ := r.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	head, _ := r.Head(ctx)

	// Porcelain v1 is a stable, machine-readable format; parsing human output
	// would break the moment git changes its wording.
	output, err := r.run(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}

	status := &Status{Branch: branch, Head: head, Clean: output == ""}
	for _, line := range strings.Split(output, "\n") {
		// Porcelain v1 is exactly "XY<space>path". The status code is always
		// two columns wide, so the path starts at offset 3 — slicing from any
		// other offset silently truncates the first character of every
		// single-column status such as " M file".
		if len(line) < 4 {
			continue
		}
		code := line[:2]
		path := strings.TrimSpace(line[2:])
		// A rename is reported as "old -> new"; the new path is what exists.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		path = strings.Trim(path, `"`)
		switch {
		case strings.HasPrefix(code, "??"):
			status.Untracked = append(status.Untracked, path)
		case strings.Contains(code, "D"):
			status.Deleted = append(status.Deleted, path)
		default:
			status.Modified = append(status.Modified, path)
		}
	}
	return status, nil
}

// Log returns recent commits with their change statistics.
func (r *Repository) Log(ctx context.Context, limit int) ([]Commit, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// A record separator that cannot occur in a commit message keeps parsing
	// unambiguous even when subjects contain newlines or pipes.
	const sep = "\x1e"
	const fieldSep = "\x1f"

	output, err := r.run(ctx, "log",
		fmt.Sprintf("--max-count=%d", limit),
		"--format="+sep+"%H"+fieldSep+"%h"+fieldSep+"%s"+fieldSep+"%an"+fieldSep+"%aI",
		"--numstat")
	if err != nil {
		// A repository with no commits is not an error condition.
		return nil, nil
	}

	var commits []Commit
	for _, record := range strings.Split(output, sep) {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}

		lines := strings.Split(record, "\n")
		fields := strings.Split(lines[0], fieldSep)
		if len(fields) < 5 {
			continue
		}

		commit := Commit{
			SHA: fields[0], Short: fields[1], Subject: fields[2], Author: fields[3],
		}
		if when, err := time.Parse(time.RFC3339, fields[4]); err == nil {
			commit.When = when
		}

		for _, line := range lines[1:] {
			parts := strings.Fields(line)
			if len(parts) != 3 {
				continue
			}
			commit.Files = append(commit.Files, parts[2])
			commit.Added += atoiSafe(parts[0])
			commit.Removed += atoiSafe(parts[1])
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

// Diff returns the unified diff of a commit, or of the working tree when the
// reference is empty.
func (r *Repository) Diff(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return r.run(ctx, "diff", "HEAD")
	}
	return r.run(ctx, "show", "--format=", ref)
}

// Branch creates and switches to a branch.
func (r *Repository) Branch(ctx context.Context, name string) error {
	safe := sanitiseRef(name)
	if safe == "" {
		return domain.Invalid("branch_invalid", "the branch name is not usable")
	}
	if _, err := r.run(ctx, "checkout", "-B", safe); err != nil {
		return err
	}
	return nil
}

// Reset discards changes and returns the tree to a commit.
//
// This is destructive by design: it is the rollback a user reaches for when an
// agent has damaged their project, and a half-measure that leaves some changes
// behind would not restore a working state.
func (r *Repository) Reset(ctx context.Context, ref string) error {
	if ref == "" {
		ref = "HEAD"
	}
	if _, err := r.run(ctx, "reset", "--hard", ref); err != nil {
		return err
	}
	// Untracked files survive a hard reset and would leave the tree in a state
	// that never existed.
	_, _ = r.run(ctx, "clean", "-fd")
	return nil
}

// sanitiseRef strips characters git refuses in a reference name.
func sanitiseRef(name string) string {
	var b strings.Builder
	previousDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			previousDash = false
		case r == '/' || r == '-' || r == '_' || r == '.':
			if !previousDash {
				b.WriteRune('-')
				previousDash = true
			}
		case r == ' ':
			if !previousDash {
				b.WriteRune('-')
				previousDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-.")
}

func atoiSafe(s string) int {
	// Binary files report "-" in numstat.
	if s == "-" {
		return 0
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Factory opens git repositories, satisfying port.VersionControlFactory.
type Factory struct{}

// NewFactory constructs the factory.
func NewFactory() *Factory { return &Factory{} }

var _ port.VersionControlFactory = (*Factory)(nil)

// Open prepares version control for a workspace.
func (f *Factory) Open(ctx context.Context, workspace string) (port.VersionControl, error) {
	return Open(ctx, workspace)
}

// Available reports whether git is installed.
func (f *Factory) Available() bool { return Available() }

var _ port.VersionControl = (*Repository)(nil)
