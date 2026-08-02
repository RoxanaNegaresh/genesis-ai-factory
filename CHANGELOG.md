# Changelog

All notable changes to Genesis AI Factory are recorded here.
Format follows [Keep a Changelog](https://keepachangelog.com/1.1.0/).

## [1.2.0] — 2026-07-30

Generated products now authenticate, write atomically across repositories, and
have their web client started and probed rather than merely type-checked.

### Added — Authentication
- **Argon2id** password hashing in PHC format, parameters stored with the
  digest so the cost factor can be raised without locking out existing accounts.
- **HS256 tokens** verified signature-first and restricted to one algorithm, so
  `alg:none` and algorithm confusion have nothing to grip.
- **Rotating refresh tokens**, stored only as a hash. Replay of a retired token
  revokes the entire family: a silent theft becomes a visible logout.
- Resource routes mounted behind `RequireAuth` **as a group**, so a handler
  added later is protected by default rather than public by omission.
- Login returns one error for wrong-password and unknown-account, and hashes
  even when the account is missing so timing does not leak the difference.

### Added — Transactions
- `port.UnitOfWork` and a PostgreSQL implementation carrying the transaction on
  the context, so repositories enlist automatically and the inner layer names
  no database concept. Nested calls join the enclosing transaction; rollback
  uses an uncancellable context so an expired deadline cannot strand locks.

### Added — Frontend runtime verification
- `NodeToolchain`: install, build, typecheck, `vite preview`, HTTP probe.
  Preview serves the built artefact, which is what ships.
- `Toolchain.ServePortArgs` with `{{port}}` substitution, for servers that take
  a port on the command line instead of from the environment.
- `port.MemoryLimitDisabled`, distinct from both zero and a large number.

### Fixed
- **Refresh-token family revocation was rolled back with the transaction that
  returned the error.** The response claimed every session was revoked while
  every stolen token kept working. Revocation now commits independently.
- **Sandboxed builds inherited no `TMPDIR`** and filled tmpfs, where the Go
  linker was killed mid-link and printed a SIGSEGV trace that read like a
  compiler bug. The benchmark fell to 35% and blamed the generated projects.
- Any `RLIMIT_AS` breaks WebAssembly; `vite build` failed at 2, 4 and 6 GiB and
  succeeded with none. Node steps now set no ceiling and say so.
- Two access tokens minted in the same second were byte-identical; added `jti`.
- OpenAPI documented only `/auth/login` while the router mounted four endpoints.

### Verified
- Full auth lifecycle against PostgreSQL 17 including replay revocation.
- Transaction rollback, commit, all-or-nothing, nesting, nested failure.
- Frontend install/build/typecheck/serve/probe → HTTP 200.
- Benchmark 100% across crm, pm, erp, marketplace and custom domains.
- Improver on a freshly generated project: 0 high, 0 medium.

## [1.1.0] — 2026-07-30

Closes the gap that had stood since v0.1: generated products now persist data.

### Added — Persistence layer
- **PostgreSQL repository implementations** for every entity, generated into
  `internal/infra/postgres` and written against pgx directly rather than an
  ORM, because a generated repository is code the user will read and edit and
  a visible SQL statement is inspectable where a struct-tag DSL is not.
- **Composition root wiring**: `registerRoutes` now constructs
  repository → service → handler for every resource. The API surface that was
  previously generated-but-unreachable is reachable.
- **Keyset pagination** over `(created_at, id)`. Not OFFSET: OFFSET makes the
  server count and discard every skipped row, and a concurrent insert causes a
  record to be shown twice or skipped entirely.
- **Constraint translation**: unique, foreign-key, check, not-null and invalid
  -UUID violations become domain errors, so a duplicate returns 409 and a bad
  identifier returns 422 rather than 500.
- **Readiness endpoint** `/ready`, separate from `/health`. Liveness must not
  touch the database or an outage causes the orchestrator to kill healthy
  processes.
- **Integration tests** in generated projects that run against a real database
  and skip with a stated reason when `TEST_DATABASE_URL` is unset.
- Generated CI now applies migrations and sets `TEST_DATABASE_URL`; without
  that the repository tests skipped and the suite reported green while
  covering nothing.

### Fixed
- Error codes for multi-word entities contained a space
  (`seller profile_identifier_invalid`), which no client can match on. Prose
  and code spellings are now derived separately.
- Routes for multi-word entities were snake_case (`/seller_profiles`). URLs are
  now kebab-case (`/seller-profiles`), while SQL identifiers keep underscores.
  OpenAPI paths, docs and contract tests were all realigned, and a test now
  asserts the router and the spec agree.
- A malformed UUID on a read path returned 500 because only writes translated
  PostgreSQL error codes. Reads now do too.
- The adapter package is named `http` and collided with `net/http` in the
  composition root; it is imported as `apphttp`.

### Changed
- `TestAnalysisReportsRealGapsInAGeneratedProject` was inverted. It asserted
  the two gaps were reported; it now fails if they reappear, and additionally
  fails on any high-severity finding in a freshly generated project.

### Verified
- A generated Jira competitor was run against PostgreSQL 17: create, read,
  partial update, search, list and archive all exercised over HTTP, with
  foreign keys enforced and `NUMERIC` values surviving the round trip exactly.
- Keyset pagination checked across 25 rows and 3 pages: no duplicates, no gaps.
- Improver findings on a freshly generated project: **0 high, 0 medium**
  (previously 2 high).
- Benchmark remains 100% across crm, pm, erp, marketplace and custom domains.

## [1.0.0] — 2026-07-29

Production release. Architecture settled, invariants verified, limitations documented.

### Added — v0.5 IDE
- **Patch engine**: atomic multi-file edits with content-anchored hunks and
  base-hash verification, so an agent cannot clobber a manual edit or leave a
  refactor half-applied.
- **Unified diff** rendering via longest-common-subsequence, with a size guard.
- **Git intelligence**: a snapshot per phase using Conventional Commits, plus
  history, diff, status, branch and rollback. Rollback removes untracked files,
  which a plain hard reset leaves behind.
- **Workspace API**: file tree, read, write with conflict detection, and
  cross-file search.
- **Monaco editor** in the desktop app with tabs, dirty markers, save shortcut,
  git panel and one-click rollback.

### Added — v0.6 Autonomous Product Builder
- **Self-healing loop**: diagnose, propose a minimal patch, apply, re-verify;
  revert anything that does not improve the failure count.
- **Error signatures** normalised so a lesson learned on one project applies to
  another.
- Repairs are recorded to memory; dependency failures are correctly classified
  as unrepairable.

### Added — v0.7 Advanced AI
- **Real static analysis** of generated projects using `go/parser`: unimplemented
  repository interfaces, unmounted handlers, test-coverage ratio, unparseable
  source, missing operational files, TODO markers.
- The Improver agent now reports what is true of the project in front of it,
  with interfaces named and counts measured.

### Fixed
- Git porcelain parsing truncated the first character of modified filenames,
  because the status code is two columns wide but a path was sliced at offset 3.
- `usecase` imported `factory` for a hash function; `HashContent` moved to
  `domain`, where content identity belongs, and git access went behind
  `port.VersionControl`.
- Route-wiring detection inspected the whole file rather than the body of
  `registerRoutes`, producing a false negative.

### Known limitations
- Generated projects are complete scaffolding: repositories are interfaces
  without implementations and handlers are not mounted. The Improver reports
  this per project rather than claiming otherwise. *(Closed in 1.1.0.)*
- Only the Go toolchain is verified end to end; the frontend is built but not
  started and probed.
- The sandbox confines writes, network and credentials but shares the host
  filesystem for reads.
- Screenshot-to-application and a visual architecture designer were deliberately
  not built; both are recorded in the roadmap with the reasoning.

## [0.4.0] — 2026-07-29

### Added — Code Factory

**Execution sandbox**
- Linux namespace isolation (user, mount, pid, ipc, uts, net) applied natively
  from Go, with no container runtime, daemon or root required.
- An empty network namespace is the default for every command, which is exactly
  `--network=none`.
- Resource ceilings (memory, file size, process count) via `prlimit`.
- The host environment is never inherited: JWT secrets and database URLs cannot
  reach generated code.
- Timeouts kill the whole process group, so build tools leave no orphans.
- Output is capped so a print loop cannot exhaust server memory.
- `IsolationReport` records what was actually achieved, including anything
  requested but unavailable, so a caller can tell a guarantee from an intention.

**Verification runner**
- Five-stage pipeline — install, build, test, serve, probe — where each stage
  gates the next and network access is granted only where genuinely required.
- Startup is detected by polling the TCP socket rather than parsing logs.
- A real `GET /health` proves the service serves, not merely that it links.
- The QA agent now runs the project it reviews, and its report carries a
  stage-by-stage table plus the isolation applied.

**Benchmark v2**
- Reweighted so running (25%) outranks compiling (22%); a test asserts the
  weighting itself so the scale cannot drift toward what is easy to measure.
- All five categories now start and answer HTTP requests: 100%.

### Fixed
- **Path confinement was defeated by its own normalisation.** `filepath.Clean`
  on a rooted path collapsed `../` to `/`, so an escape resolved to the
  workspace root and was accepted instead of rejected. Validation now precedes
  normalisation and symlinks are re-checked.
- The CLI silently ignored flags written after positional arguments, so
  `genesis artifacts <id> --name X` dropped the flag.

### Known limitations
- The sandbox shares the host filesystem for reads, so generated code can read
  host binaries. Accepted for the desktop case and reported honestly; a server
  deployment should use an OCI executor or gVisor behind the same interface.
- Only the Go toolchain is verified end to end; the generated frontend is built
  but not yet run (v0.5).

## [0.3.0] — 2026-07-28

### Added — Product Intelligence

**Model-authored business logic**
- Domain validation rules derived from the requirements by the reasoning model
  and compiled into generated entities.
- Rules are rendered from a closed set of templates, so the model chooses which
  constraint applies where while the emitted code is guaranteed to compile.

**Codegen safety layer**
- Generated function bodies are parsed with `go/parser` before use and rejected
  for syntax errors, forbidden constructs, excessive size, or defining the
  wrong function. Rejections fall back to a compiling default.

**Blueprint synthesis**
- Derives a real product template for domains no built-in blueprint covers,
  replacing the generic SaaS fallback.
- Repairs the structural mistakes models make: invalid identifiers, colliding
  table names, dangling references, one-value enums, duplicated audit columns,
  malformed routes, a missing User entity.
- `ValidateBlueprint` gates the result against the invariants every downstream
  generator assumes, and is asserted against all five built-in blueprints.

**Benchmark harness**
- Five prompts scored on facts only: generates, compiles, tests pass, category,
  entities, documentation completeness.
- Runs as a test so a quality regression fails CI like any other regression.
- `make bench` and `make bench-report`.

### Fixed
- **Optional email fields generated code that did not compile** (`*string`
  compared against `string`), shipping broken ERP and marketplace projects since
  v0.1. Found by the benchmark on its first run; score went 89% → 100%.
- Optional-field dereference in rendered rules was unparenthesised, so
  `*m.Date.IsZero()` dereferenced a bool.
- `snakeIdentifier` and `toSnake` produced doubled underscores for names like
  "Registration Number".
- Synthesised blueprints could assign one table name to several entities.
- Derived validation messages could echo the PRD, surfacing API specification
  text next to form fields.
- Rules dropped because a synthesised blueprint was rejected are now reported
  explicitly instead of vanishing.

### Known limitations
- Frontend logic is still template-only (v0.4).
- Generated projects are compiled and tested, not yet executed (v0.4).
- A 0.5B model proves the pipeline but produces too few screens for synthesis to
  accept; the system correctly declines and falls back.

## [0.2.0] — 2026-07-28

### Added — AI Core

**Inference**
- `port.LLM` boundary with an OpenAI-compatible provider, so llama.cpp, vLLM,
  Ollama and hosted APIs are one code path.
- Constrained decoding via JSON Schema, making output structurally valid by
  construction rather than by retry.
- Capability classes (`reasoning`/`code`/`fast`) resolved to concrete models at
  runtime, with model discovery, caching and health probing.

**Agent runtime**
- Schema validator reporting every defect in one pass, with repair prompts
  written for a model to act on.
- Bounded repair loop: generate → validate → correct, capped at two retries.
- Context-window discovery with prompt and output budgeting.
- Per-attempt token accounting aggregated into a per-run budget.
- Truncation detection with a distinct retry path.
- Transport errors abort immediately instead of consuming the repair budget.

**Critic pass**
- Repetition detection for near-duplicate list entries.
- Echo detection for output that restates its own input.
- Both fall back to the deterministic blueprint and say so in the event stream.

**Memory**
- Hybrid lexical + vector retrieval that works with or without an embedder.
- Scope isolation, deduplication, importance weighting, usage tracking.
- Architecture decisions persisted automatically for future runs.

**Model management (`genesis-ai`)**
- Curated catalogue with RAM-aware recommendation and a 30% headroom margin.
- Resumable, verified downloads; `serve` and `doctor` commands.

**Surfaces**
- `GET /api/v1/models`, `genesis models`, and a desktop badge showing whether
  reasoning is active.
- CLI now refreshes an expired access token transparently.

### Fixed during development
- Prompt assembly used a hard-coded budget rather than the model's real context
  window, producing `request exceeds context size` failures under llama.cpp.
- The CLI failed with "token expired" after fifteen minutes instead of using its
  stored refresh token.
- Schema validator relocated from `infra` to `port` to satisfy the dependency
  rule, which the architecture test caught.

### Known limitations
- Code-generating agents still use deterministic templates (v0.3).
- Memory is in-process; Qdrant integration is v0.3.
- A 0.5B model is sufficient to prove the pipeline, not to author production
  design documents. The critic detects this and degrades.

## [0.1.0] — 2026-07-28

### Added — Foundation

**Design**
- Architecture, system design, database design, repository topology, agent
  architecture and roadmap documents, authored before implementation.

**Control plane (Go 1.25 / Fiber v2)**
- Clean Architecture with the dependency rule enforced by an executable test.
- Argon2id password hashing with transparent parameter upgrade.
- JWT access tokens; rotating refresh tokens with reuse detection that revokes
  the whole token family.
- RBAC evaluated in the use case layer.
- Project lifecycle with workspace provisioning and slug collision resolution.
- Seven-phase run loop with cooperative cancellation and crash reconciliation.
- Append-only event log with gapless monotonic cursors.
- Websocket hub with heartbeats, cursor resume, bounded buffers and gap markers.
- Embedded migrations with checksum drift detection.
- SQLite and PostgreSQL behind one implementation and one conformance suite.
- Request IDs, redacting structured logs, strict CORS, security headers,
  auth rate limiting, unified error envelope, graceful shutdown.

**Product generation**
- Deterministic weighted-lexicon classifier with confidence reporting.
- Five blueprints: CRM, Project Management, ERP, Marketplace, SaaS.
- Eleven agents with charters, budgets and tool allowlists.
- Real artifacts: vision, PRD, design system, flows, architecture, ADRs,
  OpenAPI 3.0, ERD, PostgreSQL DDL, Go source with tests, React client,
  test plan, security review, Docker, CI, runbook, improvement plan.
- Workspace toolbelt with path confinement, secret scanning and atomic writes.

**Clients**
- `genesis` CLI with live streaming, agent board and environment diagnostics.
- Tauri 2 + React 18 desktop app with engine supervision and token injection.

**Infrastructure**
- Docker Compose stack, distroless control-plane image, CI with race detection,
  PostgreSQL conformance job, vulnerability and secret scanning.

### Fixed during development
- Placeholder reuse (`$1` referenced twice) that is valid in PostgreSQL and
  silently wrong in SQLite; now prevented structurally by a panic in `rebind`.
- Data race between the run driver goroutine and the HTTP response serialiser
  sharing a `Run` aggregate; resolved by explicit ownership transfer.
- Non-reproducible artifacts caused by embedding a generation timestamp in the
  vision document, which defeated content-addressed deduplication.
- Generated entities composed solely of optional references emitted an unused
  `strings` import, which does not compile in Go. Import blocks are now derived
  from the body that will actually be generated.
- Generated tests asserted that an empty record must fail validation, which is
  false for entities with no required fields. The assertion now matches the
  entity's actual contract.

### Known limitations
- No LLM inference (v0.2), no execution sandbox (v0.4), no Monaco editor,
  terminal or git integration (v0.5), no self-healing loop (v0.6).
- Generated projects compile and their tests pass, but repositories are
  interfaces without implementations and handlers are not yet mounted.
