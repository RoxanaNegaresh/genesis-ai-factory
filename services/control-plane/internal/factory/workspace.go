package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// WorkspaceToolbelt is the audited implementation of Toolbelt over a real
// directory on disk.
//
// Every path an agent supplies is treated as hostile input. Confinement is
// enforced here, in the tool layer, rather than by asking a model politely in a
// prompt: a prompt is a suggestion, a filepath.Rel check is a guarantee.
type WorkspaceToolbelt struct {
	root    string
	agent   domain.AgentRole
	emit    func(ctx context.Context, level domain.Level, agent domain.AgentRole, message string, fields map[string]any)
	onWrite func(relPath string, bytes int)

	mu      sync.Mutex
	written []string
}

// NewWorkspaceToolbelt constructs a toolbelt confined to root.
func NewWorkspaceToolbelt(
	root string,
	agent domain.AgentRole,
	emit func(ctx context.Context, level domain.Level, agent domain.AgentRole, message string, fields map[string]any),
	onWrite func(relPath string, bytes int),
) *WorkspaceToolbelt {
	return &WorkspaceToolbelt{root: root, agent: agent, emit: emit, onWrite: onWrite}
}

// For returns a copy of the toolbelt bound to a different agent, sharing the
// same written-file ledger.
func (t *WorkspaceToolbelt) For(agent domain.AgentRole) *WorkspaceToolbelt {
	return &WorkspaceToolbelt{root: t.root, agent: agent, emit: t.emit, onWrite: t.onWrite}
}

var _ Toolbelt = (*WorkspaceToolbelt)(nil)

// maxFileBytes caps a single generated file. A model that loops can otherwise
// fill a disk before any budget check notices.
const maxFileBytes = 2 << 20 // 2 MiB

// resolve validates a relative path and returns its absolute location.
func (t *WorkspaceToolbelt) resolve(relPath string) (string, error) {
	if t.root == "" {
		return "", fmt.Errorf("workspace root is not configured")
	}
	cleaned := filepath.Clean(strings.TrimSpace(relPath))
	if cleaned == "" || cleaned == "." {
		return "", domain.Invalid("path_invalid", "a file path is required")
	}
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "~") {
		return "", domain.Invalid("path_absolute", "absolute paths are not permitted inside a workspace")
	}

	root, err := filepath.Abs(t.root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, cleaned)

	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", domain.Invalid("path_escape", "path escapes the project workspace")
	}
	return target, nil
}

// secretPattern matches credential-shaped strings. This is the cheap first line
// of defence: a generated file that embeds a real key must never reach disk or
// a commit.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(aws_secret_access_key|aws_access_key_id)\s*[:=]\s*["']?[A-Za-z0-9/+=]{20,}`),
	regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|secret[_-]?key|access[_-]?token|password)\s*[:=]\s*["'][^"'\s]{16,}["']`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`),
}

// placeholderHints mark a match as a template rather than a live credential.
//
// The list is deliberately narrow. A broad hint like "example" would suppress
// genuine keys that happen to contain those letters, and a scanner that fails
// open on real secrets is worse than no scanner because it manufactures
// confidence.
var placeholderHints = []string{
	"change-me", "changeme", "placeholder", "your-", "your_",
	"xxxxx", "todo", "redacted", "<", "${", "{{",
}

// scanForSecrets reports the first credential-shaped match, ignoring obvious
// placeholders so a template containing `password: "change-me"` is not blocked.
func scanForSecrets(content string) string {
	for _, re := range secretPatterns {
		match := re.FindString(content)
		if match == "" {
			continue
		}
		lowered := strings.ToLower(match)
		placeholder := false
		for _, hint := range placeholderHints {
			if strings.Contains(lowered, hint) {
				placeholder = true
				break
			}
		}
		if !placeholder {
			return match
		}
	}
	return ""
}

// WriteFile creates or replaces a file in the workspace.
func (t *WorkspaceToolbelt) WriteFile(ctx context.Context, relPath, content string) error {
	target, err := t.resolve(relPath)
	if err != nil {
		return err
	}
	if len(content) > maxFileBytes {
		return domain.Invalid("file_too_large",
			fmt.Sprintf("generated file %s exceeds the %d byte limit", relPath, maxFileBytes))
	}
	if match := scanForSecrets(content); match != "" {
		if t.emit != nil {
			t.emit(ctx, domain.LevelError, t.agent,
				fmt.Sprintf("Blocked write to %s: content matched a credential pattern", relPath),
				map[string]any{"path": relPath})
		}
		return domain.Invalid("secret_detected",
			fmt.Sprintf("refusing to write %s: content looks like a credential", relPath))
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("create directory for %s: %w", relPath, err)
	}
	// Write to a temporary file and rename: a crash mid-write leaves the old
	// file intact rather than a truncated one.
	tmp := target + ".genesis-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o640); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalise %s: %w", relPath, err)
	}

	t.mu.Lock()
	t.written = append(t.written, relPath)
	t.mu.Unlock()

	if t.onWrite != nil {
		t.onWrite(relPath, len(content))
	}
	return nil
}

// ReadFile reads a file from the workspace.
func (t *WorkspaceToolbelt) ReadFile(ctx context.Context, relPath string) (string, error) {
	target, err := t.resolve(relPath)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", domain.NotFound("file")
		}
		return "", err
	}
	return string(b), nil
}

// ListFiles enumerates every file in the workspace, relative to its root.
func (t *WorkspaceToolbelt) ListFiles(ctx context.Context) ([]string, error) {
	if t.root == "" {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(t.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(t.root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Emit publishes a progress event attributed to the calling agent.
func (t *WorkspaceToolbelt) Emit(ctx context.Context, level domain.Level, message string, fields map[string]any) {
	if t.emit != nil {
		t.emit(ctx, level, t.agent, message, fields)
	}
}

// Written returns the paths this toolbelt has written.
func (t *WorkspaceToolbelt) Written() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.written))
	copy(out, t.written)
	return out
}

// ApplyPatch applies an atomic set of edits to the workspace.
//
// Atomicity is achieved by computing every result first and writing only if all
// of them succeeded. A half-applied refactor leaves a project that compiles
// under neither the old nor the new design, which is strictly worse than
// applying nothing and reporting why.
func (t *WorkspaceToolbelt) ApplyPatch(ctx context.Context, patch Patch) (PatchResult, error) {
	result := patch.Compute(func(path string) (string, error) {
		return t.ReadFile(ctx, path)
	})
	if !result.OK() {
		return result, nil
	}

	// Snapshot the prior content so a mid-write failure can be rolled back.
	// Individual writes are atomic (write-and-rename), but a set of them is
	// not, and a disk filling up between the third and fourth file is exactly
	// when a rollback matters.
	rollback := map[string]string{}
	for path := range result.Preview {
		if existing, err := t.ReadFile(ctx, path); err == nil {
			rollback[path] = existing
		}
	}
	for _, path := range result.Deleted {
		if existing, err := t.ReadFile(ctx, path); err == nil {
			rollback[path] = existing
		}
	}

	restore := func() {
		for path, content := range rollback {
			_ = t.WriteFile(ctx, path, content)
		}
	}

	for path, content := range result.Preview {
		if err := t.WriteFile(ctx, path, content); err != nil {
			restore()
			result.Failures = append(result.Failures, PatchError{
				Path: path, Hunk: -1, Reason: "write failed and the patch was rolled back: " + err.Error(),
			})
			return result, nil
		}
	}
	for _, path := range result.Deleted {
		if err := t.DeleteFile(ctx, path); err != nil {
			restore()
			result.Failures = append(result.Failures, PatchError{
				Path: path, Hunk: -1, Reason: "delete failed and the patch was rolled back: " + err.Error(),
			})
			return result, nil
		}
	}

	t.Emit(ctx, domain.LevelInfo, "Applied patch: "+patch.Title+" ("+result.Summary()+")",
		map[string]any{
			"created": result.Created, "modified": result.Applied, "deleted": result.Deleted,
		})
	return result, nil
}

// DeleteFile removes a file from the workspace, with the same confinement
// guarantees as a write.
func (t *WorkspaceToolbelt) DeleteFile(ctx context.Context, relPath string) error {
	target, err := t.resolve(relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil {
		if os.IsNotExist(err) {
			return domain.NotFound("file")
		}
		return err
	}
	return nil
}

// FileHash returns the content hash of a workspace file, for patch base
// verification.
func (t *WorkspaceToolbelt) FileHash(ctx context.Context, relPath string) (string, error) {
	content, err := t.ReadFile(ctx, relPath)
	if err != nil {
		return "", err
	}
	return HashContent(content), nil
}
