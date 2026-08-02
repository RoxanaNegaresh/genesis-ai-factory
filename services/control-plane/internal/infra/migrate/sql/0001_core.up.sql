-- 0001_core: identity, projects, runs, tasks, events, artifacts, workspaces.
--
-- Portability contract: this file is written in the intersection of SQLite and
-- PostgreSQL SQL. Types are chosen so both engines accept them verbatim:
--   * TEXT for UUIDv7 identifiers (generated in Go, never by the database)
--   * TEXT for JSON payloads (marshalled in Go; Postgres deployments may
--     migrate these to JSONB later without touching application code)
--   * TEXT for RFC3339 UTC timestamps (a single canonical representation
--     avoids driver-specific time handling differences)
-- The one construct that differs is the auto-incrementing event sequence, which
-- the migration runner rewrites per driver via the {{AUTOINC}} token.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    role          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active',
    settings      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    deleted_at    TEXT
);
CREATE UNIQUE INDEX ux_users_email ON users (email);
CREATE INDEX ix_users_status ON users (status);

CREATE TABLE refresh_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users (id),
    token_hash  TEXT NOT NULL,
    family_id   TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    revoked_at  TEXT,
    replaced_by TEXT,
    user_agent  TEXT NOT NULL DEFAULT '',
    ip          TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);
CREATE UNIQUE INDEX ux_refresh_tokens_hash ON refresh_tokens (token_hash);
CREATE INDEX ix_refresh_tokens_user ON refresh_tokens (user_id);
CREATE INDEX ix_refresh_tokens_family ON refresh_tokens (family_id);
CREATE INDEX ix_refresh_tokens_expiry ON refresh_tokens (expires_at);

CREATE TABLE projects (
    id             TEXT PRIMARY KEY,
    owner_id       TEXT NOT NULL REFERENCES users (id),
    name           TEXT NOT NULL,
    slug           TEXT NOT NULL,
    prompt         TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    category       TEXT NOT NULL DEFAULT 'custom',
    status         TEXT NOT NULL DEFAULT 'draft',
    workspace_path TEXT NOT NULL DEFAULT '',
    stack          TEXT NOT NULL DEFAULT '{}',
    settings       TEXT NOT NULL DEFAULT '{}',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    deleted_at     TEXT
);
CREATE UNIQUE INDEX ux_projects_owner_slug ON projects (owner_id, slug);
CREATE INDEX ix_projects_owner_status ON projects (owner_id, status, created_at);
CREATE INDEX ix_projects_category ON projects (category);

CREATE TABLE project_members (
    project_id TEXT NOT NULL REFERENCES projects (id),
    user_id    TEXT NOT NULL REFERENCES users (id),
    role       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX ix_project_members_user ON project_members (user_id);

CREATE TABLE runs (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects (id),
    triggered_by        TEXT NOT NULL REFERENCES users (id),
    kind                TEXT NOT NULL DEFAULT 'build',
    status              TEXT NOT NULL DEFAULT 'pending',
    current_phase       TEXT NOT NULL DEFAULT 'analyze',
    input               TEXT NOT NULL DEFAULT '{}',
    result              TEXT NOT NULL DEFAULT '{}',
    error               TEXT,
    token_budget        INTEGER NOT NULL DEFAULT 0,
    tokens_used         INTEGER NOT NULL DEFAULT 0,
    started_at          TEXT,
    finished_at         TEXT,
    cancel_requested_at TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);
CREATE INDEX ix_runs_project_created ON runs (project_id, created_at);
CREATE INDEX ix_runs_status ON runs (status);

CREATE TABLE run_phases (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES runs (id),
    name        TEXT NOT NULL,
    ordinal     INTEGER NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    summary     TEXT NOT NULL DEFAULT '{}',
    started_at  TEXT,
    finished_at TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE UNIQUE INDEX ux_run_phases_run_ordinal ON run_phases (run_id, ordinal);

CREATE TABLE tasks (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs (id),
    phase_id      TEXT NOT NULL,
    parent_id     TEXT,
    agent_role    TEXT NOT NULL,
    title         TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending',
    priority      INTEGER NOT NULL DEFAULT 0,
    depends_on    TEXT NOT NULL DEFAULT '[]',
    input         TEXT NOT NULL DEFAULT '{}',
    output        TEXT NOT NULL DEFAULT '{}',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts  INTEGER NOT NULL DEFAULT 3,
    started_at    TEXT,
    finished_at   TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE INDEX ix_tasks_run_status ON tasks (run_id, status);
CREATE INDEX ix_tasks_agent_status ON tasks (agent_role, status, priority);

CREATE TABLE task_attempts (
    id                TEXT PRIMARY KEY,
    task_id           TEXT NOT NULL REFERENCES tasks (id),
    attempt_no        INTEGER NOT NULL,
    model             TEXT NOT NULL DEFAULT '',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    status            TEXT NOT NULL,
    error             TEXT NOT NULL DEFAULT '{}',
    raw_output_ref    TEXT,
    created_at        TEXT NOT NULL
);
CREATE INDEX ix_task_attempts_task ON task_attempts (task_id, attempt_no);

CREATE TABLE events (
    seq        {{AUTOINC}},
    id         TEXT NOT NULL,
    run_id     TEXT NOT NULL DEFAULT '',
    project_id TEXT NOT NULL DEFAULT '',
    topic      TEXT NOT NULL,
    type       TEXT NOT NULL,
    agent_role TEXT NOT NULL DEFAULT '',
    level      TEXT NOT NULL DEFAULT 'info',
    message    TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX ux_events_id ON events (id);
CREATE INDEX ix_events_run_seq ON events (run_id, seq);
CREATE INDEX ix_events_topic_seq ON events (topic, seq);
CREATE INDEX ix_events_project_seq ON events (project_id, seq);

CREATE TABLE artifacts (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL DEFAULT '',
    task_id    TEXT,
    project_id TEXT NOT NULL,
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    mime       TEXT NOT NULL DEFAULT 'text/plain',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    sha256     TEXT NOT NULL,
    storage    TEXT NOT NULL DEFAULT 'db',
    body       TEXT NOT NULL DEFAULT '',
    path       TEXT NOT NULL DEFAULT '',
    metadata   TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX ux_artifacts_project_sha ON artifacts (project_id, sha256);
CREATE INDEX ix_artifacts_run ON artifacts (run_id, created_at);
CREATE INDEX ix_artifacts_project_kind ON artifacts (project_id, kind, created_at);

CREATE TABLE workspaces (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL REFERENCES projects (id),
    root_path        TEXT NOT NULL,
    vcs              TEXT NOT NULL DEFAULT 'git',
    default_branch   TEXT NOT NULL DEFAULT 'main',
    current_branch   TEXT NOT NULL DEFAULT 'main',
    head_commit      TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'ready',
    last_snapshot_at TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE UNIQUE INDEX ux_workspaces_project ON workspaces (project_id);

CREATE TABLE workspace_files (
    id                 TEXT PRIMARY KEY,
    workspace_id       TEXT NOT NULL REFERENCES workspaces (id),
    rel_path           TEXT NOT NULL,
    lang               TEXT NOT NULL DEFAULT '',
    size_bytes         INTEGER NOT NULL DEFAULT 0,
    sha256             TEXT NOT NULL DEFAULT '',
    generated_by_agent TEXT NOT NULL DEFAULT '',
    is_user_modified   INTEGER NOT NULL DEFAULT 0,
    last_modified_at   TEXT NOT NULL,
    created_at         TEXT NOT NULL
);
CREATE UNIQUE INDEX ux_workspace_files_path ON workspace_files (workspace_id, rel_path);

CREATE TABLE audit_log (
    id            TEXT PRIMARY KEY,
    actor_id      TEXT NOT NULL DEFAULT '',
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL DEFAULT '',
    ip            TEXT NOT NULL DEFAULT '',
    user_agent    TEXT NOT NULL DEFAULT '',
    before        TEXT NOT NULL DEFAULT '{}',
    after         TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL
);
CREATE INDEX ix_audit_actor ON audit_log (actor_id, created_at);
CREATE INDEX ix_audit_resource ON audit_log (resource_type, resource_id);

CREATE TABLE idempotency_keys (
    key             TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL,
    endpoint        TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    response_status INTEGER NOT NULL DEFAULT 0,
    response_body   TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    expires_at      TEXT NOT NULL
);
CREATE INDEX ix_idempotency_expiry ON idempotency_keys (expires_at);
