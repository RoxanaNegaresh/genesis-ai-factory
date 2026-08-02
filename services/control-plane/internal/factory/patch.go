package factory

import (
	"fmt"
	"sort"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// The patch engine.
//
// Up to v0.4 agents could only create files. That is sufficient for generating
// a project from nothing and useless for improving one: an agent asked to fix a
// compilation error had no way to change three lines without rewriting the
// whole file, and rewriting a file the user has since edited destroys their
// work.
//
// Two properties make patching safe enough to hand to a model:
//
//   1. Atomicity. A patch touching six files either lands completely or not at
//      all. A half-applied refactor is worse than none, because the project no
//      longer compiles and neither the old nor the new state exists.
//   2. Base verification. Every edit records the hash of the content it was
//      computed against. If the file changed underneath, the hunk is rejected
//      rather than applied to text it was never intended for.

// EditKind distinguishes the operations a patch can perform.
type EditKind string

const (
	EditCreate EditKind = "create"
	EditModify EditKind = "modify"
	EditDelete EditKind = "delete"
)

// Hunk is a contiguous replacement within a file.
//
// Hunks are expressed as "replace this exact text with that text" rather than
// as line numbers. Line numbers shift as earlier hunks apply and are wrong the
// moment anything above them changes; anchoring on content is stable and, more
// importantly, self-verifying — if the anchor is missing, the assumption behind
// the edit was false.
type Hunk struct {
	// Find is the exact text to replace. Must be unique within the file.
	Find string
	// Replace is the new text.
	Replace string
	// Context describes the intent, for review and for the event log.
	Context string
}

// FileEdit is a change to one file.
type FileEdit struct {
	Path string
	Kind EditKind
	// Hunks apply to a modify. Ignored for create and delete.
	Hunks []Hunk
	// Content is the whole file for a create.
	Content string
	// BaseSHA256 is the hash of the content this edit was computed against.
	// Empty skips the check, which is only appropriate for a create.
	BaseSHA256 string
	// Reason explains the change to a human reviewer.
	Reason string
}

// Patch is an atomic set of file edits.
type Patch struct {
	Title  string
	Author string
	Edits  []FileEdit
}

// PatchError explains why a patch could not be applied.
type PatchError struct {
	Path   string
	Reason string
	// Hunk is the index of the failing hunk, or -1 for a file-level problem.
	Hunk int
}

func (e PatchError) Error() string {
	if e.Hunk >= 0 {
		return fmt.Sprintf("%s: hunk %d: %s", e.Path, e.Hunk+1, e.Reason)
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// PatchResult reports what a patch did.
type PatchResult struct {
	Applied  []string
	Created  []string
	Deleted  []string
	Failures []PatchError
	// Preview maps each path to its resulting content, populated on a dry run.
	Preview map[string]string
}

// OK reports whether every edit applied.
func (r PatchResult) OK() bool { return len(r.Failures) == 0 }

// Summary renders a one-line description for the event log.
func (r PatchResult) Summary() string {
	parts := make([]string, 0, 4)
	if n := len(r.Created); n > 0 {
		parts = append(parts, fmt.Sprintf("%d created", n))
	}
	if n := len(r.Applied); n > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", n))
	}
	if n := len(r.Deleted); n > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", n))
	}
	if n := len(r.Failures); n > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", n))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

// FileReader supplies the current content of a file.
type FileReader func(path string) (string, error)

// Compute resolves a patch against current file contents without writing
// anything, returning the resulting content for every touched file.
//
// Separating computation from writing is what makes atomicity achievable: every
// hunk is resolved first, and only if all of them succeed does anything reach
// the disk.
func (p Patch) Compute(read FileReader) PatchResult {
	result := PatchResult{Preview: map[string]string{}}

	// Deterministic ordering keeps failure messages and event logs stable
	// across runs, which matters when diffing two builds of the same prompt.
	edits := make([]FileEdit, len(p.Edits))
	copy(edits, p.Edits)
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].Path < edits[j].Path })

	seen := map[string]bool{}
	for _, edit := range edits {
		if seen[edit.Path] {
			// Two edits to one file cannot both be verified against the same
			// base, and merging them silently would hide the conflict.
			result.Failures = append(result.Failures, PatchError{
				Path: edit.Path, Hunk: -1,
				Reason: "the patch contains more than one edit for this file",
			})
			continue
		}
		seen[edit.Path] = true

		switch edit.Kind {
		case EditCreate:
			if edit.Content == "" {
				result.Failures = append(result.Failures, PatchError{
					Path: edit.Path, Hunk: -1, Reason: "a created file must have content",
				})
				continue
			}
			result.Preview[edit.Path] = edit.Content
			result.Created = append(result.Created, edit.Path)

		case EditDelete:
			if _, err := read(edit.Path); err != nil {
				result.Failures = append(result.Failures, PatchError{
					Path: edit.Path, Hunk: -1, Reason: "cannot delete a file that does not exist",
				})
				continue
			}
			result.Deleted = append(result.Deleted, edit.Path)

		case EditModify:
			current, err := read(edit.Path)
			if err != nil {
				result.Failures = append(result.Failures, PatchError{
					Path: edit.Path, Hunk: -1, Reason: "the file does not exist",
				})
				continue
			}

			// Base verification. Without it, an edit computed against an older
			// version can corrupt a file the user has since changed.
			if edit.BaseSHA256 != "" {
				if actual := HashContent(current); actual != edit.BaseSHA256 {
					result.Failures = append(result.Failures, PatchError{
						Path: edit.Path, Hunk: -1,
						Reason: "the file changed since this edit was computed; re-read it and try again",
					})
					continue
				}
			}

			updated, failures := applyHunks(edit.Path, current, edit.Hunks)
			if len(failures) > 0 {
				result.Failures = append(result.Failures, failures...)
				continue
			}
			if updated == current {
				// A hunk that changes nothing usually means the model produced
				// an identical replacement, which is a defect worth surfacing.
				result.Failures = append(result.Failures, PatchError{
					Path: edit.Path, Hunk: -1, Reason: "the edit produced no change",
				})
				continue
			}
			result.Preview[edit.Path] = updated
			result.Applied = append(result.Applied, edit.Path)

		default:
			result.Failures = append(result.Failures, PatchError{
				Path: edit.Path, Hunk: -1, Reason: "unknown edit kind " + string(edit.Kind),
			})
		}
	}
	return result
}

// applyHunks resolves every hunk against a file's content.
func applyHunks(path, content string, hunks []Hunk) (string, []PatchError) {
	if len(hunks) == 0 {
		return content, []PatchError{{Path: path, Hunk: -1, Reason: "a modify edit needs at least one hunk"}}
	}

	var failures []PatchError
	updated := content

	for i, hunk := range hunks {
		if hunk.Find == "" {
			failures = append(failures, PatchError{path, "the anchor text is empty", i})
			continue
		}

		count := strings.Count(updated, hunk.Find)
		switch count {
		case 0:
			// The most common and most important failure: the model invented
			// text that is not in the file. Reporting it precisely is what lets
			// the healing loop retry with better context instead of guessing.
			failures = append(failures, PatchError{path,
				"the anchor text was not found: " + excerpt(hunk.Find), i})
		case 1:
			updated = strings.Replace(updated, hunk.Find, hunk.Replace, 1)
		default:
			// An ambiguous anchor would change an arbitrary occurrence.
			failures = append(failures, PatchError{path,
				fmt.Sprintf("the anchor text appears %d times and is ambiguous: %s", count, excerpt(hunk.Find)), i})
		}
	}
	return updated, failures
}

// excerpt renders a short, single-line preview of anchor text for an error.
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx] + "…"
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return strconv_Quote(s)
}

// strconv_Quote avoids importing strconv for one call while still escaping
// control characters that would corrupt a log line.
func strconv_Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// HashContent returns the content hash used for base verification.
//
// Defined in the domain so the editor and the patch engine cannot disagree
// about content identity.
func HashContent(content string) string { return domain.HashContent(content) }

// UnifiedDiff renders a change for human review.
//
// This is a readable diff, not a machine-applicable one: the patch format is
// anchor-based, so a diff exists purely so a person can see what an agent did
// before accepting it.
func UnifiedDiff(path, before, after string) string {
	if before == after {
		return ""
	}

	beforeLines := splitLines(before)
	afterLines := splitLines(after)

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)

	// A line-level longest-common-subsequence diff. The generated files are
	// small enough that the quadratic table is irrelevant, and the output is
	// far more readable than a naive prefix/suffix trim.
	ops := diffLines(beforeLines, afterLines)

	const contextLines = 3
	i := 0
	for i < len(ops) {
		if ops[i].kind == opEqual {
			i++
			continue
		}

		// Find the extent of this change, merging nearby edits into one hunk.
		start := i
		for start > 0 && ops[start-1].kind == opEqual && start > i-contextLines {
			start--
		}
		end := i
		for end < len(ops) {
			if ops[end].kind != opEqual {
				end++
				continue
			}
			run := 0
			for end+run < len(ops) && ops[end+run].kind == opEqual {
				run++
			}
			if run > contextLines*2 {
				break
			}
			end += run
		}
		tail := end
		for tail < len(ops) && ops[tail].kind == opEqual && tail < end+contextLines {
			tail++
		}

		oldStart, newStart := 1, 1
		for k := 0; k < start; k++ {
			if ops[k].kind != opInsert {
				oldStart++
			}
			if ops[k].kind != opDelete {
				newStart++
			}
		}
		oldCount, newCount := 0, 0
		for k := start; k < tail; k++ {
			if ops[k].kind != opInsert {
				oldCount++
			}
			if ops[k].kind != opDelete {
				newCount++
			}
		}

		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for k := start; k < tail; k++ {
			switch ops[k].kind {
			case opEqual:
				sb.WriteString(" " + ops[k].text + "\n")
			case opDelete:
				sb.WriteString("-" + ops[k].text + "\n")
			case opInsert:
				sb.WriteString("+" + ops[k].text + "\n")
			}
		}
		i = tail
	}
	return sb.String()
}

type diffOpKind int

const (
	opEqual diffOpKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind diffOpKind
	text string
}

// diffLines computes a line diff via longest common subsequence.
func diffLines(before, after []string) []diffOp {
	// Guard against pathological inputs: a 5000×5000 table is 25M cells, which
	// is slow and pointless for a generated source file.
	const maxLines = 3000
	if len(before) > maxLines || len(after) > maxLines {
		ops := make([]diffOp, 0, len(before)+len(after))
		for _, line := range before {
			ops = append(ops, diffOp{opDelete, line})
		}
		for _, line := range after {
			ops = append(ops, diffOp{opInsert, line})
		}
		return ops
	}

	rows, cols := len(before)+1, len(after)+1
	table := make([]int, rows*cols)
	at := func(r, c int) int { return r*cols + c }

	for r := len(before) - 1; r >= 0; r-- {
		for c := len(after) - 1; c >= 0; c-- {
			if before[r] == after[c] {
				table[at(r, c)] = table[at(r+1, c+1)] + 1
				continue
			}
			if table[at(r+1, c)] >= table[at(r, c+1)] {
				table[at(r, c)] = table[at(r+1, c)]
			} else {
				table[at(r, c)] = table[at(r, c+1)]
			}
		}
	}

	var ops []diffOp
	r, c := 0, 0
	for r < len(before) && c < len(after) {
		switch {
		case before[r] == after[c]:
			ops = append(ops, diffOp{opEqual, before[r]})
			r++
			c++
		case table[at(r+1, c)] >= table[at(r, c+1)]:
			ops = append(ops, diffOp{opDelete, before[r]})
			r++
		default:
			ops = append(ops, diffOp{opInsert, after[c]})
			c++
		}
	}
	for ; r < len(before); r++ {
		ops = append(ops, diffOp{opDelete, before[r]})
	}
	for ; c < len(after); c++ {
		ops = append(ops, diffOp{opInsert, after[c]})
	}
	return ops
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// A trailing newline produces a final empty element that is not a line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Stat counts the lines added and removed by a change.
func Stat(before, after string) (added, removed int) {
	for _, op := range diffLines(splitLines(before), splitLines(after)) {
		switch op.kind {
		case opInsert:
			added++
		case opDelete:
			removed++
		}
	}
	return added, removed
}
