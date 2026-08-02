# Version 0.1 — Foundation

**Status:** shipped · builds clean · full suite green under `-race`

The goal of v0.1 was to make the *spine* real. Every later version plugs into
it without redesign, which is only true if the spine is genuinely load-bearing
rather than a sketch. This release is therefore deliberately narrow and
deliberately complete: no LLM, no sandbox, but everything that exists actually
works and is tested.

---

## 1. Implemented features

### Design (preceded all code, as required)
Seven documents totalling the full system design: architecture with C4
decomposition and safety model, system design with protocols and state
machines, database design with every table and index justified, repository
topology, agent architecture, roadmap to v1.0, and this release note.

### Control plane — Go 1.25 / Fiber v2
- **Clean Architecture**, enforced by an executable test rather than convention
- **Identity**: Argon2id (`t=3, m=64MiB, p=2`, PHC-encoded, transparent
  parameter upgrade on login), JWT access tokens, rotating refresh tokens with
  **reuse detection that revokes the entire token family**
- **RBAC** evaluated in the use case layer, never in handlers
- **Projects** with workspace provisioning, slug collision resolution, soft
  delete that preserves generated source
- **Runs**: the seven-phase development loop as a real state machine, with
  cooperative cancellation and crash reconciliation on boot
- **Append-only event log** with gapless monotonic cursors
- **Websocket hub**: single-writer connections, heartbeats, resume-from-cursor,
  bounded buffers with drop-oldest and gap markers
- **Storage**: embedded migrations with checksum drift detection; SQLite (pure
  Go, zero dependencies) and PostgreSQL behind one implementation and **one
  conformance suite**
- Request IDs, structured logging with credential redaction, strict CORS
  allowlist, security headers, rate-limited auth endpoints, one error envelope
- Graceful shutdown with request draining

### Product generation engine
- **Deterministic classifier** over a weighted lexicon with confidence and
  runner-up reporting — instant, free, offline, reproducible
- **Five blueprints**: CRM (7 entities), Project Management (10), ERP (16),
  Marketplace (13), SaaS (7) — each with personas, epics, entities, screens,
  roles and non-functional requirements

### The organization — eleven agents
Each with a `Charter` (mission, typed inputs and outputs, tool allowlist, model
class, budget), coordinating through a typed blackboard. Real deliverables per
run: vision, PRD with acceptance criteria, design system with tokens, user
flows, architecture spec, ADRs, **parseable OpenAPI 3.0**, ERD, **runnable
PostgreSQL DDL in dependency order**, layered Go source with tests, typed React
client, test plan, security review, Dockerfiles, compose, CI, runbook,
improvement plan.

### Tool layer
Path confinement verified with `filepath.Rel` (traversal, absolute paths and
symlink escapes rejected), secret scanning with placeholder discrimination,
size caps, atomic write-and-rename.

### CLI — `genesis`
`create · watch · status · agents · artifacts · projects · blueprints ·
analyze · cancel · login · register · doctor`. Live streaming, colour that
disables itself when piped, and Ctrl-C that detaches the viewer rather than
killing the build.

### Desktop — Tauri 2 + React 18 + TypeScript
Token-driven design system (dark and light), AI workspace with live
classification preview, run view with pipeline chips, agent dashboard, event
stream with intelligent auto-scroll, artifact browser. Rust shell supervises
the Go engine as a child process, waits for health, injects the loopback token,
and guarantees termination on close. Production bundle: **312 KB (100 KB
gzipped)**.

---

## 2. Completed components

| Component | State |
|---|---|
| Monorepo + Go workspace + Makefile | ✅ |
| Control plane (Go/Fiber, Clean Architecture) | ✅ |
| Domain model, ports, use cases | ✅ |
| SQLite + PostgreSQL storage, one conformance suite | ✅ |
| Auth, RBAC, token rotation | ✅ |
| Event log + websocket hub with backpressure | ✅ |
| Run driver, seven-phase loop, task DAG | ✅ |
| Blueprints + classifier | ✅ |
| Eleven agents producing real artifacts | ✅ |
| Workspace toolbelt with confinement + secret scan | ✅ |
| CLI | ✅ |
| Desktop shell (workspace, agents, stream, artifacts) | ✅ |
| Docker, compose, CI, docs | ✅ |
| LLM inference | v0.2 |
| Execution sandbox | v0.4 |
| Monaco editor, terminal, git | v0.5 |
| Self-healing loop | v0.6 |

---

## 3. How to run

**Requires Go 1.25+. Nothing else.**

```bash
make build
./bin/genesis-server &
./bin/genesis doctor
./bin/genesis create "Build a Jira competitor with kanban boards and sprints"
```

Desktop:

```bash
cd apps/desktop && npm install && npm run desktop
```

Server topology:

```bash
export GENESIS_JWT_SECRET=$(openssl rand -hex 32)
docker compose up -d
```

Verify the generated product really works:

```bash
cd ~/.config/genesis/workspaces/*/jira-competitor*/api
go test ./...            # generated tests pass
go build ./cmd/server    # produces a working binary
```

---

## 4. Tests

```
ok  internal/adapter/http    0.70s
ok  internal/arch            0.03s
ok  internal/domain          0.01s
ok  internal/factory         0.20s
ok  internal/infra/bus       0.03s
ok  internal/infra/crypto    0.09s
ok  internal/infra/sqlstore   0.13s
```

Green under `-race`. Desktop `tsc --noEmit` clean. Whole suite runs in under
60 seconds with **no external services**.

Notable coverage, chosen for what would actually break in production:

- **Auth**: forged payloads, `alg:none`, wrong secret, expiry, clock skew,
  Argon2 rehash-on-policy-change, timing equivalence between unknown account
  and wrong password
- **Repository conformance**: the same suite against SQLite and PostgreSQL —
  this is what caught a real `$N`-reuse bug that is valid Postgres and broken
  SQLite, now prevented structurally by a panic in `rebind`
- **Event bus**: 10,000 messages to a subscriber that never reads, asserting
  the publisher never blocks, the newest events survive, and a gap marker is
  emitted
- **Concurrency**: race detector across the suite — it caught the driver
  goroutine and the HTTP serialiser sharing a `Run` aggregate, now fixed by
  explicit ownership transfer via `Clone()`
- **Toolbelt**: `../`, absolute paths, `~`, embedded traversal — all rejected,
  with the filesystem asserted clean afterwards
- **Generated output**: SQL foreign keys never forward-reference, no floating
  point for money, OpenAPI covers every entity and never exposes
  `password_hash`, artifacts are byte-reproducible across runs
- **Isolation**: a second user gets 404 (not 403) on every route of another
  user's project
- **Generated code compiles**: the suite invokes the real Go toolchain on a
  generated project, builds it and runs the tests the factory wrote. Parsing
  proves syntax; only this proves validity. It caught an unused-import defect
  that appeared solely in the marketplace blueprint, and a generated assertion
  that was false by construction.
- **Every blueprint parses**: import usage is verified per file across all five
  categories, because a defect present in one category ships broken code for
  that category
- **Architecture**: verified to actually fail — a deliberate violation was
  introduced and the test caught it

---

## 5. Honest assessment

The generated repository is a **complete specification and a structurally
correct, compiling skeleton**. Its domain layer, validation, enums and tests are
real and pass. What it does not yet contain is business logic wired to a
database — repositories are interfaces without PostgreSQL implementations, and
handlers are written but not mounted. The Improver agent says exactly this in
its output rather than reporting success.

That gap is the v0.2–v0.4 scope, and the boundary was chosen deliberately:
template-driven *structure* plus model-authored *logic* is the right division of
labour. A model that invents a different layout per file produces an
unmaintainable repository; a template that tries to encode business semantics
produces a toy. v0.1 built the half that must be consistent.

---

## 6. Next — v0.2 AI Core

1. **Python AI engine**: FastAPI + gRPC, llama.cpp (GGUF) primary, HF
   transformers fallback, vLLM optional; model registry, download manager,
   VRAM-aware router with graceful degradation
2. **Constrained decoding**: JSON-mode / GBNF grammars so output is schema-valid
   by construction rather than by retry
3. **Agent runtime**: `Charter` → context packing → inference → JSON Schema
   validation → bounded repair → typed artifact
4. **Real inference** for CEO, Product Manager and Architect first — the
   artifacts where model judgement adds the most over a template
5. **Memory v1**: Qdrant, AST-aware chunking, hybrid retrieval, reranking,
   token-budgeted context packing
6. **Accounting**: per-attempt tokens, latency and cost on `task_attempts`

**Exit criteria:** `genesis create "Build a CRM"` produces a genuinely
LLM-authored vision, PRD and architecture spec — schema-valid, reproducible,
and fully offline on a laptop.
