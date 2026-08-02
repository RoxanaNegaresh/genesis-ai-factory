package sqlstore

import (
	"context"
	"database/sql"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// ArtifactRepo implements port.ArtifactRepository.
type ArtifactRepo struct{ s *Store }

// NewArtifactRepo constructs the repository.
func NewArtifactRepo(s *Store) *ArtifactRepo { return &ArtifactRepo{s: s} }

var _ port.ArtifactRepository = (*ArtifactRepo)(nil)

const artifactColumns = `id, run_id, task_id, project_id, kind, name, mime, size_bytes,
	sha256, storage, body, path, metadata, created_at`

// Create persists an artifact. Content addressing makes regeneration of an
// identical document a no-op rather than a duplicate row.
func (r *ArtifactRepo) Create(ctx context.Context, a *domain.Artifact) error {
	metadata, err := encodeJSON(a.Metadata)
	if err != nil {
		return err
	}
	_, err = r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		INSERT INTO artifacts (`+artifactColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`),
		a.ID.String(), a.RunID.String(), idPtrValue(a.TaskID), a.ProjectID.String(),
		string(a.Kind), a.Name, a.MIME, a.SizeBytes, a.SHA256, string(a.Storage),
		a.Body, a.Path, metadata, encodeTime(a.CreatedAt))
	if isUniqueViolation(err) {
		return domain.Conflict("artifact_exists", "an identical artifact already exists for this project")
	}
	return err
}

func (r *ArtifactRepo) ByID(ctx context.Context, id domain.ID) (*domain.Artifact, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT `+artifactColumns+` FROM artifacts WHERE id=$1`), id.String())
	return scanArtifact(row)
}

func (r *ArtifactRepo) ByRun(ctx context.Context, runID domain.ID) ([]*domain.Artifact, error) {
	rows, err := r.s.conn(ctx).QueryContext(ctx,
		r.s.rebind(`SELECT `+artifactColumns+` FROM artifacts WHERE run_id=$1 ORDER BY created_at, id`),
		runID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ByProjectKind returns the most recent artifact of a kind, which is how a
// downstream agent fetches its inputs ("give me the current PRD").
func (r *ArtifactRepo) ByProjectKind(ctx context.Context, projectID domain.ID, kind domain.ArtifactKind) (*domain.Artifact, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx, r.s.rebind(`
		SELECT `+artifactColumns+` FROM artifacts
		WHERE project_id=$1 AND kind=$2 ORDER BY created_at DESC, id DESC LIMIT 1`),
		projectID.String(), string(kind))
	return scanArtifact(row)
}

// ExistsBySHA supports deduplication before an insert.
func (r *ArtifactRepo) ExistsBySHA(ctx context.Context, projectID domain.ID, sha string) (*domain.Artifact, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT `+artifactColumns+` FROM artifacts WHERE project_id=$1 AND sha256=$2`),
		projectID.String(), sha)
	a, err := scanArtifact(row)
	if err != nil {
		if domainNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

func domainNotFound(err error) bool {
	var de *domain.Error
	if ok := asDomainError(err, &de); ok {
		return de.Kind == domain.ErrNotFound
	}
	return false
}

func asDomainError(err error, target **domain.Error) bool {
	for err != nil {
		if de, ok := err.(*domain.Error); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func scanArtifact(row rowScanner) (*domain.Artifact, error) {
	var (
		a         domain.Artifact
		id        string
		runID     string
		taskID    sql.NullString
		projectID string
		kind      string
		storage   string
		metadata  string
		createdAt string
	)
	err := row.Scan(&id, &runID, &taskID, &projectID, &kind, &a.Name, &a.MIME,
		&a.SizeBytes, &a.SHA256, &storage, &a.Body, &a.Path, &metadata, &createdAt)
	if err != nil {
		return nil, notFound(err, "artifact")
	}
	a.ID = domain.ID(id)
	a.RunID = domain.ID(runID)
	a.TaskID = idPtr(taskID)
	a.ProjectID = domain.ID(projectID)
	a.Kind = domain.ArtifactKind(kind)
	a.Storage = domain.Storage(storage)
	if a.Metadata, err = decodeSettings(metadata); err != nil {
		return nil, err
	}
	if a.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, err
	}
	return &a, nil
}

// TaskRepo implements port.TaskRepository.
type TaskRepo struct{ s *Store }

// NewTaskRepo constructs the repository.
func NewTaskRepo(s *Store) *TaskRepo { return &TaskRepo{s: s} }

var _ port.TaskRepository = (*TaskRepo)(nil)

const taskColumns = `id, run_id, phase_id, parent_id, agent_role, title, description,
	status, priority, depends_on, input, output, attempt_count, max_attempts,
	started_at, finished_at, created_at, updated_at`

// CreateBatch inserts an entire task DAG atomically. A half-written plan would
// leave the scheduler with dangling dependencies.
func (r *TaskRepo) CreateBatch(ctx context.Context, tasks []*domain.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	return r.s.WithTx(ctx, func(ctx context.Context) error {
		for _, t := range tasks {
			dependsOn, err := encodeJSON(t.DependsOn)
			if err != nil {
				return err
			}
			input, err := encodeJSON(t.Input)
			if err != nil {
				return err
			}
			output, err := encodeJSON(t.Output)
			if err != nil {
				return err
			}
			_, err = r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
				INSERT INTO tasks (`+taskColumns+`)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`),
				t.ID.String(), t.RunID.String(), t.PhaseID.String(), idPtrValue(t.ParentID),
				string(t.AgentRole), t.Title, t.Description, string(t.Status), t.Priority,
				dependsOn, input, output, t.AttemptCount, t.MaxAttempts,
				encodeTimePtr(t.StartedAt), encodeTimePtr(t.FinishedAt),
				encodeTime(t.CreatedAt), encodeTime(t.UpdatedAt))
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *TaskRepo) Update(ctx context.Context, t *domain.Task) error {
	dependsOn, err := encodeJSON(t.DependsOn)
	if err != nil {
		return err
	}
	input, err := encodeJSON(t.Input)
	if err != nil {
		return err
	}
	output, err := encodeJSON(t.Output)
	if err != nil {
		return err
	}
	res, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		UPDATE tasks SET status=$1, priority=$2, depends_on=$3, input=$4, output=$5,
			attempt_count=$6, started_at=$7, finished_at=$8, updated_at=$9
		WHERE id=$10`),
		string(t.Status), t.Priority, dependsOn, input, output, t.AttemptCount,
		encodeTimePtr(t.StartedAt), encodeTimePtr(t.FinishedAt), encodeTime(t.UpdatedAt),
		t.ID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NotFound("task")
	}
	return nil
}

func (r *TaskRepo) ByRun(ctx context.Context, runID domain.ID) ([]*domain.Task, error) {
	rows, err := r.s.conn(ctx).QueryContext(ctx, r.s.rebind(`
		SELECT `+taskColumns+` FROM tasks WHERE run_id=$1 ORDER BY priority DESC, created_at, id`),
		runID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *TaskRepo) ByID(ctx context.Context, id domain.ID) (*domain.Task, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT `+taskColumns+` FROM tasks WHERE id=$1`), id.String())
	return scanTask(row)
}

func scanTask(row rowScanner) (*domain.Task, error) {
	var (
		t          domain.Task
		id         string
		runID      string
		phaseID    string
		parentID   sql.NullString
		agentRole  string
		status     string
		dependsOn  string
		input      string
		output     string
		startedAt  sql.NullString
		finishedAt sql.NullString
		createdAt  string
		updatedAt  string
	)
	err := row.Scan(&id, &runID, &phaseID, &parentID, &agentRole, &t.Title, &t.Description,
		&status, &t.Priority, &dependsOn, &input, &output, &t.AttemptCount, &t.MaxAttempts,
		&startedAt, &finishedAt, &createdAt, &updatedAt)
	if err != nil {
		return nil, notFound(err, "task")
	}
	t.ID = domain.ID(id)
	t.RunID = domain.ID(runID)
	t.PhaseID = domain.ID(phaseID)
	t.ParentID = idPtr(parentID)
	t.AgentRole = domain.AgentRole(agentRole)
	t.Status = domain.TaskStatus(status)
	t.DependsOn = []domain.ID{}
	if err := decodeInto(dependsOn, &t.DependsOn); err != nil {
		return nil, err
	}
	if t.Input, err = decodeSettings(input); err != nil {
		return nil, err
	}
	if t.Output, err = decodeSettings(output); err != nil {
		return nil, err
	}
	if t.StartedAt, err = decodeTimePtr(startedAt); err != nil {
		return nil, err
	}
	if t.FinishedAt, err = decodeTimePtr(finishedAt); err != nil {
		return nil, err
	}
	if t.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, err
	}
	if t.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}
