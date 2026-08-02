package vcs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genesis-ai-factory/control-plane/internal/infra/vcs"
)

func newRepo(t *testing.T) (*vcs.Repository, string) {
	t.Helper()
	if !vcs.Available() {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repo, err := vcs.Open(context.Background(), root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return repo, root
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestOpenInitialisesRepository(t *testing.T) {
	repo, root := newRepo(t)

	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Fatalf("no git directory was created: %v", err)
	}

	status, err := repo.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Clean {
		t.Errorf("a fresh repository should be clean: %+v", status)
	}

	// Opening an existing repository must not reinitialise or fail.
	if _, err := vcs.Open(context.Background(), root); err != nil {
		t.Fatalf("reopening an existing repository failed: %v", err)
	}
}

func TestSnapshotCommitsChanges(t *testing.T) {
	repo, root := newRepo(t)
	ctx := context.Background()

	write(t, root, "main.go", "package main\n\nfunc main() {}\n")

	sha, err := repo.Snapshot(ctx, "feat: initial scaffold", "Forge")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if sha == "" {
		t.Fatal("a snapshot with changes returned no commit")
	}

	status, _ := repo.Status(ctx)
	if !status.Clean {
		t.Errorf("the tree is dirty after a snapshot: %+v", status)
	}
	if status.Head != sha {
		t.Errorf("HEAD %q does not match the returned SHA %q", status.Head, sha)
	}
}

// An empty commit per phase would bury real changes in noise.
func TestSnapshotIsNoOpWhenClean(t *testing.T) {
	repo, root := newRepo(t)
	ctx := context.Background()

	write(t, root, "a.go", "package a\n")
	if _, err := repo.Snapshot(ctx, "first", "Forge"); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}

	sha, err := repo.Snapshot(ctx, "second", "Forge")
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if sha != "" {
		t.Fatal("a snapshot with no changes created a commit")
	}

	commits, _ := repo.Log(ctx, 10)
	if len(commits) != 1 {
		t.Fatalf("expected exactly 1 commit, got %d", len(commits))
	}
}

func TestStatusReportsWorkingTree(t *testing.T) {
	repo, root := newRepo(t)
	ctx := context.Background()

	write(t, root, "tracked.go", "package a\n")
	_, _ = repo.Snapshot(ctx, "seed", "Forge")

	write(t, root, "tracked.go", "package a\n\nvar X = 1\n")
	write(t, root, "new.go", "package b\n")

	status, err := repo.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Clean {
		t.Fatal("a modified tree reported clean")
	}
	if !contains(status.Modified, "tracked.go") {
		t.Errorf("the modified file was not reported: %+v", status)
	}
	if !contains(status.Untracked, "new.go") {
		t.Errorf("the untracked file was not reported: %+v", status)
	}
}

func TestLogReportsHistoryWithStats(t *testing.T) {
	repo, root := newRepo(t)
	ctx := context.Background()

	write(t, root, "a.go", "package a\n")
	_, _ = repo.Snapshot(ctx, "feat: add a", "Forge")

	write(t, root, "b.go", "package b\nvar Y = 2\nvar Z = 3\n")
	_, _ = repo.Snapshot(ctx, "feat: add b", "Prism")

	commits, err := repo.Log(ctx, 10)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}

	// Newest first.
	newest := commits[0]
	if newest.Subject != "feat: add b" {
		t.Errorf("wrong ordering or subject: %q", newest.Subject)
	}
	if newest.Added != 3 {
		t.Errorf("expected 3 added lines, got %d", newest.Added)
	}
	if !contains(newest.Files, "b.go") {
		t.Errorf("the changed file was not recorded: %v", newest.Files)
	}
	if newest.Author != "Prism" {
		t.Errorf("the agent author was not recorded: %q", newest.Author)
	}
	if newest.When.IsZero() {
		t.Error("the commit timestamp was not parsed")
	}
	if newest.Short == "" || len(newest.Short) >= len(newest.SHA) {
		t.Errorf("the short SHA is wrong: %q", newest.Short)
	}
}

func TestLogOnEmptyRepositoryIsNotAnError(t *testing.T) {
	repo, _ := newRepo(t)
	commits, err := repo.Log(context.Background(), 10)
	if err != nil {
		t.Fatalf("log on an empty repository failed: %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("an empty repository reported %d commits", len(commits))
	}
}

// Rollback is the remedy when an agent damages a project; it must restore the
// tree completely, including removing files the agent created.
func TestResetRestoresPreviousState(t *testing.T) {
	repo, root := newRepo(t)
	ctx := context.Background()

	write(t, root, "good.go", "package good\n")
	sha, _ := repo.Snapshot(ctx, "known good", "Forge")

	// An agent then damages the project.
	write(t, root, "good.go", "package good\nthis is not valid go\n")
	write(t, root, "junk.go", "garbage\n")

	if err := repo.Reset(ctx, sha); err != nil {
		t.Fatalf("reset: %v", err)
	}

	restored, err := os.ReadFile(filepath.Join(root, "good.go"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(restored) != "package good\n" {
		t.Fatalf("the file was not restored: %q", restored)
	}
	// Untracked junk survives a plain hard reset, which would leave a state
	// that never existed.
	if _, err := os.Stat(filepath.Join(root, "junk.go")); err == nil {
		t.Fatal("an untracked file created by the agent survived the rollback")
	}

	status, _ := repo.Status(ctx)
	if !status.Clean {
		t.Errorf("the tree is not clean after rollback: %+v", status)
	}
}

func TestBranchCreatesAndSwitches(t *testing.T) {
	repo, root := newRepo(t)
	ctx := context.Background()

	write(t, root, "a.go", "package a\n")
	_, _ = repo.Snapshot(ctx, "seed", "Forge")

	if err := repo.Branch(ctx, "feature/Add Authentication"); err != nil {
		t.Fatalf("branch: %v", err)
	}

	status, _ := repo.Status(ctx)
	// The name must be sanitised into something git accepts.
	if strings.Contains(status.Branch, " ") || strings.Contains(status.Branch, "A") {
		t.Errorf("the branch name was not sanitised: %q", status.Branch)
	}
	if !strings.Contains(status.Branch, "feature") {
		t.Errorf("the branch name lost its meaning: %q", status.Branch)
	}
}

func TestBranchRejectsUnusableNames(t *testing.T) {
	repo, root := newRepo(t)
	ctx := context.Background()
	write(t, root, "a.go", "package a\n")
	_, _ = repo.Snapshot(ctx, "seed", "Forge")

	if err := repo.Branch(ctx, "!!!"); err == nil {
		t.Fatal("a branch name with no usable characters was accepted")
	}
}

func TestDiffShowsChanges(t *testing.T) {
	repo, root := newRepo(t)
	ctx := context.Background()

	write(t, root, "a.go", "package a\nvar X = 1\n")
	_, _ = repo.Snapshot(ctx, "seed", "Forge")

	write(t, root, "a.go", "package a\nvar X = 2\n")

	diff, err := repo.Diff(ctx, "")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(diff, "-var X = 1") || !strings.Contains(diff, "+var X = 2") {
		t.Fatalf("the working-tree diff is wrong:\n%s", diff)
	}

	sha, _ := repo.Snapshot(ctx, "change", "Forge")
	commitDiff, err := repo.Diff(ctx, sha)
	if err != nil {
		t.Fatalf("commit diff: %v", err)
	}
	if !strings.Contains(commitDiff, "+var X = 2") {
		t.Fatalf("the commit diff is wrong:\n%s", commitDiff)
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
