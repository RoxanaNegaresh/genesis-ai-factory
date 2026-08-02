# GENESIS AI FACTORY — Repository Structure

Monorepo. Go workspace at the root ties the server and CLI together; pnpm
workspace ties the desktop app and shared TS packages together.

```
genesis-ai-factory/
├── go.work                        # Go workspace: services/control-plane, apps/cli
├── Makefile                       # one entry point for every task
├── README.md
├── docker-compose.yml             # full stack: postgres, redis, qdrant, temporal, ai-engine
│
├── docs/
│   ├── 01-ARCHITECTURE.md
│   ├── 02-SYSTEM-DESIGN.md
│   ├── 03-DATABASE.md
│   ├── 04-REPOSITORY.md
│   ├── 05-AGENTS.md
│   ├── 06-ROADMAP.md
│   ├── 07-API.md
│   ├── VERSION-0.1.md             # release note per version
│   └── adr/                       # architecture decision records
│
├── services/
│   ├── control-plane/             # Go / Fiber — the spine
│   │   ├── go.mod
│   │   ├── cmd/genesis-server/main.go
│   │   └── internal/
│   │       ├── config/            # env parsing + validation
│   │       ├── domain/            # entities, value objects, errors  (no deps)
│   │       │   ├── user.go project.go run.go task.go event.go agent.go
│   │       │   ├── artifact.go errors.go id.go
│   │       ├── port/              # interfaces owned by inner layers
│   │       │   ├── repository.go clock.go hasher.go token.go bus.go llm.go
│   │       ├── usecase/           # application services
│   │       │   ├── auth.go project.go run.go agent.go
│   │       ├── adapter/
│   │       │   ├── http/          # Fiber handlers, DTO, middleware, router
│   │       │   └── ws/            # hub, subscriber, protocol
│   │       ├── infra/
│   │       │   ├── sqlstore/      # shared SQL repositories (sqlite + postgres)
│   │       │   ├── migrate/       # embedded migrations runner
│   │       │   ├── crypto/        # argon2id, jwt
│   │       │   ├── bus/           # in-process event bus (+ redis bridge later)
│   │       │   └── logging/
│   │       ├── factory/           # run driver / phase engine (agents land here in 0.2)
│   │       └── arch/              # layering enforcement test
│   │
│   └── ai-engine/                 # Python — v0.2
│       ├── pyproject.toml
│       └── genesis_ai/            # server.py, models/, prompts/, embeddings/
│
├── apps/
│   ├── desktop/                   # Tauri 2 + React + TS
│   │   ├── package.json vite.config.ts tailwind.config.ts
│   │   ├── src-tauri/             # Rust shell: sidecar supervision, IPC, updater
│   │   └── src/
│   │       ├── main.tsx App.tsx
│   │       ├── lib/               # api client, ws client, store, utils
│   │       ├── components/ui/     # shadcn primitives
│   │       ├── components/        # TitleBar, Sidebar, AgentCard, EventStream…
│   │       └── views/             # Workspace, Agents, Editor, Terminal, Preview
│   │
│   └── cli/                       # Go / Cobra — `genesis`
│       ├── go.mod
│       └── cmd/genesis/ + internal/commands/
│
├── packages/
│   └── shared-types/              # TS types generated from Go domain (single source of truth)
│
├── migrations/                    # numbered SQL, embedded into the server binary
├── deploy/                        # Dockerfiles, nginx, k8s, CI workflows
└── scripts/                       # dev bootstrap, codegen, release
```

## Conventions

- **Package naming:** short, lowercase, no `utils`/`common`/`helpers`. A package
  is named for what it *provides*.
- **Errors:** typed sentinels in `domain` (`ErrNotFound`, `ErrConflict`,
  `ErrUnauthorized`, `ErrValidation`), wrapped with `%w`, mapped to HTTP once in
  a single middleware. Handlers never build error envelopes by hand.
- **No global state.** Dependencies are constructor-injected; `main.go` is the
  only composition root.
- **Tests** live beside code (`_test.go`). Repository implementations share one
  conformance suite so SQLite and Postgres cannot diverge.
- **Generated code** is committed (protobuf stubs, TS types) so a clean clone
  builds without codegen toolchains.
- **Commit convention:** Conventional Commits; the Git Intelligence agent
  (v0.5) emits the same format, so human and AI history are indistinguishable in
  tooling.
