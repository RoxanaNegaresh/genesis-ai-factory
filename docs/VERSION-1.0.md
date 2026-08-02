# Version 1.0 — Production Release

**Status:** shipped · 10 Go packages green · benchmark 100% · self-healing verified

Genesis AI Factory turns a natural-language brief into a real, runnable,
editable software repository — and proves it runs.

```
$ genesis create "Build a Jira competitor with kanban boards and sprints"

› Interpreted as Project & Issue Tracking (pm, 99% confidence)

▸ Product Analysis      Atlas → VISION.md      Nova → PRD.md
▸ Design & Architecture Iris  → DESIGN_SYSTEM  Vector → ARCHITECTURE, openapi.yaml
                        Strata → 10 tables with indexes
▸ Task Planning         11 tasks, 21 dependencies
▸ Code Generation       Forge → 36 Go files    Prism → 20 TypeScript files
▸ Testing & Review      Sentry → build ✔ test ✔ serve ✔ probe 200 ✔
▸ Packaging             Relay → Docker, CI, runbook
                        Kaizen → analysed 80 files: 2 high, 1 low findings

✔ Build complete — 95 files, 16 artifacts
```

---

## What it does

**Generates a complete product** from one sentence: vision, PRD with testable
acceptance criteria, design system, architecture with ADRs, a parseable OpenAPI
contract, PostgreSQL DDL in dependency order, a layered Go service, a typed
React client, tests, Docker, CI, and a runbook.

**Proves it works.** Every generated project is built, tested, started and sent
a real HTTP request inside an isolated sandbox. The benchmark measures this
across all five product categories, and it currently scores 100%.

**Repairs itself.** A build that fails verification is diagnosed, patched,
re-verified and — if the repair did not help — reverted. Bounded, monotonic,
and remembered.

**Stays yours.** A Monaco editor over the generated workspace, cross-file
search, git history per phase, and one-click rollback. Nothing is locked.

**Runs locally.** No paid API, no container runtime, no cloud dependency. A
model is optional: the factory works without one and thinks better with one.

---

## Architecture at 1.0

```
        Desktop (Tauri + React + Monaco)          CLI (genesis)
                        │                              │
                        └──────────────┬───────────────┘
                                       ▼
                    ┌──────────────────────────────────────┐
                    │   CONTROL PLANE (Go / Fiber)          │
                    │   auth · projects · runs · workspace  │
                    │   event log · websocket hub           │
                    └──┬─────────┬──────────┬───────────┬───┘
                       ▼         ▼          ▼           ▼
                   Storage    Factory    Sandbox     Git
              SQLite/Postgres 11 agents  namespaces  history
                              blueprints  rlimits    rollback
                              healing    no network
                                 │
                                 ▼
                          AI engine (optional)
                        llama.cpp · vLLM · Ollama
```

Clean Architecture enforced by a test that parses the import graph and fails the
build on a violation. It caught three real violations during development, each
of which was fixed by moving code to the correct layer rather than widening the
rule.

---

## Version history

| Version | Capability | Proof |
|---|---|---|
| **0.1** Foundation | Spine: identity, projects, runs, events, CLI, desktop | Generated project compiles, 22 generated tests pass |
| **0.2** AI Core | Local inference, constrained decoding, repair, memory, critics | Real llama.cpp; critics reject degenerate output |
| **0.3** Product Intelligence | Model-authored domain rules, blueprint synthesis, benchmark | Benchmark 100%; found a bug shipping since v0.1 |
| **0.4** Code Factory | Namespace sandbox, five-stage verification | Generated service answers `GET /health` with 200 |
| **0.5** IDE | Patch engine, git intelligence, Monaco editor | Atomic multi-file edits; rollback restores state |
| **0.6** Autonomous Builder | Self-healing loop | Broken project repairs itself and serves |
| **0.7** Advanced AI | Real static analysis of generated code | Findings named per project, verified to change with the code |
| **1.0** Production | Consolidation, honest documentation | 10 packages green, benchmark 100% |

---

## Bugs found by our own tooling

The tests and benchmark repeatedly found real defects that review had missed.
Each is worth recording because each argues for a specific kind of test:

| Bug | Found by | Why review missed it |
|---|---|---|
| `$1` reused twice — valid Postgres, broken SQLite | Repository conformance suite | Only appears on the second backend |
| Data race between run driver and HTTP serialiser | `-race` | Timing-dependent |
| Optional `email` generating `*string` compared to `string` | Benchmark, first run | Only in ERP and marketplace |
| Path confinement accepting `../` after normalisation | Sandbox security test | `Clean("/"+"../")` silently *looks* correct |
| Git porcelain parsing truncating filenames | Asserting content, not length | `racked.go` looks like a plausible name |
| `*m.Date.IsZero()` dereferencing a bool | Compiling generated output | Only with an optional timestamp rule |
| CLI silently dropping flags after positional args | Manual use during development | The command still "worked" |

The pattern: **defects hide in the case you did not test**, and the highest-value
tests are the ones that run the real thing — a second database, the race
detector, an actual compiler, an actual HTTP request.

---

## Honest limitations

These are stated plainly because a product that overstates itself cannot be
trusted about anything else.

**Generated projects are complete scaffolding, not finished products.** The
domain layer, validation, enums, tests, schema, API contract and deployment are
real and work. Repositories are interfaces without PostgreSQL implementations,
and handlers are written but not mounted. The Improver agent reports exactly
this, per project, with the interfaces named — it does not claim otherwise.

**Model quality bounds output quality.** A 0.5B model proves the pipeline and
cannot author production design documents. The critics detect this and fall back
to blueprints, which is the architecture working correctly rather than a
workaround. Larger models are one `make model-pull` away.

**The sandbox shares the host filesystem for reads.** Generated code cannot
reach the network, cannot escape the workspace for writes, and cannot see host
credentials — but it can read host binaries. This is an accepted trade for
desktop use and is reported through `IsolationReport` rather than glossed over.
A server deployment should use an OCI executor or gVisor behind the same
`port.Sandbox` interface.

**Only Go is verified end to end.** The generated frontend is typechecked and
built but not started and probed. TypeScript, Python, Rust and C# toolchain
adapters are defined but not wired.

**Not built, deliberately:** screenshot-to-application needs a vision model and
would break local-first; a visual architecture designer would duplicate the
Mermaid ERD already generated.

---

## Running it

```bash
./scripts/dev-setup.sh     # installs Go into the workspace
make build
make test                  # 10 Go packages, 11 Python tests, desktop typecheck
make bench                 # generation quality across every category
make heal-demo             # watch a broken project repair itself

./bin/genesis-server &
./bin/genesis create "Build a CRM for a solar panel installation company"
```

With local reasoning:

```bash
make models && make model-pull && make model-serve
GENESIS_LLM_URL=http://127.0.0.1:8791 ./bin/genesis-server &
```

Desktop:

```bash
cd apps/desktop && npm install && npm run desktop
```

---

## What 1.0 means

It does not mean finished. It means the architecture is settled, the invariants
hold, the claims are tested, and the limitations are documented rather than
hidden.

Everything above is verified by something that runs: `make test`, `make bench`,
`make heal-demo`. Where a claim could not be verified in this environment, it is
listed as a limitation instead of being asserted.
