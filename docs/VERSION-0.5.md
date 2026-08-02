# Version 0.5 — IDE

**Status:** shipped · builds clean · 10 Go packages green · desktop typechecks and builds

v0.4 proved generated code runs. v0.5 makes it **yours**: readable, editable,
searchable, and — most importantly — reversible.

This is the release that discharges invariant **I2, human sovereignty**. A
factory that writes code you cannot inspect or change is a black box, and a
black box that authors your production system is not something anyone should
trust.

---

## 1. The patch engine

Agents could previously only *create* files. That is enough to generate a
project from nothing and useless for improving one: fixing a three-line
compilation error meant rewriting a whole file, which destroys any manual edit
made since.

Two properties make patching safe enough to hand to a model:

**Atomicity.** A patch touching six files either lands completely or not at all.
Everything is computed first; only if every hunk resolves does anything reach
disk, and a mid-write failure rolls back from a snapshot.

**Base verification.** Every edit records the hash of the content it was
computed against. If the file changed underneath, the patch is refused rather
than applied to text it was never intended for.

Hunks are anchored on content, not line numbers:

```go
Hunk{Find: "\treturn nil\n}", Replace: "\tif d.Value == \"\" { … }\n\treturn nil\n}"}
```

Line numbers shift as earlier hunks apply and are wrong the moment anything
above them changes. Content anchors are stable and self-verifying: a missing
anchor means the assumption behind the edit was false, and an ambiguous one is
rejected rather than applied to an arbitrary occurrence.

---

## 2. Git intelligence

Every generated project is now a real git repository with real history:

```
d3d3576 chore: Packaging & Deployment
80bc5f1 test: Testing & Review
01c2b18 feat: Code Generation
c7f686a docs: Design & Architecture
f1d7d24 docs: Product Analysis
```

Conventional Commits, one per phase — fine enough to roll back to any point,
coarse enough that the narrative is readable. A commit per file would bury it.

Snapshots are taken automatically, so an agent's mistake is one `git reset`
away. Rollback removes untracked files too, because a plain hard reset leaves
agent-created junk behind and a state that never existed.

Shelling out to `git` rather than using a Go library is deliberate: the
repository belongs to the user, and it must be an ordinary one they can open in
any editor and push anywhere.

---

## 3. The editor

Monaco with a real file tree, tabs, cross-file search, git history and one-click
rollback. Not a preview pane — a working editor over the generated project.

| Capability | Detail |
|---|---|
| File tree | Directories first, generated noise (`node_modules`, `dist`, `.git`) hidden |
| Editing | Syntax highlighting for 17 languages, `⌘S` / `Ctrl+S` to save |
| Conflict detection | A save whose base hash is stale returns 409 and explains why |
| Search | Debounced cross-file search with file and line jump |
| History | Commits with author, timestamp and `+/-` line counts |
| Rollback | Restore any commit from the sidebar |
| Unsaved work | Dirty markers per tab, and a warning before the window closes |

---

## 4. Completed components

| Component | State |
|---|---|
| Patch engine: atomic, base-verified, anchor-based | ✅ |
| Unified diff rendering with LCS | ✅ |
| Git snapshots per phase, Conventional Commits | ✅ |
| History, diff, status, branch, rollback | ✅ |
| Workspace API: tree, read, write, search | ✅ |
| Monaco editor with tabs and conflict detection | ✅ |
| Git panel with rollback in the desktop app | ✅ |
| Integrated terminal | v0.7 |
| Live preview pane | v0.7 |

---

## 5. Tests

```
ok  internal/adapter/http     2.23s
ok  internal/arch             0.05s
ok  internal/domain           0.01s
ok  internal/factory         16.84s
ok  internal/infra/bus        0.02s
ok  internal/infra/crypto     0.07s
ok  internal/infra/sandbox    8.15s
ok  internal/infra/sqlstore   0.19s
ok  internal/infra/vcs        0.24s
ok  internal/port             0.01s
```

The patch suite tests the failure modes a model actually produces:

- **Missing anchor** — the most common failure; the error quotes the text so a
  retry can be informed
- **Ambiguous anchor** — reports the occurrence count rather than editing an
  arbitrary match
- **Stale base** — an edit computed against older content is refused
- **Atomicity** — one bad hunk prevents the entire patch, verified by asserting
  the workspace is byte-identical afterwards
- **Confinement** — a patch cannot write outside the workspace
- **Large-file guard** — the quadratic LCS table has a cutoff, tested against a
  5000-line change with a timeout

The workspace API suite asserts path traversal is rejected for `../../etc/passwd`,
absolute paths **and `.git/config`** — editing git internals through the editor
would corrupt history irrecoverably.

---

## 6. Bugs found while building this

**Git porcelain parsing truncated filenames.** Status codes are two columns
wide, so a path starts at offset 3 for untracked files (`?? name`) but offset 2
after a single-column status (` M name`). Slicing at a fixed 3 turned
`tracked.go` into `racked.go`. Caught by asserting the *content* of the modified
list rather than only its length.

**`usecase` imported `factory` for a hash function.** The architecture test
rejected it. Rather than widen the rule, `HashContent` moved to `domain` — it
defines content identity for the patch engine, the editor and artifact
deduplication alike, so the innermost layer is where it belongs. Git access was
likewise put behind `port.VersionControl`.

---

## 7. Next — v0.6 Autonomous Product Builder

1. **Self-healing loop** — feed a verification failure back to the model,
   generate a patch, re-verify, repeat under a bounded budget. Everything this
   needs now exists: the runner reports precisely what failed, the patch engine
   applies a minimal fix atomically, and git makes every attempt reversible.
2. **Lesson memory** — record successful fixes keyed by error signature so the
   same failure is cheaper the second time
3. **Escalation** — when healing exhausts its budget, report a precise failure
   rather than looping
4. **Approval gates** by autonomy level

**Exit criteria:** a build that fails verification, repairs itself, and passes —
with every attempt visible in the event stream and revertible in git.
