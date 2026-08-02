# GENESIS AI FACTORY — Development Roadmap

Each version is a *shippable increment*: it builds, tests pass, and it does
something a user can observe. No version is a stub for the next one.

---

> **Status at v1.0:** every version below shipped. Two v0.7 features were
> deliberately not built — screenshot-to-application (needs a vision model,
> would break the local-first invariant) and a visual architecture designer
> (duplicates the Mermaid ERD already generated). Both are recorded here rather
> than silently dropped.

## v0.1 — Foundation (shipped)
**Goal:** a real spine — identity, projects, runs, events, realtime, CLI, desktop shell.

- Monorepo + Go workspace + Makefile + CI
- Go/Fiber control plane, Clean Architecture, layering enforced by test
- Domain: User, Project, Run, Phase, Task, Event, Agent, Artifact
- Storage: embedded migrations, SQLite (pure Go) + Postgres, one conformance suite
- Auth: Argon2id, JWT access + rotating refresh with reuse detection, RBAC
- Event log + websocket hub with resume cursor and backpressure
- Run driver: real phase state machine executing the 7-phase loop with
  deterministic scaffold agents (real artifacts, no LLM yet)
- `genesis` CLI: login, project, run, watch (live stream), events, agents
- Tauri + React desktop shell: workspace, agent dashboard, event stream, editor placeholder
- Docs: architecture, system design, DB, repo, agents, roadmap, API

**Exit criteria:** `make test` green; `genesis create "Build a CRM"` produces a
run that streams phases/events to CLI and desktop in real time.

---

## v0.2 — AI Core
**Goal:** replace scaffold agents with real inference.

- Python AI engine: FastAPI + gRPC, llama.cpp (GGUF) primary, HF transformers
  fallback, optional vLLM; model registry, download manager, VRAM-aware router
- Streaming token API, JSON-mode/GBNF grammar constraint, embeddings endpoint
- Go agent runtime: `Charter`, `Toolbelt`, schema validation + bounded repair
- CEO / PM / Architect agents producing validated artifacts from real models
- Memory v1: Qdrant, chunking, embeddings, hybrid retrieval, RAG context packer
- Cost/latency accounting per task attempt

**Exit:** "Build a CRM" yields a genuine LLM-authored vision + PRD + arch spec,
schema-valid, reproducible, fully offline.

---

## v0.3 — Product Intelligence
- Category classifier (CRM/ERP/PM/marketplace/SaaS/custom) + confidence
- Blueprint system: declarative product templates (entities, flows, screens,
  permissions, integrations) — CRM, ERP, PM, Marketplace shipped built-in
- Blueprint → requirement expansion; user story generation with acceptance criteria
- Architecture generation: service map, OpenAPI contract, ERD, ADRs
- UX agent: design tokens, component inventory, flows, structured wireframes
- Critic pass + artifact quality scoring

**Exit:** a complete, coherent product specification package for any of the four
categories, rendered in the desktop app.

---

## v0.4 — Code Factory
- Deterministic scaffolders (Go service, React app, migrations) + LLM
  in-fill for business logic — templates for structure, models for semantics
- Patch engine: unified diffs, atomic multi-file transactions, conflict handling
- Execution sandbox: rootless Docker/Podman, image cache, network policy,
  resource limits, streamed output
- Language toolchain adapters: Go, TypeScript/JS, Python, Rust, C#
- Secret scanner, dependency policy, license check on every write

**Exit:** a generated project that compiles and runs inside the sandbox.

---

## v0.5 — IDE
- Monaco: file explorer, tabs, multi-cursor, search/replace across files,
  formatting, LSP bridge (gopls, tsserver, pyright)
- AI diff review UI: per-hunk accept/reject, inline explanations
- Integrated terminal: PTY over websocket, multiple sessions, run/build/test
- Git intelligence: auto-commits, branch per feature, timeline, rollback,
  AI change explanations, review comments
- Live preview: process supervision, port detection, embedded webview, log/error pane

**Exit:** a user can drive the whole product from the desktop app without a
separate terminal or editor.

---

## v0.6 — Autonomous Product Builder
- Temporal workflows: durable, resumable, cancellable runs with retries
- Full loop end-to-end: idea → … → tests → healing → shipped repo
- QA agent: test generation, execution, coverage, flake detection
- Self-healing loop with lesson memory and escalation
- Pair-programmer mode: inline completion, refactor, explain, fix-this-error
- Approval gates by autonomy level

**Exit:** "Build a Jira competitor" → a repository that builds, tests green,
runs, with PRD/arch/docs/CI attached — unattended.

---

## v0.7 — Advanced AI
- Screenshot → UI (vision model → component tree → styled React)
- Architecture visual designer (interactive graph, edit-back to spec)
- Improver agent: gap analysis, perf profiling, security posture, UX audit
- Auto-documentation: README, API reference, ADR log, user guide
- Multi-model routing + speculative drafting for latency
- Benchmark harness: N prompts × repeatable scoring (builds? tests? lints? runs?)

---

## v1.0 — Production
- Windows/Linux signed installers, delta auto-updates
- Multi-user teams, RBAC UI, audit console, SSO (OIDC)
- Kubernetes reference deployment, Helm chart, HA control plane
- Observability: dashboards, tracing, SLOs, error budget
- Plugin SDK: custom agents, blueprints, tools
- Security hardening: gVisor sandboxes, SBOM, supply-chain attestation
- Docs site, onboarding, sample gallery

---

## Cross-cutting, every version
Tests green · docs updated · CHANGELOG · migrations reversible · no TODO left in
shipped paths · release note (`docs/VERSION-x.y.md`) with features, how to run,
tests, next roadmap.
