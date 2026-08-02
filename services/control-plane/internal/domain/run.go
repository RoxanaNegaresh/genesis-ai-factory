package domain

import "time"

// Phase is a stage of the autonomous development loop. Ordinal order is the
// execution order.
type Phase string

const (
	PhaseAnalyze Phase = "analyze"
	PhaseDesign  Phase = "design"
	PhasePlan    Phase = "plan"
	PhaseBuild   Phase = "build"
	PhaseVerify  Phase = "verify"
	PhaseHeal    Phase = "heal"
	PhaseShip    Phase = "ship"
)

// PhaseOrder is the canonical sequence of the development loop.
var PhaseOrder = []Phase{PhaseAnalyze, PhaseDesign, PhasePlan, PhaseBuild, PhaseVerify, PhaseHeal, PhaseShip}

// PhaseTitle renders a phase for humans.
func (p Phase) Title() string {
	switch p {
	case PhaseAnalyze:
		return "Product Analysis"
	case PhaseDesign:
		return "Design & Architecture"
	case PhasePlan:
		return "Task Planning"
	case PhaseBuild:
		return "Code Generation"
	case PhaseVerify:
		return "Testing & Review"
	case PhaseHeal:
		return "Self Healing"
	case PhaseShip:
		return "Packaging & Deployment"
	}
	return string(p)
}

func (p Phase) Valid() bool {
	for _, k := range PhaseOrder {
		if k == p {
			return true
		}
	}
	return false
}

// Ordinal is the zero-based position of the phase in the loop.
func (p Phase) Ordinal() int {
	for i, k := range PhaseOrder {
		if k == p {
			return i
		}
	}
	return -1
}

// RunStatus is the lifecycle state of a build.
type RunStatus string

const (
	RunPending     RunStatus = "pending"
	RunRunning     RunStatus = "running"
	RunSucceeded   RunStatus = "succeeded"
	RunFailed      RunStatus = "failed"
	RunCanceled    RunStatus = "canceled"
	RunInterrupted RunStatus = "interrupted"
)

// Terminal reports whether no further transitions are possible.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunSucceeded, RunFailed, RunCanceled:
		return true
	}
	return false
}

// Active reports whether the scheduler should consider this run.
func (s RunStatus) Active() bool {
	return s == RunPending || s == RunRunning
}

func (s RunStatus) Valid() bool {
	switch s {
	case RunPending, RunRunning, RunSucceeded, RunFailed, RunCanceled, RunInterrupted:
		return true
	}
	return false
}

// RunKind distinguishes the intent of a run.
type RunKind string

const (
	RunBuild   RunKind = "build"
	RunImprove RunKind = "improve"
	RunFix     RunKind = "fix"
	RunAnalyze RunKind = "analyze"
)

func (k RunKind) Valid() bool {
	switch k {
	case RunBuild, RunImprove, RunFix, RunAnalyze:
		return true
	}
	return false
}

// RunError is the typed failure attached to a terminal run.
type RunError struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Phase   Phase     `json:"phase,omitempty"`
	Agent   AgentRole `json:"agent,omitempty"`
}

// Run is one durable execution of the development loop against a project.
type Run struct {
	ID                ID
	ProjectID         ID
	TriggeredBy       ID
	Kind              RunKind
	Status            RunStatus
	CurrentPhase      Phase
	Input             Settings
	Result            Settings
	Error             *RunError
	TokenBudget       int64
	TokensUsed        int64
	StartedAt         *time.Time
	FinishedAt        *time.Time
	CancelRequestedAt *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time

	// Phases is populated by the repository when loading a run detail. It is
	// not persisted on the run row itself.
	Phases []RunPhase
}

// Clone returns a deep copy of the run.
//
// Handing the same aggregate to a background worker and to a response
// serialiser is a data race: the worker mutates state while the handler reads
// it. Ownership transfer across a goroutine boundary must copy, and doing it
// here means no caller can forget.
func (r *Run) Clone() *Run {
	if r == nil {
		return nil
	}
	clone := *r

	clone.Input = cloneSettings(r.Input)
	clone.Result = cloneSettings(r.Result)

	if r.Error != nil {
		e := *r.Error
		clone.Error = &e
	}
	clone.StartedAt = cloneTime(r.StartedAt)
	clone.FinishedAt = cloneTime(r.FinishedAt)
	clone.CancelRequestedAt = cloneTime(r.CancelRequestedAt)

	if r.Phases != nil {
		clone.Phases = make([]RunPhase, len(r.Phases))
		for i, p := range r.Phases {
			p.Summary = cloneSettings(p.Summary)
			p.StartedAt = cloneTime(p.StartedAt)
			p.FinishedAt = cloneTime(p.FinishedAt)
			clone.Phases[i] = p
		}
	}
	return &clone
}

func cloneSettings(s Settings) Settings {
	if s == nil {
		return nil
	}
	out := make(Settings, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// NewRun constructs a pending run. Input is a snapshot of everything the run
// depends on, which is what makes a run reproducible after the project has
// since been edited.
func NewRun(projectID, triggeredBy ID, kind RunKind, input Settings, tokenBudget int64, now time.Time) (*Run, error) {
	if projectID.IsZero() {
		return nil, Invalid("project_required", "project is required")
	}
	if !kind.Valid() {
		return nil, Invalid("run_kind_invalid", "unknown run kind")
	}
	if input == nil {
		input = Settings{}
	}
	if tokenBudget <= 0 {
		tokenBudget = 2_000_000
	}
	return &Run{
		ID:           NewID(),
		ProjectID:    projectID,
		TriggeredBy:  triggeredBy,
		Kind:         kind,
		Status:       RunPending,
		CurrentPhase: PhaseAnalyze,
		Input:        input,
		Result:       Settings{},
		TokenBudget:  tokenBudget,
		CreatedAt:    now.UTC(),
		UpdatedAt:    now.UTC(),
	}, nil
}

// Start transitions pending -> running.
func (r *Run) Start(now time.Time) error {
	if r.Status != RunPending {
		return Conflict("run_not_pending", "run has already started")
	}
	t := now.UTC()
	r.Status = RunRunning
	r.StartedAt = &t
	r.UpdatedAt = t
	return nil
}

// Succeed transitions a running run to its successful terminal state.
func (r *Run) Succeed(result Settings, now time.Time) error {
	if r.Status.Terminal() {
		return Conflict("run_terminal", "run has already finished")
	}
	t := now.UTC()
	r.Status = RunSucceeded
	r.CurrentPhase = PhaseShip
	if result != nil {
		r.Result = result
	}
	r.FinishedAt = &t
	r.UpdatedAt = t
	return nil
}

// Fail transitions a run to the failed terminal state with a typed cause.
func (r *Run) Fail(cause RunError, now time.Time) error {
	if r.Status.Terminal() {
		return Conflict("run_terminal", "run has already finished")
	}
	t := now.UTC()
	r.Status = RunFailed
	r.Error = &cause
	r.FinishedAt = &t
	r.UpdatedAt = t
	return nil
}

// RequestCancel marks a cooperative cancellation. The driver observes this and
// unwinds; it does not kill anything mid-transaction.
func (r *Run) RequestCancel(now time.Time) error {
	if r.Status.Terminal() {
		return Conflict("run_terminal", "run has already finished")
	}
	if r.CancelRequestedAt != nil {
		return nil // idempotent
	}
	t := now.UTC()
	r.CancelRequestedAt = &t
	r.UpdatedAt = t
	return nil
}

// Cancel completes a cancellation.
func (r *Run) Cancel(now time.Time) error {
	if r.Status.Terminal() {
		return Conflict("run_terminal", "run has already finished")
	}
	t := now.UTC()
	r.Status = RunCanceled
	r.FinishedAt = &t
	r.UpdatedAt = t
	return nil
}

// CancelRequested reports whether a cancellation is pending.
func (r *Run) CancelRequested() bool { return r.CancelRequestedAt != nil }

// Duration returns how long the run took, or how long it has been going.
func (r *Run) Duration(now time.Time) time.Duration {
	if r.StartedAt == nil {
		return 0
	}
	end := now
	if r.FinishedAt != nil {
		end = *r.FinishedAt
	}
	return end.Sub(*r.StartedAt)
}

// PhaseStatus is the state of a single phase within a run.
type PhaseStatus string

const (
	PhasePending   PhaseStatus = "pending"
	PhaseRunning   PhaseStatus = "running"
	PhaseSucceeded PhaseStatus = "succeeded"
	PhaseFailed    PhaseStatus = "failed"
	PhaseSkipped   PhaseStatus = "skipped"
)

// RunPhase is a persisted phase record.
type RunPhase struct {
	ID         ID
	RunID      ID
	Name       Phase
	Ordinal    int
	Status     PhaseStatus
	Summary    Settings
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewRunPhases materialises the canonical phase list for a run so the UI can
// render the full pipeline (including not-yet-started stages) immediately.
func NewRunPhases(runID ID, now time.Time) []RunPhase {
	out := make([]RunPhase, 0, len(PhaseOrder))
	for i, p := range PhaseOrder {
		out = append(out, RunPhase{
			ID:        NewID(),
			RunID:     runID,
			Name:      p,
			Ordinal:   i,
			Status:    PhasePending,
			Summary:   Settings{},
			CreatedAt: now.UTC(),
			UpdatedAt: now.UTC(),
		})
	}
	return out
}
