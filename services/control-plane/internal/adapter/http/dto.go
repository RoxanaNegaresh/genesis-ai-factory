// Package http is the transport adapter: it translates HTTP requests into use
// case calls and domain results into JSON. It contains no business logic.
package http

import (
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/usecase"
)

// DTOs are deliberately separate from domain entities. Serialising the domain
// directly couples the wire format to internal refactoring and makes accidental
// disclosure (password hashes, internal flags) a one-field mistake.

type userDTO struct {
	ID          string            `json:"id"`
	Email       string            `json:"email"`
	DisplayName string            `json:"display_name"`
	Role        domain.Role       `json:"role"`
	Status      domain.UserStatus `json:"status"`
	Settings    domain.Settings   `json:"settings"`
	CreatedAt   time.Time         `json:"created_at"`
}

func toUser(u *domain.User) userDTO {
	return userDTO{
		ID: u.ID.String(), Email: u.Email, DisplayName: u.DisplayName,
		Role: u.Role, Status: u.Status, Settings: u.Settings, CreatedAt: u.CreatedAt,
	}
}

type sessionDTO struct {
	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RefreshToken string    `json:"refresh_token"`
	User         userDTO   `json:"user"`
}

func toSession(s *usecase.Session) sessionDTO {
	return sessionDTO{
		AccessToken: s.AccessToken, ExpiresAt: s.ExpiresAt,
		RefreshToken: s.RefreshToken, User: toUser(s.User),
	}
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type projectDTO struct {
	ID            string                 `json:"id"`
	OwnerID       string                 `json:"owner_id"`
	Name          string                 `json:"name"`
	Slug          string                 `json:"slug"`
	Prompt        string                 `json:"prompt"`
	Description   string                 `json:"description"`
	Category      domain.ProjectCategory `json:"category"`
	Status        domain.ProjectStatus   `json:"status"`
	WorkspacePath string                 `json:"workspace_path"`
	Stack         domain.Settings        `json:"stack"`
	Settings      domain.ProjectSettings `json:"settings"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

func toProject(p *domain.Project) projectDTO {
	return projectDTO{
		ID: p.ID.String(), OwnerID: p.OwnerID.String(), Name: p.Name, Slug: p.Slug,
		Prompt: p.Prompt, Description: p.Description, Category: p.Category, Status: p.Status,
		WorkspacePath: p.WorkspacePath, Stack: p.Stack, Settings: p.Settings,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func toProjects(items []*domain.Project) []projectDTO {
	out := make([]projectDTO, 0, len(items))
	for _, p := range items {
		out = append(out, toProject(p))
	}
	return out
}

type createProjectRequest struct {
	Name     string                  `json:"name"`
	Prompt   string                  `json:"prompt"`
	Settings *domain.ProjectSettings `json:"settings"`
	Start    bool                    `json:"start"`
}

type updateProjectRequest struct {
	Name        *string                 `json:"name"`
	Description *string                 `json:"description"`
	Status      *domain.ProjectStatus   `json:"status"`
	Category    *domain.ProjectCategory `json:"category"`
	Settings    *domain.ProjectSettings `json:"settings"`
}

type phaseDTO struct {
	ID         string             `json:"id"`
	Name       domain.Phase       `json:"name"`
	Title      string             `json:"title"`
	Ordinal    int                `json:"ordinal"`
	Status     domain.PhaseStatus `json:"status"`
	Summary    domain.Settings    `json:"summary"`
	StartedAt  *time.Time         `json:"started_at"`
	FinishedAt *time.Time         `json:"finished_at"`
}

type runDTO struct {
	ID           string           `json:"id"`
	ProjectID    string           `json:"project_id"`
	Kind         domain.RunKind   `json:"kind"`
	Status       domain.RunStatus `json:"status"`
	CurrentPhase domain.Phase     `json:"current_phase"`
	Result       domain.Settings  `json:"result"`
	Error        *domain.RunError `json:"error,omitempty"`
	TokenBudget  int64            `json:"token_budget"`
	TokensUsed   int64            `json:"tokens_used"`
	Phases       []phaseDTO       `json:"phases"`
	Progress     float64          `json:"progress"`
	StartedAt    *time.Time       `json:"started_at"`
	FinishedAt   *time.Time       `json:"finished_at"`
	CreatedAt    time.Time        `json:"created_at"`
}

func toRun(r *domain.Run) runDTO {
	phases := make([]phaseDTO, 0, len(r.Phases))
	completed := 0
	for _, p := range r.Phases {
		phases = append(phases, phaseDTO{
			ID: p.ID.String(), Name: p.Name, Title: p.Name.Title(), Ordinal: p.Ordinal,
			Status: p.Status, Summary: p.Summary, StartedAt: p.StartedAt, FinishedAt: p.FinishedAt,
		})
		if p.Status == domain.PhaseSucceeded || p.Status == domain.PhaseSkipped {
			completed++
		}
	}
	// Progress is reported from persisted phase state rather than estimated, so
	// it can never claim more than has actually happened.
	progress := 0.0
	if len(phases) > 0 {
		progress = float64(completed) / float64(len(phases))
	}
	if r.Status == domain.RunSucceeded {
		progress = 1
	}
	return runDTO{
		ID: r.ID.String(), ProjectID: r.ProjectID.String(), Kind: r.Kind, Status: r.Status,
		CurrentPhase: r.CurrentPhase, Result: r.Result, Error: r.Error,
		TokenBudget: r.TokenBudget, TokensUsed: r.TokensUsed, Phases: phases, Progress: progress,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CreatedAt: r.CreatedAt,
	}
}

func toRuns(items []*domain.Run) []runDTO {
	out := make([]runDTO, 0, len(items))
	for _, r := range items {
		out = append(out, toRun(r))
	}
	return out
}

type startRunRequest struct {
	Kind   domain.RunKind `json:"kind"`
	Prompt string         `json:"prompt"`
}

type eventDTO struct {
	Seq       int64            `json:"seq"`
	ID        string           `json:"id"`
	RunID     string           `json:"run_id,omitempty"`
	ProjectID string           `json:"project_id,omitempty"`
	Topic     string           `json:"topic"`
	Type      domain.EventType `json:"type"`
	AgentRole domain.AgentRole `json:"agent_role,omitempty"`
	AgentName string           `json:"agent_name,omitempty"`
	Level     domain.Level     `json:"level"`
	Message   string           `json:"message"`
	Payload   domain.Settings  `json:"payload,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

func toEvent(e *domain.Event) eventDTO {
	dto := eventDTO{
		Seq: e.Seq, ID: e.ID.String(), Topic: e.Topic, Type: e.Type,
		AgentRole: e.AgentRole, Level: e.Level, Message: e.Message,
		Payload: e.Payload, CreatedAt: e.CreatedAt,
	}
	if !e.RunID.IsZero() {
		dto.RunID = e.RunID.String()
	}
	if !e.ProjectID.IsZero() {
		dto.ProjectID = e.ProjectID.String()
	}
	if e.AgentRole != "" {
		if p, ok := domain.AgentProfileFor(e.AgentRole); ok {
			dto.AgentName = p.Name
		} else if e.AgentRole == domain.RoleSystem {
			dto.AgentName = "Genesis"
		}
	}
	return dto
}

func toEvents(items []*domain.Event) []eventDTO {
	out := make([]eventDTO, 0, len(items))
	for _, e := range items {
		out = append(out, toEvent(e))
	}
	return out
}

type artifactDTO struct {
	ID        string              `json:"id"`
	RunID     string              `json:"run_id"`
	Kind      domain.ArtifactKind `json:"kind"`
	Name      string              `json:"name"`
	MIME      string              `json:"mime"`
	SizeBytes int64               `json:"size_bytes"`
	SHA256    string              `json:"sha256"`
	Body      string              `json:"body,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
}

func toArtifact(a *domain.Artifact, includeBody bool) artifactDTO {
	dto := artifactDTO{
		ID: a.ID.String(), RunID: a.RunID.String(), Kind: a.Kind, Name: a.Name,
		MIME: a.MIME, SizeBytes: a.SizeBytes, SHA256: a.SHA256, CreatedAt: a.CreatedAt,
	}
	if includeBody {
		dto.Body = a.Body
	}
	return dto
}

func toArtifacts(items []*domain.Artifact) []artifactDTO {
	out := make([]artifactDTO, 0, len(items))
	for _, a := range items {
		out = append(out, toArtifact(a, false))
	}
	return out
}

type taskDTO struct {
	ID        string            `json:"id"`
	AgentRole domain.AgentRole  `json:"agent_role"`
	Title     string            `json:"title"`
	Status    domain.TaskStatus `json:"status"`
	Priority  int               `json:"priority"`
	DependsOn []string          `json:"depends_on"`
}

func toTasks(items []*domain.Task) []taskDTO {
	out := make([]taskDTO, 0, len(items))
	for _, t := range items {
		deps := make([]string, 0, len(t.DependsOn))
		for _, d := range t.DependsOn {
			deps = append(deps, d.String())
		}
		out = append(out, taskDTO{
			ID: t.ID.String(), AgentRole: t.AgentRole, Title: t.Title,
			Status: t.Status, Priority: t.Priority, DependsOn: deps,
		})
	}
	return out
}
