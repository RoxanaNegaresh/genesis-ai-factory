package usecase

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// RunExecutor starts and cancels builds. The run service depends on this
// interface rather than on the factory package, so the executor can be swapped
// for a Temporal client in v0.6 without touching application logic.
type RunExecutor interface {
	Start(run *domain.Run, project *domain.Project)
	Cancel(runID domain.ID) bool
	ActiveCount() int
}

// Runs is the application service for builds.
type Runs struct {
	repo      port.RunRepository
	projects  port.ProjectRepository
	tasks     port.TaskRepository
	artifacts port.ArtifactRepository
	events    port.EventRepository
	recorder  *Recorder
	executor  RunExecutor
	clock     port.Clock
	tx        port.TxManager
	log       *slog.Logger
}

// NewRuns constructs the run service.
func NewRuns(
	repo port.RunRepository,
	projects port.ProjectRepository,
	tasks port.TaskRepository,
	artifacts port.ArtifactRepository,
	events port.EventRepository,
	recorder *Recorder,
	executor RunExecutor,
	clock port.Clock,
	tx port.TxManager,
	log *slog.Logger,
) *Runs {
	if log == nil {
		log = slog.Default()
	}
	return &Runs{repo: repo, projects: projects, tasks: tasks, artifacts: artifacts,
		events: events, recorder: recorder, executor: executor, clock: clock, tx: tx, log: log}
}

// StartInput describes a requested build.
type StartInput struct {
	Kind   domain.RunKind
	Prompt string
}

// Start creates a run and hands it to the executor.
func (s *Runs) Start(ctx context.Context, actor domain.Principal, projectID domain.ID, in StartInput) (*domain.Run, error) {
	project, err := s.projects.ByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(actor, project); err != nil {
		return nil, err
	}

	// One build at a time per project. Two concurrent runs writing the same
	// workspace would interleave file writes and produce an incoherent result.
	active, _, err := s.repo.List(ctx, port.RunFilter{ProjectID: projectID, Status: domain.RunRunning, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(active) > 0 {
		return nil, domain.Conflict("run_already_active", "a build is already running for this project")
	}
	pending, _, err := s.repo.List(ctx, port.RunFilter{ProjectID: projectID, Status: domain.RunPending, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, domain.Conflict("run_already_queued", "a build is already queued for this project")
	}

	kind := in.Kind
	if kind == "" {
		kind = domain.RunBuild
	}
	if !kind.Valid() {
		return nil, domain.Invalid("run_kind_invalid", "unknown run kind")
	}

	prompt := in.Prompt
	if prompt == "" {
		prompt = project.Prompt
	}

	now := s.clock.Now().UTC()
	// The input snapshot makes a run reproducible even after the project's
	// prompt or settings are later edited.
	input := domain.Settings{
		"prompt":   prompt,
		"settings": project.Settings,
		"category": string(project.Category),
	}
	run, err := domain.NewRun(projectID, actor.UserID, kind, input, project.Settings.TokenBudget, now)
	if err != nil {
		return nil, err
	}
	phases := domain.NewRunPhases(run.ID, now)

	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.repo.Create(ctx, run, phases); err != nil {
			return err
		}
		project.Status = domain.ProjectBuilding
		project.UpdatedAt = now
		if err := s.projects.Update(ctx, project); err != nil {
			return err
		}
		return s.recorder.Record(ctx, domain.
			NewEvent(domain.RunTopic(run.ID), domain.EventRunCreated, domain.LevelInfo,
				fmt.Sprintf("Build queued for %q", project.Name)).
			For(run.ID, project.ID).
			By(domain.RoleSystem).
			With("kind", string(kind)).
			With("phases", len(phases)))
	})
	if err != nil {
		return nil, err
	}

	if s.executor != nil {
		s.executor.Start(run, project)
	}
	s.log.Info("run started", "run_id", run.ID.String(), "project_id", projectID.String())
	return run, nil
}

// Get returns a run with its phases.
func (s *Runs) Get(ctx context.Context, actor domain.Principal, runID domain.ID) (*domain.Run, error) {
	run, err := s.repo.ByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	project, err := s.projects.ByID(ctx, run.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(actor, project); err != nil {
		return nil, err
	}
	return run, nil
}

// List returns the runs of a project.
func (s *Runs) List(ctx context.Context, actor domain.Principal, projectID domain.ID, limit, offset int) ([]*domain.Run, int64, error) {
	project, err := s.projects.ByID(ctx, projectID)
	if err != nil {
		return nil, 0, err
	}
	if err := s.authorize(actor, project); err != nil {
		return nil, 0, err
	}
	return s.repo.List(ctx, port.RunFilter{ProjectID: projectID, Limit: limit, Offset: offset})
}

// Cancel requests cooperative cancellation.
func (s *Runs) Cancel(ctx context.Context, actor domain.Principal, runID domain.ID) (*domain.Run, error) {
	run, err := s.repo.ByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	project, err := s.projects.ByID(ctx, run.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(actor, project); err != nil {
		return nil, err
	}
	if run.Status.Terminal() {
		return nil, domain.Conflict("run_terminal", "this build has already finished")
	}

	if err := run.RequestCancel(s.clock.Now()); err != nil {
		return nil, err
	}
	if err := s.repo.Update(ctx, run); err != nil {
		return nil, err
	}
	if s.executor != nil {
		s.executor.Cancel(runID)
	}
	s.recorder.Emit(ctx, domain.
		NewEvent(domain.RunTopic(run.ID), domain.EventLog, domain.LevelWarn, "Cancellation requested").
		For(run.ID, run.ProjectID).
		By(domain.RoleSystem))
	return run, nil
}

// Events returns a page of the run's event log.
func (s *Runs) Events(ctx context.Context, actor domain.Principal, runID domain.ID, afterSeq int64, limit int) ([]*domain.Event, error) {
	if _, err := s.Get(ctx, actor, runID); err != nil {
		return nil, err
	}
	return s.events.Query(ctx, port.EventQuery{RunID: runID, AfterSeq: afterSeq, Limit: limit})
}

// Tasks returns the run's work DAG.
func (s *Runs) Tasks(ctx context.Context, actor domain.Principal, runID domain.ID) ([]*domain.Task, error) {
	if _, err := s.Get(ctx, actor, runID); err != nil {
		return nil, err
	}
	return s.tasks.ByRun(ctx, runID)
}

// Artifacts returns the documents produced by a run.
func (s *Runs) Artifacts(ctx context.Context, actor domain.Principal, runID domain.ID) ([]*domain.Artifact, error) {
	if _, err := s.Get(ctx, actor, runID); err != nil {
		return nil, err
	}
	return s.artifacts.ByRun(ctx, runID)
}

// Artifact returns one artifact's full content.
func (s *Runs) Artifact(ctx context.Context, actor domain.Principal, artifactID domain.ID) (*domain.Artifact, error) {
	a, err := s.artifacts.ByID(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	project, err := s.projects.ByID(ctx, a.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(actor, project); err != nil {
		return nil, err
	}
	return a, nil
}

// AgentStatus is the live view of one agent for the monitoring dashboard.
type AgentStatus struct {
	Profile   domain.AgentProfile `json:"profile"`
	Status    domain.AgentStatus  `json:"status"`
	Task      string              `json:"task,omitempty"`
	RunID     domain.ID           `json:"run_id,omitempty"`
	Artifacts int                 `json:"artifacts"`
	LastEvent string              `json:"last_event,omitempty"`
}

// AgentBoard assembles the agent dashboard. Status is derived from the event
// log rather than tracked in a separate mutable structure: one source of truth
// means the dashboard cannot disagree with the timeline.
func (s *Runs) AgentBoard(ctx context.Context, actor domain.Principal, runID domain.ID) ([]AgentStatus, error) {
	board := make([]AgentStatus, 0, 11)
	index := map[domain.AgentRole]int{}
	for _, p := range domain.AgentRoster() {
		index[p.Role] = len(board)
		board = append(board, AgentStatus{Profile: p, Status: domain.AgentIdle})
	}
	if runID.IsZero() {
		return board, nil
	}

	events, err := s.Events(ctx, actor, runID, 0, 1000)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		i, ok := index[e.AgentRole]
		if !ok {
			continue
		}
		entry := &board[i]
		entry.RunID = runID
		entry.LastEvent = e.Message
		switch e.Type {
		case domain.EventAgentAssigned, domain.EventAgentThinking:
			if entry.Status != domain.AgentDone && entry.Status != domain.AgentFailed {
				entry.Status = domain.AgentWorking
				entry.Task = e.Message
			}
		case domain.EventAgentCompleted:
			entry.Status = domain.AgentDone
			entry.Task = e.Message
		case domain.EventAgentFailed:
			entry.Status = domain.AgentFailed
			entry.Task = e.Message
		case domain.EventArtifactCreated:
			entry.Artifacts++
		}
	}
	return board, nil
}

// RecoverInterrupted marks runs that were active when the process died. A run
// left in "running" after a crash would otherwise show a build that no
// goroutine is executing — a lie the UI cannot detect.
func (s *Runs) RecoverInterrupted(ctx context.Context) (int, error) {
	active, err := s.repo.ActiveRuns(ctx)
	if err != nil {
		return 0, err
	}
	now := s.clock.Now()
	recovered := 0
	for _, run := range active {
		run.Status = domain.RunInterrupted
		run.UpdatedAt = now
		run.FinishedAt = &now
		if run.Error == nil {
			run.Error = &domain.RunError{
				Code:    "interrupted",
				Message: "the server restarted while this build was running",
				Phase:   run.CurrentPhase,
			}
		}
		if err := s.repo.Update(ctx, run); err != nil {
			s.log.Warn("failed to mark run interrupted", "run_id", run.ID.String(), "error", err)
			continue
		}
		s.recorder.Emit(ctx, domain.
			NewEvent(domain.RunTopic(run.ID), domain.EventRunFailed, domain.LevelWarn,
				"Build interrupted by a server restart").
			For(run.ID, run.ProjectID).
			By(domain.RoleSystem).
			With("recoverable", true))
		recovered++
	}
	return recovered, nil
}

func (s *Runs) authorize(actor domain.Principal, project *domain.Project) error {
	if project.OwnerID == actor.UserID || actor.Role.AtLeast(domain.RoleAdmin) {
		return nil
	}
	return domain.NotFound("project")
}
