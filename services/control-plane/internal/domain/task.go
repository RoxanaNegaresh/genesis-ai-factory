package domain

import "time"

// TaskStatus is the state of a unit of agent work.
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending" // created, dependencies unmet
	TaskReady     TaskStatus = "ready"   // dependencies satisfied, awaiting a worker
	TaskRunning   TaskStatus = "running" // claimed by an agent
	TaskBlocked   TaskStatus = "blocked" // waiting on human approval
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskSkipped   TaskStatus = "skipped"
)

func (s TaskStatus) Terminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskSkipped:
		return true
	}
	return false
}

// Task is a node in the run's work DAG, assigned to exactly one agent role.
type Task struct {
	ID           ID
	RunID        ID
	PhaseID      ID
	ParentID     *ID
	AgentRole    AgentRole
	Title        string
	Description  string
	Status       TaskStatus
	Priority     int
	DependsOn    []ID
	Input        Settings
	Output       Settings
	AttemptCount int
	MaxAttempts  int
	StartedAt    *time.Time
	FinishedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewTask constructs a pending task.
func NewTask(runID, phaseID ID, role AgentRole, title, description string, priority int, now time.Time) *Task {
	return &Task{
		ID:          NewID(),
		RunID:       runID,
		PhaseID:     phaseID,
		AgentRole:   role,
		Title:       title,
		Description: description,
		Status:      TaskPending,
		Priority:    priority,
		DependsOn:   []ID{},
		Input:       Settings{},
		Output:      Settings{},
		MaxAttempts: 3,
		CreatedAt:   now.UTC(),
		UpdatedAt:   now.UTC(),
	}
}

// DependsOnTasks records DAG edges.
func (t *Task) DependsOnTasks(ids ...ID) *Task {
	t.DependsOn = append(t.DependsOn, ids...)
	return t
}

// CanRun reports whether every dependency has succeeded.
func (t *Task) CanRun(done map[ID]TaskStatus) bool {
	if t.Status != TaskPending && t.Status != TaskReady {
		return false
	}
	for _, dep := range t.DependsOn {
		if done[dep] != TaskSucceeded {
			return false
		}
	}
	return true
}

// Claim marks the task as running by an agent.
func (t *Task) Claim(now time.Time) error {
	if t.Status.Terminal() {
		return Conflict("task_terminal", "task has already finished")
	}
	if t.Status == TaskRunning {
		return Conflict("task_running", "task is already claimed")
	}
	if t.AttemptCount >= t.MaxAttempts {
		return Conflict("task_attempts_exhausted", "task has no attempts left")
	}
	ts := now.UTC()
	t.Status = TaskRunning
	t.AttemptCount++
	if t.StartedAt == nil {
		t.StartedAt = &ts
	}
	t.UpdatedAt = ts
	return nil
}

// Succeed completes the task with its output artifact reference.
func (t *Task) Succeed(output Settings, now time.Time) {
	ts := now.UTC()
	t.Status = TaskSucceeded
	if output != nil {
		t.Output = output
	}
	t.FinishedAt = &ts
	t.UpdatedAt = ts
}

// Fail records a failed attempt. The task returns to pending while attempts
// remain, which is what allows the healing loop to retry without bookkeeping in
// the caller.
func (t *Task) Fail(reason string, now time.Time) {
	ts := now.UTC()
	t.Output["error"] = reason
	t.UpdatedAt = ts
	if t.AttemptCount >= t.MaxAttempts {
		t.Status = TaskFailed
		t.FinishedAt = &ts
		return
	}
	t.Status = TaskPending
}

// TaskAttempt is the forensic record of one LLM interaction for a task.
type TaskAttempt struct {
	ID               ID
	TaskID           ID
	AttemptNo        int
	Model            string
	PromptTokens     int
	CompletionTokens int
	LatencyMS        int64
	Status           string
	Error            Settings
	RawOutputRef     *ID
	CreatedAt        time.Time
}
