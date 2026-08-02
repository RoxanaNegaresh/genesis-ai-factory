package factory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/infra/vcs"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Driver executes the autonomous development loop for one run.
//
// In v0.1 this is an in-process supervised state machine. In v0.6 the same
// phase and event contract is executed by Temporal so runs survive process
// restarts. Keeping the contract identical now is what makes that swap a
// substitution rather than a rewrite: every consumer (UI, CLI, persistence)
// depends on the events, not on the executor.
type Driver struct {
	runs      port.RunRepository
	projects  port.ProjectRepository
	artifacts port.ArtifactRepository
	tasks     port.TaskRepository
	recorder  Recorder
	registry  *Registry
	clock     func() time.Time
	log       *slog.Logger

	maxParallel int

	// reasoner, memory and runner are optional. A driver with none of them
	// still executes every phase and produces every artifact — just without
	// model judgement, recall, or proof that the result runs.
	reasoner *Reasoner
	memory   *MemoryService
	runner   *Runner
	// vcs enables per-phase snapshots so any agent action is reversible.
	vcs bool

	mu     sync.Mutex
	active map[domain.ID]context.CancelFunc
}

// Recorder is the subset of event recording the driver needs. Declaring it here
// rather than importing the usecase package keeps the dependency pointing the
// right way.
type Recorder interface {
	Record(ctx context.Context, e *domain.Event) error
}

// DriverConfig carries tuning parameters.
type DriverConfig struct {
	MaxParallelAgents int
	Clock             func() time.Time
	// LLM enables model-backed reasoning when non-nil.
	LLM port.LLM
	// Memory enables long-term recall when non-nil.
	Memory *MemoryService
	// Sandbox enables building and running generated projects when non-nil.
	Sandbox port.Sandbox
	// VersionControl enables git snapshots of the generated workspace.
	VersionControl bool
}

// NewDriver constructs the run driver.
func NewDriver(
	runs port.RunRepository,
	projects port.ProjectRepository,
	artifacts port.ArtifactRepository,
	tasks port.TaskRepository,
	recorder Recorder,
	cfg DriverConfig,
	log *slog.Logger,
) *Driver {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.MaxParallelAgents <= 0 {
		cfg.MaxParallelAgents = 4
	}
	var reasoner *Reasoner
	if cfg.LLM != nil {
		reasoner = NewReasoner(cfg.LLM, log)
	}

	var runner *Runner
	if cfg.Sandbox != nil {
		runner = NewRunner(cfg.Sandbox)
	}

	return &Driver{
		runs: runs, projects: projects, artifacts: artifacts, tasks: tasks,
		recorder:    recorder,
		registry:    NewRegistry(),
		clock:       cfg.Clock,
		log:         log,
		maxParallel: cfg.MaxParallelAgents,
		reasoner:    reasoner,
		memory:      cfg.Memory,
		runner:      runner,
		vcs:         cfg.VersionControl && vcs.Available(),
		active:      map[domain.ID]context.CancelFunc{},
	}
}

// Start launches a run in the background and returns immediately.
//
// Two ownership rules are enforced here:
//   - The caller's request context is deliberately not propagated: a build
//     outlives the HTTP request that triggered it.
//   - The aggregates are cloned. The caller keeps its copy to serialise into a
//     response while this goroutine mutates its own; sharing them would be a
//     data race between the handler and the driver.
func (d *Driver) Start(callerRun *domain.Run, callerProject *domain.Project) {
	run := callerRun.Clone()
	project := callerProject.Clone()

	ctx, cancel := context.WithCancel(context.Background())

	d.mu.Lock()
	d.active[run.ID] = cancel
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.active, run.ID)
			d.mu.Unlock()
			cancel()

			if r := recover(); r != nil {
				d.log.Error("run driver panicked", "run_id", run.ID.String(), "panic", r)
				d.failRun(context.Background(), run, domain.RunError{
					Code: "internal_error", Message: fmt.Sprintf("the build crashed: %v", r),
				})
			}
		}()
		d.execute(ctx, run, project)
	}()
}

// Cancel requests cooperative cancellation of a running build.
func (d *Driver) Cancel(runID domain.ID) bool {
	d.mu.Lock()
	cancel, ok := d.active[runID]
	d.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// ActiveCount reports how many runs are executing.
func (d *Driver) ActiveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.active)
}

// execute drives the full phase sequence.
func (d *Driver) execute(ctx context.Context, run *domain.Run, project *domain.Project) {
	now := d.clock()
	if err := run.Start(now); err != nil {
		d.log.Warn("run already started", "run_id", run.ID.String(), "error", err)
		return
	}
	if err := d.runs.Update(ctx, run); err != nil {
		d.log.Error("failed to mark run started", "run_id", run.ID.String(), "error", err)
		return
	}

	classification := Classify(project.Prompt)
	blueprint := BlueprintFor(classification.Category)

	bb := NewBlackboard(project, run)
	bb.Classification = classification
	bb.Blueprint = blueprint
	bb.Runner = d.runner
	if d.reasoner != nil {
		bb.Reasoning = &Reasoning{
			reasoner: d.reasoner,
			memory:   d.memory,
			budget:   NewTokenBudget(run.TokenBudget),
		}
	}

	// Announcing whether inference is active matters for trust: an operator
	// must be able to tell whether the artifacts they are reading were reasoned
	// about or derived from a template.
	d.emit(ctx, run, domain.EventRunStarted, domain.LevelInfo, domain.RoleSystem,
		fmt.Sprintf("Build started for %q", project.Name), map[string]any{
			"category":   string(classification.Category),
			"confidence": classification.Confidence,
			"blueprint":  blueprint.Key,
			"reasoning":  bb.Reasoning.Enabled(),
		})

	// Record the classification on the project so the UI can show it before the
	// first artifact lands.
	if project.Category != classification.Category && classification.Confidence > 0 {
		project.Category = classification.Category
		project.Status = domain.ProjectBuilding
		project.UpdatedAt = d.clock()
		if err := d.projects.Update(ctx, project); err != nil {
			d.log.Warn("failed to record project category", "error", err)
		}
	}

	fileCount := 0
	var fileMu sync.Mutex
	onWrite := func(relPath string, bytes int) {
		fileMu.Lock()
		fileCount++
		fileMu.Unlock()
		d.emit(ctx, run, domain.EventFileWritten, domain.LevelDebug, domain.RoleSystem,
			relPath, map[string]any{"path": relPath, "bytes": bytes})
	}
	emit := func(ctx context.Context, level domain.Level, agent domain.AgentRole, message string, fields map[string]any) {
		d.emit(ctx, run, domain.EventAgentThinking, level, agent, message, fields)
	}
	toolbelt := NewWorkspaceToolbelt(project.WorkspacePath, domain.RoleSystem, emit, onWrite)

	// A brief matching no built-in category previously fell back to a generic
	// SaaS template, which is the most visible way this product can fail to
	// understand a user. With a model available, derive a real blueprint for
	// the actual domain instead.
	if classification.Category == domain.CategoryCustom && bb.Reasoning.Enabled() {
		if synthesized, ok := SynthesizeBlueprint(ctx, toolbelt.For(domain.RoleArchitect),
			bb.Reasoning, project.Prompt); ok {
			bb.Blueprint = synthesized
			blueprint = synthesized
		}
	}

	phases, err := d.runs.Phases(ctx, run.ID)
	if err != nil {
		d.failRun(ctx, run, domain.RunError{Code: "phases_unavailable", Message: err.Error()})
		return
	}

	// Version control is initialised before any agent writes, so the very first
	// generated file is already covered by history.
	repo := d.openRepository(ctx, run, project)

	for i := range phases {
		phase := &phases[i]

		// Healing runs only when verification actually failed. Skipping it
		// explicitly keeps the timeline honest rather than showing a fake step.
		if phase.Name == domain.PhaseHeal {
			if !d.runHealing(ctx, run, project, phase, bb, toolbelt) {
				d.skipPhase(ctx, run, phase, "verification passed; nothing to repair")
			} else {
				d.snapshot(ctx, repo, run, "fix: self-healing repairs")
			}
			continue
		}

		if err := ctx.Err(); err != nil {
			d.cancelRun(ctx, run, phase)
			return
		}

		if err := d.runPhase(ctx, run, project, phase, bb, toolbelt); err != nil {
			d.snapshot(ctx, repo, run, "wip("+string(phase.Name)+"): incomplete phase")
			if errors.Is(err, context.Canceled) {
				d.cancelRun(context.Background(), run, phase)
				return
			}
			d.failPhase(ctx, run, phase, err)
			d.failRun(ctx, run, domain.RunError{
				Code: "phase_failed", Message: err.Error(), Phase: phase.Name,
			})
			return
		}

		// Snapshot after each completed phase rather than after each file. A
		// commit per file would bury the narrative; a commit per phase is
		// exactly the granularity a rollback needs.
		d.snapshot(ctx, repo, run,
			fmt.Sprintf("%s: %s", commitPrefix(phase.Name), phase.Name.Title()))
	}

	d.completeRun(ctx, run, project, bb, fileCount)
}

// runPhase executes every agent assigned to a phase.
func (d *Driver) runPhase(
	ctx context.Context,
	run *domain.Run,
	project *domain.Project,
	phase *domain.RunPhase,
	bb *Blackboard,
	toolbelt *WorkspaceToolbelt,
) error {
	started := d.clock()
	phase.Status = domain.PhaseRunning
	phase.StartedAt = &started
	phase.UpdatedAt = started
	if err := d.runs.UpdatePhase(ctx, phase); err != nil {
		return fmt.Errorf("update phase: %w", err)
	}

	run.CurrentPhase = phase.Name
	run.UpdatedAt = started
	if err := d.runs.Update(ctx, run); err != nil {
		return fmt.Errorf("update run phase: %w", err)
	}

	d.emit(ctx, run, domain.EventPhaseStarted, domain.LevelInfo, domain.RoleSystem,
		phase.Name.Title(), map[string]any{"phase": string(phase.Name), "ordinal": phase.Ordinal})

	profiles := domain.AgentsForPhase(phase.Name)

	// The planning phase produces the task DAG rather than running an agent:
	// it is the orchestrator's own work, and materialising it makes the plan
	// visible and auditable instead of implicit in code.
	if phase.Name == domain.PhasePlan {
		if err := d.buildPlan(ctx, run, phase, bb); err != nil {
			return err
		}
	}

	produced := 0
	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		agent, ok := d.registry.Get(profile.Role)
		if !ok {
			continue
		}
		artifacts, err := d.runAgent(ctx, run, project, phase, agent, bb, toolbelt)
		if err != nil {
			return err
		}
		produced += len(artifacts)
	}

	finished := d.clock()
	phase.Status = domain.PhaseSucceeded
	phase.FinishedAt = &finished
	phase.UpdatedAt = finished
	phase.Summary = domain.Settings{
		"agents":      len(profiles),
		"artifacts":   produced,
		"duration_ms": finished.Sub(started).Milliseconds(),
	}
	if err := d.runs.UpdatePhase(ctx, phase); err != nil {
		return fmt.Errorf("finalise phase: %w", err)
	}

	d.emit(ctx, run, domain.EventPhaseCompleted, domain.LevelInfo, domain.RoleSystem,
		fmt.Sprintf("%s complete", phase.Name.Title()), map[string]any{
			"phase":       string(phase.Name),
			"artifacts":   produced,
			"duration_ms": finished.Sub(started).Milliseconds(),
		})
	return nil
}

// runAgent executes one agent with its own budget and records the outcome.
func (d *Driver) runAgent(
	ctx context.Context,
	run *domain.Run,
	project *domain.Project,
	phase *domain.RunPhase,
	agent Agent,
	bb *Blackboard,
	toolbelt *WorkspaceToolbelt,
) ([]*domain.Artifact, error) {
	charter := agent.Charter()

	d.emit(ctx, run, domain.EventAgentAssigned, domain.LevelInfo, charter.Role,
		charter.Role.DisplayName()+" starting", map[string]any{
			"mission":     charter.Mission,
			"model_class": charter.ModelClass,
			"outputs":     kindStrings(charter.Outputs),
		})

	// Every agent runs under its own deadline. An agent that hangs must not
	// hold the entire build hostage.
	budget := charter.Budget
	if budget.MaxDuration <= 0 {
		budget.MaxDuration = DefaultBudget().MaxDuration
	}
	agentCtx, cancel := context.WithTimeout(ctx, budget.MaxDuration)
	defer cancel()

	started := d.clock()
	artifacts, err := agent.Execute(agentCtx, bb, toolbelt.For(charter.Role))
	elapsed := d.clock().Sub(started)

	if err != nil {
		if errors.Is(agentCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%s exceeded its %s budget", charter.Role, budget.MaxDuration)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, context.Canceled
		}
		d.emit(ctx, run, domain.EventAgentFailed, domain.LevelError, charter.Role,
			fmt.Sprintf("%s failed: %v", charter.Role.DisplayName(), err),
			map[string]any{"error": err.Error(), "duration_ms": elapsed.Milliseconds()})
		return nil, fmt.Errorf("%s: %w", charter.Role, err)
	}

	for _, a := range artifacts {
		if err := d.persistArtifact(ctx, run, a); err != nil {
			d.log.Warn("failed to persist artifact",
				"artifact", a.Name, "agent", string(charter.Role), "error", err)
			continue
		}
		d.emit(ctx, run, domain.EventArtifactCreated, domain.LevelInfo, charter.Role,
			fmt.Sprintf("Produced %s", a.Name), map[string]any{
				"artifact_id": a.ID.String(),
				"kind":        string(a.Kind),
				"name":        a.Name,
				"size_bytes":  a.SizeBytes,
			})
	}

	d.emit(ctx, run, domain.EventAgentCompleted, domain.LevelInfo, charter.Role,
		fmt.Sprintf("%s finished in %s", charter.Role.DisplayName(), elapsed.Round(time.Millisecond)),
		map[string]any{"artifacts": len(artifacts), "duration_ms": elapsed.Milliseconds()})

	_ = project
	_ = phase
	return artifacts, nil
}

// persistArtifact stores an artifact, tolerating content-hash duplicates.
func (d *Driver) persistArtifact(ctx context.Context, run *domain.Run, a *domain.Artifact) error {
	existing, err := d.artifacts.ExistsBySHA(ctx, a.ProjectID, a.SHA256)
	if err != nil {
		return err
	}
	if existing != nil {
		// Identical content already stored: reuse it rather than duplicating.
		a.ID = existing.ID
		return nil
	}
	if err := d.artifacts.Create(ctx, a); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil
		}
		return err
	}
	_ = run
	return nil
}

// buildPlan materialises the task DAG for the build so the plan is inspectable
// before any code is generated.
func (d *Driver) buildPlan(ctx context.Context, run *domain.Run, phase *domain.RunPhase, bb *Blackboard) error {
	now := d.clock()
	var (
		tasks  []*domain.Task
		byRole = map[domain.AgentRole]*domain.Task{}
	)

	priority := 100
	for _, p := range domain.PhaseOrder {
		for _, profile := range domain.AgentsForPhase(p) {
			t := domain.NewTask(run.ID, phase.ID, profile.Role,
				profile.Title+" — "+profile.Phase.Title(), profile.Mission, priority, now)
			t.Input = domain.Settings{
				"phase":    string(profile.Phase),
				"produces": profile.Produces,
				"consumes": profile.Consumes,
			}
			byRole[profile.Role] = t
			tasks = append(tasks, t)
			priority -= 5
		}
	}

	// Edges are derived from artifact dependencies: an agent depends on
	// whichever agents produce the artifacts it consumes. Deriving the DAG from
	// data rather than hard-coding it means adding an agent cannot desynchronise
	// the plan from reality.
	producers := map[string]domain.AgentRole{}
	for _, profile := range domain.AgentRoster() {
		for _, out := range profile.Produces {
			producers[out] = profile.Role
		}
	}
	for _, profile := range domain.AgentRoster() {
		t, ok := byRole[profile.Role]
		if !ok {
			continue
		}
		for _, in := range profile.Consumes {
			if producerRole, ok := producers[in]; ok && producerRole != profile.Role {
				if dep, ok := byRole[producerRole]; ok {
					t.DependsOnTasks(dep.ID)
				}
			}
		}
	}

	if err := d.tasks.CreateBatch(ctx, tasks); err != nil {
		return fmt.Errorf("persist task plan: %w", err)
	}

	edges := 0
	for _, t := range tasks {
		edges += len(t.DependsOn)
	}
	bb.SetValue("task_count", len(tasks))

	d.emit(ctx, run, domain.EventLog, domain.LevelInfo, domain.RoleArchitect,
		fmt.Sprintf("Planned %d tasks with %d dependencies", len(tasks), edges),
		map[string]any{"tasks": len(tasks), "edges": edges})
	return nil
}

func (d *Driver) skipPhase(ctx context.Context, run *domain.Run, phase *domain.RunPhase, reason string) {
	now := d.clock()
	phase.Status = domain.PhaseSkipped
	phase.FinishedAt = &now
	phase.UpdatedAt = now
	phase.Summary = domain.Settings{"reason": reason}
	if err := d.runs.UpdatePhase(ctx, phase); err != nil {
		d.log.Warn("failed to skip phase", "phase", string(phase.Name), "error", err)
	}
	d.emit(ctx, run, domain.EventPhaseCompleted, domain.LevelDebug, domain.RoleSystem,
		fmt.Sprintf("%s skipped — %s", phase.Name.Title(), reason),
		map[string]any{"phase": string(phase.Name), "skipped": true})
}

func (d *Driver) failPhase(ctx context.Context, run *domain.Run, phase *domain.RunPhase, cause error) {
	now := d.clock()
	phase.Status = domain.PhaseFailed
	phase.FinishedAt = &now
	phase.UpdatedAt = now
	phase.Summary = domain.Settings{"error": cause.Error()}
	if err := d.runs.UpdatePhase(ctx, phase); err != nil {
		d.log.Warn("failed to record phase failure", "error", err)
	}
	d.emit(ctx, run, domain.EventPhaseFailed, domain.LevelError, domain.RoleSystem,
		fmt.Sprintf("%s failed: %v", phase.Name.Title(), cause),
		map[string]any{"phase": string(phase.Name), "error": cause.Error()})
}

func (d *Driver) failRun(ctx context.Context, run *domain.Run, cause domain.RunError) {
	if err := run.Fail(cause, d.clock()); err != nil {
		return
	}
	if err := d.runs.Update(ctx, run); err != nil {
		d.log.Error("failed to persist run failure", "run_id", run.ID.String(), "error", err)
	}
	d.emit(ctx, run, domain.EventRunFailed, domain.LevelError, domain.RoleSystem,
		"Build failed: "+cause.Message,
		map[string]any{"code": cause.Code, "phase": string(cause.Phase)})

	if project, err := d.projects.ByID(ctx, run.ProjectID); err == nil {
		project.Status = domain.ProjectFailed
		project.UpdatedAt = d.clock()
		_ = d.projects.Update(ctx, project)
	}
}

func (d *Driver) cancelRun(ctx context.Context, run *domain.Run, phase *domain.RunPhase) {
	now := d.clock()
	if phase != nil && phase.Status == domain.PhaseRunning {
		phase.Status = domain.PhaseFailed
		phase.FinishedAt = &now
		phase.UpdatedAt = now
		phase.Summary = domain.Settings{"reason": "canceled"}
		_ = d.runs.UpdatePhase(ctx, phase)
	}
	if err := run.Cancel(now); err != nil {
		return
	}
	if err := d.runs.Update(ctx, run); err != nil {
		d.log.Error("failed to persist cancellation", "run_id", run.ID.String(), "error", err)
	}
	d.emit(ctx, run, domain.EventRunCanceled, domain.LevelWarn, domain.RoleSystem,
		"Build canceled", map[string]any{"phase": string(run.CurrentPhase)})

	if project, err := d.projects.ByID(ctx, run.ProjectID); err == nil {
		project.Status = domain.ProjectDraft
		project.UpdatedAt = now
		_ = d.projects.Update(ctx, project)
	}
}

func (d *Driver) completeRun(ctx context.Context, run *domain.Run, project *domain.Project, bb *Blackboard, fileCount int) {
	artifacts := bb.Artifacts()
	kinds := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		kinds = append(kinds, string(a.Kind))
	}

	if bb.Reasoning != nil {
		run.TokensUsed = bb.Reasoning.budget.Used()
	}

	result := domain.Settings{
		"tokens_used":    run.TokensUsed,
		"reasoning":      bb.Reasoning.Enabled(),
		"artifacts":      len(artifacts),
		"artifact_kinds": kinds,
		"files_written":  fileCount,
		"category":       string(bb.Classification.Category),
		"blueprint":      bb.Blueprint.Key,
		"synthesized":    bb.Blueprint.Key == "synthesized",
		"verified":       verificationVerified(bb),
		"entities":       len(bb.Blueprint.Entities),
		"screens":        len(bb.Blueprint.Screens),
		"workspace":      project.WorkspacePath,
	}
	if err := run.Succeed(result, d.clock()); err != nil {
		return
	}
	if err := d.runs.Update(ctx, run); err != nil {
		d.log.Error("failed to persist run completion", "run_id", run.ID.String(), "error", err)
	}

	project.Status = domain.ProjectReady
	project.UpdatedAt = d.clock()
	if err := d.projects.Update(ctx, project); err != nil {
		d.log.Warn("failed to mark project ready", "error", err)
	}

	d.emit(ctx, run, domain.EventRunCompleted, domain.LevelInfo, domain.RoleSystem,
		fmt.Sprintf("Build complete: %d files, %d artifacts", fileCount, len(artifacts)), result)

	d.log.Info("run completed",
		"run_id", run.ID.String(), "project", project.Slug,
		"files", fileCount, "artifacts", len(artifacts),
		"duration", run.Duration(d.clock()).String())
}

func (d *Driver) emit(
	ctx context.Context,
	run *domain.Run,
	typ domain.EventType,
	level domain.Level,
	agent domain.AgentRole,
	message string,
	fields map[string]any,
) {
	e := domain.NewEvent(domain.RunTopic(run.ID), typ, level, message).
		For(run.ID, run.ProjectID).
		By(agent)
	for k, v := range fields {
		e.With(k, v)
	}
	// Event recording uses a detached context: a canceled build must still be
	// able to announce that it was canceled.
	recordCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		recordCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	if err := d.recorder.Record(recordCtx, e); err != nil {
		d.log.Warn("failed to record event", "type", string(typ), "error", err)
	}
}

func kindStrings(kinds []domain.ArtifactKind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// verificationVerified reports whether the QA agent proved the generated
// project runs. Surfacing this on the run result is what lets a client
// distinguish "we generated code" from "we generated code that works".
func verificationVerified(bb *Blackboard) bool {
	value, ok := bb.Value("verification")
	if !ok {
		return false
	}
	report, ok := value.(*VerificationReport)
	return ok && report.Verified()
}

// openRepository prepares version control for the generated workspace.
//
// Failure is not fatal: a machine without git still produces a working project,
// it just cannot offer rollback. Silently continuing without saying so would be
// worse, because a user would believe their work was protected.
func (d *Driver) openRepository(ctx context.Context, run *domain.Run, project *domain.Project) *vcs.Repository {
	if !d.vcs || project.WorkspacePath == "" {
		return nil
	}
	repo, err := vcs.Open(ctx, project.WorkspacePath)
	if err != nil {
		d.emit(ctx, run, domain.EventLog, domain.LevelWarn, domain.RoleSystem,
			"Version control is unavailable; this build will not be reversible: "+err.Error(), nil)
		return nil
	}
	return repo
}

// snapshot commits the workspace, reporting the result on the event stream.
func (d *Driver) snapshot(ctx context.Context, repo *vcs.Repository, run *domain.Run, message string) {
	if repo == nil {
		return
	}
	sha, err := repo.Snapshot(ctx, message, "Genesis")
	if err != nil {
		d.log.Warn("snapshot failed", "run_id", run.ID.String(), "error", err)
		return
	}
	if sha == "" {
		return // nothing changed
	}
	d.emit(ctx, run, domain.EventLog, domain.LevelDebug, domain.RoleSystem,
		"Snapshot "+sha[:8]+": "+message,
		map[string]any{"commit": sha, "message": message})
}

// commitPrefix maps a phase to a Conventional Commits type, so generated
// history is indistinguishable from human history in any tool that parses it.
func commitPrefix(phase domain.Phase) string {
	switch phase {
	case domain.PhaseAnalyze, domain.PhaseDesign, domain.PhasePlan:
		return "docs"
	case domain.PhaseBuild:
		return "feat"
	case domain.PhaseVerify:
		return "test"
	case domain.PhaseHeal:
		return "fix"
	case domain.PhaseShip:
		return "chore"
	}
	return "chore"
}

// runHealing attempts to repair a project that failed verification.
//
// Returns true when healing ran, whether or not it succeeded, so the caller can
// distinguish "nothing was wrong" from "we tried".
func (d *Driver) runHealing(
	ctx context.Context,
	run *domain.Run,
	project *domain.Project,
	phase *domain.RunPhase,
	bb *Blackboard,
	toolbelt *WorkspaceToolbelt,
) bool {
	value, ok := bb.Value("verification")
	if !ok {
		return false
	}
	verification, ok := value.(*VerificationReport)
	if !ok || verification.Verified() {
		return false
	}

	healer := NewHealer(d.runner, bb.Reasoning, project.Settings.MaxHealAttempts)
	if !healer.Available() {
		// Be explicit rather than silently skipping: an operator seeing a
		// failed build needs to know repair was possible but unavailable.
		d.emit(ctx, run, domain.EventLog, domain.LevelWarn, domain.RoleQA,
			"Verification failed and automatic repair is unavailable (no model or sandbox configured)", nil)
		return false
	}

	started := d.clock()
	phase.Status = domain.PhaseRunning
	phase.StartedAt = &started
	phase.UpdatedAt = started
	if err := d.runs.UpdatePhase(ctx, phase); err != nil {
		d.log.Warn("could not start the healing phase", "error", err)
	}

	d.emit(ctx, run, domain.EventPhaseStarted, domain.LevelInfo, domain.RoleQA,
		phase.Name.Title(), map[string]any{"phase": string(phase.Name)})

	report := healer.Heal(ctx, toolbelt.For(domain.RoleQA), bb, verification)
	bb.SetValue("healing", report)

	finished := d.clock()
	phase.FinishedAt = &finished
	phase.UpdatedAt = finished
	phase.Status = domain.PhaseSucceeded
	if !report.Healed {
		// A failed repair is not a failed phase: the phase did its job, which
		// was to try. The build's own status already records the failure.
		phase.Status = domain.PhaseFailed
	}
	phase.Summary = domain.Settings{
		"attempts": len(report.Attempts),
		"healed":   report.Healed,
		"outcome":  report.Summary(),
	}
	if err := d.runs.UpdatePhase(ctx, phase); err != nil {
		d.log.Warn("could not finalise the healing phase", "error", err)
	}

	level := domain.LevelWarn
	eventType := domain.EventPhaseFailed
	if report.Healed {
		level = domain.LevelInfo
		eventType = domain.EventPhaseCompleted
	}
	d.emit(ctx, run, eventType, level, domain.RoleQA,
		"Self healing: "+report.Summary(),
		map[string]any{
			"attempts": len(report.Attempts), "healed": report.Healed,
			"initial": report.Initial, "final": report.Final,
		})

	for _, attempt := range report.Attempts {
		d.emit(ctx, run, domain.EventHealAttempted, domain.LevelDebug, domain.RoleQA,
			fmt.Sprintf("Attempt %d: improved=%v reverted=%v %s",
				attempt.Number, attempt.Improved, attempt.Reverted, attempt.Error),
			map[string]any{
				"attempt": attempt.Number, "patched": attempt.Patched,
				"improved": attempt.Improved, "reverted": attempt.Reverted,
			})
	}
	return true
}
