# GENESIS AI FACTORY — Architecture Document

Version: 0.1 (foundation) — living document
Status: authoritative. All ADRs refine, never contradict, this file.

---

## 1. Product thesis

Genesis is an **autonomous software factory**: a user states an outcome in natural
language ("Build a Jira competitor") and the system returns a *real, runnable,
editable software repository* plus the artifacts a real company would produce
around it (PRD, architecture, schema, tests, CI, docs, deployment).

The mental model is **not** "LLM writes a file". It is:

> a simulated engineering organization, driven by a durable workflow engine,
> where every agent is a deterministic program that uses an LLM as a
> *component*, and every side effect (file write, command run, git commit) is
> a typed, audited, reversible transaction against a sandboxed workspace.

Three invariants define the product:

| Invariant | Meaning |
|---|---|
| **I1 — Determinism envelope** | LLMs are non-deterministic; the *system* is not. Every agent output is validated against a JSON Schema, every side effect goes through a tool-call layer, every run is resumable and replayable from its event log. |
| **I2 — Human sovereignty** | The user can read, edit, revert, or override anything at any time. Agents never hold an exclusive lock on the truth; the git worktree is the truth. |
| **I3 — Local-first** | The whole stack must run on one developer machine with open-weight models and zero paid API keys. Cloud models are an *optional accelerator*, never a dependency. |

---

## 2. System context (C4 level 1)

```
                        ┌──────────────────────────────┐
                        │        Human operator        │
                        └───────┬──────────────┬───────┘
                                │              │
                    desktop app │              │ terminal
                 (Tauri+React)  │              │ (genesis CLI)
                                ▼              ▼
                    ┌───────────────────────────────────────┐
                    │      CONTROL PLANE  (Go / Fiber)      │
                    │  authn/z · projects · runs · events   │
                    │  websocket hub · workflow client      │
                    └───┬───────────┬──────────┬────────────┘
                        │gRPC       │SQL       │ workflow gRPC
                        ▼           ▼          ▼
             ┌──────────────┐  ┌──────────┐  ┌──────────────┐
             │  AI ENGINE   │  │ Postgres │  │  Temporal    │
             │  (Python)    │  │  Redis   │  │  (durable    │
             │ llama.cpp /  │  │  Qdrant  │  │   workflows) │
             │ vLLM / HF    │  └──────────┘  └──────┬───────┘
             └──────────────┘                       │
                                                    ▼
                                       ┌────────────────────────┐
                                       │   FACTORY WORKERS (Go) │
                                       │  agents · tools · git  │
                                       └────────┬───────────────┘
                                                │ OCI exec
                                                ▼
                                       ┌────────────────────────┐
                                       │  EXECUTION SANDBOX     │
                                       │  Docker/Podman rootless│
                                       │  per-project workspace │
                                       └────────────────────────┘
```

### Why the control plane is Go and the AI engine is Python

The control plane is an *orchestration and I/O* problem: thousands of concurrent
websocket subscribers, streaming logs, process supervision, transactional state.
Go's goroutine model, static binaries and predictable memory make it the correct
tool, and it is a hard product requirement. Model inference is a *Python
ecosystem* problem (transformers, llama-cpp-python, tokenizers, GGUF tooling).
The seam between them is a narrow, versioned gRPC contract (`genesis.ai.v1`),
which also lets the AI engine be replaced by a remote GPU box without touching
the control plane.

---

## 3. Component decomposition (C4 level 2)

| # | Component | Language | Deliverable | Introduced |
|---|---|---|---|---|
| 1 | **Control plane** | Go 1.23 / Fiber v2 | `genesis-server` binary | v0.1 |
| 2 | **CLI** | Go / Cobra | `genesis` binary | v0.1 |
| 3 | **Desktop app** | Tauri 2 + React 18 + TS | installers (msi/deb/AppImage) | v0.1 shell → v0.5 IDE |
| 4 | **AI engine** | Python 3.11 / FastAPI + gRPC | `genesis-ai` service | v0.2 |
| 5 | **Agent runtime** | Go | in-process library + worker | v0.2 |
| 6 | **Memory service** | Go + Qdrant | library over Qdrant/pgvector | v0.2 |
| 7 | **Product intelligence** | Go + templates | blueprint compiler | v0.3 |
| 8 | **Code factory** | Go | codegen + patch engine | v0.4 |
| 9 | **Sandbox** | Go + OCI runtime | executor daemon | v0.4 |
| 10 | **Git intelligence** | Go (go-git) | library | v0.5 |
| 11 | **Autonomy loop** | Go + Temporal | workflows/activities | v0.6 |

---

## 4. Layering rules (Clean Architecture, enforced)

```
internal/
  domain/        entities + value objects + domain errors.  ZERO imports outside stdlib.
  usecase/       application services. Depends on domain + port interfaces only.
  port/          interfaces owned by the inner layers (Repository, Clock, Hasher,
                 TokenIssuer, EventBus, LLM, Sandbox, VCS).
  adapter/
    http/        Fiber handlers, DTOs, middleware. Translates HTTP <-> usecase.
    grpc/        gRPC servers/clients.
    ws/          websocket hub.
  infra/
    postgres/    pgx repositories implementing port.*Repository
    sqlite/      pure-Go fallback repositories (dev/CI, no external services)
    redis/       cache + pub/sub event bus
    memory/      in-process implementations for tests
    crypto/      argon2id hasher, JWT issuer
    logging/     slog setup
```

Dependency rule: **imports point inward only.** `domain` cannot import
`usecase`; `usecase` cannot import `adapter` or `infra`. This is verified by a
test (`internal/arch/layers_test.go`) that parses the import graph and fails the
build on violation — architecture as a unit test, not a wiki page.

Rationale for the SQLite fallback: invariant I3. `docker compose up` is *not* a
prerequisite for `go test ./...` or for a first run. Postgres is the production
store; SQLite (pure Go, `modernc.org/sqlite`, no cgo) is the zero-dependency
store. Both satisfy the same port interfaces and both are exercised by the same
repository conformance test suite, so they cannot drift.

---

## 5. Runtime model of a build ("run")

A **Run** is a durable execution of the autonomous development loop against one
project. It is a Temporal workflow (v0.6); in v0.1–0.5 it is an in-process
state machine with the identical event contract, so the swap is invisible to
clients.

```
Run
 ├─ Phase: ANALYZE      (CEO, Product Manager)
 ├─ Phase: DESIGN       (UX Designer, System Architect, Database Engineer)
 ├─ Phase: PLAN         (Architect → task DAG)
 ├─ Phase: BUILD        (Backend, Frontend, Database engineers — parallel)
 ├─ Phase: VERIFY       (QA, Security)
 ├─ Phase: HEAL         (loop back to BUILD on failure, bounded)
 └─ Phase: SHIP         (DevOps, Docs)
```

Every phase emits **events** onto an append-only log:

```
run.created · phase.started · agent.assigned · agent.thinking · tool.invoked
· file.written · command.executed · test.completed · error.detected
· heal.attempted · phase.completed · run.completed | run.failed
```

The event log is the single source of truth for the UI. The desktop app and the
CLI are both *pure projections* of it (`GET /runs/:id/events` for history +
`WS /ws` for tail). This is why the agent dashboard, the log pane and the CLI
never disagree, and why a crashed run can be resumed: replaying events
reconstructs state.

Backpressure: the hub uses per-subscriber bounded buffers (256 events) with
*drop-oldest + gap marker* semantics. A slow UI can never stall the factory.

---

## 6. Agent architecture (summary — full detail in `05-AGENTS.md`)

An agent is:

```go
type Agent interface {
    Role() Role
    Charter() Charter                 // system prompt, tools, output schema, budget
    Execute(ctx, Task, Toolbelt) (Artifact, error)
}
```

Not a prompt. A prompt is one field of a `Charter`. The agent is a Go program
that: builds context (RAG + project state) → calls the LLM with a constrained
grammar → validates output against JSON Schema → repairs on validation failure
(bounded) → executes tool calls through the audited `Toolbelt` → returns a typed
`Artifact` that is persisted and content-addressed.

Eleven roles ship: CEO, Product Manager, UX Designer, System Architect, Backend
Engineer, Frontend Engineer, Database Engineer, QA Engineer, Security Engineer,
DevOps Engineer, Improver.

---

## 7. Safety architecture

| Threat | Control |
|---|---|
| Generated code exfiltrates data | Rootless container, `--network=none` by default, egress allowlist per project |
| Generated code escapes workspace | Every path resolved and `filepath.Rel`-checked against the project root; symlinks rejected; enforced in the tool layer, not the prompt |
| Runaway agents | Per-run token/wall-clock/tool-call budgets; hard cancel via context tree |
| Destructive edits | Every mutating tool call is a git-tracked change; automatic snapshot commit before each phase; `genesis rollback` |
| Leaked secrets in generated code | Entropy + rule-based scanner on every write; blocks commit, emits `error.detected` |
| Prompt injection from fetched content | Untrusted content is fenced and never granted tool authority; tool allowlist is per-charter, not per-prompt |

---

## 8. Technology decisions (see `docs/adr/` for the full record)

| Concern | Choice | One-line reason |
|---|---|---|
| Backend language | **Go 1.23** | Required; correct for concurrent orchestration; single static binary |
| HTTP framework | **Fiber v2** | Required; fasthttp performance, familiar middleware model |
| Durable workflows | **Temporal** | Runs last hours and must survive restarts; hand-rolled state machines don't |
| Primary DB | **PostgreSQL 16** | JSONB for artifacts, LISTEN/NOTIFY, pgvector escape hatch |
| Dev DB | **SQLite (pure Go)** | Zero-dependency local start (I3) |
| Cache/bus | **Redis 7** | Pub/sub fan-out across control-plane replicas, rate limiting |
| Vector DB | **Qdrant** | Payload filtering, HNSW, runs locally in one container |
| Desktop shell | **Tauri 2** | ~10 MB binaries, ~40 MB RSS vs Electron's ~200 MB; Rust IPC |
| UI | React 18 + TS + Tailwind + shadcn/ui + Framer Motion + Monaco | Required; the exact stack of the products we benchmark against |
| Inference | llama.cpp (GGUF) primary, vLLM optional, HF transformers fallback | CPU-capable, quantized, no paid API |

---

## 9. Non-functional targets

| Metric | Target |
|---|---|
| Control-plane p99 (non-LLM endpoint) | < 25 ms |
| Event → desktop UI latency | < 100 ms |
| Desktop cold start | < 1.5 s |
| Desktop idle RSS | < 220 MB |
| Concurrent runs per control plane | 50 |
| Sandbox cold start | < 2 s (warm pool) |
| `go test ./...` wall clock | < 60 s, no external services |

---

## 10. Repository topology

Single monorepo, no submodules. Go workspace (`go.work`) so the CLI and server
share `internal` packages without publishing modules. See `04-REPOSITORY.md`.

---

## 11. What v0.1 deliberately does *not* do

No LLM calls, no sandbox, no codegen. v0.1 exists to make the **spine** real:
identity, projects, runs, the event log, the websocket hub, the CLI, the desktop
shell, migrations, CI, and the architecture test that keeps the layering honest.
Every later version plugs into this spine without redesign — which is the entire
point of building it first.
