<div align="center">

# Genesis AI Factory

**Describe a product in plain English. Get a repository that compiles, tests and runs.**

<img alt="Screenshot 2026-07-31 154242" src="https://github.com/user-attachments/assets/03d3c66e-fcc7-4568-a46d-ffb885565296" />


Local-first · No account · No API key · No telemetry

</div>

---

## Table of contents

- [What it is](#what-it-is)
- [What makes it different](#what-makes-it-different)
- [Screenshots](#screenshots)
- [Requirements](#requirements)
- [Installation](#installation)
- [Using it](#using-it)
- [What you receive](#what-you-receive)
- [Running a generated project](#running-a-generated-project)
- [Architecture](#architecture)
- [The agent organization](#the-agent-organization)
- [CLI reference](#cli-reference)
- [Configuration](#configuration)
- [Local AI models](#local-ai-models-optional)
- [Testing and quality gates](#testing-and-quality-gates)
- [Known limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)
- [Building installers](#building-installers)
- [Contributing](#contributing)
- [FAQ](#faq)
- [License](#license)

---

## What it is

Genesis AI Factory turns a sentence into a working software repository.

```
"Build an online shop with products, a basket and customer accounts"
```

Roughly thirty seconds later you have ~150 files: a Go API with PostgreSQL
persistence and authentication, a React front end, database migrations, tests,
a Dockerfile, a CI pipeline, and the documents an engineering team would
normally write by hand — PRD, architecture decisions, data model, test plan,
security review.

**It is not a chatbot and not an autocomplete.** It is a simulated engineering
organization: eleven specialist agents that hand work to each other through
seven phases, and a verification stage that compiles the result, runs its
tests, starts it and probes it before declaring it done.
<img alt="Screenshot 2026-07-31 154311" src="https://github.com/user-attachments/assets/8a5f59f7-456f-490e-8c99-12e46b2f4334" />


## What makes it different

Most code generators emit text that resembles code. Genesis is built around
three invariants, each enforced by a test that fails the build when violated:

| Invariant | How it is enforced |
|---|---|
| **Generated code compiles and runs** | Every project is built, tested, started and HTTP-probed in a sandbox before the run is marked successful |
| **The architecture cannot rot** | `internal/arch` parses the import graph and fails CI on a layering violation |
| **Quality is measured, not claimed** | A benchmark generates five product categories and scores compile/test/run/serve — the baseline is enforced at 85%, current score **100%** |

Concretely, that means the difference between *"here is some code"* and *"here
is a project that has been compiled, started, and answered a request."*

### No accounts, no API keys, no subscription

Genesis contacts nothing. Verified by generating a complete 113-file project
inside a network namespace with no route to the internet.

There is no licence check, no telemetry, no account and no paid API. The
optional `GENESIS_LLM_API_KEY` exists solely for users who point Genesis at
their *own* OpenAI-compatible endpoint that happens to require a key — a
self-hosted vLLM behind a gateway, for instance. It defaults to empty and is
never required; without any model, agents fall back to curated blueprints.

## Screenshots

| | |
|---|---|
| **Describe what you want** — one text box, four example prompts | **Watch it build** — seven phases, plain-language narration |
| **Delivery summary** — file counts, download, run instructions | **Built-in editor** — Monaco, file tree, search, git history |

> Add your own captures to `docs/images/` and link them here before publishing.

---

## Requirements

### To run Genesis

| Tool | Version | Required for |
|---|---|---|
| **Go** | 1.25+ | Control plane and CLI |
| **Node.js** | 20+ | Desktop UI |
| **Rust** | 1.85+ | Desktop shell only — CLI users can skip it |

Rust 1.85 is a hard floor: Tauri 2's dependency tree contains `edition2024`
manifests that older Cargo versions cannot parse.

**Linux also needs** GTK 3 and WebKit2GTK development headers. Tauri links the
platform webview rather than bundling one. `./scripts/desktop-deps.sh` installs
them for Debian/Ubuntu, Fedora/RHEL, Arch and openSUSE.

Genesis itself stores data in **SQLite** and needs no database server.

### To run what Genesis generates

| Tool | Version | Why |
|---|---|---|
| **PostgreSQL** | 14+ | Generated projects use pgx and real migrations |
| **Go** | 1.23+ | Generated API |
| **Node.js** | 20+ | Generated front end |

### Hardware

Two CPU cores and 2 GB RAM is enough — that is the machine this was developed
and tested on. Cargo is pinned to two build jobs in `.cargo/config.toml` so a
release build does not exhaust memory on small machines; raise it with
`cargo build -j8` on a larger one.

---

## Installation

```bash
git clone https://github.com/YOUR_ORG/genesis-ai-factory.git
cd genesis-ai-factory
chmod +x scripts/*.sh        # required if you downloaded a zip
./scripts/desktop-deps.sh    # Linux only
```

### Desktop application

```bash
make desktop
```

One command. It builds the engine, generates the application icons, stages the
engine as a sidecar inside the app bundle, and launches. The first run takes a
few minutes while Rust compiles; subsequent runs are seconds.

The engine ships **inside** the application — there is no separate server to
install, nothing to add to `PATH`, and no process left running after you close
the window.

### Command line only

```bash
make build
export GENESIS_SINGLE_USER=1
./bin/genesis-server &
./bin/genesis doctor
./bin/genesis create "Build a CRM for a sales team"
```

### Verify the installation

```bash
make test      # 10 Go packages, 227 test functions
make bench     # generation quality — expect 100%
make arch      # Clean Architecture dependency rule
```

---

## Using it

1. **Describe the product.** Be specific about the *things* in your business —
   products, orders, customers, bookings, invoices. Each becomes part of the
   system. *"Build a shop"* works; *"Build an online shop selling handmade
   furniture where customers leave reviews and I track stock"* works better.

2. **Wait.** Roughly 30 seconds. Seven phases run in order and the interface
   narrates each one:

   | Phase | What happens |
   |---|---|
   | Product Analysis | Vision and PRD from your description |
   | Design & Architecture | Screens, data model, architecture decisions |
   | Task Planning | Work broken into tasks |
   | Code Generation | Backend, frontend, migrations, tests |
   | **Testing & Review** | **Compiled, tested, started and probed** |
   | Self Healing | Any failure diagnosed and repaired |
   | Packaging & Deployment | Dockerfile, CI, runbook, README |

   Testing & Review is the slow part, and deliberately so. Most of the wall
   clock is `npm install`, `go build` and `go test` — real work that buys the
   guarantee the output actually runs.

3. **Take your project.** The delivery screen shows what was built and offers
   **Download project** (a normal zip), **View the code** (built-in editor) and
   **Open folder**.

A full walkthrough written for non-technical users lives in
**[`GUIDE.md`](GUIDE.md)**.

---

## What you receive

```
your-project/
├── api/                    Go service — Fiber, Clean Architecture
│   ├── cmd/server/         Composition root
│   ├── internal/domain/    Entities and invariants
│   ├── internal/port/      Repository interfaces
│   ├── internal/usecase/   Application services
│   ├── internal/adapter/   HTTP handlers, auth middleware
│   └── internal/infra/     PostgreSQL repositories, Argon2id, JWT
├── web/                    React + TypeScript + Tailwind + Vite
├── migrations/             SQL schema, up and down
├── docs/                   PRD, architecture, data model, test plan,
│                           security review, runbook, improvement plan
├── docker-compose.yml      Full stack
├── .github/workflows/      CI pipeline
├── Makefile
└── README.md               Written for a developer
```

**Included in every generated project:**

- **Authentication** — Argon2id password hashing, HS256 tokens verified
  signature-first, rotating refresh tokens whose replay revokes the whole
  session family
- **Persistence** — PostgreSQL repositories written against pgx, keyset
  pagination, soft deletes, constraint violations mapped to proper HTTP codes
- **Transactions** — a `port.UnitOfWork` carrying its transaction on the
  context, so repositories enlist automatically
- **Protected routes** — applied at group level, so a handler added later is
  private by default rather than public by omission
- **Health and readiness** — separated, because liveness must not fail when the
  database blips

---

## Running a generated project
<img alt="Screenshot 2026-07-31 140836" src="https://github.com/user-attachments/assets/cc9a9546-bf4a-4cfe-8b87-200811cc1b21" />

```bash
cd your-project

# 1. Database
createdb app
psql -d app -f migrations/0001_init.up.sql

# 2. API
cd api
go mod tidy
export DATABASE_URL="postgres://postgres@localhost:5432/app?sslmode=disable"
export JWT_SECRET=$(openssl rand -hex 32)
go run ./cmd/server

# 3. Front end (second terminal)
cd web && npm install && npm run dev
```

```bash
curl localhost:8080/health   # {"status":"ok"}
curl localhost:8080/ready    # {"database":"ok","status":"ready"}
```

Resource routes require authentication:

```bash
TOKEN=$(curl -s -X POST localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","display_name":"You","password":"a-sufficiently-long-password"}' \
  | jq -r .tokens.access_token)

curl -H "Authorization: Bearer $TOKEN" localhost:8080/api/v1/products
```

If you are handing the project to a developer, its own `README.md` is all they
need.

---

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│  Desktop (Tauri 2 + React)      CLI (genesis)            │
│  Supervises the engine as a     Same HTTP API            │
│  child process                                           │
└────────────────────┬─────────────────────────────────────┘
                     │  HTTP + WebSocket · 127.0.0.1:8787
┌────────────────────▼─────────────────────────────────────┐
│  Control plane — Go, Clean Architecture                  │
│                                                          │
│  adapter/  HTTP handlers, websocket hub                  │
│  usecase/  auth, projects, runs, workspace               │
│  factory/  11 agents, blueprints, codegen, verification  │
│  port/     interfaces — LLM, Sandbox, VCS, storage       │
│  domain/   entities and invariants (no imports outward)  │
│  infra/    SQLite/Postgres, sandbox, git, OpenAI client  │
└────────────────────┬─────────────────────────────────────┘
                     │
┌────────────────────▼─────────────────────────────────────┐
│  Sandbox — Linux namespaces, rlimits, no host env        │
│  Builds, tests, starts and probes generated projects     │
└──────────────────────────────────────────────────────────┘
```

**Deliberate deviations from the obvious choices**, each documented in
[`docs/01-ARCHITECTURE.md`](docs/01-ARCHITECTURE.md):

- **Linux namespaces, not Docker.** Requiring Docker Desktop would break
  local-first for a desktop application. Isolation degrades gracefully and is
  reported honestly through `IsolationReport`.
- **SQLite by default, PostgreSQL for production.** Both sit behind one
  conformance suite, so neither can drift.
- **In-process run driver, not Temporal.** Identical event contract; the swap
  point is preserved.
- **git by shelling out, not a library.** The repository belongs to the user
  and must be an ordinary git repository.

### Statistics

| | |
|---|---|
| Go | 34,713 lines · 227 test functions |
| TypeScript | 2,965 lines |
| Rust | 403 lines |
| Python | 583 lines |
| Documentation | 17 documents |
| Blueprints | 5 (CRM, project management, ERP, marketplace, SaaS) |

---

## The agent organization

Eleven agents, each producing real artifacts rather than narration:

| Agent | Role | Produces |
|---|---|---|
| **Atlas** | Chief Executive | Vision, success criteria |
| **Nova** | Product Manager | PRD, user stories with acceptance criteria |
| **Iris** | UX Designer | Screen flows, design tokens |
| **Vector** | System Architect | Architecture, decision records, OpenAPI |
| **Strata** | Database Engineer | Schema, migrations, ERD |
| **Forge** | Backend Engineer | Domain, ports, use cases, handlers, repositories |
| **Prism** | Frontend Engineer | React screens, API client, layout |
| **Sentry** | QA Engineer | Test plan — **and runs the project it reviews** |
| **Aegis** | Security Engineer | Security review, threat notes |
| **Relay** | DevOps Engineer | Dockerfile, compose, CI, runbook |
| **Kaizen** | Improvement Agent | Static analysis of the real output |

Kaizen is worth singling out: it parses the generated project with `go/parser`
and reports what is actually true of it — unimplemented interfaces, unmounted
handlers, test coverage ratio. On a current build it reports **0 high, 0
medium** findings.

---

## CLI reference

```
BUILD
  genesis create "Build a CRM system"      Describe a product and build it
  genesis create ... --no-watch            Start without streaming
  genesis watch <run-id>                   Stream a build's event log
  genesis status <run-id>                  Phase-by-phase progress
  genesis cancel <run-id>                  Stop a running build

INSPECT
  genesis projects                         List your projects
  genesis agents [run-id]                  Roster, or a live board
  genesis artifacts <run-id>               List generated documents
  genesis artifacts <run-id> --name PRD.md Print one document
  genesis blueprints                       Built-in product templates
  genesis models                           Whether reasoning is active
  genesis analyze [path]                   Analyse an existing codebase

ACCOUNT
  genesis doctor                           Check environment and connection
  genesis login / register                 Multi-user installations
```

---

## Configuration

Every setting has a working default. Nothing must be configured to start.

| Variable | Default | Purpose |
|---|---|---|
| `GENESIS_ADDR` | `127.0.0.1:8787` | Listen address |
| `GENESIS_DATA_DIR` | platform config dir | Projects, database, session |
| `GENESIS_SINGLE_USER` | `true` | Skip login on a personal machine |
| `GENESIS_DB_DRIVER` | `sqlite` | `sqlite` or `postgres` |
| `GENESIS_DB_DSN` | — | Required when driver is `postgres` |
| `GENESIS_JWT_SECRET` | generated | Signing key |
| `GENESIS_ACCESS_TTL` | `15m` | Access token lifetime |
| `GENESIS_REFRESH_TTL` | `720h` | Refresh token lifetime |
| `GENESIS_LLM_URL` | — | OpenAI-compatible endpoint |
| `GENESIS_LLM_MODEL` | — | Model identifier |
| `GENESIS_LLM_API_KEY` | — | Only if *your* endpoint needs one |
| `GENESIS_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Client side: `GENESIS_API`, `GENESIS_TOKEN`, `NO_COLOR`.

---

## Local AI models (optional)

Genesis generates complete projects with **no model at all**, using curated
blueprints. A local model adds product-specific reasoning: domain validation
rules derived from your description rather than from a template.

```bash
make models        # what fits this machine, RAM-aware
make model-pull    # Qwen2.5-0.5B GGUF, open weights, ~469 MB
make model-serve   # llama.cpp on 127.0.0.1:8791
make run-ai        # control plane with reasoning enabled
```

Everything stays on your machine. Any OpenAI-compatible server works —
llama.cpp, vLLM, Ollama, LM Studio.

**Honest note on model size:** a 0.5B model is large enough to prove the
machinery and too small to author production design documents. The critic pass
detects degenerate output and falls back to blueprints, which is the designed
behaviour, but meaningful model-authored design needs a larger model.

---

## Testing and quality gates

```bash
make test                 # every suite
make bench                # generation quality across 5 categories
make arch                 # Clean Architecture dependency rule
make lint                 # go vet + gofmt
make verify-persistence   # end-to-end proof against real PostgreSQL
make heal-demo            # self-healing on a deliberately broken project
make test-race            # race detector
make test-cover           # coverage report
```

`verify-persistence` is the one to run if you want convincing: it generates a
product, applies its schema, builds it, runs its tests against a live server,
then exercises CRUD and the full authentication lifecycle over HTTP — 18
assertions ending with *"replaying a retired token revokes the whole family"*.

### Current benchmark

| Case | Generates | Compiles | Tests | Runs | Serves | Score |
|---|---|---|---|---|---|---|
| CRM | ✔ | ✔ | ✔ | ✔ | ✔ | 100% |
| Project management | ✔ | ✔ | ✔ | ✔ | ✔ | 100% |
| ERP | ✔ | ✔ | ✔ | ✔ | ✔ | 100% |
| Marketplace | ✔ | ✔ | ✔ | ✔ | ✔ | 100% |
| Custom domain | ✔ | ✔ | ✔ | ✔ | ✔ | 100% |

---

## Known limitations

Stated plainly. Every one of these is a real constraint, not a caveat.

1. **Only Go and Node projects are verified end to end.** Python, Rust and C#
   are a `Toolchain` table entry away but are not written.
2. **Generated projects need PostgreSQL.** Genesis itself does not.
3. **Node build steps run without a memory ceiling.** Any `RLIMIT_AS` breaks
   WebAssembly, which esbuild uses; a number large enough to avoid that would
   be a limit in name only. Every other sandbox constraint still applies. The
   honest fix is cgroup v2, which needs a delegated subtree.
4. **The sandbox shares the host filesystem for reads.** Writes, network and
   credentials are confined; reads are not. Reported via
   `IsolationReport.Degraded`.
5. **On Windows and macOS there is no namespace isolation** — that is a Linux
   facility. The isolation report says so rather than implying confinement it
   does not have.
6. **No integrated terminal or live preview pane** in the desktop app.
7. **Cannot open an existing project** from disk. Genesis works with projects
   it generated. Importing arbitrary repositories is a genuinely different
   feature and is not implemented.
8. **A 0.5B model is too small for real design work** (see above).
9. **`go test -race` OOMs on SQLite-dependent packages** on a 2 GB machine.
   Environment limit, not a code defect.
10. **Generated integrations are scaffolded, not connected.** Payments, email
    and similar are structured correctly but not wired to real accounts.

---

## Troubleshooting

<details>
<summary><b>"The engine did not start"</b></summary>

The bundled engine failed to launch. On that screen:

1. **Restart engine**
2. **View log** — the engine's own output, the actual diagnosis
3. **Data folder** — where projects and the database live

Log locations: `%APPDATA%\genesis\engine.log` (Windows),
`~/Library/Application Support/genesis/engine.log` (macOS),
`~/.config/genesis/engine.log` (Linux).

After extracting a zip, the commonest cause is a lost executable bit:
`chmod +x bin/genesis-server`.
</details>

<details>
<summary><b>"token expired"</b></summary>

**Not a licence or billing problem.** It is your own local sign-in session.
Access tokens live 15 minutes because they cannot be revoked; the client now
refreshes them silently and the server keeps `session.json` fresh.

If you are on an older build, restart the app. For longer sessions:
`GENESIS_ACCESS_TTL=8h`.
</details>

<details>
<summary><b><code>Package 'glib-2.0' not found</code> while building</b></summary>

GTK development headers are missing. The error names a Rust crate rather than
the distribution package, which sends people to the wrong place.

```bash
./scripts/desktop-deps.sh
```
</details>

<details>
<summary><b><code>proc macro panicked</code> / <code>icon is not RGBA</code></b></summary>

Icons are generated, not committed. Tauri requires true 8-bit RGBA and
ImageMagick optimises small images into palette PNGs.

```bash
make icons
file apps/desktop/src-tauri/icons/32x32.png
# PNG image data, 32 x 32, 8-bit/color RGBA, non-interlaced
```
</details>

<details>
<summary><b><code>feature edition2024 is required</code></b></summary>

Cargo is older than 1.85. If `rustc --version` looks new enough, a rustup
directory override is pinning an old toolchain:

```bash
rustup show active-toolchain    # look for "(directory override)"
rustup override unset
rustup update stable
```
</details>

<details>
<summary><b>Build fails with a Go runtime stack trace</b></summary>

Almost always memory, not a compiler bug. `rustc` and the Go compiler both hold
large arenas; on a small machine one gets OOM-killed and prints a stack dump.

Cargo is already pinned to two jobs. For Go, the sandbox grants build steps
3 GiB of address space and `-p=1`. If you still see it, close other
applications and retry.
</details>

<details>
<summary><b>Generated project will not build</b></summary>

Check the build log in the run view. The commonest causes are a missing
`go mod tidy`, or forgetting `GOWORK=off` when the project sits inside another
Go workspace.
</details>

---

## Building installers

Tauri links the platform webview, so bundles cannot be cross-compiled — each
must be produced on its own operating system. The release workflow does all
three:

```bash
git tag v1.2.0 && git push --tags
```

Produces MSI and NSIS for Windows, DMG for macOS, DEB and AppImage for Linux,
attached to a draft GitHub release.

Locally, for the current platform:

```bash
make build-desktop    # → apps/desktop/src-tauri/target/release/bundle/
```

**Code signing is not optional for distribution.** Unsigned installers trigger
SmartScreen on Windows and Gatekeeper on macOS. Full instructions, including
the release checklist and support diagnostics, are in
[`docs/SHIPPING.md`](docs/SHIPPING.md).

---

## Contributing

```bash
make test && make lint && make arch    # must pass before a PR
```

The architecture test is not advisory. `internal/arch` parses the import graph
and fails on a layering violation: `domain` imports nothing outward, `usecase`
depends only on `port` and `domain`, and infrastructure is reachable only
through interfaces. If your change needs to break that rule, the rule is
probably right and the change needs rethinking.

Repository layout:

| Path | Contents |
|---|---|
| `services/control-plane/` | Go control plane |
| `services/ai-engine/` | Python model manager |
| `apps/desktop/` | Tauri + React application |
| `apps/cli/` | `genesis` command line |
| `docs/` | Architecture, agents, roadmap, version notes |
| `scripts/` | Dependency installer, verification script |

---

## FAQ

**Does this cost anything?**
No. No account, no subscription, no API key, no telemetry. Disconnect from the
internet and it still works — that is a tested property, not a claim.

**Is the generated code really mine?**
Yes. Ordinary code in ordinary languages, Apache 2.0. No licence check, no
phone-home, nothing that stops working if you stop using Genesis.

**Why is it slower than a chatbot?**
Because it compiles and runs what it writes. A chatbot returns text that looks
like code in two seconds. Genesis returns a project that has been built,
tested, started and probed, in about thirty.

**Can I edit the result?**
Yes — in the built-in Monaco editor with git history and rollback, or in any
editor after downloading.

**Can I open an existing project?**
Not yet. See limitation 7.

**Is it production-ready?**
The factory is. Generated projects are a solid foundation — real
authentication, real persistence, real tests — but they are a starting point,
not a finished business. Read the generated `IMPROVEMENT_PLAN.md`, which is
candid about what each project still needs.

**What happens with no AI model configured?**
It works. Blueprints cover CRM, project management, ERP, marketplace and SaaS,
and unknown domains get a synthesised blueprint with structural repair. A model
adds product-specific reasoning on top.

---


</div>
