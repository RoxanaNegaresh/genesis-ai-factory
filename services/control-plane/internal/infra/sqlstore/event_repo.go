package sqlstore

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// EventRepo implements port.EventRepository over the append-only log.
type EventRepo struct {
	s *Store
	// appendMu serialises writes on SQLite. SQLite allows exactly one writer;
	// serialising in-process turns lock contention (SQLITE_BUSY retries) into a
	// cheap mutex wait and keeps sequence assignment strictly ordered.
	appendMu sync.Mutex
}

// NewEventRepo constructs the repository.
func NewEventRepo(s *Store) *EventRepo { return &EventRepo{s: s} }

var _ port.EventRepository = (*EventRepo)(nil)

// Append persists an event and back-fills the sequence number assigned by the
// database, which is the cursor clients resume from.
func (r *EventRepo) Append(ctx context.Context, e *domain.Event) error {
	payload, err := encodeJSON(e.Payload)
	if err != nil {
		return err
	}
	if e.Level == "" {
		e.Level = domain.LevelInfo
	}
	if e.ID.IsZero() {
		e.ID = domain.NewID()
	}

	args := []any{
		e.ID.String(), e.RunID.String(), e.ProjectID.String(), e.Topic, string(e.Type),
		string(e.AgentRole), string(e.Level), e.Message, payload, encodeTime(e.CreatedAt),
	}
	const insert = `INSERT INTO events (id, run_id, project_id, topic, type, agent_role,
		level, message, payload, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

	if r.s.dialect == Postgres {
		// RETURNING gives us the sequence without a second round trip.
		return r.s.conn(ctx).QueryRowContext(ctx, insert+" RETURNING seq", args...).Scan(&e.Seq)
	}

	r.appendMu.Lock()
	defer r.appendMu.Unlock()

	res, err := r.s.conn(ctx).ExecContext(ctx, r.s.rebind(insert), args...)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	e.Seq = seq
	return nil
}

// Query reads a page of the log after a cursor. Ordering is by seq, which is
// gapless and monotonic, so a reconnecting client resumes exactly.
func (r *EventRepo) Query(ctx context.Context, q port.EventQuery) ([]*domain.Event, error) {
	var (
		where []string
		args  []any
	)
	if !q.RunID.IsZero() {
		args = append(args, q.RunID.String())
		where = append(where, "run_id = $"+strconv.Itoa(len(args)))
	}
	if !q.ProjectID.IsZero() {
		args = append(args, q.ProjectID.String())
		where = append(where, "project_id = $"+strconv.Itoa(len(args)))
	}
	if q.AfterSeq > 0 {
		args = append(args, q.AfterSeq)
		where = append(where, "seq > $"+strconv.Itoa(len(args)))
	}
	if len(q.Types) > 0 {
		placeholders := make([]string, 0, len(q.Types))
		for _, t := range q.Types {
			args = append(args, string(t))
			placeholders = append(placeholders, "$"+strconv.Itoa(len(args)))
		}
		where = append(where, "type IN ("+strings.Join(placeholders, ",")+")")
	}

	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, clampLimit(q.Limit, 200, 1000))

	rows, err := r.s.conn(ctx).QueryContext(ctx, r.s.rebind(`
		SELECT seq, id, run_id, project_id, topic, type, agent_role, level, message, payload, created_at
		FROM events`+clause+` ORDER BY seq ASC LIMIT $`+strconv.Itoa(len(args))), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*domain.Event, 0, 64)
	for rows.Next() {
		var (
			e         domain.Event
			id        string
			runID     string
			projectID string
			typ       string
			agentRole string
			level     string
			payload   string
			createdAt string
		)
		if err := rows.Scan(&e.Seq, &id, &runID, &projectID, &e.Topic, &typ,
			&agentRole, &level, &e.Message, &payload, &createdAt); err != nil {
			return nil, err
		}
		e.ID = domain.ID(id)
		e.RunID = domain.ID(runID)
		e.ProjectID = domain.ID(projectID)
		e.Type = domain.EventType(typ)
		e.AgentRole = domain.AgentRole(agentRole)
		e.Level = domain.Level(level)
		if e.Payload, err = decodeSettings(payload); err != nil {
			return nil, err
		}
		if e.CreatedAt, err = decodeTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// LatestSeq reports the head of the log.
func (r *EventRepo) LatestSeq(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := r.s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&seq); err != nil {
		return 0, err
	}
	return seq.Int64, nil
}
