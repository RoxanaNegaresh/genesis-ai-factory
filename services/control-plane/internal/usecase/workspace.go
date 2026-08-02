package usecase

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"unicode/utf8"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Workspaces exposes a generated project's files and history to the editor.
//
// This is the service behind invariant I2 — human sovereignty. Every generated
// file must be readable and editable by the user, and every agent action must
// be reviewable and reversible. Without this the factory produces a black box.
type Workspaces struct {
	projects port.ProjectRepository
	recorder *Recorder
	clock    port.Clock
	vcs      port.VersionControlFactory
	log      *slog.Logger
}

// NewWorkspaces constructs the service.
func NewWorkspaces(
	projects port.ProjectRepository,
	recorder *Recorder,
	clock port.Clock,
	versionControl port.VersionControlFactory,
	log *slog.Logger,
) *Workspaces {
	if log == nil {
		log = slog.Default()
	}
	return &Workspaces{projects: projects, recorder: recorder, clock: clock,
		vcs: versionControl, log: log}
}

// FileNode is an entry in the workspace tree.
type FileNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	Dir      bool       `json:"dir"`
	Size     int64      `json:"size,omitempty"`
	Language string     `json:"language,omitempty"`
	Children []FileNode `json:"children,omitempty"`
}

// FileContent is a file returned to the editor.
type FileContent struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Language string `json:"language"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	// Binary reports that the content was not returned because it is not text.
	Binary bool `json:"binary"`
	// ReadOnly marks files the editor should not let a user change, such as
	// anything inside .git.
	ReadOnly bool `json:"read_only"`
}

// maxEditableBytes bounds what the editor will load. Monaco becomes unusable
// well before this, and streaming a 50 MB file into a webview helps nobody.
const maxEditableBytes = 2 << 20

// skipDirs are never shown in the tree. They are large, generated, and not
// what a user came to inspect.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	"vendor": true, "target": true, ".next": true, "__pycache__": true,
}

// Tree returns the file tree of a project's workspace.
func (s *Workspaces) Tree(ctx context.Context, actor domain.Principal, projectID domain.ID) ([]FileNode, error) {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return nil, err
	}
	if project.WorkspacePath == "" {
		return nil, domain.NotFound("workspace")
	}

	root := project.WorkspacePath
	tree, err := buildTree(root, root, 0)
	if err != nil {
		return nil, err
	}
	return tree, nil
}

// buildTree walks a directory into a nested node list.
func buildTree(root, dir string, depth int) ([]FileNode, error) {
	// A depth limit prevents a symlink loop or a pathological generated
	// structure from producing an unbounded response.
	const maxDepth = 12
	if depth > maxDepth {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	nodes := make([]FileNode, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && name != ".github" && name != ".env.example" && name != ".gitignore" {
			continue
		}
		if entry.IsDir() && skipDirs[name] {
			continue
		}

		full := filepath.Join(dir, name)
		relative, err := filepath.Rel(root, full)
		if err != nil {
			continue
		}
		relative = filepath.ToSlash(relative)

		if entry.IsDir() {
			children, err := buildTree(root, full, depth+1)
			if err != nil {
				continue
			}
			nodes = append(nodes, FileNode{Name: name, Path: relative, Dir: true, Children: children})
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		nodes = append(nodes, FileNode{
			Name: name, Path: relative, Size: info.Size(), Language: languageOf(name),
		})
	}

	// Directories first, then alphabetical: the ordering every file explorer
	// uses, because it is the one people can scan quickly.
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Dir != nodes[j].Dir {
			return nodes[i].Dir
		}
		return nodes[i].Name < nodes[j].Name
	})
	return nodes, nil
}

// ReadFile returns the content of a workspace file.
func (s *Workspaces) ReadFile(ctx context.Context, actor domain.Principal, projectID domain.ID, relPath string) (*FileContent, error) {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return nil, err
	}

	full, err := resolveWithin(project.WorkspacePath, relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(full)
	if err != nil {
		return nil, domain.NotFound("file")
	}
	if info.IsDir() {
		return nil, domain.Invalid("path_is_directory", "that path is a directory")
	}

	content := &FileContent{
		Path: relPath, Size: info.Size(), Language: languageOf(relPath),
	}
	if info.Size() > maxEditableBytes {
		content.Binary = true
		content.ReadOnly = true
		return content, nil
	}

	raw, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	// Returning binary as a JSON string produces mojibake in the editor and
	// corrupts the file if it is ever saved back.
	if !utf8.Valid(raw) {
		content.Binary = true
		content.ReadOnly = true
		return content, nil
	}

	content.Content = string(raw)
	content.SHA256 = domain.HashContent(content.Content)
	return content, nil
}

// WriteFile saves a user's manual edit.
//
// The expected hash guards against a lost update: if an agent changed the file
// while the editor held it open, the save is refused rather than silently
// discarding the agent's work or the user's.
func (s *Workspaces) WriteFile(
	ctx context.Context,
	actor domain.Principal,
	projectID domain.ID,
	relPath, content, expectedSHA string,
) (*FileContent, error) {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return nil, err
	}

	full, err := resolveWithin(project.WorkspacePath, relPath)
	if err != nil {
		return nil, err
	}
	if len(content) > maxEditableBytes {
		return nil, domain.Invalid("file_too_large", "the file exceeds the editable size limit")
	}

	if expectedSHA != "" {
		existing, err := os.ReadFile(full)
		if err == nil {
			if actual := domain.HashContent(string(existing)); actual != expectedSHA {
				return nil, domain.Conflict("file_changed",
					"this file changed since you opened it; reload before saving")
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return nil, err
	}
	// Write and rename so a crash cannot truncate the user's file.
	temp := full + ".genesis-tmp"
	if err := os.WriteFile(temp, []byte(content), 0o640); err != nil {
		return nil, err
	}
	if err := os.Rename(temp, full); err != nil {
		_ = os.Remove(temp)
		return nil, err
	}

	s.recorder.Emit(ctx, domain.
		NewEvent(domain.ProjectTopic(projectID), domain.EventFileWritten, domain.LevelInfo,
			"You edited "+relPath).
		For(domain.Nil, projectID).
		By(domain.RoleSystem).
		With("path", relPath).
		With("by", "user"))

	return &FileContent{
		Path: relPath, Content: content, Language: languageOf(relPath),
		Size: int64(len(content)), SHA256: domain.HashContent(content),
	}, nil
}

// Search finds text across the workspace.
func (s *Workspaces) Search(ctx context.Context, actor domain.Principal, projectID domain.ID, query string, limit int) ([]SearchHit, error) {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, domain.Invalid("query_required", "a search term is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	lowered := strings.ToLower(query)
	var hits []SearchHit

	err = filepath.WalkDir(project.WorkspacePath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(hits) >= limit {
			return filepath.SkipAll
		}

		info, err := entry.Info()
		if err != nil || info.Size() > maxEditableBytes {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil || !utf8.Valid(raw) {
			return nil
		}

		relative, _ := filepath.Rel(project.WorkspacePath, path)
		relative = filepath.ToSlash(relative)

		for number, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(strings.ToLower(line), lowered) {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if len(trimmed) > 200 {
				trimmed = trimmed[:200] + "…"
			}
			hits = append(hits, SearchHit{Path: relative, Line: number + 1, Text: trimmed})
			if len(hits) >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// SearchHit is one matching line.
type SearchHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// History returns the git log of a project.
func (s *Workspaces) History(ctx context.Context, actor domain.Principal, projectID domain.ID, limit int) ([]port.Commit, error) {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return nil, err
	}
	repo, err := s.repository(ctx, project)
	if err != nil {
		return nil, err
	}
	return repo.Log(ctx, limit)
}

// Diff returns a commit's changes, or the working tree's when ref is empty.
func (s *Workspaces) Diff(ctx context.Context, actor domain.Principal, projectID domain.ID, ref string) (string, error) {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return "", err
	}
	repo, err := s.repository(ctx, project)
	if err != nil {
		return "", err
	}
	return repo.Diff(ctx, ref)
}

// VCSStatus reports the working tree state.
func (s *Workspaces) VCSStatus(ctx context.Context, actor domain.Principal, projectID domain.ID) (*port.VCSStatus, error) {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return nil, err
	}
	repo, err := s.repository(ctx, project)
	if err != nil {
		return nil, err
	}
	return repo.Status(ctx)
}

// Rollback restores the workspace to a commit.
//
// This is destructive and therefore requires more than read access, is audited,
// and is announced on the event stream so a collaborator sees it happen.
func (s *Workspaces) Rollback(ctx context.Context, actor domain.Principal, projectID domain.ID, ref string) error {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return err
	}
	if project.OwnerID != actor.UserID && !actor.Role.AtLeast(domain.RoleAdmin) {
		return domain.Forbidden("only the project owner may roll back a workspace")
	}

	repo, err := s.repository(ctx, project)
	if err != nil {
		return err
	}
	if err := repo.Reset(ctx, ref); err != nil {
		return err
	}

	s.recorder.Emit(ctx, domain.
		NewEvent(domain.ProjectTopic(projectID), domain.EventProjectUpdated, domain.LevelWarn,
			fmt.Sprintf("Workspace rolled back to %s", shortRef(ref))).
		For(domain.Nil, projectID).
		By(domain.RoleSystem).
		With("action", "rollback").
		With("ref", ref))

	s.log.Warn("workspace rolled back",
		"project_id", projectID.String(), "ref", ref, "actor", actor.Email)
	return nil
}

func (s *Workspaces) repository(ctx context.Context, project *domain.Project) (port.VersionControl, error) {
	if project.WorkspacePath == "" {
		return nil, domain.NotFound("workspace")
	}
	if s.vcs == nil || !s.vcs.Available() {
		return nil, domain.Unavailable("vcs_unavailable",
			"version control is not available, so history and rollback are disabled")
	}
	return s.vcs.Open(ctx, project.WorkspacePath)
}

func (s *Workspaces) authorize(ctx context.Context, actor domain.Principal, projectID domain.ID) (*domain.Project, error) {
	project, err := s.projects.ByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project.OwnerID != actor.UserID && !actor.Role.AtLeast(domain.RoleAdmin) {
		// Not-found rather than forbidden: confirming a project id exists is
		// itself a disclosure.
		return nil, domain.NotFound("project")
	}
	return project, nil
}

// resolveWithin confines a path to the workspace.
func resolveWithin(root, relPath string) (string, error) {
	if root == "" {
		return "", domain.NotFound("workspace")
	}
	if relPath == "" {
		return "", domain.Invalid("path_required", "a file path is required")
	}
	if filepath.IsAbs(relPath) {
		return "", domain.Invalid("path_absolute", "the path must be relative to the workspace")
	}

	cleaned := filepath.Clean(relPath)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", domain.Invalid("path_escape", "the path escapes the workspace")
	}
	// Editing anything inside .git would corrupt history in ways a user cannot
	// undo through the editor.
	if cleaned == ".git" || strings.HasPrefix(cleaned, ".git"+string(os.PathSeparator)) {
		return "", domain.Forbidden("the git directory is not editable")
	}

	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(absoluteRoot, cleaned)

	if real, err := filepath.EvalSymlinks(full); err == nil {
		full = real
	}
	relative, err := filepath.Rel(absoluteRoot, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", domain.Invalid("path_escape", "the path escapes the workspace")
	}
	return full, nil
}

// languageOf maps a filename to a Monaco language identifier.
func languageOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go":
		return "go"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescript"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".cs":
		return "csharp"
	case ".sql":
		return "sql"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md":
		return "markdown"
	case ".css":
		return "css"
	case ".html":
		return "html"
	case ".sh":
		return "shell"
	case ".toml":
		return "toml"
	}
	switch strings.ToLower(filepath.Base(name)) {
	case "dockerfile":
		return "dockerfile"
	case "makefile":
		return "makefile"
	case ".gitignore", ".env.example":
		return "plaintext"
	}
	return "plaintext"
}

func shortRef(ref string) string {
	if len(ref) > 8 {
		return ref[:8]
	}
	if ref == "" {
		return "HEAD"
	}
	return ref
}

// ExportArchive writes the project as a zip to w.
//
// This is the single most requested capability of a code generator and the one
// most often missing: without it a user can look at their project through a
// web view but cannot take it anywhere. Streaming into the writer rather than
// building the archive in memory keeps a large project from costing a large
// allocation, and means the download starts immediately instead of after a
// pause the user reads as a hang.
//
// The exported tree deliberately differs from the browsable tree: dotfiles the
// editor hides are included, because .gitignore and .env.example are part of a
// working repository, while build output and dependency directories are not.
func (s *Workspaces) ExportArchive(
	ctx context.Context,
	actor domain.Principal,
	projectID domain.ID,
	w io.Writer,
) (string, error) {
	project, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return "", err
	}
	if project.WorkspacePath == "" {
		return "", domain.NotFound("workspace")
	}

	root := project.WorkspacePath
	archive := zip.NewWriter(w)

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable file must not abort an otherwise good export.
			return nil
		}
		// Cancellation matters here: a user who navigates away should not
		// leave the server walking a large tree.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		name := entry.Name()
		if entry.IsDir() {
			// .git is excluded: it can dwarf the source, and a recipient who
			// wants history is better served by `git clone` of the workspace.
			//
			// .genesis-tmp is the sandbox's build scratch directory. It is
			// inside the workspace so it shares the filesystem and is cleaned
			// up with it, but it holds compiled object files and a linked
			// binary — 12 MB of the first export was one stale `server`
			// executable, which is both useless to the recipient and alarming
			// to anyone who inspects the archive.
			if name == ".git" || name == ".genesis-tmp" || skipDirs[name] {
				return fs.SkipDir
			}
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return nil
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		// Forward slashes: the zip specification requires them, and Windows
		// tooling reads backslashes as part of the filename rather than as a
		// separator, producing files literally named "api\main.go".
		header.Name = filepath.ToSlash(relative)
		header.Method = zip.Deflate

		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()

		_, err = io.Copy(writer, file)
		return err
	})

	if walkErr != nil {
		_ = archive.Close()
		return "", walkErr
	}
	if err := archive.Close(); err != nil {
		return "", err
	}

	filename := project.Slug
	if filename == "" {
		filename = "project"
	}
	return filename + ".zip", nil
}
