# Version 0.7 — Advanced AI

**Status:** shipped · 10 Go packages green

v0.7 replaces the last piece of the factory that was still describing its own
intentions rather than reality.

---

## 1. Real static analysis

Until now the Improver agent produced a backlog derived from what the factory
*meant* to generate. It said the same thing for every project, because it never
looked at one. That is a template dressed as an analysis, and it is worse than
no analysis: it looks like insight and carries none.

The Improver now parses the generated source with `go/parser` and reports only
what is true of that specific project:

```
80 files (36 Go, 7 tests) across 8 packages, 1840 lines. 7 entities, 30 registered endpoints.

Findings: 2 high, 0 medium, 1 low.

## HIGH priority

### 6 repository interfaces have no implementation
The use cases are complete but nothing persists data: ActivityRepository,
CompanyRepository, ContactRepository, DealRepository, LeadRepository,
PipelineStageRepository.
Do this: Implement each repository against PostgreSQL using pgx, then wire
them in cmd/server.

### 7 handler files are never mounted on the router
`api/cmd/server/main.go`
Every resource handler is generated but registerRoutes wires none of them, so
the API surface is unreachable.
```

Those two findings are the honest, precise statement of the gap between what
the factory produces today and a finished product — named interfaces, real
counts, and the exact file to change. Previous releases described that gap in
prose in the release notes; the product now finds it itself, per project.

### How findings are derived

| Finding | Derived from |
|---|---|
| Unimplemented repositories | AST: interfaces ending in `Repository` with no implementing struct |
| Unmounted handlers | The body of `registerRoutes` contains no `.Register(` call |
| Thin test coverage | Ratio of `_test.go` files to source files, not an absolute count |
| Unparseable source | `go/parser` returning an error |
| Missing operational files | The filesystem, checked rather than assumed |
| TODO markers | Counted in source |

A finding that is not true of the code is a bug in the analyser, and the tests
enforce that: adding a Dockerfile, a CI workflow and migrations to a bare
project must make the corresponding findings *disappear*.

---

## 2. Tests

- **Reports real gaps** in a genuinely generated project, asserting the two
  known structural gaps are named
- **Responds to the actual project** — findings vanish when the missing pieces
  are supplied
- **Detects unparseable source** and marks it high severity
- **Orders by severity** so the important things are first
- **Renders a useful report** with counts, locations and actions

---

## 3. What was deliberately not built

Two features from the original v0.7 scope were dropped after examination:

**Screenshot to application.** This requires a vision model. The local-first
invariant means shipping a feature that only works with a paid cloud API would
either break the invariant or ship broken. A stub that "supports" it without
working would be worse than its absence.

**Architecture visual designer.** An interactive graph that edits back into the
spec is a substantial product in itself, and the same information is already
delivered as a Mermaid ERD in `DATA_MODEL.md`, which renders in every markdown
viewer and in the editor. Building a bespoke canvas to duplicate that would be
effort spent on novelty rather than value.

Both are recorded in the roadmap rather than silently dropped.
