# Version 0.2 — AI Core

**Status:** shipped · builds clean · full suite green under `-race` · verified against a real local model

v0.1 built the spine. v0.2 makes it *think*: real inference, schema-constrained
decoding, bounded repair, long-term memory, and — the part that turned out to
matter most — a critic that refuses model output which is technically valid and
substantively worthless.

---

## 1. Implemented features

### Inference (`port.LLM` + `infra/llm`)
- **OpenAI-compatible provider**, which is the wire format llama.cpp, vLLM,
  Ollama, LM Studio and the hosted APIs all speak. Targeting the protocol rather
  than a vendor is why local-first and optional cloud acceleration are one code
  path, not two.
- **Constrained decoding** via `response_format: json_schema`. The server
  enforces the grammar during sampling, so output is valid JSON matching the
  schema *by construction* rather than by retrying until the model complies.
- **Capability classes** (`reasoning` / `code` / `fast`) instead of model names.
  A charter says what it needs; the router decides what the machine can serve.
- Model discovery with caching, health probing, and error messages that name the
  actual problem ("cannot reach the model server at …, is it running?").

### Schema validation and repair (`port.Validator`)
A hand-written JSON Schema subset validator, deliberately not a dependency. It
must run on every response — including from providers with no grammar support —
and it must emit prose a *model* can act on, not JSON pointers. It reports every
defect in one pass, because repairing one error per round trip would multiply
latency by the number of mistakes.

The repair loop: generate → extract → validate → feed the validator's own
complaints back as a correction turn → bounded at two retries. A model that
cannot satisfy a schema in three attempts will not satisfy it in thirty.

### Agent runtime (`factory.Reasoner`)
The single choke point every model interaction passes through, which is what
makes the determinism envelope real rather than aspirational:
- Context-window discovery and prompt budgeting
- Token accounting per attempt, aggregated per run
- Fixed seed for reproducibility where the provider honours it
- Truncation detection with a distinct retry ("your response was cut off")
- Transport errors abort immediately instead of burning the repair budget

### The critic pass
Constrained decoding guarantees *shape*, not *substance*. Two detectors, both
found by watching a real 0.5B model:

| Detector | Catches |
|---|---|
| `critique` | Repetition — near-duplicate entries across a list |
| `critiqueEcho` | Parroting — output that restates its own input |

When either fires the agent discards the model output, says so in the event
stream, and falls back to the blueprint. Shipping repetitive filler that *looks*
authored is worse than shipping an honest template.

### Memory (`factory.MemoryService`)
Hybrid retrieval: lexical overlap always, cosine similarity when an embedder is
configured. Pure-vector memory would be unavailable on a machine without an
embedding model; pure-lexical misses paraphrase. Combined, it degrades in the
right direction. Includes scope isolation (a project's decisions can never leak
into another), deduplication, importance weighting and usage tracking.

Architecture decisions are written to memory automatically, so a later run does
not contradict an earlier one.

### Model manager (`genesis-ai`)
The four manual steps between "installed Genesis" and "agents can reason",
reduced to one command each:

```
genesis-ai list     # what fits in this machine's RAM
genesis-ai pull     # download it, resumable, verified
genesis-ai serve    # start llama.cpp correctly configured
genesis-ai doctor   # diagnose what is missing
```

Selection applies a 30% headroom margin. Recommending a model that *just* fits
produces swapping and a user who concludes local inference does not work.

---

## 2. Completed components

| Component | State |
|---|---|
| `port.LLM` / `port.Embedder` / `port.MemoryStore` interfaces | ✅ |
| OpenAI-compatible provider with constrained decoding | ✅ |
| JSON Schema validator with actionable repair prompts | ✅ |
| Agent runtime: budgets, repair, accounting, seeding | ✅ |
| Context-window discovery and prompt budgeting | ✅ |
| Critic pass: repetition and echo detection | ✅ |
| Memory service with hybrid retrieval and scope isolation | ✅ |
| CEO, Product Manager, Architect on real inference | ✅ |
| Model catalogue, downloader, server launcher | ✅ |
| `/api/v1/models`, `genesis models`, desktop status badge | ✅ |
| Graceful degradation to blueprints throughout | ✅ |
| Code-generating agents on inference | v0.3 |
| Qdrant-backed memory at scale | v0.3 |
| Execution sandbox | v0.4 |

---

## 3. How to run

**Without a model** — unchanged from v0.1, still zero dependencies:

```bash
make build && ./bin/genesis-server &
./bin/genesis create "Build a CRM system"      # 307ms, 80 files
```

**With local reasoning:**

```bash
make models          # see what fits
make model-pull      # download it
make model-serve     # start inference on :8791

GENESIS_LLM_URL=http://127.0.0.1:8791 ./bin/genesis-server &
./bin/genesis models                            # confirm reasoning is on
./bin/genesis create "Build a CRM for a solar panel installation company"
```

---

## 4. Tests

```
ok  internal/adapter/http    3.9s
ok  internal/arch            0.03s
ok  internal/domain          0.01s
ok  internal/factory        12.9s   (-race)
ok  internal/infra/bus       0.03s
ok  internal/infra/crypto    0.09s
ok  internal/infra/sqlstore  0.13s
ok  internal/port            1.0s
11 passed                            (ai-engine, pytest)
```

New coverage, chosen for the failure modes real models actually exhibit:

- **Repair loop**: invalid → corrected in one retry, with assertions that the
  repair prompt *names the failing fields* and demands complete JSON
- **Repair bound**: exactly 1 + 2 calls, never a fourth — an unbounded loop is
  how a defect becomes an unbounded spend
- **Prose recovery**: markdown fences, preambles, trailing commentary, and
  braces *inside string literals* that must not terminate the scan
- **Truncation**: distinct retry path, verified the model is told it was cut off
- **Transport errors**: must abort in one call, not consume the repair budget
- **Context sizing**: output capped to half the window, budget reserves room for
  the reply — the regression test for a bug found in production
- **Critic**: degenerate and echoed output both rejected, genuine output accepted
- **Local-first**: the full pipeline with reasoning disabled still produces every
  artifact
- **Total model failure**: six consecutive transport errors, build still completes
- **Memory**: relevance ranking, cross-project isolation, deduplication, and
  operation with no embedder at all

---

## 5. What running a real model taught us

This is the part that could not have been designed in advance. Three defects
were found only by pointing the system at an actual llama.cpp server with
Qwen2.5-0.5B, and all three are now regression-tested:

**Context overflow.** The architect agent failed with
`request (4112 tokens) exceeds the available context size (4096)`. The prompt
assembler had a budget, but it was a number I chose, not the model's actual
window. Fixed by discovering the window from the provider and reserving output
space inside it.

**Degenerate output.** The PM agent produced five schema-valid user stories that
were near-verbatim repetitions of each other and of the prompt — "As a Company,
I want to Lead qualification, Pipeline & deals … so that the system will track
all customer interactions." Every field satisfied its constraints. This is why
the critic exists.

**Echoed input.** The architect then passed the repetition check while copying
the PRD's user stories into its "architecture decisions" — distinct entries,
zero new information. A second detector was needed, because it is a different
defect wearing the same clothes.

The honest summary: **a 0.5B model is large enough to prove the pipeline and too
small to do the job.** The v0.2 machinery — constrained decoding, validation,
repair, critics, graceful degradation — is exactly what makes that statement
safe rather than embarrassing. The system detects its own model's inadequacy and
degrades to something defensible.

---

## 6. Next — v0.3 Product Intelligence

1. **Code-generating agents on inference**: Backend and Frontend agents fill
   logic bodies inside the deterministic structure, which is the division of
   labour v0.1 was built for
2. **LLM-assisted classification** for briefs the lexicon scores as low-confidence
3. **Blueprint synthesis**: derive a new blueprint for a category not in the
   catalogue, instead of falling back to generic SaaS
4. **Critic v2**: a small model scoring artifact completeness against acceptance
   criteria, not just structural heuristics
5. **Qdrant memory** with AST-aware chunking for the code index
6. **Benchmark harness**: N prompts × repeatable scoring (builds? tests pass?
   lints? runs?) so quality changes are measured rather than felt

**Exit criteria:** a generated project where the business logic — not just the
scaffolding — was authored by a model, compiles and passes its own tests.
