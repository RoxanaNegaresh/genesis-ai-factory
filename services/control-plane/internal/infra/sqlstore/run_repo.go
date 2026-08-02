package sqlstore

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// RunRepo implements port.RunRepository.
type RunRepo struct{ s *Store }

// NewRunRepo constructs the repository.
func NewRunRepo(s *Store) *RunRepo { return &RunRepo{s: s} }

var _ port.RunRepository = (*RunRepo)(nil)

const runColumns = `id, project_id, triggered_by, kind, status, current_phase, input, result,
	error, token_budget, tokens_used, started_at, finished_at, cancel_requested_at,
	created_at, updated_at`

// Create persists a run together with its full phase list in one transaction:
// a run whose pipeline is invisible to the UI would be worse than no run.
func (r *RunRepo) Create(ctx context.Context, run *domain.Run, phases []domain.RunPhase) error {
	return r.s.WithTx(ctx, func(ctx context.Context) error {
		input, err := encodeJSON(run.Input)
		if err != nil {
			return err
		}
		result, err := encodeJSON(run.Result)
		if err != nil {
			return err
		}
		var errJSON any
		if run.Error != nil {
			s, err := encodeJSON(run.Error)
			if err != nil {
				return err
			}
			errJSON = s
		}
		_, err = r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
			INSERT INTO runs (`+runColumns+`)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`),
			run.ID.String(), run.ProjectID.String(), run.TriggeredBy.String(), string(run.Kind),
			string(run.Status), string(run.CurrentPhase), input, result, errJSON,
			run.TokenBudget, run.TokensUsed, encodeTimePtr(run.StartedAt),
			encodeTimePtr(run.FinishedAt), encodeTimePtr(run.CancelRequestedAt),
			encodeTime(run.CreatedAt), encodeTime(run.UpdatedAt))
		if err != nil {
			return err
		}
		for i := range phases {
			if err := r.insertPhase(ctx, &phases[i]); err != nil {
				return err
			}
		}
		run.Phases = phases
		return nil
	})
}

func (r *RunRepo) insertPhase(ctx context.Context, p *domain.RunPhase) error {
	summary, err := encodeJSON(p.Summary)
	if err != nil {
		return err
	}
	_, err = r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		INSERT INTO run_phases (id, run_id, name, ordinal, status, summary,
			started_at, finished_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`),
		p.ID.String(), p.RunID.String(), string(p.Name), p.Ordinal, string(p.Status), summary,
		encodeTimePtr(p.StartedAt), encodeTimePtr(p.FinishedAt),
		encodeTime(p.CreatedAt), encodeTime(p.UpdatedAt))
	return err
}

func (r *RunRepo) Update(ctx context.Context, run *domain.Run) error {
	input, err := encodeJSON(run.Input)
	if err != nil {
		return err
	}
	result, err := encodeJSON(run.Result)
	if err != nil {
		return err
	}
	var errJSON any
	if run.Error != nil {
		s, err := encodeJSON(run.Error)
		if err != nil {
			return err
		}
		errJSON = s
	}
	res, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		UPDATE runs SET status=$1, current_phase=$2, input=$3, result=$4, error=$5,
			token_budget=$6, tokens_used=$7, started_at=$8, finished_at=$9,
			cancel_requested_at=$10, updated_at=$11
		WHERE id=$12`),
		string(run.Status), string(run.CurrentPhase), input, result, errJSON,
		run.TokenBudget, run.TokensUsed, encodeTimePtr(run.StartedAt),
		encodeTimePtr(run.FinishedAt), encodeTimePtr(run.CancelRequestedAt),
		encodeTime(run.UpdatedAt), run.ID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NotFound("run")
	}
	return nil
}

func (r *RunRepo) ByID(ctx context.Context, id domain.ID) (*domain.Run, error) {
	row := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT `+runColumns+` FROM runs WHERE id=$1`), id.String())
	run, err := scanRun(row)
	if err != nil {
		return nil, err
	}
	if run.Phases, err = r.Phases(ctx, run.ID); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *RunRepo) List(ctx context.Context, f port.RunFilter) ([]*domain.Run, int64, error) {
	var (
		where []string
		args  []any
	)
	if !f.ProjectID.IsZero() {
		args = append(args, f.ProjectID.String())
		where = append(where, "project_id = $"+strconv.Itoa(len(args)))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		where = append(where, "status = $"+strconv.Itoa(len(args)))
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := r.s.conn(ctx).QueryRowContext(ctx,
		r.s.rebind(`SELECT COUNT(*) FROM runs`+clause), args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := clampLimit(f.Limit, 50, 200)
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	query := `SELECT ` + runColumns + ` FROM runs` + clause +
		` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args)-1) +
		` OFFSET $` + strconv.Itoa(len(args))

	rows, err := r.s.conn(ctx).QueryContext(ctx, r.s.rebind(query), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*domain.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, run)
	}
	return out, total, rows.Err()
}

func (r *RunRepo) Phases(ctx context.Context, runID domain.ID) ([]domain.RunPhase, error) {
	rows, err := r.s.conn(ctx).QueryContext(ctx, r.s.rebind(`
		SELECT id, run_id, name, ordinal, status, summary, started_at, finished_at, created_at, updated_at
		FROM run_phases WHERE run_id=$1 ORDER BY ordinal`), runID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.RunPhase
	for rows.Next() {
		var (
			p          domain.RunPhase
			id         string
			rid        string
			name       string
			status     string
			summary    string
			startedAt  sql.NullString
			finishedAt sql.NullString
			createdAt  string
			updatedAt  string
		)
		if err := rows.Scan(&id, &rid, &name, &p.Ordinal, &status, &summary,
			&startedAt, &finishedAt, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		p.ID = domain.ID(id)
		p.RunID = domain.ID(rid)
		p.Name = domain.Phase(name)
		p.Status = domain.PhaseStatus(status)
		if p.Summary, err = decodeSettings(summary); err != nil {
			return nil, err
		}
		if p.StartedAt, err = decodeTimePtr(startedAt); err != nil {
			return nil, err
		}
		if p.FinishedAt, err = decodeTimePtr(finishedAt); err != nil {
			return nil, err
		}
		if p.CreatedAt, err = decodeTime(createdAt); err != nil {
			return nil, err
		}
		if p.UpdatedAt, err = decodeTime(updatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *RunRepo) UpdatePhase(ctx context.Context, p *domain.RunPhase) error {
	summary, err := encodeJSON(p.Summary)
	if err != nil {
		return err
	}
	res, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(`
		UPDATE run_phases SET status=$1, summary=$2, started_at=$3, finished_at=$4, updated_at=$5
		WHERE id=$6`),
		string(p.Status), summary, encodeTimePtr(p.StartedAt), encodeTimePtr(p.FinishedAt),
		encodeTime(p.UpdatedAt), p.ID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NotFound("run_phase")
	}
	return nil
}

// ActiveRuns powers crash recovery: on boot the server marks anything still
// flagged running as interrupted so the UI never shows a phantom build.
func (r *RunRepo) ActiveRuns(ctx context.Context) ([]*domain.Run, error) {
	rows, err := r.s.conn(ctx).QueryContext(ctx, r.s.rebind(`
		SELECT `+runColumns+` FROM runs WHERE status IN ($1,$2) ORDER BY created_at`),
		string(domain.RunPending), string(domain.RunRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func scanRun(row rowScanner) (*domain.Run, error) {
	var (
		run          domain.Run
		id           string
		projectID    string
		triggeredBy  string
		kind         string
		status       string
		currentPhase string
		input        string
		result       string
		errJSON      sql.NullString
		startedAt    sql.NullString
		finishedAt   sql.NullString
		cancelAt     sql.NullString
		createdAt    string
		updatedAt    string
	)
	err := row.Scan(&id, &projectID, &triggeredBy, &kind, &status, &currentPhase,
		&input, &result, &errJSON, &run.TokenBudget, &run.TokensUsed,
		&startedAt, &finishedAt, &cancelAt, &createdAt, &updatedAt)
	if err != nil {
		return nil, notFound(err, "run")
	}
	run.ID = domain.ID(id)
	run.ProjectID = domain.ID(projectID)
	run.TriggeredBy = domain.ID(triggeredBy)
	run.Kind = domain.RunKind(kind)
	run.Status = domain.RunStatus(status)
	run.CurrentPhase = domain.Phase(currentPhase)
	if run.Input, err = decodeSettings(input); err != nil {
		return nil, err
	}
	if run.Result, err = decodeSettings(result); err != nil {
		return nil, err
	}
	if errJSON.Valid && errJSON.String != "" {
		var re domain.RunError
		if err := decodeInto(errJSON.String, &re); err != nil {
			return nil, err
		}
		run.Error = &re
	}
	if run.StartedAt, err = decodeTimePtr(startedAt); err != nil {
		return nil, err
	}
	if run.FinishedAt, err = decodeTimePtr(finishedAt); err != nil {
		return nil, err
	}
	if run.CancelRequestedAt, err = decodeTimePtr(cancelAt); err != nil {
		return nil, err
	}
	if run.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, err
	}
	if run.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return nil, err
	}
	return &run, nil
}
