# Version 0.4 — Code Factory

**Status:** shipped · builds clean · full suite green · benchmark 100% on the execution bar

v0.3 could prove a generated project **compiles**. That is a meaningful bar and a
low one: a service that compiles and then panics on startup, binds no port, or
returns 500 to every request is not a working product.

v0.4 closes that gap. Generated code is now built, tested, **started, and sent a
real HTTP request** — inside an isolated sandbox with no network access.

```
| Stage   | Result | Time  | Detail |
|---------|--------|-------|--------|
| install | ✔ pass | 42ms  |        |
| build   | ✔ pass | 791ms |        |
| test    | ✔ pass | 217ms |        |
| serve   | ✔ pass | 457ms |        |
| probe   | ✔ pass | 1ms   | 200    |

Executed under user, mount, pid, ipc, uts, net isolation (network isolated: true).
```

---

## 1. The execution sandbox

### Why not Docker

The v0.1 architecture named Docker as the sandbox. Building it revealed that
requirement to be wrong for the primary deployment target.

Genesis is a desktop application. Requiring a container runtime means a user
cannot run generated code until they install and configure Docker Desktop —
exactly the friction the local-first invariant exists to eliminate. Worse, this
very environment has no container runtime at all, so a Docker-based sandbox
would have been untestable as well as unusable.

Linux namespaces are in the kernel. They need no daemon, no root, and no
installation, and they provide the controls that actually matter:

| Control | Mechanism | Equivalent to |
|---|---|---|
| No network access | empty network namespace | `--network=none` |
| Isolated process tree | PID namespace | container PID 1 |
| Confined writes | mount namespace + path validation | bind-mounted volume |
| Memory / process / file ceilings | rlimits via `prlimit` | `--memory`, `--pids-limit` |
| No host credentials | environment allowlist | `--env-file` only |

**What this does not provide, and Docker would:** a separate filesystem image,
so generated code can *read* host binaries and libraries. That is an accepted
trade for the desktop case, and it is reported through `IsolationReport` rather
than glossed over. A server deployment should use an OCI executor or gVisor —
the `port.Sandbox` interface exists so that is a substitution, not a rewrite.

### Honest capability reporting

The executor probes what this host actually supports by attempting it at boot,
because `/proc` knobs, seccomp, AppArmor and container-in-container all affect
the answer in ways configuration cannot predict. Every result carries what was
achieved:

```go
type IsolationReport struct {
    Namespaces         []string
    NetworkIsolated    bool
    FilesystemConfined bool
    MemoryLimited      bool
    Degraded           []string  // requested but unavailable
}
```

A caller can therefore distinguish "it ran safely" as a guarantee from "it ran"
as an aspiration. The server logs this at startup and the QA report includes it.

---

## 2. The verification runner

A five-stage pipeline where each stage gates the next, expressed as a data
table so adding a language is a table entry rather than a new code path:

| Stage | Network | Why |
|---|---|---|
| install | **host** | `go mod tidy` cannot work without the module proxy |
| build | none | compilation needs nothing external |
| test | none | tests that need the network are not unit tests |
| serve | host | the probe runs outside the namespace and must be able to reach it |
| probe | — | a real `GET /health` over TCP |

Network access is granted at exactly two points, both necessary, both reported.
Everything else runs with an empty network namespace.

Startup detection polls the TCP socket rather than parsing output, because a
service may log nothing at all; output is used only to diagnose an early crash.
A free port is allocated before launch to avoid colliding with whatever else is
running on a developer's machine.

---

## 3. Benchmark v2

The scoring was reweighted so that **running outranks compiling**:

| Weight | Measure |
|---|---|
| 25% | **Starts and answers a request** (10% start, 15% respond) |
| 22% | Compiles |
| 18% | Tests pass |
| 10% | Generated at all |
| 10% | Documentation complete |
| 15% | Category and entities correct |

A test asserts the weighting itself — that running scores above compiling, that
a perfect case is exactly 1.0 — so the scale cannot quietly drift toward
measuring what is easy instead of what matters.

```
**Overall score:** 100.0%

| Case               | Category    | Gen | Compiles | Tests | Runs | Serves | Docs |
|--------------------|-------------|-----|----------|-------|------|--------|------|
| crm                | crm         | ✔   | ✔        | ✔     | ✔    | ✔      | 4/4  |
| custom-domain      | custom      | ✔   | ✔        | ✔     | ✔    | ✔      | 4/4  |
| erp                | erp         | ✔   | ✔        | ✔     | ✔    | ✔      | 4/4  |
| marketplace        | marketplace | ✔   | ✔        | ✔     | ✔    | ✔      | 4/4  |
| project-management | pm          | ✔   | ✔        | ✔     | ✔    | ✔      | 4/4  |
```

Every generated product, in every category, now runs.

---

## 4. Completed components

| Component | State |
|---|---|
| `port.Sandbox` / `port.Process` interfaces | ✅ |
| Namespace executor (user, mount, pid, ipc, uts, net) | ✅ |
| Resource limits via `prlimit` | ✅ |
| Host-credential isolation | ✅ |
| Process-group kill on timeout | ✅ |
| Output capping | ✅ |
| Port discovery from process output | ✅ |
| Five-stage verification runner | ✅ |
| QA agent runs the project it reviews | ✅ |
| Benchmark measures execution | ✅ |
| Patch engine for multi-file edits | v0.5 |
| Frontend runtime verification | v0.5 |
| Self-healing from verification failures | v0.6 |

---

## 5. Tests

```
ok  internal/adapter/http     0.50s
ok  internal/arch             0.05s
ok  internal/domain           0.01s
ok  internal/factory         16.60s
ok  internal/infra/bus        0.02s
ok  internal/infra/crypto     0.12s
ok  internal/infra/sandbox    8.32s
ok  internal/infra/sqlstore   0.12s
ok  internal/port             0.00s
```

The sandbox suite tests isolation as a security property, not a feature:

- **Network egress is blocked** — asserts exactly one interface (loopback)
  inside the namespace, and that isolation is the *default* for a request that
  says nothing about networking
- **Host secrets do not leak** — sets `GENESIS_JWT_SECRET` and `DATABASE_URL`,
  then asserts neither appears in the sandboxed environment
- **Path traversal is rejected** — `../`, `/etc`, `sub/../../..`
- **The whole process group dies** on timeout, verified by a child process that
  touches a file every 200ms
- **Output is capped** so a print loop cannot exhaust server memory
- **Relaxed isolation is reported**, so an audit can find every place it happened
- **Port discovery** across four framework log formats, and ignores unrelated
  numbers in build output

---

## 6. Bugs found while building this

**Path confinement was defeated by its own normalisation.** The original check
was `filepath.Join(root, filepath.Clean("/"+dir))`. For `dir = "../"` that
collapses to `/`, which resolves harmlessly to the workspace root — so the
escape was *accepted* rather than rejected, with the caller believing its path
was honoured while confinement quietly rewrote it. Validation now happens before
normalisation, and symlinks are resolved and re-checked.

**The CLI silently dropped flags after positional arguments.** `genesis
artifacts <id> --name QA_REPORT.md` listed everything and ignored `--name`,
because Go's flag package stops parsing at the first positional argument. Users
type flags last; silently discarding them is worse than an error.

---

## 7. Next — v0.5 IDE

1. **Patch engine** — unified diffs and atomic multi-file transactions, so
   agents edit rather than only create. This is the prerequisite for healing.
2. **Monaco editor** with a file tree, tabs, search, and per-hunk accept/reject
   of AI changes
3. **Integrated terminal** — PTY over websocket, reusing the sandbox
4. **Git intelligence** — auto-commits, branch per feature, rollback
5. **Live preview** — the running generated service embedded in the desktop app,
   which the runner already makes possible

**Exit criteria:** a user can drive the whole product from the desktop app —
read the plan, edit generated code, run it, and see it — without a separate
terminal or editor.
