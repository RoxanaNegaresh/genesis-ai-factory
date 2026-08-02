// Package migrate applies embedded SQL migrations. The server migrates itself
// on boot: a desktop user must never be asked to install a migration CLI.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed sql/*.sql
var migrationFS embed.FS

// Migration is one versioned schema change.
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// Driver selects the dialect-specific rewrites.
type Driver string

const (
	SQLite   Driver = "sqlite"
	Postgres Driver = "postgres"
)

// autoincFor returns the driver-specific definition of the monotonic event
// sequence column. This is the only construct the two engines cannot share.
func autoincFor(d Driver) string {
	if d == Postgres {
		return "BIGSERIAL PRIMARY KEY"
	}
	return "INTEGER PRIMARY KEY AUTOINCREMENT"
}

// Load parses the embedded migration set.
func Load() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "sql")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	byVersion := map[int]*Migration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, direction, err := parseName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := migrationFS.ReadFile("sql/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		m, ok := byVersion[version]
		if !ok {
			m = &Migration{Version: version, Name: name}
			byVersion[version] = m
		}
		if direction == "up" {
			m.Up = string(body)
		} else {
			m.Down = string(body)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" {
			return nil, fmt.Errorf("migration %04d_%s has no up script", m.Version, m.Name)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseName splits "0001_core.up.sql" into (1, "core", "up").
func parseName(filename string) (int, string, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return 0, "", "", fmt.Errorf("migration %q must end in .up.sql or .down.sql", filename)
	}
	direction := base[dot+1:]
	if direction != "up" && direction != "down" {
		return 0, "", "", fmt.Errorf("migration %q has unknown direction %q", filename, direction)
	}
	rest := base[:dot]
	underscore := strings.Index(rest, "_")
	if underscore < 0 {
		return 0, "", "", fmt.Errorf("migration %q must be named NNNN_name.dir.sql", filename)
	}
	version, err := strconv.Atoi(rest[:underscore])
	if err != nil {
		return 0, "", "", fmt.Errorf("migration %q has a non-numeric version: %w", filename, err)
	}
	return version, rest[underscore+1:], direction, nil
}

// Runner applies migrations to a database.
type Runner struct {
	db     *sql.DB
	driver Driver
	log    *slog.Logger
}

// NewRunner constructs a migration runner.
func NewRunner(db *sql.DB, driver Driver, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{db: db, driver: driver, log: log}
}

// Up applies every pending migration inside its own transaction. A partially
// applied migration is impossible: either the whole file lands and the version
// is recorded, or nothing changes.
func (r *Runner) Up(ctx context.Context) (int, error) {
	migrations, err := Load()
	if err != nil {
		return 0, err
	}
	if err := r.ensureVersionTable(ctx); err != nil {
		return 0, err
	}
	applied, err := r.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, m := range migrations {
		if checksum, ok := applied[m.Version]; ok {
			// Detect an edited migration: silently diverging schemas across
			// machines is one of the worst failure modes in a team product.
			if want := checksumOf(m.Up); checksum != "" && checksum != want {
				return count, fmt.Errorf(
					"migration %04d_%s was modified after being applied (recorded %s, computed %s)",
					m.Version, m.Name, checksum[:12], want[:12])
			}
			continue
		}
		if err := r.apply(ctx, m); err != nil {
			return count, err
		}
		r.log.Info("migration applied", "version", m.Version, "name", m.Name)
		count++
	}
	return count, nil
}

// Down reverts the most recently applied migration.
func (r *Runner) Down(ctx context.Context) error {
	migrations, err := Load()
	if err != nil {
		return err
	}
	applied, err := r.appliedVersions(ctx)
	if err != nil {
		return err
	}
	for i := len(migrations) - 1; i >= 0; i-- {
		m := migrations[i]
		if _, ok := applied[m.Version]; !ok {
			continue
		}
		if strings.TrimSpace(m.Down) == "" {
			return fmt.Errorf("migration %04d_%s is irreversible", m.Version, m.Name)
		}
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := execScript(ctx, tx, r.rewrite(m.Down)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("revert %04d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.ExecContext(ctx, r.q("DELETE FROM schema_migrations WHERE version = $1"), m.Version); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return nil
}

// Version reports the highest applied migration version.
func (r *Runner) Version(ctx context.Context) (int, error) {
	if err := r.ensureVersionTable(ctx); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	if err := r.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func (r *Runner) apply(ctx context.Context, m Migration) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := execScript(ctx, tx, r.rewrite(m.Up)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply %04d_%s: %w", m.Version, m.Name, err)
	}
	_, err = tx.ExecContext(ctx,
		r.q("INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES ($1, $2, $3, $4)"),
		m.Version, m.Name, checksumOf(m.Up), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *Runner) ensureVersionTable(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			checksum   TEXT NOT NULL DEFAULT '',
			applied_at TEXT NOT NULL
		)`)
	return err
}

func (r *Runner) appliedVersions(ctx context.Context) (map[int]string, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int]string{}
	for rows.Next() {
		var v int
		var c string
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		out[v] = c
	}
	return out, rows.Err()
}

// rewrite substitutes dialect tokens.
func (r *Runner) rewrite(script string) string {
	return strings.ReplaceAll(script, "{{AUTOINC}}", autoincFor(r.driver))
}

// q converts $N placeholders to ? for SQLite.
func (r *Runner) q(query string) string {
	if r.driver == Postgres {
		return query
	}
	var sb strings.Builder
	for i := 0; i < len(query); i++ {
		if query[i] == '$' && i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9' {
			sb.WriteByte('?')
			for i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9' {
				i++
			}
			continue
		}
		sb.WriteByte(query[i])
	}
	return sb.String()
}

// execScript runs a multi-statement script. SQLite's driver accepts multiple
// statements in one Exec, but splitting keeps error messages pointing at the
// offending statement rather than the whole file.
func execScript(ctx context.Context, tx *sql.Tx, script string) error {
	for _, stmt := range splitStatements(script) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w\nstatement: %s", err, truncate(stmt, 200))
		}
	}
	return nil
}

// splitStatements splits on semicolons that terminate a statement, ignoring
// those inside string literals or comments.
func splitStatements(script string) []string {
	var (
		out     []string
		current strings.Builder
		inStr   bool
		inLine  bool
	)
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				current.WriteByte(c)
			}
			continue
		case inStr:
			current.WriteByte(c)
			if c == '\'' {
				if i+1 < len(script) && script[i+1] == '\'' {
					current.WriteByte(script[i+1])
					i++
					continue
				}
				inStr = false
			}
			continue
		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			inLine = true
			i++
			continue
		case c == '\'':
			inStr = true
			current.WriteByte(c)
			continue
		case c == ';':
			if s := strings.TrimSpace(current.String()); s != "" {
				out = append(out, s)
			}
			current.Reset()
			continue
		}
		current.WriteByte(c)
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func checksumOf(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
