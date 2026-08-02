# GENESIS AI FACTORY — Database Design

Target: PostgreSQL 16 (production) and SQLite (local/CI). Migrations are written
in portable SQL with per-driver variants where types differ (`UUID`→`TEXT`,
`JSONB`→`TEXT`, `TIMESTAMPTZ`→`TEXT` ISO-8601, `BIGSERIAL`→`INTEGER PRIMARY KEY
AUTOINCREMENT`).

Migration tooling: embedded (`embed.FS`) sequential SQL files applied inside a
transaction with an advisory lock; recorded in `schema_migrations`. No external
CLI needed — the server migrates itself on boot (`GENESIS_AUTO_MIGRATE=true`,
default on for sqlite, opt-in for postgres).

---

## 1. Entity–relationship overview

```
users ──< project_members >── projects ──< runs ──< run_phases
                                  │          │
                                  │          ├──< tasks ──< task_attempts
                                  │          ├──< events
                                  │          └──< artifacts
                                  ├──< workspaces ──< files (metadata only)
                                  ├──< blueprints
                                  └──< memories (vector-backed)
refresh_tokens >── users
audit_log >── users
```

Design rules:
1. **UUIDv7** primary keys everywhere (time-sortable → index locality, no
   enumeration leak). Generated in Go, not the DB, so the same code path works
   on SQLite.
2. **Soft delete** (`deleted_at`) on user-visible aggregates; hard delete only
   via an explicit purge job.
3. **`created_at` / `updated_at`** on every table, UTC.
4. **No ON DELETE CASCADE across aggregate roots.** Deletion is an application
   use case that emits events, not a silent DB sweep.
5. Every FK is indexed. Every query in the codebase has a supporting index —
   verified by an `EXPLAIN`-based test on Postgres.

---

## 2. Tables (v0.1 core)

### users
| column | type | notes |
|---|---|---|
| id | uuid PK | UUIDv7 |
| email | citext UNIQUE NOT NULL | lowercased at the boundary |
| password_hash | text NOT NULL | Argon2id PHC string |
| display_name | text NOT NULL | |
| role | text NOT NULL | `owner\|admin\|member\|viewer` |
| status | text NOT NULL | `active\|suspended` |
| settings | jsonb NOT NULL DEFAULT '{}' | theme, editor prefs, model prefs |
| created_at / updated_at / deleted_at | timestamptz | |

Index: `ux_users_email` (unique, where `deleted_at is null`).

### refresh_tokens
`id, user_id FK, token_hash (sha256, unique), family_id, expires_at, revoked_at, replaced_by, user_agent, ip, created_at`
Rotation with reuse detection: presenting a revoked token revokes the whole
`family_id` — a stolen refresh token is neutralized on first double-use.

### projects
| column | type | notes |
|---|---|---|
| id | uuid PK | |
| owner_id | uuid FK users | |
| name | text NOT NULL | |
| slug | text NOT NULL | unique per owner |
| prompt | text NOT NULL | the original natural-language brief |
| description | text | |
| category | text | `crm\|erp\|pm\|marketplace\|saas\|custom` (v0.3 classifier) |
| status | text NOT NULL | `draft\|building\|ready\|failed\|archived` |
| workspace_path | text | absolute path of the generated repo |
| stack | jsonb | resolved technology choices |
| settings | jsonb | budgets, model overrides, autonomy level |
| created_at / updated_at / deleted_at | | |

Indexes: `ux_projects_owner_slug`, `ix_projects_owner_status`, `ix_projects_category`.

### project_members
`(project_id, user_id) PK, role, created_at` — team access.

### runs
| column | type | notes |
|---|---|---|
| id | uuid PK | |
| project_id | uuid FK | |
| triggered_by | uuid FK users | |
| kind | text | `build\|improve\|fix\|analyze` |
| status | text | `pending\|running\|succeeded\|failed\|canceled\|interrupted` |
| current_phase | text | |
| input | jsonb | prompt + options snapshot (runs are reproducible) |
| result | jsonb | summary, artifact ids, metrics |
| error | jsonb | typed error: `{code,message,phase,agent}` |
| token_budget / tokens_used | bigint | |
| started_at / finished_at | timestamptz | |
| cancel_requested_at | timestamptz | |
| created_at / updated_at | | |

Indexes: `ix_runs_project_created`, `ix_runs_status` (partial on active states —
the scheduler's hot query).

### run_phases
`id, run_id FK, name, ordinal, status, started_at, finished_at, summary jsonb`
Unique `(run_id, ordinal)`.

### tasks
The unit of agent work; a DAG per run.
`id, run_id, phase_id, parent_id, agent_role, title, description, status
(pending|ready|running|blocked|succeeded|failed|skipped), priority int,
depends_on uuid[] (json array on sqlite), input jsonb, output jsonb,
attempt_count, max_attempts, created_at, updated_at`

Index: `ix_tasks_run_status`, `ix_tasks_agent_status`.

### task_attempts
`id, task_id, attempt_no, model, prompt_tokens, completion_tokens, latency_ms,
status, error jsonb, raw_output_ref (artifact id), created_at`
Keeps full LLM forensics without bloating `tasks`.

### events
`seq bigserial PK, id uuid, run_id, project_id, topic, type, agent_role,
level (debug|info|warn|error), message, payload jsonb, created_at`
Indexes: `ix_events_run_seq (run_id, seq)`, `ix_events_topic_seq`.
Append-only; no UPDATE/DELETE grants in production.

### artifacts
Content-addressed outputs (PRD, architecture doc, schema, diff, test report).
`id, run_id, task_id, project_id, kind, name, mime, size_bytes,
sha256 (unique per project), storage (db|fs|s3), body text NULL, path text NULL,
metadata jsonb, created_at`
Small artifacts inline (`body`), large ones on disk/S3 (`path`). Dedup by sha256
means regenerating an identical file costs zero storage.

### workspaces
`id, project_id, root_path, vcs (git), default_branch, current_branch,
head_commit, status, last_snapshot_at, created_at, updated_at`

### files
Metadata index over the workspace for fast tree/search in the desktop app
(the bytes live on disk, git is the truth).
`id, workspace_id, rel_path, lang, size_bytes, sha256, generated_by_agent,
is_user_modified bool, last_modified_at` — `is_user_modified` is how the system
honors invariant I2: agents must not blind-overwrite a human edit.

### blueprints (v0.3)
`id, key, version, category, name, description, spec jsonb, is_builtin,
created_at` — reusable product templates (CRM, ERP, PM, marketplace).

### memories (v0.2)
`id, scope (global|user|project), user_id, project_id, kind
(preference|decision|snippet|lesson), title, content, vector_id, metadata jsonb,
importance real, created_at, last_used_at, use_count`
Vectors live in Qdrant keyed by `vector_id`; Postgres holds the authoritative
record so memory survives a vector-store rebuild (reindex = replay this table).

### audit_log
`id, actor_id, action, resource_type, resource_id, ip, user_agent, before jsonb,
after jsonb, created_at` — every mutating use case writes one row.

### idempotency_keys
`key PK, user_id, endpoint, request_hash, response_status, response_body,
created_at, expires_at`

---

## 3. Key access patterns → index justification

| Query | Index |
|---|---|
| list my projects by recency | `ix_projects_owner_status (owner_id, status, created_at desc)` |
| active runs for scheduler | `ix_runs_status` partial `where status in ('pending','running')` |
| tail events for a run after cursor | `ix_events_run_seq (run_id, seq)` |
| next ready tasks for an agent | `ix_tasks_agent_status (agent_role, status, priority desc)` |
| dedup artifact | `ux_artifacts_project_sha (project_id, sha256)` |
| file tree of a workspace | `ix_files_workspace_path (workspace_id, rel_path)` |

## 4. Consistency & transactions

- One use case = one transaction. Repositories accept a `port.Tx` so a use case
  can compose repository calls atomically.
- Run/phase/task transitions and their announcing event are written together.
- Optimistic concurrency on `projects` and `tasks` via `updated_at` compare-and-set;
  a conflicting write returns `domain.ErrConflict` (HTTP 409) rather than
  silently clobbering.
- Isolation: `READ COMMITTED` default; `REPEATABLE READ` for the task scheduler's
  claim query, which uses `SELECT … FOR UPDATE SKIP LOCKED` so N workers can
  drain the queue without contention.

## 5. Migration policy

- Forward-only, numbered `0001_*.up.sql` with a matching `.down.sql` for local
  reversal.
- Additive first: add column → backfill → switch reads → drop old, across
  releases. Never a destructive change in a single deploy.
- Every migration is tested by applying all up migrations to an empty DB and
  asserting the resulting schema matches a golden snapshot (both drivers).
