# Version 1.1 — Persistence

**Status:** shipped · **Date:** 2026-07-30

Since v0.1 the factory generated repository *interfaces* and no implementations,
and generated handlers that `registerRoutes` never mounted. A generated product
compiled, started, and answered `/health` while being structurally incapable of
storing a row or serving a resource. The Improver agent reported this honestly
on every run — 2 high-severity findings — which made it the most visible
outstanding defect in the system.

v1.1 closes it.

---

## What was built

### 1. PostgreSQL repositories

One implementation per entity in `api/internal/infra/postgres/`, satisfying the
`port.*Repository` interface declared in the inner layer.

Written against **pgx directly, not an ORM**. A generated repository is code the
user owns and will edit; a hand-readable SQL statement can be inspected and
tuned, whereas a struct-tag DSL hides the query that actually runs. The
generator never interpolates a value into SQL text — only identifiers it
produced itself from the blueprint.

| Operation | Behaviour |
|---|---|
| `Create` | `INSERT ... RETURNING id::text, created_at, updated_at` — the database owns identity and audit columns |
| `Update` | `WHERE id = $n::uuid AND deleted_at IS NULL`, returns not-found for an archived row |
| `ByID` | Excludes soft-deleted rows |
| `List` | Keyset pagination, optional `ILIKE` search across text columns |
| `Archive` | Sets `deleted_at`; the row is retained so audit history and foreign keys stay intact |

**Type projections.** Two columns are cast rather than read natively:

- `NUMERIC` → `::text`, because the domain models decimals as strings.
  Scanning into a Go float would reintroduce exactly the rounding error the
  schema chose `NUMERIC` to avoid.
- `UUID` → `::text`, because the domain models identifiers as strings. Casting
  in the projection makes the scan a plain string read that does not depend on
  which UUID representations the driver supports.

Write placeholders carry the inverse cast (`$1::uuid`, `$3::numeric`).
PostgreSQL infers an untyped parameter as `text`, which does not implicitly
coerce to either.

### 2. Keyset pagination

Cursors encode `(created_at, id)` base64url-encoded.

OFFSET was rejected deliberately: it makes the server count and discard every
skipped row, so page 500 costs 500 pages of work, and a row inserted while the
client pages causes a record to be shown twice or skipped entirely. `created_at`
gives the ordering the API promises; `id` breaks ties so the order is total.

The query fetches `limit+1` rows — one beyond the page tells us a next page
exists without a second `COUNT` over the same predicate.

### 3. Constraint translation

The database is the last line of defence for invariants the application also
checks: a uniqueness rule enforced only in Go loses to a concurrent request,
because two transactions can both observe "no existing row" before either
commits. So the constraint stays in the schema, and the violation is translated
into the vocabulary the validation layer already uses.

| PostgreSQL code | Meaning | Result |
|---|---|---|
| `23505` | unique violation | `409` naming the constraint |
| `23503` | foreign key violation | `422` |
| `23514` | check violation | `422` |
| `23502` | not-null violation | `422` |
| `22P02` | invalid text representation | `422` |

### 4. Composition root

`registerRoutes` now builds the full chain per resource:

```go
func registerRoutes(r fiber.Router, db *postgres.DB) {
	apphttp.NewProjectHandler(usecase.NewProjectService(postgres.NewProjectRepo(db))).Register(r)
	apphttp.NewIssueHandler(usecase.NewIssueService(postgres.NewIssueRepo(db))).Register(r)
	...
}
```

The adapter package is named `http` and collides with `net/http` in this file,
so it is imported as `apphttp`. Aliasing at the one point of collision is
clearer than renaming a package whose name is correct everywhere else.

### 5. Liveness and readiness are separate

`/health` reports that the process is running and **does not touch the
database**. `/ready` reports whether the database is reachable.

Conflating them is a real outage pattern: if liveness queries the database, a
database blip causes the orchestrator to kill every healthy application
process, turning a recoverable incident into a self-inflicted outage. For the
same reason the pool is created without dialling, so the process starts and
serves liveness even when the database is briefly unreachable.

### 6. Integration tests in generated projects

Repositories are the one layer that cannot be meaningfully tested with a fake:
their entire job is to speak SQL correctly, and a fake only proves the fake
agrees with itself. Placeholder numbering, casts, constraint translation and
keyset ordering are all properties of the real server.

Generated tests skip with a stated reason unless `TEST_DATABASE_URL` is set —
green on a laptop with no database, loud in CI where the variable is set. The
generated CI workflow now applies migrations and sets it; previously it started
a Postgres service that nothing ever connected to.

---

## Bugs found while building this

Each was found by running the thing, not by reading it.

| Bug | Found by |
|---|---|
| `net/http` vs adapter `http` import collision | Compiling the generated project |
| Malformed UUID returned **500** — only writes translated pg error codes | Curling `/projects/not-a-uuid` |
| Error code `"seller profile_identifier_invalid"` contained a space | Exercising a multi-word entity |
| Routes were snake_case `/seller_profiles`, not REST-conventional kebab-case | `Cannot POST /api/v1/seller-profiles` |
| OpenAPI documented `/SellerProfiles` while the router mounted something else | New test comparing spec to router |
| Generated README omitted the migration step, so a follower hit "relation does not exist" | Following the README verbatim |
| `go vet` format-arg count mismatch after refactoring | `go vet` |

---

## Verified

Against **PostgreSQL 17.10**, generated Jira competitor and marketplace:

```
migration applied            ✔ 10 tables, indexes, triggers
/health                      ✔ {"status":"ok"}
/ready                       ✔ {"database":"ok","status":"ready"}
POST   /api/v1/projects      ✔ 201, database-assigned UUID
POST   /api/v1/sprints       ✔ 201, foreign key resolved
GET    /api/v1/listings/:id  ✔ 200
PATCH  /api/v1/listings/:id  ✔ 200, price 42.5000 → 99.9900 exactly
GET    /api/v1/listings?q=   ✔ ILIKE search
DELETE /api/v1/listings/:id  ✔ 204, row retained with deleted_at set
GET    (archived)            ✔ 404
```

Error mapping: duplicate → 409 · bad FK → 422 · bad enum → 422 ·
malformed UUID → 422 · missing → 404 · bad cursor → 422.

Pagination: 25 rows over 3 pages, no duplicates, no gaps.

Generated project's own integration suite against a real server:
`TestRoundTripCompany` and `TestByIDRejectsAMalformedIdentifierCompany` pass.

Factory itself: 10 Go packages green · `go vet` clean · `gofmt` clean ·
architecture rule enforced · benchmark **100%** across crm, pm, erp,
marketplace and custom domains.

Improver on a freshly generated project: **0 high, 0 medium, 1 low** —
previously 2 high.

---

## Still not done

1. **Only Go is verified end to end.** The generated frontend is built but never
   started and probed.
2. **Authentication is not wired to the database.** The `users` table exists and
   the schema is generated, but login/refresh handlers are not generated against
   it.
3. **No transaction boundary spanning repositories.** Each method is atomic on
   its own; a use case needing two writes to commit together has no unit of work.
4. **The sandbox shares the host filesystem for reads.** Reported honestly via
   `IsolationReport.Degraded`.
5. **A 0.5B model is too small for real design work.** Critics catch the
   degenerate output and fall back to blueprints.
6. `go test -race` OOMs on sqlite-dependent packages on a 2-CPU / 2 GB machine.

---

## Next

1. Transactional unit of work (`port.UnitOfWork`) for multi-repository writes.
2. Generate authentication against the `users` table.
3. Frontend runtime verification — start Vite and probe it.
4. Multi-language toolchain adapters (TypeScript, Python, Rust, C#).
