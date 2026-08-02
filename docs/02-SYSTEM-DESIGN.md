# GENESIS AI FACTORY — System Design

Companion to `01-ARCHITECTURE.md`. This document specifies *mechanisms*:
protocols, state machines, concurrency, failure handling.

---

## 1. Identity & access

- **Password hashing:** Argon2id, `t=3, m=64 MiB, p=2`, 16-byte salt, 32-byte
  key. Encoded in the standard PHC string so parameters can be upgraded per-user
  on next login without a migration.
- **Tokens:** short-lived access JWT (HS256, 15 min) + opaque refresh token
  (32 random bytes, SHA-256 stored, 30 days, rotating with reuse detection).
  Access tokens carry `sub`, `email`, `role`, `jti`, `exp`, `iat`.
- **Local-first mode:** a desktop install with `GENESIS_SINGLE_USER=true`
  bootstraps a `local@genesis` owner account on first boot and the desktop app
  authenticates via a machine-scoped token stored in the OS keychain. No login
  wall for a single-machine user; full auth for team/server deployments.
- **Authorization:** RBAC with roles `owner | admin | member | viewer`, evaluated
  in the usecase layer (never in handlers) against `(subject, action, resource)`.

## 2. API surface (v0.1)

REST, JSON, `/api/v1`. All errors share one envelope:

```json
{ "error": { "code": "project_not_found", "message": "...", "request_id": "..." } }
```

| Method | Path | Purpose |
|---|---|---|
| GET | `/health`, `/ready` | liveness / readiness (checks DB) |
| GET | `/api/v1/meta` | version, build, capabilities |
| POST | `/api/v1/auth/register` | create account |
| POST | `/api/v1/auth/login` | access + refresh tokens |
| POST | `/api/v1/auth/refresh` | rotate refresh token |
| GET | `/api/v1/auth/me` | current principal |
| GET/POST | `/api/v1/projects` | list / create |
| GET/PATCH/DELETE | `/api/v1/projects/:id` | read / update / soft-delete |
| POST | `/api/v1/projects/:id/runs` | start a run (v0.1: creates + drives the skeleton loop) |
| GET | `/api/v1/projects/:id/runs` | list runs |
| GET | `/api/v1/runs/:id` | run detail incl. phases |
| POST | `/api/v1/runs/:id/cancel` | cooperative cancel |
| GET | `/api/v1/runs/:id/events` | paginated event history (`?after_seq=&limit=`) |
| GET | `/api/v1/agents` | agent roster + live status |
| GET | `/ws?token=…` | websocket stream |

Pagination is **seq/cursor based**, never offset: events are append-only and
`after_seq` is monotonic, so a reconnecting client resumes with zero duplicates
and zero gaps.

## 3. Websocket protocol

Client → server:

```json
{"type":"subscribe","topics":["run:<uuid>","project:<uuid>"],"after_seq":142}
{"type":"unsubscribe","topics":["run:<uuid>"]}
{"type":"ping"}
```

Server → client:

```json
{"type":"event","topic":"run:<uuid>","seq":143,"event":{...}}
{"type":"gap","topic":"run:<uuid>","from":143,"to":180}
{"type":"pong"}
```

Mechanics:
- One goroutine reads, one writes, per connection; the writer owns the socket.
- Bounded outbound channel (256). On overflow: drop oldest, coalesce into a
  `gap` frame. The client refetches the range over REST. **Never block the bus.**
- Heartbeat: server pings every 20 s, closes after 2 missed pongs.
- Auth: token in query string (browsers can't set WS headers), validated before
  upgrade; connection is closed when the token expires.

Fan-out: in-process `Hub` for a single node; Redis pub/sub bridge when
`GENESIS_REDIS_URL` is set, so N control-plane replicas behave as one.

## 4. Run state machine

```
        ┌─────────┐  start   ┌──────────┐
        │ PENDING ├─────────►│ RUNNING  │
        └─────────┘          └────┬─────┘
                        cancel│   │ all phases ok
                              ▼   ▼
                        ┌──────────┐   ┌───────────┐
                        │ CANCELED │   │ SUCCEEDED │
                        └──────────┘   └───────────┘
                              ▲
                    unrecoverable│
                        ┌──────────┐
                        │  FAILED  │
                        └──────────┘
```

Phase state machine: `pending → running → (succeeded | failed | skipped)`.
Transitions are written **inside the same DB transaction** as the event that
announces them. There is no window in which the UI has seen an event that the
database does not reflect.

Cancellation is cooperative: `cancel` sets `cancel_requested_at` and cancels the
run's `context.Context`; agents check it between tool calls; the sandbox gets
`SIGTERM` then `SIGKILL` after a 10 s grace period.

## 5. Event log

```
events(seq BIGSERIAL PK, id UUID, run_id, project_id, topic, type,
       agent_role, level, message, payload JSONB, created_at)
```

- `seq` is the global ordering key and the resume cursor.
- Events are **immutable**; corrections are new events.
- Retention: hot in Postgres, compacted to a per-run JSONL artifact when the run
  reaches a terminal state (keeps the table small; the artifact is what gets
  attached to a bug report).
- The `payload` schema is versioned per event type in `internal/domain/event.go`.

## 6. Concurrency model

| Unit | Model |
|---|---|
| HTTP | Fiber (fasthttp) worker pool |
| Run driver | one supervising goroutine per run + `errgroup` per parallel phase |
| Agent | one goroutine, owns its context, budget-limited |
| Hub | single-owner map guarded by RWMutex; per-subscriber writer goroutine |
| DB | pgxpool, max conns = `4 × GOMAXPROCS`, 5 s acquire timeout |
| Shutdown | `SIGINT/SIGTERM` → stop accepting → 20 s drain → cancel runs → close pools |

Rule: **no unbounded goroutine spawning.** Every fan-out is bounded by a
semaphore sized from config (`FACTORY_MAX_PARALLEL_AGENTS`, default 4).

## 7. Failure handling

| Failure | Response |
|---|---|
| DB transient | pgx retry on serialization failure, 3 attempts, jittered backoff |
| LLM timeout/garbage | schema-validated retry (max 2), then downgrade to smaller model, then fail the task with a typed error |
| Sandbox OOM/timeout | kill, emit `error.detected`, hand to the healing loop |
| Control-plane crash mid-run | run resumes from the event log on boot (v0.1: marked `interrupted` and resumable; v0.6: Temporal handles it natively) |
| Websocket drop | client reconnects with `after_seq`; no state is lost |

Idempotency: `POST` endpoints accept `Idempotency-Key`; the key + response is
cached for 24 h so a retried "start run" cannot create two runs.

## 8. Observability

- **Logs:** `log/slog` JSON, always carrying `request_id`, `run_id`, `agent`,
  `project_id`. One log schema for the whole system.
- **Metrics:** Prometheus at `/metrics` — `http_request_duration_seconds`,
  `run_phase_duration_seconds`, `agent_tokens_total`, `sandbox_exec_seconds`,
  `ws_subscribers`, `events_published_total`.
- **Tracing:** OpenTelemetry spans; a run is one trace, each phase and each LLM
  call a span. This is how "why did this build take 40 minutes" is answerable.

## 9. Configuration

12-factor, env-first, `.env` supported for local dev, everything defaulted so
`./genesis-server` with **no configuration at all** boots on SQLite at
`:8787`. Config is parsed once into a validated struct; nothing reads
`os.Getenv` outside `internal/config`.

Key variables: `GENESIS_ADDR`, `GENESIS_DB_DRIVER` (`sqlite|postgres`),
`GENESIS_DB_DSN`, `GENESIS_REDIS_URL`, `GENESIS_JWT_SECRET`,
`GENESIS_DATA_DIR`, `GENESIS_SINGLE_USER`, `GENESIS_LOG_LEVEL`,
`GENESIS_AI_ENGINE_ADDR`, `GENESIS_CORS_ORIGINS`.

Secrets: never logged, never returned by the API, redacted by a slog
`ReplaceAttr` hook that matches key names and high-entropy values.

## 10. Deployment topologies

1. **Desktop (default).** Tauri sidecar-spawns `genesis-server` on a loopback
   port with SQLite + local model. One process tree, no daemons.
2. **Workstation.** `docker compose up`: control plane, Postgres, Redis, Qdrant,
   Temporal, AI engine (GPU passthrough optional). Desktop connects over HTTP.
3. **Team/self-hosted.** Kubernetes: control plane (HPA, stateless), workers
   (KEDA on task queue depth), managed Postgres/Redis, Qdrant StatefulSet,
   Temporal cluster. Sandboxes as short-lived Jobs with gVisor.
