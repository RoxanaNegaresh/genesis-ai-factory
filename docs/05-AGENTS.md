# GENESIS AI FACTORY — Agent Architecture

---

## 1. What an agent is (and is not)

An agent is **not** a prompt with a name. It is a Go program with:

```go
type Agent interface {
    Role() domain.AgentRole
    Charter() Charter
    Execute(ctx context.Context, t domain.Task, tb Toolbelt) (domain.Artifact, error)
}

type Charter struct {
    Role         domain.AgentRole
    Mission      string          // stable system prompt
    Inputs       []ArtifactKind  // what it requires to exist
    Outputs      []ArtifactKind  // what it must produce
    OutputSchema *jsonschema.Schema
    Tools        []ToolName      // capability allowlist — enforced, not suggested
    Model        ModelHint       // size/latency class, not a hard model id
    Budget       Budget          // tokens, wall clock, tool calls, retries
    Temperature  float32
}
```

The charter is data. It is versioned, diffable, and testable. Swapping a model
or tightening a tool allowlist is a data change, not a code change.

## 2. Execution contract (the same six steps for every agent)

```
1. GATHER    project state + required input artifacts + RAG memory (top-k, filtered by project)
2. RENDER    charter.Mission + structured context + output JSON Schema
3. INFER     LLM call with grammar/JSON-mode constraint, budget-guarded
4. VALIDATE  parse → JSON Schema → domain invariants
             on failure: repair prompt with the validator error (≤2 attempts)
5. ACT       execute tool calls through Toolbelt (audited, sandboxed, allowlisted)
6. EMIT      persist typed Artifact + events; update task state
```

Step 4 is the difference between a demo and a product. **No unvalidated model
output ever reaches the filesystem or the database.**

## 3. Tool layer

Tools are the *only* way an agent touches the world.

| Tool | Signature | Guarantees |
|---|---|---|
| `fs.read` | `(path) → content` | path confined to workspace root |
| `fs.write` | `(path, content) → diff` | secret scan, size cap, `is_user_modified` check, snapshot |
| `fs.patch` | `(path, unified_diff) → result` | atomic apply, rejected hunks reported |
| `fs.list` | `(glob) → []entry` | respects `.gitignore` |
| `fs.delete` | `(path)` | soft (git rm), reversible |
| `exec.run` | `(cmd, args, cwd, timeout) → {stdout,stderr,code}` | sandboxed container, no network by default |
| `git.commit` | `(message, paths) → sha` | Conventional Commits enforced |
| `git.branch` / `git.diff` / `git.revert` | | |
| `test.run` | `(suite) → report` | structured parse of go test / vitest / pytest |
| `memory.search` | `(query, k) → []memory` | project-scoped by default |
| `memory.write` | `(kind, content)` | dedup by embedding similarity |
| `blueprint.get` | `(key) → spec` | |
| `http.fetch` | `(url) → content` | egress allowlist; result marked *untrusted* |

Every invocation is recorded (`tool.invoked` event + audit row) with arguments,
duration, and result hash. Tool authority comes from the **charter**, never from
the prompt — a prompt-injected "call exec.run" from fetched content fails at the
authorization check, not at the model's discretion.

## 4. The roster

| Agent | Consumes | Produces | Model class |
|---|---|---|---|
| **CEO** | user prompt | `product.vision` — goal, audience, differentiators, success metrics, scope guardrails | reasoning |
| **Product Manager** | vision | `product.prd` — personas, epics, user stories w/ acceptance criteria, MVP cut, roadmap | reasoning |
| **UX Designer** | prd | `design.system` (tokens, components), `design.flows`, `design.wireframes` (structured JSON, renderable) | reasoning |
| **System Architect** | prd, vision | `arch.spec` — services, boundaries, stack, API contracts (OpenAPI), NFRs, ADRs | reasoning |
| **Database Engineer** | prd, arch | `db.schema` — ERD, DDL migrations, indexes, seed data | code |
| **Backend Engineer** | arch, db.schema | Go source: domain, usecase, handlers, auth, tests | code |
| **Frontend Engineer** | design.*, arch | React+TS source: pages, components, state, API client | code |
| **QA Engineer** | all code | test suites, `qa.report`, reproducible failures | code |
| **Security Engineer** | all code | `sec.report` — findings w/ severity + patches (authz, injection, secrets, deps) | reasoning+code |
| **DevOps Engineer** | arch | Dockerfile, compose, CI, nginx, env templates, runbook | code |
| **Improver** | shipped product + reports | `improve.plan` — prioritized backlog, feeds the next run | reasoning |

Model classes map to concrete models at runtime by capability, not by name:
`reasoning` → Qwen2.5-32B-Instruct / DeepSeek-R1-Distill; `code` →
Qwen2.5-Coder-7B/32B, DeepSeek-Coder-V2; `fast` → Qwen2.5-3B for
classification/summarization. Degradation is graceful: if a 32B won't fit in
VRAM the router picks the largest that does and marks the artifact
`quality_hint: degraded`.

## 5. Orchestration

**Blackboard + DAG**, not free-form chat. Agents never talk to each other in
natural language — that's a lossy, unbounded, unverifiable channel. They read
and write **typed artifacts** on the run's blackboard. The orchestrator computes
a task DAG from artifact dependencies and schedules any task whose inputs exist.

```
        ┌── UX Designer ────┐
CEO → PM ┤                  ├→ Backend ─┐
        └── Architect → DB ─┘           ├→ QA → Security → DevOps → Improver
                        └── Frontend ───┘
```

Parallelism is bounded by `FACTORY_MAX_PARALLEL_AGENTS`. Determinism where it
matters: task ordering is stable (topological + priority + id), so two runs of
the same prompt produce the same *plan*, even if the prose differs.

### Critic pass
Each artifact class has a cheap **critic** (schema + rules + a small model) that
scores completeness before dependents consume it. A PRD with zero acceptance
criteria never reaches the Backend agent — failing early is 100× cheaper than
failing after code generation.

## 6. Self-healing loop

```go
for attempt := 1; attempt <= maxHealAttempts; attempt++ {
    res := run(build ∘ test)
    if res.OK { break }
    d := diagnose(res)                    // classify: compile | test | runtime | dep | config
    ctx := gather(d.files, d.symbols, memory.lessons(d.signature))
    fix := heal(d, ctx)                   // minimal patch, never a rewrite
    apply(fix); snapshot()
    if !improved(res, rerun()) { revert(); escalate(model↑ | agent↑) }
}
memory.write(lesson{signature: d.signature, fix: fix.summary})
```

Principles: **minimal diff** (never regenerate a file to fix one line);
**monotonic progress** (an attempt that increases failures is reverted);
**bounded** (default 5 attempts, then escalate to the user with a precise
report); **learned** (every successful fix is written to memory keyed by error
signature, so the same error is cheaper next time).

## 7. Memory & RAG

Four scopes: `global` (framework knowledge, lessons), `user` (preferences,
style), `project` (decisions, code index), `run` (scratch, discarded).

Pipeline: chunk (AST-aware for code — function/class boundaries, never
mid-token windows) → embed (bge-small / nomic-embed local) → Qdrant with payload
filters `{scope, project_id, kind, lang}` → hybrid retrieve (dense + BM25) →
rerank (cross-encoder, top-50 → top-8) → pack into a token-budgeted context with
hard priority: *task inputs > project decisions > user prefs > global lessons*.

Writes are deduplicated by cosine similarity ≥ 0.95 and decay in `importance`
unless reused — memory that is never retrieved is eventually pruned, which is
what keeps retrieval precision from collapsing as projects age.

## 8. Budgets & safety rails

Per run: token budget, wall-clock, max tool calls, max files touched, max heal
attempts. Per agent: same, scoped. Exceeding a budget is a **typed failure**
with a partial-result artifact — never an infinite loop, never a silent stop.

Autonomy levels (user-selectable per project):
- `L1 supervised` — every write requires approval
- `L2 checkpointed` — approval at phase boundaries (default)
- `L3 autonomous` — approval only on destructive ops and budget overrun
