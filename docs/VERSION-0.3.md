# Version 0.3 — Product Intelligence

**Status:** shipped · builds clean · full suite green · benchmark 100% · verified against a real local model

v0.1 built the spine. v0.2 made it think. v0.3 makes it think **about code** — and,
for the first time, measures whether any of it is actually working.

---

## 1. Implemented features

### Model-authored business logic
The division of labour v0.1 was designed for is now real:

```
structure  → templates   (must be identical across files, or the repository
                          becomes unnavigable)
semantics  → models      (must be specific to this product, which a template
                          cannot know)
```

The model derives **domain validation rules** from the requirements — that a
deal value must be positive, that a close date lies in the future — and those
rules are compiled into the generated `Validate()` methods.

Critically, the model chooses *which constraint applies where*; it does not
write the code that implements it. Rules are rendered from a closed set of
templates, so the output is guaranteed to compile. A model proposing
`positive` on a text field produces nothing rather than a type error.

### The codegen safety layer
Generated bodies are never trusted as text. Every one is:

1. Unwrapped (models add signatures and fences despite instructions)
2. Parsed as Go with `go/parser` — catching unbalanced braces, stray prose,
   syntax errors
3. Checked against forbidden constructs (`panic`, `os.Exit`, added imports,
   goroutines, `exec`, `unsafe`)
4. Size-bounded
5. Verified to define the function that was actually requested

Anything failing is replaced with a compiling fallback. **A model cannot break
the build here — only decline to improve it.**

### Blueprint synthesis
v0.1's weakest point was the generic SaaS fallback: a user asking for a
veterinary clinic got "Organization, User, Resource" and correctly concluded the
system had not understood them.

Synthesis now derives a real blueprint for the actual domain, then *repairs* the
structural mistakes models reliably make:

| Mistake | Repair |
|---|---|
| `"vehicle fleet"` as an entity name | → `VehicleFleet` |
| `"Registration Number"` as a field | → `registration_number` |
| Several entities sharing the plural `"name"` | → derived from each entity name |
| A `ref` to a nonexistent entity | → degraded to text |
| A one-value enum | → degraded to text |
| Duplicated `id`/`created_at` | → deduplicated |
| Route without a leading slash | → normalised |
| Missing `User` entity | → added |

Then `ValidateBlueprint` gates the result against the same invariants every
built-in blueprint satisfies — verified by a test that runs the gate over all
five built-ins. If it cannot be made sound, the generic template is used and the
event stream says so.

### The benchmark harness
Quality is now a number, not an opinion. Five prompts (one per category, plus a
domain no blueprint covers) scored on facts only:

| Weight | Measure |
|---|---|
| 30% | The generated project **compiles** |
| 25% | Its **tests pass** |
| 15% | It generated at all |
| 10% | Category classified correctly |
| 10% | Expected entities present |
| 10% | Documentation complete (PRD criteria, schema, OpenAPI, deployment) |

Compilation dominates deliberately: a beautiful specification attached to code
that does not build is worth less than a plain one attached to code that does.
A test asserts the weighting itself, so the scale cannot quietly drift.

```
**Overall score:** 100.0%

| Case               | Category    | Gen | Compiles | Tests | Entities | Docs |
|--------------------|-------------|-----|----------|-------|----------|------|
| crm                | crm         | ✔   | ✔        | ✔     | 3/3      | 4/4  |
| custom-domain      | custom      | ✔   | ✔        | ✔     | —        | 4/4  |
| erp                | erp         | ✔   | ✔        | ✔     | 3/3      | 4/4  |
| marketplace        | marketplace | ✔   | ✔        | ✔     | 3/3      | 4/4  |
| project-management | pm          | ✔   | ✔        | ✔     | 3/3      | 4/4  |
```

`make bench` runs it; `make bench-report` writes comparable JSON.

---

## 2. What the benchmark found immediately

**It paid for itself on the first run**, scoring 89% and exposing a bug that had
been shipping broken ERP and marketplace projects since v0.1:

```
internal/domain/customer.go:26:16: invalid operation: m.Email != ""
    (mismatched types *string and untyped string)
```

An optional `email` field generates `*string`, but the email validation was
written assuming `string`. Every category with an optional email produced a
repository that did not compile. Two versions of manual review had missed it;
an automated check found it in four seconds. After the fix: **100%**.

---

## 3. Completed components

| Component | State |
|---|---|
| Codegen safety layer (parse, rules, fallback) | ✅ |
| Model-derived domain rules compiled into entities | ✅ |
| Blueprint synthesis with structural repair | ✅ |
| `ValidateBlueprint` gate, applied to built-ins too | ✅ |
| Benchmark harness with weighted, factual scoring | ✅ |
| Rule-message quality gate | ✅ |
| Honest attribution in the generation manifest | ✅ |
| Frontend logic generation | v0.4 |
| Execution sandbox | v0.4 |
| Self-healing loop | v0.6 |

---

## 4. How to run

```bash
make build
make bench                    # measure generation quality (no model needed)

# With local reasoning
make model-pull && make model-serve
GENESIS_LLM_URL=http://127.0.0.1:8791 ./bin/genesis-server &
./bin/genesis create "Build a CRM for a solar installer with leads and deals"
```

---

## 5. Tests

```
ok  internal/adapter/http    0.49s
ok  internal/arch            0.04s
ok  internal/domain          0.01s
ok  internal/factory        11.19s
ok  internal/infra/bus       0.02s
ok  internal/infra/crypto    0.09s
ok  internal/infra/sqlstore  0.16s
ok  internal/port            0.01s
11 passed                          (ai-engine)
```

New coverage:

- **Body validation**: syntax errors, prose, unbalanced braces, `panic`,
  `os.Exit`, injected imports, goroutines, `exec`, `unsafe`, oversized bodies,
  and bodies defining the wrong function — all rejected with actionable messages
- **Rule rendering compiles**: every rule type × field type is parsed as Go
- **Optional-field dereference**: regression test for `*m.X.IsZero()`
- **Synthesis repair**: each structural mistake above, asserted individually
- **Table-name collision**: regression from a real model run
- **Built-in blueprints pass the synthesis gate** — otherwise the gate is
  testing something the pipeline does not require
- **End-to-end compilation** for both model-derived rules and synthesised
  blueprints
- **Benchmark scoring honesty**: working code must outrank documentation, a
  perfect case must score exactly 1.0, an empty case must score near zero

---

## 6. What the live model taught us

Three defects found only by pointing the system at a real llama.cpp server,
all now regression-tested:

**Table-name collisions.** The model returned the literal word `"name"` as the
plural for three entities, collapsing them onto one table. Synthesis correctly
rejected the whole blueprint — but rejecting is worse than repairing when the
fix is mechanical. Now repaired, with the plural only trusted when it plausibly
relates to its entity.

**Echoed validation messages.** Derived rules arrived with messages lifted
verbatim from the PRD: a user would have seen *"email: Validates every required
field and returns 201 with the created record"* next to a form field. The rule
pipeline now runs the same echo critic v0.2 built for prose, plus a gate that
rejects text which is plainly an API specification rather than validation copy.

**Silent rule loss.** When synthesis is rejected, the model reasons about the
domain it proposed while code is generated from the fallback — so every derived
rule legitimately references a nonexistent field. Dropping all ten silently
looked like model failure. It now reports the distinction explicitly.

The standing honest note remains: **a 0.5B model proves the pipeline and cannot
do the job.** It still produces too few screens for synthesis to accept, and the
system still declines to ship the result. That is the architecture working.

---

## 7. Next — v0.4 Code Factory

1. **Execution sandbox**: rootless containers, resource limits, network policy —
   the prerequisite for running generated code rather than only compiling it
2. **Frontend logic generation** behind the same parse-and-reject safety layer
3. **Patch engine**: unified diffs and atomic multi-file transactions, so agents
   edit rather than only create
4. **Benchmark v2**: does the generated project *run* and serve a request, not
   just compile
5. **Multi-language toolchain adapters**: TypeScript, Python, Rust, C#

**Exit criteria:** a generated project that starts inside the sandbox and
answers a real HTTP request, verified by the benchmark.
