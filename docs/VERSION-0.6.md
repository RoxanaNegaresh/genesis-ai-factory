# Version 0.6 — Autonomous Product Builder

**Status:** shipped · 10 Go packages green · self-repair verified end to end

This is the capability the previous five versions were building toward, and it
only became possible once each piece existed:

| Prerequisite | Shipped in | What healing needs it for |
|---|---|---|
| Verification runner | v0.4 | Knowing precisely *what* failed |
| Patch engine | v0.5 | Applying a minimal fix atomically |
| Git snapshots | v0.5 | Making every attempt reversible |
| Constrained inference | v0.2 | Getting a structured repair, not prose |

```
attempt 1: patched=[api/internal/domain/deal.go] improved=true reverted=false
healing: repaired after 1 attempt(s):
  the generated service builds, tests, starts and answers requests
```

---

## 1. The loop

```
verify → diagnose → propose patch → apply → re-verify
                          ↑                      │
                          └──── if improved ─────┘
                                if not: revert
```

Four principles govern it, each chosen because its opposite is a known way for
automated repair to make things worse.

**Minimal diff.** The repair schema has no field in which to return a rewritten
file — only anchored edits. A model asked to "fix this" will happily regenerate
a whole file and discard working code to fix one line.

**Monotonic progress.** An attempt that does not reduce the failure count is
reverted from a snapshot taken before it ran. Without this, a model fixing one
error while creating two walks the project steadily downhill and reports
progress the whole way.

**Bounded.** A fixed attempt budget. A model that cannot fix an error in three
tries will not fix it in thirty, and an unbounded loop is how a defect becomes
an unbounded spend.

**Learned.** Every successful fix is written to memory keyed by an error
signature, so the same failure is cheaper the second time.

---

## 2. Diagnosis

Classification decides which files the model sees, what it is asked for, and
whether to attempt repair at all. Getting it wrong wastes an inference call on a
problem no patch can fix.

| Category | Repairable | Why |
|---|---|---|
| `compile` | yes | The compiler names the file, line and mistake |
| `test` | yes | A failing assertion is a code problem |
| `startup` | yes | Usually a missing configuration guard |
| `health` | yes | Usually a route or handler problem |
| `dependencies` | **no** | A proxy outage cannot be fixed by editing source |

### Signatures transfer between projects

```
crm/internal/domain/contact.go:26:16: invalid operation: m.Email != "" (mismatched types…)
erp/internal/domain/supplier.go:41:16: invalid operation: m.Email != "" (mismatched types…)
                                    ↓
                    compile:invalid operation: f.email != q …
```

Line numbers, quoted identifiers and file paths are stripped. Both produce the
same signature, so a lesson learned on one build applies to the next — which is
the only way memory is worth keeping.

---

## 3. Tests

The healing suite tests the loop, not the model. A scripted provider returns a
known-correct repair, a known-harmful one, and one that never applies:

- **Repairs a broken project** — a real generated CRM is broken with an invalid
  operator, then verified broken, healed, and verified working. The assertion is
  that the *file no longer contains the defect* and the service serves HTTP.
- **Reverts unhelpful repairs** — a model that introduces a second error must
  have its change rolled back, asserted by reading the file afterwards.
- **Respects the attempt budget** — a model whose patches never apply must stop
  at the budget, not loop.
- **Signatures are stable across projects** and do not collide between genuinely
  different errors.
- **Dependency failures are marked unrepairable** rather than burning attempts.

---

## 4. Next — v0.7

The Improver agent still produces a backlog derived from what the factory
*intended* to generate rather than from the code in front of it.
