package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Projects is the application service for product projects.
type Projects struct {
	repo          port.ProjectRepository
	runs          port.RunRepository
	artifacts     port.ArtifactRepository
	recorder      *Recorder
	clock         port.Clock
	tx            port.TxManager
	workspaceRoot string
	log           *slog.Logger
}

// NewProjects constructs the project service.
func NewProjects(
	repo port.ProjectRepository,
	runs port.RunRepository,
	artifacts port.ArtifactRepository,
	recorder *Recorder,
	clock port.Clock,
	tx port.TxManager,
	workspaceRoot string,
	log *slog.Logger,
) *Projects {
	if log == nil {
		log = slog.Default()
	}
	return &Projects{repo: repo, runs: runs, artifacts: artifacts, recorder: recorder,
		clock: clock, tx: tx, workspaceRoot: workspaceRoot, log: log}
}

// CreateInput is the request to start a new product.
type CreateInput struct {
	Name     string
	Prompt   string
	Settings *domain.ProjectSettings
}

// Create registers a project and prepares its workspace directory.
func (s *Projects) Create(ctx context.Context, actor domain.Principal, in CreateInput) (*domain.Project, error) {
	settings := domain.DefaultProjectSettings()
	if in.Settings != nil {
		settings = *in.Settings
		settings.Normalize()
	}

	project, err := domain.NewProject(actor.UserID, in.Name, in.Prompt, settings, s.clock.Now())
	if err != nil {
		return nil, err
	}

	// Slug collisions are resolved by suffixing rather than rejecting: a user
	// who asks for two CRMs should get two CRMs, not an error dialog.
	slug, err := s.uniqueSlug(ctx, actor.UserID, project.Slug)
	if err != nil {
		return nil, err
	}
	project.Slug = slug

	path, err := s.prepareWorkspace(actor.UserID, slug)
	if err != nil {
		return nil, err
	}
	project.WorkspacePath = path

	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.repo.Create(ctx, project); err != nil {
			return err
		}
		return s.recorder.Record(ctx, domain.
			NewEvent(domain.ProjectTopic(project.ID), domain.EventProjectUpdated, domain.LevelInfo,
				fmt.Sprintf("Project %q created", project.Name)).
			For(domain.Nil, project.ID).
			By(domain.RoleSystem).
			With("action", "created").
			With("slug", project.Slug))
	})
	if err != nil {
		// Do not leave an orphan directory behind if the transaction failed.
		_ = os.Remove(path)
		return nil, err
	}

	s.log.Info("project created", "project_id", project.ID.String(), "slug", project.Slug)
	return project, nil
}

// Get returns a project the actor is allowed to see.
func (s *Projects) Get(ctx context.Context, actor domain.Principal, id domain.ID) (*domain.Project, error) {
	project, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(actor, project, domain.RoleViewer); err != nil {
		return nil, err
	}
	return project, nil
}

// List returns the actor's projects.
func (s *Projects) List(ctx context.Context, actor domain.Principal, f port.ProjectFilter) ([]*domain.Project, int64, error) {
	// Non-admins only ever see their own projects, enforced here rather than in
	// the handler so every caller inherits the restriction.
	if !actor.Role.AtLeast(domain.RoleAdmin) {
		f.OwnerID = actor.UserID
	}
	return s.repo.List(ctx, f)
}

// UpdateInput carries optional field changes.
type UpdateInput struct {
	Name        *string
	Description *string
	Status      *domain.ProjectStatus
	Category    *domain.ProjectCategory
	Settings    *domain.ProjectSettings
}

// Update applies a partial change to a project.
func (s *Projects) Update(ctx context.Context, actor domain.Principal, id domain.ID, in UpdateInput) (*domain.Project, error) {
	project, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(actor, project, domain.RoleMember); err != nil {
		return nil, err
	}

	now := s.clock.Now()
	changed := map[string]any{}

	if in.Name != nil && *in.Name != project.Name {
		if err := project.Rename(*in.Name, now); err != nil {
			return nil, err
		}
		slug, err := s.uniqueSlug(ctx, project.OwnerID, project.Slug)
		if err != nil {
			return nil, err
		}
		project.Slug = slug
		changed["name"] = project.Name
	}
	if in.Description != nil {
		project.Description = strings.TrimSpace(*in.Description)
		changed["description"] = true
	}
	if in.Status != nil {
		if !in.Status.Valid() {
			return nil, domain.Invalid("status_invalid", "unknown project status")
		}
		project.Status = *in.Status
		changed["status"] = string(project.Status)
	}
	if in.Category != nil {
		if !in.Category.Valid() {
			return nil, domain.Invalid("category_invalid", "unknown project category")
		}
		project.Category = *in.Category
		changed["category"] = string(project.Category)
	}
	if in.Settings != nil {
		settings := *in.Settings
		settings.Normalize()
		project.Settings = settings
		changed["settings"] = true
	}
	if len(changed) == 0 {
		return project, nil
	}
	project.UpdatedAt = now

	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.repo.Update(ctx, project); err != nil {
			return err
		}
		return s.recorder.Record(ctx, domain.
			NewEvent(domain.ProjectTopic(project.ID), domain.EventProjectUpdated, domain.LevelInfo,
				fmt.Sprintf("Project %q updated", project.Name)).
			For(domain.Nil, project.ID).
			By(domain.RoleSystem).
			With("changed", changed))
	})
	if err != nil {
		return nil, err
	}
	return project, nil
}

// Delete archives a project. The workspace on disk is intentionally preserved:
// deleting a user's generated source code because they archived a card in the
// UI would be indefensible.
func (s *Projects) Delete(ctx context.Context, actor domain.Principal, id domain.ID) error {
	project, err := s.repo.ByID(ctx, id)
	if err != nil {
		return err
	}
	if err := s.authorize(actor, project, domain.RoleAdmin); err != nil {
		return err
	}

	active, _, err := s.runs.List(ctx, port.RunFilter{ProjectID: id, Status: domain.RunRunning, Limit: 1})
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return domain.Conflict("project_has_active_run", "cancel the running build before archiving this project")
	}

	return s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.repo.SoftDelete(ctx, id, s.clock.Now()); err != nil {
			return err
		}
		return s.recorder.Record(ctx, domain.
			NewEvent(domain.ProjectTopic(id), domain.EventProjectUpdated, domain.LevelWarn,
				fmt.Sprintf("Project %q archived", project.Name)).
			For(domain.Nil, id).
			By(domain.RoleSystem).
			With("action", "archived").
			With("workspace_preserved", project.WorkspacePath))
	})
}

// Artifacts returns the documents produced for a project.
func (s *Projects) Artifacts(ctx context.Context, actor domain.Principal, id domain.ID) ([]*domain.Artifact, error) {
	project, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(actor, project, domain.RoleViewer); err != nil {
		return nil, err
	}
	runs, _, err := s.runs.List(ctx, port.RunFilter{ProjectID: id, Limit: 50})
	if err != nil {
		return nil, err
	}
	var out []*domain.Artifact
	for _, r := range runs {
		items, err := s.artifacts.ByRun(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// authorize enforces ownership and role requirements.
func (s *Projects) authorize(actor domain.Principal, project *domain.Project, minRole domain.Role) error {
	if project.OwnerID == actor.UserID {
		return nil
	}
	if actor.Role.AtLeast(domain.RoleAdmin) && actor.Role.AtLeast(minRole) {
		return nil
	}
	// Return not-found rather than forbidden for non-members: confirming that a
	// project id exists is itself an information leak.
	return domain.NotFound("project")
}

// uniqueSlug appends a numeric suffix until the slug is free for this owner.
func (s *Projects) uniqueSlug(ctx context.Context, ownerID domain.ID, base string) (string, error) {
	candidate := base
	for attempt := 2; attempt < 1000; attempt++ {
		_, err := s.repo.BySlug(ctx, ownerID, candidate)
		if errors.Is(err, domain.ErrNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		candidate = fmt.Sprintf("%s-%d", base, attempt)
	}
	return "", domain.Conflict("slug_exhausted", "too many projects with a similar name")
}

// prepareWorkspace creates the project directory, guarding against traversal.
func (s *Projects) prepareWorkspace(ownerID domain.ID, slug string) (string, error) {
	if s.workspaceRoot == "" {
		return "", nil
	}
	// slug is produced by domain.Slugify and cannot contain separators, but the
	// containment check is asserted anyway: a path escape here would let a
	// project name write anywhere on the filesystem.
	root, err := filepath.Abs(s.workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	path := filepath.Join(root, ownerID.String(), slug)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", domain.Invalid("workspace_path_invalid", "resolved workspace path escapes the workspace root")
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	return path, nil
}
