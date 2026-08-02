package factory

import (
	"fmt"
	"strings"
)

// This file closes the largest gap the Improver agent has reported since v0.1:
// generated projects declared repository interfaces in the inner layer but
// shipped nothing that implemented them, so a generated product compiled, ran
// and answered /health while being incapable of storing a single row.
//
// The implementations here are written against pgx v5 directly rather than an
// ORM. A generated repository is code the user owns and will edit; a hand
// readable SQL statement is inspectable and tunable, whereas a struct-tag DSL
// hides the query that actually runs. Every statement is parameterised — the
// generator never interpolates a value into SQL text, only identifiers it
// itself produced from the blueprint.

// writableFields returns the columns a caller supplies. Identity and audit
// columns are owned by the database: id is defaulted by gen_random_uuid(),
// created_at and updated_at by now(). Letting the application invent them
// invites clock skew between application servers.
func writableFields(e Entity) []Field {
	out := make([]Field, 0, len(e.Fields))
	for _, f := range e.Fields {
		if isSystemField(f.Name) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// searchableFields returns the text columns a free-text query filters on.
func searchableFields(e Entity) []Field {
	out := make([]Field, 0, 4)
	for _, f := range e.Fields {
		if f.Type == "text" && !isSystemField(f.Name) {
			out = append(out, f)
		}
	}
	return out
}

// readExpr renders how a column is read.
//
// Two columns are projected through ::text rather than read natively.
//
// NUMERIC, because the domain models decimals as strings: scanning a NUMERIC
// into a Go float would reintroduce exactly the rounding error the schema chose
// NUMERIC to avoid.
//
// UUID, because the domain models identifiers as strings. Casting in the
// projection means the scan is a plain string read that does not depend on
// which UUID representations the driver happens to support.
func readExpr(f Field) string {
	switch f.Type {
	case "decimal":
		return f.Name + "::text"
	case "uuid", "ref":
		return f.Name + "::text"
	}
	return f.Name
}

// writePlaceholder renders how a column is written. The inverse of readExpr:
// the driver sends a Go string and the cast restores the column's real type.
// Without the cast PostgreSQL would have to infer the parameter type, and it
// infers text, which does not implicitly coerce to uuid or numeric.
func writePlaceholder(f Field, n int) string {
	switch f.Type {
	case "decimal":
		return fmt.Sprintf("$%d::numeric", n)
	case "uuid", "ref":
		return fmt.Sprintf("$%d::uuid", n)
	}
	return fmt.Sprintf("$%d", n)
}

// scanTargets renders the address-of expressions for a full row read.
func scanTargets(e Entity) string {
	parts := make([]string, 0, len(e.Fields)+1)
	for _, f := range e.Fields {
		parts = append(parts, "&m."+goFieldName(f.Name))
	}
	parts = append(parts, "&m.DeletedAt")
	return strings.Join(parts, ", ")
}

// readColumns renders the projection for a full row read, in struct order so
// the scan targets line up positionally.
func readColumns(e Entity) string {
	parts := make([]string, 0, len(e.Fields)+1)
	for _, f := range e.Fields {
		parts = append(parts, readExpr(f))
	}
	parts = append(parts, "deleted_at")
	return strings.Join(parts, ", ")
}

// backendPostgresPool emits the connection pool and its lifecycle.
func backendPostgresPool() string {
	return `// Package postgres implements the repository interfaces declared in
// internal/port against PostgreSQL using pgx.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the connection pool shared by every repository. It is aliased so the
// composition root depends on this package rather than on pgx directly.
type DB = pgxpool.Pool

// NewPool builds a connection pool from a URL.
//
// The pool is created without contacting the server. That is deliberate: a
// process that refuses to start because the database is briefly unreachable
// turns a recoverable blip into a crash loop, and an orchestrator cannot tell
// the difference between "starting" and "broken". The process starts, serves
// its liveness endpoint, and reports the database separately through Ping.
func NewPool(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	// Bounded so a traffic spike cannot exhaust the server's connection slots
	// and lock out every other client, including the operator.
	cfg.MaxConns = 16
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// Ping reports whether the database is currently reachable. Readiness probes
// call this; liveness probes must not, or a database outage will cause the
// orchestrator to kill healthy application processes.
func Ping(ctx context.Context, db *DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.Ping(ctx)
}
`
}

// backendPostgresCursor emits keyset pagination helpers.
func backendPostgresCursor() string {
	return `package postgres

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Cursors encode the sort key of the last row of a page.
//
// Keyset pagination is used rather than OFFSET because OFFSET makes the
// database count and discard every skipped row, so page 500 costs 500 pages of
// work, and a row inserted while the client pages causes a row to be shown
// twice or skipped entirely. The key is (created_at, id): created_at gives the
// ordering the API promises and id breaks ties so the order is total.

func encodeCursor(ts time.Time, id string) string {
	raw := ts.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor is not valid base64")
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("cursor is malformed")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor timestamp is malformed")
	}
	if parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("cursor id is empty")
	}
	return ts, parts[1], nil
}
`
}

// backendPostgresRepo emits a repository implementation for one entity.
func backendPostgresRepo(module string, e Entity) string {
	table := tableName(e)
	writable := writableFields(e)
	// Two spellings of the resource name, for two different audiences.
	//
	// "prose" goes into comments and human-readable messages, where "seller
	// profile" reads correctly. "code" goes into error codes, which clients
	// match on and must therefore be a single stable token — a space in an
	// error code produced "seller profile_identifier_invalid", which is not
	// something a caller can switch on.
	prose := strings.ToLower(humanize(e.Name))
	code := toSnake(e.Name)

	var sb strings.Builder

	fmt.Fprintf(&sb, `package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	%q
	%q
)

// %sRepo implements port.%sRepository against PostgreSQL.
type %sRepo struct {
	db *pgxpool.Pool
}

// New%sRepo constructs the repository.
func New%sRepo(db *pgxpool.Pool) *%sRepo { return &%sRepo{db: db} }

// q returns the transaction on the context when there is one, and the pool
// otherwise. This is what makes a repository participate in a UnitOfWork
// without being told: the same method works inside and outside a transaction.
func (r *%sRepo) q(ctx context.Context) Querier {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	return r.db
}

`,
		module+"/internal/domain",
		module+"/internal/port",
		e.Name, e.Name,
		e.Name,
		e.Name, e.Name, e.Name, e.Name, e.Name)

	// --- Create ---------------------------------------------------------
	insertCols := make([]string, 0, len(writable))
	insertVals := make([]string, 0, len(writable))
	insertArgs := make([]string, 0, len(writable))
	for i, f := range writable {
		insertCols = append(insertCols, f.Name)
		insertVals = append(insertVals, writePlaceholder(f, i+1))
		insertArgs = append(insertArgs, "m."+goFieldName(f.Name))
	}

	fmt.Fprintf(&sb, "// Create inserts a %s and reads back the values the database assigned.\nfunc (r *%sRepo) Create(ctx context.Context, m *domain.%s) error {\n",
		prose, e.Name, e.Name)
	if len(insertCols) == 0 {
		fmt.Fprintf(&sb, "\tconst q = `INSERT INTO %s DEFAULT VALUES RETURNING id::text, created_at, updated_at`\n", table)
		fmt.Fprintf(&sb, "\terr := r.q(ctx).QueryRow(ctx, q).Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)\n")
	} else {
		fmt.Fprintf(&sb, "\tconst q = `INSERT INTO %s (%s)\n\t\tVALUES (%s)\n\t\tRETURNING id::text, created_at, updated_at`\n",
			table, strings.Join(insertCols, ", "), strings.Join(insertVals, ", "))
		fmt.Fprintf(&sb, "\terr := r.q(ctx).QueryRow(ctx, q, %s).Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)\n",
			strings.Join(insertArgs, ", "))
	}
	fmt.Fprintf(&sb, `	if err != nil {
		return wrapWriteError(%q, err)
	}
	return nil
}

`, code)

	// --- Update ---------------------------------------------------------
	setClauses := make([]string, 0, len(writable))
	updateArgs := make([]string, 0, len(writable)+1)
	for i, f := range writable {
		setClauses = append(setClauses, f.Name+" = "+writePlaceholder(f, i+1))
		updateArgs = append(updateArgs, "m."+goFieldName(f.Name))
	}
	setClauses = append(setClauses, "updated_at = now()")
	updateArgs = append(updateArgs, "m.ID")

	fmt.Fprintf(&sb, "// Update writes a %s. A soft-deleted row is not updatable: it reports\n// not-found so an archived record cannot be silently resurrected.\nfunc (r *%sRepo) Update(ctx context.Context, m *domain.%s) error {\n",
		prose, e.Name, e.Name)
	fmt.Fprintf(&sb, "\tconst q = `UPDATE %s SET %s\n\t\tWHERE id = $%d::uuid AND deleted_at IS NULL\n\t\tRETURNING updated_at`\n",
		table, strings.Join(setClauses, ", "), len(writable)+1)
	fmt.Fprintf(&sb, `	err := r.q(ctx).QueryRow(ctx, q, %s).Scan(&m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound(%q)
	}
	if err != nil {
		return wrapWriteError(%q, err)
	}
	return nil
}

`, strings.Join(updateArgs, ", "), code, code)

	// --- ByID -----------------------------------------------------------
	fmt.Fprintf(&sb, `// ByID loads a single %s that has not been archived.
func (r *%sRepo) ByID(ctx context.Context, id string) (*domain.%s, error) {
	const q = `+"`"+`SELECT %s
		FROM %s
		WHERE id = $1::uuid AND deleted_at IS NULL`+"`"+`

	m := &domain.%s{}
	err := r.q(ctx).QueryRow(ctx, q, id).Scan(%s)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.NotFound(%q)
	}
	if err != nil {
		return nil, wrapReadError(%q, err)
	}
	return m, nil
}

`, prose, e.Name, e.Name, readColumns(e), table, e.Name, scanTargets(e), code, code)

	// --- List -----------------------------------------------------------
	fmt.Fprintf(&sb, `// List returns one page of %s, newest first, using keyset pagination.
func (r *%sRepo) List(ctx context.Context, f port.%sFilter) ([]*domain.%s, string, error) {
	var sb strings.Builder
	sb.WriteString(`+"`"+`SELECT %s FROM %s WHERE deleted_at IS NULL`+"`"+`)
	args := make([]any, 0, 4)

`, e.Plural, e.Name, e.Name, e.Name, readColumns(e), table)

	if search := searchableFields(e); len(search) > 0 {
		ors := make([]string, len(search))
		for i, f := range search {
			ors[i] = f.Name + " ILIKE $%[1]d"
		}
		fmt.Fprintf(&sb, `	// The same placeholder is referenced by every branch because it is the
	// same value being matched, not a coincidence of numbering.
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%%"+q+"%%")
		fmt.Fprintf(&sb, " AND (%s)", len(args))
	}

`, strings.Join(ors, " OR "))
	}

	fmt.Fprintf(&sb, `	if f.Cursor != "" {
		ts, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", domain.Invalid("cursor_invalid", err.Error())
		}
		args = append(args, ts)
		tsIdx := len(args)
		args = append(args, id)
		fmt.Fprintf(&sb, " AND (created_at, id) < ($%%d::timestamptz, $%%d::uuid)", tsIdx, len(args))
	}

	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// One row beyond the page tells us whether a next page exists without a
	// second COUNT query over the same predicate.
	args = append(args, limit+1)
	fmt.Fprintf(&sb, " ORDER BY created_at DESC, id DESC LIMIT $%%d", len(args))

	rows, err := r.q(ctx).Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, "", wrapReadError(%q, err)
	}
	defer rows.Close()

	items := make([]*domain.%s, 0, limit)
	for rows.Next() {
		m := &domain.%s{}
		if err := rows.Scan(%s); err != nil {
			return nil, "", fmt.Errorf("scan %s: %%w", err)
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", wrapReadError(%q, err)
	}

	next := ""
	if len(items) > limit {
		last := items[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
		items = items[:limit]
	}
	return items, next, nil
}

`, code, e.Name, e.Name, scanTargets(e), prose, code)

	// --- Archive --------------------------------------------------------
	fmt.Fprintf(&sb, `// Archive soft deletes a %s. The row is retained so audit history and
// foreign keys pointing at it stay intact.
func (r *%sRepo) Archive(ctx context.Context, id string) error {
	const q = `+"`"+`UPDATE %s SET deleted_at = now(), updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL`+"`"+`

	tag, err := r.q(ctx).Exec(ctx, q, id)
	if err != nil {
		return wrapReadError(%q, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NotFound(%q)
	}
	return nil
}
`, prose, e.Name, table, code, code)

	return sb.String()
}

// backendPostgresErrors emits the constraint-violation translation shared by
// every repository.
func backendPostgresErrors() string {
	return `package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL error codes that correspond to a client mistake rather than a
// server fault. Anything not listed here is an internal error.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeNotNullViolation    = "23502"
	codeInvalidTextRep      = "22P02"
)

// wrapWriteError turns a database constraint violation into a domain error.
//
// The database is the last line of defence for invariants the application also
// checks: a uniqueness rule enforced only in Go loses to a concurrent request,
// because two transactions can both observe "no existing row" before either
// commits. So the constraint stays in the schema and the violation is
// translated here into the same vocabulary the validation layer uses, which
// means the client sees 409 rather than 500 for a duplicate.
func wrapWriteError(resource string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("write %s: %w", resource, err)
	}
	switch pgErr.Code {
	case codeUniqueViolation:
		return domainConflict(resource, pgErr)
	case codeForeignKeyViolation:
		return domainInvalid(resource+"_reference_invalid",
			"a referenced record does not exist")
	case codeCheckViolation:
		return domainInvalid(resource+"_value_invalid",
			"a value is outside the range the schema allows")
	case codeNotNullViolation:
		return domainInvalid(resource+"_field_required",
			"a required field was not supplied")
	case codeInvalidTextRep:
		return domainInvalid(resource+"_identifier_invalid",
			"an identifier is not a valid UUID")
	}
	return fmt.Errorf("write %s: %w", resource, err)
}

// wrapReadError translates the failures a read can produce.
//
// Reads have one client-correctable failure that writes share: a path
// parameter that is not a UUID reaches the server as text and PostgreSQL
// rejects the cast with 22P02. Without this translation that surfaces as 500,
// which tells the caller the server is broken when in fact their request was
// malformed — and it pollutes error budgets with faults that are not faults.
func wrapReadError(resource string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == codeInvalidTextRep {
		return domainInvalid(resource+"_identifier_invalid",
			"an identifier is not a valid UUID")
	}
	return fmt.Errorf("read %s: %w", resource, err)
}
`
}

// backendPostgresErrorBridge emits the small adapter that lets the postgres
// package build domain errors without the domain package importing pgx.
func backendPostgresErrorBridge(module string) string {
	return fmt.Sprintf(`package postgres

import (
	"github.com/jackc/pgx/v5/pgconn"

	%q
)

// domainConflict reports a uniqueness violation, naming the constraint so the
// operator can find the index without reading the server log.
func domainConflict(resource string, pgErr *pgconn.PgError) error {
	detail := "a record with the same unique value already exists"
	if pgErr.ConstraintName != "" {
		detail = "violates " + pgErr.ConstraintName
	}
	return domain.Conflict(resource+"_conflict", detail)
}

// domainInvalid reports a client-correctable problem.
func domainInvalid(code, message string) error {
	return domain.Invalid(code, message)
}
`, module+"/internal/domain")
}

// backendRepositoryContractTest emits a compile-time proof that every
// generated repository satisfies the port it claims to implement.
//
// This is cheap and catches the failure mode that matters: a schema change
// regenerates the port but not the implementation, and without this the
// mismatch only surfaces at the composition root, in an error message that
// points at main.go rather than at the repository that drifted.
func backendRepositoryContractTest(module string, entities []Entity) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `package postgres

import (
	"testing"

	%q
)

// TestRepositoriesSatisfyPorts is a compile-time assertion. If a repository
// stops implementing its interface this file fails to build, which is the
// earliest and clearest place for that error to appear.
func TestRepositoriesSatisfyPorts(t *testing.T) {
`, module+"/internal/port")

	for _, e := range entities {
		if e.Name == "User" {
			continue
		}
		fmt.Fprintf(&sb, "\tvar _ port.%sRepository = (*%sRepo)(nil)\n", e.Name, e.Name)
	}

	sb.WriteString("}\n")
	return sb.String()
}

// backendCursorTest emits round-trip tests for the pagination helpers.
func backendCursorTest() string {
	return `package postgres

import (
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2024, 3, 9, 12, 30, 45, 123456789, time.UTC)
	id := "5f9d2c7a-1b3e-4a8c-9d0f-2e4b6c8a0d1f"

	gotTS, gotID, err := decodeCursor(encodeCursor(ts, id))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !gotTS.Equal(ts) {
		t.Errorf("timestamp changed: want %s, got %s", ts, gotTS)
	}
	if gotID != id {
		t.Errorf("id changed: want %s, got %s", id, gotID)
	}
}

// A cursor arrives from an untrusted client, so every malformed shape must
// produce an error rather than a panic or a silently wrong page.
func TestDecodeCursorRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"not base64":       "!!!!not-base64!!!!",
		"missing sepator":  "bm8tc2VwYXJhdG9y",
		"bad timestamp":    "bm90LWEtdGltZXxhYmM",
		"empty identifier": "MjAyNC0wMy0wOVQxMjozMDo0NVp8",
	}
	for name, cursor := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeCursor(cursor); err == nil {
				t.Fatalf("expected an error for %q", cursor)
			}
		})
	}
}
`
}

// backendPostgresIntegrationTest emits a test that exercises a real database.
//
// It skips unless TEST_DATABASE_URL is set. That is the difference between a
// test suite that runs everywhere and one that is quietly disabled: skipping
// with a stated reason keeps `go test ./...` green on a laptop with no
// database while still failing loudly in CI, where the variable is set.
func backendPostgresIntegrationTest(module string, e Entity) string {
	table := tableName(e)

	// Choose a required text column to write, so the round trip proves a real
	// value survives the journey rather than only that a row appeared.
	probe := ""
	for _, f := range e.Fields {
		if f.Type == "text" && f.Required && !isSystemField(f.Name) {
			probe = goFieldName(f.Name)
			break
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, `package postgres_test

import (
	"context"
	"os"
	"testing"

	%q
	%q
	%q
)

// newTestDB connects to a real PostgreSQL instance.
//
// Repositories are the one layer that cannot be meaningfully tested with a
// fake: their entire job is to speak SQL correctly, and a fake would only
// prove that the fake agrees with itself. Placeholder numbering, type casts,
// constraint translation and keyset ordering are all properties of the real
// server.
func newTestDB(t *testing.T) *postgres.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run repository integration tests")
	}
	db, err := postgres.NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf%s"connect: %%v", err)
	}
	if err := postgres.Ping(context.Background(), db); err != nil {
		t.Fatalf%s"ping: %%v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// TestRoundTrip%s writes a row, reads it back, lists it and archives it.
func TestRoundTrip%s(t *testing.T) {
	if %sNeedsFixtures() {
		t.Skip("this entity has required references; create the parent rows first")
	}
	db := newTestDB(t)
	ctx := context.Background()
	repo := postgres.New%sRepo(db)

	m := &domain.%s{}
	seed%s(m)

	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf%s"create: %%v", err)
	}
	if m.ID == "" {
		t.Fatal("create did not populate the identifier")
	}
	if m.CreatedAt.IsZero() {
		t.Error("create did not populate created_at")
	}

	loaded, err := repo.ByID(ctx, m.ID)
	if err != nil {
		t.Fatalf%s"read back: %%v", err)
	}
	if loaded.ID != m.ID {
		t.Errorf("identifier changed: wrote %%s, read %%s", m.ID, loaded.ID)
	}
`, module+"/internal/domain", module+"/internal/port", module+"/internal/infra/postgres",
		"(", "(", e.Name, e.Name, e.Name, e.Name, e.Name, e.Name, "(", "(")

	if probe != "" {
		fmt.Fprintf(&sb, `	if loaded.%s != m.%s {
		t.Errorf("value changed in the round trip: wrote %%q, read %%q", m.%s, loaded.%s)
	}
`, probe, probe, probe, probe)
	}

	fmt.Fprintf(&sb, `
	items, _, err := repo.List(ctx, port.%sFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list: %%v", err)
	}
	if !containsID(idsOf(items), m.ID) {
		t.Error("the created record is missing from the listing")
	}

	if err := repo.Archive(ctx, m.ID); err != nil {
		t.Fatalf("archive: %%v", err)
	}

	// A soft-deleted row must disappear from both reads. If it does not, the
	// deleted_at predicate is missing and "delete" does nothing observable.
	if _, err := repo.ByID(ctx, m.ID); err == nil {
		t.Error("an archived record is still readable by id")
	}
	after, _, err := repo.List(ctx, port.%sFilter{Limit: 200})
	if err != nil {
		t.Fatalf("list after archive: %%v", err)
	}
	if containsID(idsOf(after), m.ID) {
		t.Error("an archived record still appears in the listing")
	}
}

// A malformed identifier is a client error, not a server fault. It must not
// surface as a 500.
func TestByIDRejectsAMalformedIdentifier%s(t *testing.T) {
	db := newTestDB(t)
	repo := postgres.New%sRepo(db)

	if _, err := repo.ByID(context.Background(), "not-a-uuid"); err == nil {
		t.Fatal("a malformed identifier was accepted")
	}
}

func idsOf(items []*domain.%s) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
`, e.Name, e.Name, e.Name, e.Name, e.Name)

	_ = table
	return sb.String()
}

// backendPostgresSeed emits a helper that fills an entity's required fields
// with values the schema will accept.
//
// Required references are the one thing this cannot invent: a foreign key
// needs a row that already exists. Rather than fabricate a UUID that will fail
// the constraint, the seed leaves references empty and the emitted test skips
// with an explanation, which is honest about what is and is not covered.
func backendPostgresSeed(module string, e Entity) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `package postgres_test

import (
	"fmt"
	"time"

	%q
)

// seed%s populates the fields the schema requires.
func seed%s(m *domain.%s) {
	unique := fmt.Sprintf("%%d", time.Now().UnixNano())
	_ = unique
`, module+"/internal/domain", e.Name, e.Name, e.Name)

	for _, f := range e.Fields {
		if isSystemField(f.Name) || !f.Required {
			continue
		}
		name := goFieldName(f.Name)
		switch f.Type {
		case "text":
			fmt.Fprintf(&sb, "\tm.%s = \"test-\" + unique\n", name)
		case "enum":
			if len(f.Enum) > 0 {
				fmt.Fprintf(&sb, "\tm.%s = %q\n", name, f.Enum[0])
			}
		case "int":
			fmt.Fprintf(&sb, "\tm.%s = 1\n", name)
		case "decimal":
			fmt.Fprintf(&sb, "\tm.%s = \"1.0000\"\n", name)
		case "bool":
			fmt.Fprintf(&sb, "\tm.%s = false\n", name)
		case "timestamp":
			fmt.Fprintf(&sb, "\tm.%s = time.Now().UTC()\n", name)
		}
	}

	sb.WriteString("}\n\n")

	// Report whether this entity can be tested standalone.
	fmt.Fprintf(&sb, `// %sNeedsFixtures reports whether this entity has required references that a
// test must create first. The generated round-trip test consults it and skips
// rather than failing on a foreign key it was never going to satisfy.
func %sNeedsFixtures() bool { return %t }
`, e.Name, e.Name, entityHasRequiredRef(e))

	return sb.String()
}

// entityHasRequiredRef reports whether an entity cannot be inserted without
// another row existing first.
func entityHasRequiredRef(e Entity) bool {
	for _, f := range e.Fields {
		if f.Type == "ref" && f.Required {
			return true
		}
	}
	return false
}

// standaloneEntity picks an entity that can be inserted without first creating
// another row, which is what makes an unattended integration test possible.
// Selection is deterministic — first match in blueprint order — so a
// regenerated project produces byte-identical output.
func standaloneEntity(entities []Entity) (Entity, bool) {
	for _, e := range entities {
		if !entityHasRequiredRef(e) {
			return e, true
		}
	}
	return Entity{}, false
}

// backendPostgresUnitOfWork emits the transaction boundary.
//
// The problem it solves: a use case that must write to two repositories has no
// way to make both writes commit together. Each repository method is atomic on
// its own, which is not the same thing — an order that creates a payment and
// decrements stock can leave the payment recorded and the stock untouched.
//
// The transaction is carried on the context rather than threaded through every
// repository signature. Two alternatives were rejected:
//
//   - Passing a transaction argument to every method. This puts a database
//     concept into the port interfaces, which live in the inner layer and must
//     not know that a database exists.
//   - Returning a second set of "transactional repositories" from Begin. This
//     doubles the constructor surface and the two sets drift.
//
// Context carriage keeps the ports clean and makes enlistment automatic: a
// repository called inside Within participates, one called outside does not,
// and neither has to be told which.
func backendPostgresUnitOfWork(module string) string {
	return fmt.Sprintf(`package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	%q
)

// Querier is the subset of pgx that repositories use. Both a pool and a
// transaction satisfy it, which is what lets one repository implementation
// serve both cases without knowing which it has.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// txKey is unexported so nothing outside this package can put a transaction
// into a context or take one out. Enlistment is not something a caller should
// be able to fake.
type txKey struct{}

func withTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func txFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// UnitOfWork runs a function inside a single database transaction.
type UnitOfWork struct {
	db *pgxpool.Pool
}

// NewUnitOfWork constructs the transaction boundary.
func NewUnitOfWork(db *pgxpool.Pool) *UnitOfWork { return &UnitOfWork{db: db} }

// Within runs fn inside a transaction, committing if it returns nil and
// rolling back otherwise.
//
// Nested calls join the outer transaction rather than opening a second one.
// PostgreSQL has no true nested transactions, and opening a second connection
// mid-flight would deadlock against the first one's locks.
//
// A panic rolls back and re-panics. Swallowing it would commit a transaction
// whose work is in an unknown state, which is worse than a crash.
func (u *UnitOfWork) Within(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, joined := txFrom(ctx); joined {
		return fn(ctx)
	}

	tx, err := u.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %%w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			// Rollback with a context that cannot already be cancelled: if the
			// caller's deadline expired, a rollback on that context is a no-op
			// and the transaction is left to time out server-side, holding
			// locks the whole time.
			if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil &&
				!errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("rollback: %%w", rbErr))
			}
		}
	}()

	if err = fn(withTx(ctx, tx)); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %%w", err)
	}
	return nil
}

// Compile-time proof that the concrete type satisfies the port.
var _ port.UnitOfWork = (*UnitOfWork)(nil)
`, module+"/internal/port")
}

// backendPortUnitOfWork emits the inner-layer declaration of the transaction
// boundary. It names no database concept, so the use case layer can depend on
// it without depending on PostgreSQL.
func backendPortUnitOfWork() string {
	return `package port

import "context"

// UnitOfWork runs a function as a single atomic operation.
//
// A use case that writes through more than one repository needs both writes to
// succeed or neither to happen. Individual repository methods are each atomic,
// which does not compose: two atomic writes are two outcomes, and the failure
// mode is a half-finished business operation that no error message describes.
//
// The implementation carries its transaction on the context, so repositories
// enlist automatically and this interface stays free of database vocabulary.
type UnitOfWork interface {
	// Within runs fn atomically, committing when it returns nil and rolling
	// back otherwise. Nested calls join the enclosing transaction.
	Within(ctx context.Context, fn func(ctx context.Context) error) error
}
`
}

// backendUnitOfWorkTest emits tests for the transaction boundary.
//
// These need a real server: rollback semantics, nesting behaviour and lock
// interaction are properties of PostgreSQL, not of the Go code wrapping it.
func backendUnitOfWorkTest(module string, resources []Entity) string {
	probe, ok := standaloneEntity(resources)
	if !ok {
		return `package postgres_test

// Every entity in this product requires a foreign key to exist before it can
// be inserted, so there is no fixture-free entity to exercise the transaction
// boundary against. Add a test here once you have a fixture builder.
`
	}

	return fmt.Sprintf(`package postgres_test

import (
	"context"
	"errors"
	"testing"

	%q
	%q
	%q
)

// A transaction that fails must leave nothing behind. This is the property the
// whole abstraction exists for, so it is tested against a real server rather
// than asserted about the code.
func TestUnitOfWorkRollsBackOnError(t *testing.T) {
	db := newTestDB(t)
	uow := postgres.NewUnitOfWork(db)
	repo := postgres.New%sRepo(db)
	ctx := context.Background()

	m := &domain.%s{}
	seed%s(m)
	sentinel := errors.New("deliberate failure")

	err := uow.Within(ctx, func(ctx context.Context) error {
		if err := repo.Create(ctx, m); err != nil {
			return err
		}
		// The row exists inside the transaction.
		if _, err := repo.ByID(ctx, m.ID); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the sentinel error back, got %%v", err)
	}
	if m.ID == "" {
		t.Fatal("no identifier was assigned, so the rollback proves nothing")
	}

	// Outside the transaction the row must not exist.
	if _, err := repo.ByID(ctx, m.ID); err == nil {
		t.Error("the record survived a rolled-back transaction")
	}
}

// The committed case must be equally definite.
func TestUnitOfWorkCommitsOnSuccess(t *testing.T) {
	db := newTestDB(t)
	uow := postgres.NewUnitOfWork(db)
	repo := postgres.New%sRepo(db)
	ctx := context.Background()

	m := &domain.%s{}
	seed%s(m)

	if err := uow.Within(ctx, func(ctx context.Context) error {
		return repo.Create(ctx, m)
	}); err != nil {
		t.Fatalf("commit failed: %%v", err)
	}

	if _, err := repo.ByID(ctx, m.ID); err != nil {
		t.Errorf("a committed record is not readable: %%v", err)
	}
}

// Two writes in one transaction either both land or neither does. This is the
// case that individual atomic methods cannot express.
func TestUnitOfWorkIsAllOrNothingAcrossWrites(t *testing.T) {
	db := newTestDB(t)
	uow := postgres.NewUnitOfWork(db)
	repo := postgres.New%sRepo(db)
	ctx := context.Background()

	first := &domain.%s{}
	seed%s(first)
	second := &domain.%s{}
	seed%s(second)

	err := uow.Within(ctx, func(ctx context.Context) error {
		if err := repo.Create(ctx, first); err != nil {
			return err
		}
		if err := repo.Create(ctx, second); err != nil {
			return err
		}
		return errors.New("fail after both writes")
	})
	if err == nil {
		t.Fatal("expected an error")
	}

	for label, record := range map[string]*domain.%s{"first": first, "second": second} {
		if record.ID == "" {
			continue
		}
		if _, err := repo.ByID(ctx, record.ID); err == nil {
			t.Errorf("the %%s record survived the rollback", label)
		}
	}
}

// Nesting must join the outer transaction. Opening a second one would deadlock
// against the first one's locks.
func TestUnitOfWorkNestsWithoutDeadlocking(t *testing.T) {
	db := newTestDB(t)
	uow := postgres.NewUnitOfWork(db)
	repo := postgres.New%sRepo(db)
	ctx := context.Background()

	m := &domain.%s{}
	seed%s(m)

	err := uow.Within(ctx, func(outer context.Context) error {
		return uow.Within(outer, func(inner context.Context) error {
			return repo.Create(inner, m)
		})
	})
	if err != nil {
		t.Fatalf("nested transaction failed: %%v", err)
	}
	if _, err := repo.ByID(ctx, m.ID); err != nil {
		t.Errorf("the record written in a nested transaction is missing: %%v", err)
	}
}

// A rollback triggered by the inner function must also roll back the outer
// one, or "join the enclosing transaction" would be a lie.
func TestNestedFailureRollsBackTheOuterTransaction(t *testing.T) {
	db := newTestDB(t)
	uow := postgres.NewUnitOfWork(db)
	repo := postgres.New%sRepo(db)
	ctx := context.Background()

	m := &domain.%s{}
	seed%s(m)

	err := uow.Within(ctx, func(outer context.Context) error {
		if err := repo.Create(outer, m); err != nil {
			return err
		}
		return uow.Within(outer, func(inner context.Context) error {
			return errors.New("inner failure")
		})
	})
	if err == nil {
		t.Fatal("expected the inner failure to propagate")
	}
	if m.ID != "" {
		if _, err := repo.ByID(ctx, m.ID); err == nil {
			t.Error("the outer write survived an inner failure")
		}
	}
}

// Work done outside Within must not be captured by an unrelated transaction.
func TestRepositoryWorksWithoutATransaction(t *testing.T) {
	db := newTestDB(t)
	repo := postgres.New%sRepo(db)
	ctx := context.Background()

	m := &domain.%s{}
	seed%s(m)

	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create outside a transaction failed: %%v", err)
	}
	if _, err := repo.ByID(ctx, m.ID); err != nil {
		t.Errorf("read outside a transaction failed: %%v", err)
	}
	_ = port.%sFilter{}
}
`,
		module+"/internal/domain", module+"/internal/port", module+"/internal/infra/postgres",
		probe.Name, probe.Name, probe.Name,
		probe.Name, probe.Name, probe.Name,
		probe.Name, probe.Name, probe.Name, probe.Name, probe.Name, probe.Name,
		probe.Name, probe.Name, probe.Name,
		probe.Name, probe.Name, probe.Name,
		probe.Name, probe.Name, probe.Name, probe.Name)
}
