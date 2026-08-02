# Version 1.2 — Auth, Transactions, Frontend Runtime

**Status:** shipped · **Date:** 2026-07-30

v1.1 made generated products store data. v1.2 closes the three gaps that
remained between "stores data" and "is a product": nothing authenticated,
nothing could write to two tables atomically, and the frontend was never run.

---

## 1. Authentication

Before this, every generated product carried a `users` table, demanded a
`JWT_SECRET` at boot, and advertised `Authorization` in its CORS headers —
while issuing no tokens, checking no tokens, and leaving every resource route
public. The contract test that listed those routes skipped all of them, which
is the worst state a test can be in: green, and asserting nothing.

**Password hashing — Argon2id.** bcrypt truncates at 72 bytes and has no memory
hardness, so a GPU attacks it cheaply. Argon2id at 64 MiB is the RFC 9106
memory-constrained profile; a card with thousands of cores cannot give each one
64 MiB, so the parallelism that makes bcrypt cheap to attack never materialises.
Hashes are stored in PHC format with their parameters embedded, so raising the
cost factor later does not lock out existing accounts — an old hash verifies
with the parameters it was created with.

**Tokens — HS256, verified signature-first.** The JWT is hand-rolled: about
eighty lines for the subset used, one fewer dependency, and — the real reason —
every historical JWT vulnerability lives in the flexible parts. `alg: none`,
HMAC/RSA confusion, unverified `kid` lookups. A verifier that accepts exactly
one algorithm and checks the signature *before* parsing any claim cannot be
confused about which one it is.

**Sessions — rotating refresh tokens, stored hashed.** Each refresh mints a
replacement and retires its predecessor. Presenting a retired token means
replay, so the whole family is revoked: a silent theft becomes a visible logout.
Only the SHA-256 hash is stored, so a database leak yields no working sessions.
SHA-256 is correct here where Argon2id is correct for passwords — the input is
256 bits from a CSPRNG, so brute force is infeasible regardless, and refresh is
a hot path.

**Route protection is applied to the group, not per route.** Opt-in protection
fails open: the day someone adds a handler and forgets the middleware, that
endpoint is public and nothing complains. A test now asserts that no resource
handler is mounted outside the guarded group.

**Login does not say which half was wrong**, and hashes even when the account
does not exist. Returning early on a missing account makes it measurably faster
than a wrong password, and that timing difference is the same enumeration
oracle the uniform error message was written to close.

## 2. Transactional unit of work

Repository methods were each atomic, which does not compose: two atomic writes
are two outcomes, and the failure mode is a half-finished business operation.

The transaction rides on the `context`. Two alternatives were rejected —
passing a transaction argument to every method puts a database concept into the
inner-layer ports, and returning a separate set of "transactional repositories"
doubles the constructor surface and lets the two sets drift. Context carriage
keeps `port.UnitOfWork` free of database vocabulary and makes enlistment
automatic.

Nested calls join the enclosing transaction; PostgreSQL has no true nesting and
a second connection would deadlock against the first one's locks. Rollback uses
`context.WithoutCancel`, because a rollback on an already-expired context is a
no-op that leaves the transaction holding locks until the server times it out.

## 3. Frontend runtime verification

`NodeToolchain` runs install → build → typecheck → `vite preview` → HTTP probe.
Preview is used rather than dev because preview serves the built artefact, which
is what ships; dev serves through the transform pipeline and can succeed on a
bundle that would fail in production. A frontend failure is reported separately
and does not fail the backend report — different artefacts, different toolchains.

---

## Bugs found by running it

| Bug | Found by | Severity |
|---|---|---|
| **Family revocation was rolled back with the transaction that returned the error.** The response said "all sessions have been revoked" while every stolen token kept working. | Live replay test against PostgreSQL | **Critical** |
| Any `RLIMIT_AS` breaks WebAssembly — `vite build` failed at 2, 4 and 6 GiB, succeeded with no ceiling | Sandbox probe | High |
| Vite ignores `PORT`, so the probe waited on a port nothing was bound to | Frontend stage timing out | High |
| Two access tokens minted in the same second were byte-identical (`iat`/`exp` are second-precision) | Generated rotation test | Medium |
| OpenAPI documented only `/auth/login` while the router mounted four endpoints | Spec-vs-router test | Medium |
| Sandboxed builds inherited no `TMPDIR`, filled tmpfs, and the linker died with a SIGSEGV trace that read like a compiler bug — **benchmark fell to 35% and blamed the generated projects** | Benchmark regression | High |
| A generated Go comment lost its `//` and produced a syntax error | Compiling the output | Low |

The revocation bug is the one worth dwelling on. It was introduced by doing the
right thing in the wrong place: revoking inside the unit of work, which is
exactly where you would put it, and then returning an error from the same
closure. The rollback that makes the abstraction correct is what made the
security control useless. No unit test would have caught it — only a test that
commits, then asks a second time.

## Verified

Against **PostgreSQL 17.10**, generated CRM:

```
resource route, no token     → 401
register                     → 201, email normalised, password_hash absent
login, wrong password        → 401 invalid_credentials
login, unknown account       → 401 invalid_credentials   (identical: no oracle)
login, uppercase email       → 200
password shorter than 12     → 422
refresh                      → 200, token rotated
replay the retired token     → 401 refresh_token_reused
descendant token afterwards  → 401   (family genuinely revoked)
refresh after logout         → 401
```

Transactions: rollback, commit, all-or-nothing across two writes, nesting
without deadlock, inner-failure-rolls-back-outer — all against a live server.

Frontend: `install ✔  build ✔  typecheck ✔  serve ✔  probe 200 ✔`

Factory: 10 Go packages green · `go vet` clean · `gofmt` clean · architecture
rule enforced · benchmark **100%** across crm, pm, erp, marketplace, custom ·
Improver on a fresh project **0 high, 0 medium**.

**Totals:** 34,153 Go LOC · 220 Go test functions · 2,108 TS · 583 Py.

---

## Still not done

1. **Only Go and Node are verified.** Python, Rust and C# adapters are a
   `Toolchain` table entry away but are not written.
2. **Node processes run without a memory ceiling.** Any `RLIMIT_AS` breaks
   WebAssembly, and a number large enough to avoid that would be theatre. The
   honest fix is a cgroup v2 memory limit, which bounds resident memory instead
   of address space; it needs a delegated subtree this host will not grant. The
   swap point is `port.Sandbox`.
3. **The sandbox shares the host filesystem for reads.** Reported via
   `IsolationReport.Degraded`.
4. **No integrated terminal or live preview pane** in the desktop app. The
   runner now makes preview genuinely possible.
5. **A 0.5B model is too small for real design work.** Critics catch the
   degenerate output and fall back to blueprints.
6. `go test -race` OOMs on sqlite-dependent packages on a 2-CPU / 2 GB machine.

## Next

1. Live preview pane in the desktop app, reusing `NodeToolchain`.
2. Integrated terminal (PTY over websocket) over the existing sandbox.
3. Python and Rust toolchain adapters.
4. cgroup v2 sandbox backend for hosts that permit delegation.
