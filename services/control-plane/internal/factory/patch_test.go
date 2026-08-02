package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
)

// memoryFiles is an in-memory filesystem for patch tests.
func memoryFiles(files map[string]string) factory.FileReader {
	return func(path string) (string, error) {
		content, ok := files[path]
		if !ok {
			return "", errors.New("not found")
		}
		return content, nil
	}
}

const sampleGo = `package domain

import "strings"

// Deal is a revenue opportunity.
type Deal struct {
	ID    string
	Title string
	Value string
}

func (d *Deal) Validate() error {
	if strings.TrimSpace(d.Title) == "" {
		return errors.New("title is required")
	}
	return nil
}
`

func TestPatchAppliesHunks(t *testing.T) {
	read := memoryFiles(map[string]string{"api/deal.go": sampleGo})

	patch := factory.Patch{
		Title: "add value validation",
		Edits: []factory.FileEdit{{
			Path: "api/deal.go", Kind: factory.EditModify,
			Hunks: []factory.Hunk{{
				Find:    "\treturn nil\n}",
				Replace: "\tif d.Value == \"\" {\n\t\treturn errors.New(\"value is required\")\n\t}\n\treturn nil\n}",
				Context: "reject deals with no value",
			}},
		}},
	}

	result := patch.Compute(read)
	if !result.OK() {
		t.Fatalf("patch failed: %v", result.Failures)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "api/deal.go" {
		t.Fatalf("unexpected applied set: %v", result.Applied)
	}
	updated := result.Preview["api/deal.go"]
	if !strings.Contains(updated, "value is required") {
		t.Fatalf("the replacement did not land:\n%s", updated)
	}
	// Everything outside the hunk must be untouched.
	if !strings.Contains(updated, "title is required") {
		t.Fatal("the patch damaged content outside its hunk")
	}
}

// The most common model failure: inventing anchor text that is not in the file.
func TestPatchRejectsMissingAnchor(t *testing.T) {
	read := memoryFiles(map[string]string{"api/deal.go": sampleGo})

	patch := factory.Patch{Edits: []factory.FileEdit{{
		Path: "api/deal.go", Kind: factory.EditModify,
		Hunks: []factory.Hunk{{Find: "func (d *Deal) Save() error {", Replace: "x"}},
	}}}

	result := patch.Compute(read)
	if result.OK() {
		t.Fatal("a hunk with a nonexistent anchor was applied")
	}
	failure := result.Failures[0]
	if !strings.Contains(failure.Reason, "not found") {
		t.Fatalf("unhelpful failure reason: %s", failure.Reason)
	}
	// The message must quote the anchor so a retry can be informed.
	if !strings.Contains(failure.Reason, "Save") {
		t.Fatalf("the failure does not quote the missing anchor: %s", failure.Reason)
	}
	if failure.Hunk != 0 {
		t.Errorf("the failing hunk index is wrong: %d", failure.Hunk)
	}
}

// An ambiguous anchor would replace an arbitrary occurrence, which is silently
// wrong rather than loudly wrong.
func TestPatchRejectsAmbiguousAnchor(t *testing.T) {
	read := memoryFiles(map[string]string{"f.go": "return nil\nreturn nil\n"})

	patch := factory.Patch{Edits: []factory.FileEdit{{
		Path: "f.go", Kind: factory.EditModify,
		Hunks: []factory.Hunk{{Find: "return nil", Replace: "return err"}},
	}}}

	result := patch.Compute(read)
	if result.OK() {
		t.Fatal("an ambiguous anchor was applied")
	}
	if !strings.Contains(result.Failures[0].Reason, "ambiguous") {
		t.Fatalf("the failure does not explain the ambiguity: %s", result.Failures[0].Reason)
	}
	if !strings.Contains(result.Failures[0].Reason, "2 times") {
		t.Fatalf("the failure does not report the occurrence count: %s", result.Failures[0].Reason)
	}
}

// Base verification is what protects a user's manual edits from being clobbered
// by an agent working from a stale read.
func TestPatchRejectsStaleBase(t *testing.T) {
	read := memoryFiles(map[string]string{"f.go": "current content\n"})

	patch := factory.Patch{Edits: []factory.FileEdit{{
		Path: "f.go", Kind: factory.EditModify,
		BaseSHA256: factory.HashContent("what the agent read earlier\n"),
		Hunks:      []factory.Hunk{{Find: "current", Replace: "new"}},
	}}}

	result := patch.Compute(read)
	if result.OK() {
		t.Fatal("an edit computed against stale content was applied")
	}
	if !strings.Contains(result.Failures[0].Reason, "changed since") {
		t.Fatalf("the failure does not explain the conflict: %s", result.Failures[0].Reason)
	}
}

func TestPatchAcceptsMatchingBase(t *testing.T) {
	content := "current content\n"
	read := memoryFiles(map[string]string{"f.go": content})

	patch := factory.Patch{Edits: []factory.FileEdit{{
		Path: "f.go", Kind: factory.EditModify,
		BaseSHA256: factory.HashContent(content),
		Hunks:      []factory.Hunk{{Find: "current", Replace: "updated"}},
	}}}

	result := patch.Compute(read)
	if !result.OK() {
		t.Fatalf("a correctly based edit was rejected: %v", result.Failures)
	}
	if result.Preview["f.go"] != "updated content\n" {
		t.Fatalf("wrong result: %q", result.Preview["f.go"])
	}
}

// Atomicity: one bad hunk must prevent the entire patch, or a refactor can land
// half-applied and leave the project in a state that never existed.
func TestPatchIsAtomicAcrossFiles(t *testing.T) {
	read := memoryFiles(map[string]string{
		"a.go": "package a\nvar X = 1\n",
		"b.go": "package b\nvar Y = 2\n",
	})

	patch := factory.Patch{Edits: []factory.FileEdit{
		{Path: "a.go", Kind: factory.EditModify,
			Hunks: []factory.Hunk{{Find: "var X = 1", Replace: "var X = 10"}}},
		{Path: "b.go", Kind: factory.EditModify,
			Hunks: []factory.Hunk{{Find: "var Z = 99", Replace: "var Z = 0"}}},
	}}

	result := patch.Compute(read)
	if result.OK() {
		t.Fatal("a patch with a failing edit reported success")
	}
	// The good edit is computed but the caller must not write anything, which
	// is why Compute never touches disk. Verify the failure is attributed.
	if len(result.Failures) != 1 || result.Failures[0].Path != "b.go" {
		t.Fatalf("the failure is misattributed: %v", result.Failures)
	}
}

func TestPatchCreateAndDelete(t *testing.T) {
	read := memoryFiles(map[string]string{"old.go": "package old\n"})

	patch := factory.Patch{Edits: []factory.FileEdit{
		{Path: "new.go", Kind: factory.EditCreate, Content: "package new\n"},
		{Path: "old.go", Kind: factory.EditDelete},
	}}

	result := patch.Compute(read)
	if !result.OK() {
		t.Fatalf("create/delete failed: %v", result.Failures)
	}
	if len(result.Created) != 1 || result.Created[0] != "new.go" {
		t.Errorf("create not recorded: %v", result.Created)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != "old.go" {
		t.Errorf("delete not recorded: %v", result.Deleted)
	}
	if result.Preview["new.go"] != "package new\n" {
		t.Error("created content is wrong")
	}
}

func TestPatchRejectsInvalidEdits(t *testing.T) {
	read := memoryFiles(map[string]string{"exists.go": "content\n"})

	cases := map[string]factory.Patch{
		"empty create": {Edits: []factory.FileEdit{
			{Path: "x.go", Kind: factory.EditCreate, Content: ""}}},
		"delete missing": {Edits: []factory.FileEdit{
			{Path: "nope.go", Kind: factory.EditDelete}}},
		"modify missing": {Edits: []factory.FileEdit{
			{Path: "nope.go", Kind: factory.EditModify,
				Hunks: []factory.Hunk{{Find: "a", Replace: "b"}}}}},
		"no hunks": {Edits: []factory.FileEdit{
			{Path: "exists.go", Kind: factory.EditModify}}},
		"empty anchor": {Edits: []factory.FileEdit{
			{Path: "exists.go", Kind: factory.EditModify,
				Hunks: []factory.Hunk{{Find: "", Replace: "b"}}}}},
		"duplicate file": {Edits: []factory.FileEdit{
			{Path: "exists.go", Kind: factory.EditModify,
				Hunks: []factory.Hunk{{Find: "content", Replace: "a"}}},
			{Path: "exists.go", Kind: factory.EditModify,
				Hunks: []factory.Hunk{{Find: "content", Replace: "b"}}}}},
		"no-op": {Edits: []factory.FileEdit{
			{Path: "exists.go", Kind: factory.EditModify,
				Hunks: []factory.Hunk{{Find: "content", Replace: "content"}}}}},
		"unknown kind": {Edits: []factory.FileEdit{
			{Path: "exists.go", Kind: "teleport"}}},
	}

	for name, patch := range cases {
		if result := patch.Compute(read); result.OK() {
			t.Errorf("%s: an invalid edit was accepted", name)
		}
	}
}

func TestPatchAppliesMultipleHunksInOneFile(t *testing.T) {
	read := memoryFiles(map[string]string{"f.go": "alpha\nbeta\ngamma\n"})

	patch := factory.Patch{Edits: []factory.FileEdit{{
		Path: "f.go", Kind: factory.EditModify,
		Hunks: []factory.Hunk{
			{Find: "alpha", Replace: "ALPHA"},
			{Find: "gamma", Replace: "GAMMA"},
		},
	}}}

	result := patch.Compute(read)
	if !result.OK() {
		t.Fatalf("multi-hunk patch failed: %v", result.Failures)
	}
	if result.Preview["f.go"] != "ALPHA\nbeta\nGAMMA\n" {
		t.Fatalf("wrong result: %q", result.Preview["f.go"])
	}
}

func TestPatchSummary(t *testing.T) {
	result := factory.PatchResult{
		Applied: []string{"a", "b"}, Created: []string{"c"},
		Deleted: []string{"d"}, Failures: []factory.PatchError{{Path: "e"}},
	}
	summary := result.Summary()
	for _, want := range []string{"1 created", "2 modified", "1 deleted", "1 failed"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q is missing %q", summary, want)
		}
	}
	if empty := (factory.PatchResult{}).Summary(); empty != "no changes" {
		t.Errorf("an empty result should say so, got %q", empty)
	}
}

// --- diff rendering -------------------------------------------------------

func TestUnifiedDiffRendersChanges(t *testing.T) {
	before := "line one\nline two\nline three\n"
	after := "line one\nline TWO\nline three\n"

	diff := factory.UnifiedDiff("f.txt", before, after)
	for _, want := range []string{"--- a/f.txt", "+++ b/f.txt", "@@", "-line two", "+line TWO", " line one"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff is missing %q:\n%s", want, diff)
		}
	}
}

func TestUnifiedDiffIsEmptyForNoChange(t *testing.T) {
	if diff := factory.UnifiedDiff("f.txt", "same\n", "same\n"); diff != "" {
		t.Fatalf("an unchanged file produced a diff:\n%s", diff)
	}
}

func TestUnifiedDiffHandlesInsertAndDelete(t *testing.T) {
	diff := factory.UnifiedDiff("f.txt", "a\nb\nc\n", "a\nc\n")
	if !strings.Contains(diff, "-b") {
		t.Errorf("a deletion was not shown:\n%s", diff)
	}

	diff = factory.UnifiedDiff("f.txt", "a\nc\n", "a\nb\nc\n")
	if !strings.Contains(diff, "+b") {
		t.Errorf("an insertion was not shown:\n%s", diff)
	}

	// Whole-file creation and removal must not panic or produce nonsense.
	if d := factory.UnifiedDiff("f.txt", "", "new\n"); !strings.Contains(d, "+new") {
		t.Errorf("creation diff is wrong:\n%s", d)
	}
	if d := factory.UnifiedDiff("f.txt", "old\n", ""); !strings.Contains(d, "-old") {
		t.Errorf("removal diff is wrong:\n%s", d)
	}
}

func TestStatCountsLines(t *testing.T) {
	added, removed := factory.Stat("a\nb\nc\n", "a\nX\nY\nc\n")
	if added != 2 || removed != 1 {
		t.Fatalf("expected +2/-1, got +%d/-%d", added, removed)
	}

	if a, r := factory.Stat("same\n", "same\n"); a != 0 || r != 0 {
		t.Fatalf("an unchanged file reported +%d/-%d", a, r)
	}
}

func TestDiffHandlesLargeFilesWithoutHanging(t *testing.T) {
	// The LCS table is quadratic; the guard must keep a pathological input from
	// making the server unresponsive.
	big := strings.Repeat("line\n", 5000)
	other := strings.Repeat("other\n", 5000)

	done := make(chan string, 1)
	go func() { done <- factory.UnifiedDiff("big.txt", big, other) }()

	select {
	case diff := <-done:
		if diff == "" {
			t.Fatal("a large change produced no diff")
		}
	case <-timeoutAfterSeconds(10):
		t.Fatal("diffing a large file did not complete in time")
	}
}

func timeoutAfterSeconds(n int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(time.Duration(n) * time.Second)
		close(ch)
	}()
	return ch
}

// --- workspace integration ------------------------------------------------

func TestApplyPatchWritesAtomically(t *testing.T) {
	root := t.TempDir()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleBackend, nil, nil)
	ctx := context.Background()

	if err := tb.WriteFile(ctx, "a.go", "package a\nvar X = 1\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := tb.WriteFile(ctx, "b.go", "package b\nvar Y = 2\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	patch := factory.Patch{Title: "bump values", Edits: []factory.FileEdit{
		{Path: "a.go", Kind: factory.EditModify,
			Hunks: []factory.Hunk{{Find: "var X = 1", Replace: "var X = 100"}}},
		{Path: "b.go", Kind: factory.EditModify,
			Hunks: []factory.Hunk{{Find: "var Y = 2", Replace: "var Y = 200"}}},
		{Path: "c.go", Kind: factory.EditCreate, Content: "package c\n"},
	}}

	result, err := tb.ApplyPatch(ctx, patch)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !result.OK() {
		t.Fatalf("patch failed: %v", result.Failures)
	}

	for path, want := range map[string]string{
		"a.go": "var X = 100", "b.go": "var Y = 200", "c.go": "package c",
	} {
		got, err := tb.ReadFile(ctx, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s does not contain %q: %q", path, want, got)
		}
	}
}

// A failing patch must leave the workspace exactly as it was.
func TestApplyPatchLeavesWorkspaceUntouchedOnFailure(t *testing.T) {
	root := t.TempDir()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleBackend, nil, nil)
	ctx := context.Background()

	original := "package a\nvar X = 1\n"
	if err := tb.WriteFile(ctx, "a.go", original); err != nil {
		t.Fatalf("seed: %v", err)
	}

	patch := factory.Patch{Edits: []factory.FileEdit{
		{Path: "a.go", Kind: factory.EditModify,
			Hunks: []factory.Hunk{{Find: "var X = 1", Replace: "var X = 100"}}},
		{Path: "missing.go", Kind: factory.EditModify,
			Hunks: []factory.Hunk{{Find: "anything", Replace: "x"}}},
	}}

	result, err := tb.ApplyPatch(ctx, patch)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.OK() {
		t.Fatal("a patch referencing a missing file reported success")
	}

	after, _ := tb.ReadFile(ctx, "a.go")
	if after != original {
		t.Fatalf("a failed patch modified the workspace:\n%s", after)
	}
}

func TestApplyPatchRespectsWorkspaceConfinement(t *testing.T) {
	root := t.TempDir()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleBackend, nil, nil)
	ctx := context.Background()

	// A patch must not be able to write outside the workspace, whatever path
	// the model supplies.
	patch := factory.Patch{Edits: []factory.FileEdit{
		{Path: "../escaped.go", Kind: factory.EditCreate, Content: "package evil\n"},
	}}

	result, _ := tb.ApplyPatch(ctx, patch)
	if result.OK() {
		t.Fatal("a patch escaped the workspace")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.go")); err == nil {
		t.Fatal("a file was written outside the workspace")
	}
}

func TestApplyPatchDeletesFiles(t *testing.T) {
	root := t.TempDir()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleBackend, nil, nil)
	ctx := context.Background()

	_ = tb.WriteFile(ctx, "doomed.go", "package doomed\n")

	result, err := tb.ApplyPatch(ctx, factory.Patch{Edits: []factory.FileEdit{
		{Path: "doomed.go", Kind: factory.EditDelete},
	}})
	if err != nil || !result.OK() {
		t.Fatalf("delete patch failed: %v %v", err, result.Failures)
	}
	if _, err := tb.ReadFile(ctx, "doomed.go"); err == nil {
		t.Fatal("the file still exists after a delete patch")
	}
}

func TestFileHashSupportsBaseVerification(t *testing.T) {
	root := t.TempDir()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleBackend, nil, nil)
	ctx := context.Background()

	content := "package a\nvar X = 1\n"
	_ = tb.WriteFile(ctx, "a.go", content)

	hash, err := tb.FileHash(ctx, "a.go")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash != factory.HashContent(content) {
		t.Fatal("the workspace hash does not match the content hash")
	}

	// An edit based on that hash applies; after an external change it does not.
	patch := factory.Patch{Edits: []factory.FileEdit{{
		Path: "a.go", Kind: factory.EditModify, BaseSHA256: hash,
		Hunks: []factory.Hunk{{Find: "var X = 1", Replace: "var X = 2"}},
	}}}

	_ = tb.WriteFile(ctx, "a.go", "package a\nvar X = 999\n")
	result, _ := tb.ApplyPatch(ctx, patch)
	if result.OK() {
		t.Fatal("an edit was applied over an external modification")
	}
}
