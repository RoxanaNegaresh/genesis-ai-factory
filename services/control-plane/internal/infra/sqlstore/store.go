// Package sqlstore implements the persistence ports over database/sql. One
// implementation serves both SQLite and PostgreSQL: the dialect differences are
// confined to placeholder rewriting and a handful of upsert clauses, and both
// engines are exercised by the same conformance test suite so they cannot
// drift.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// Dialect selects engine-specific SQL.
type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// Store owns the database handle and produces repositories.
type Store struct {
	db      *sql.DB
	dialect Dialect
}

// New wraps an open database handle.
func New(db *sql.DB, dialect Dialect) *Store {
	return &Store{db: db, dialect: dialect}
}

// DB exposes the handle for migrations and health checks.
func (s *Store) DB() *sql.DB { return s.db }

// Dialect reports the active engine.
func (s *Store) Dialect() Dialect { return s.dialect }

// Ping verifies connectivity for the readiness probe.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// txKey carries an ambient transaction through the context so repositories can
// participate in a use case's unit of work without changing their signatures.
type txKey struct{}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on error or panic. Nested calls join the outer transaction rather than
// opening a second one (which would deadlock on SQLite).
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return fn(ctx)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// querier is the subset of database/sql shared by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// conn returns the ambient transaction if one is active, else the pool.
func (s *Store) conn(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return tx
	}
	return s.db
}

// rebind converts $N placeholders to the dialect's form. Queries are written in
// Postgres style and downgraded, never the reverse, because $N is unambiguous.
//
// SQLite maps each `?` to the next argument positionally, so a query that
// references the same $N twice is silently correct on Postgres and broken on
// SQLite. rebind detects that mistake and reports it rather than letting a
// dialect-specific bug reach production; every placeholder must be distinct and
// the sequence must be dense.
func (s *Store) rebind(query string) string {
	if s.dialect == Postgres {
		return query
	}
	var (
		sb   strings.Builder
		seen = map[int]bool{}
		next = 1
	)
	sb.Grow(len(query))
	for i := 0; i < len(query); i++ {
		if query[i] == '$' && i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9' {
			n := 0
			for i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9' {
				n = n*10 + int(query[i+1]-'0')
				i++
			}
			if seen[n] || n != next {
				panic(fmt.Sprintf(
					"sqlstore: placeholder $%d is repeated or out of order (expected $%d); "+
						"bind each argument exactly once so the query is portable to SQLite:\n%s",
					n, next, query))
			}
			seen[n] = true
			next++
			sb.WriteByte('?')
			continue
		}
		sb.WriteByte(query[i])
	}
	return sb.String()
}

// --- scalar codecs -------------------------------------------------------
//
// Timestamps are stored as RFC3339Nano UTC strings and JSON as text on both
// engines. This costs a little space and buys exact cross-engine equivalence,
// which is worth far more while two backends must stay in lockstep.

const tsLayout = time.RFC3339Nano

func encodeTime(t time.Time) string { return t.UTC().Format(tsLayout) }

func encodeTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return encodeTime(*t)
}

func decodeTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(tsLayout, s)
}

func decodeTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := decodeTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func encodeJSON(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode json: %w", err)
	}
	return string(b), nil
}

func decodeSettings(s string) (domain.Settings, error) {
	if strings.TrimSpace(s) == "" {
		return domain.Settings{}, nil
	}
	var out domain.Settings
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	if out == nil {
		out = domain.Settings{}
	}
	return out, nil
}

func decodeInto(s string, target any) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), target)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func idPtr(ns sql.NullString) *domain.ID {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	id := domain.ID(ns.String)
	return &id
}

func idPtrValue(p *domain.ID) any {
	if p == nil || p.IsZero() {
		return nil
	}
	return p.String()
}

// isUniqueViolation recognises a duplicate-key error on either engine without
// importing driver packages: SQLite reports "UNIQUE constraint failed" and
// Postgres reports SQLSTATE 23505.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "23505")
}

// notFound maps sql.ErrNoRows onto the domain sentinel.
func notFound(err error, resource string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NotFound(resource)
	}
	return err
}

func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}
