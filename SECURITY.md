# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| 1.2.x | ✅ |
| < 1.2 | ❌ |

## Reporting a vulnerability

Please report privately rather than in a public issue.

Use [GitHub's private vulnerability reporting](../../security/advisories/new),
or email the maintainers. Include what you found, how to reproduce it, and what
an attacker could achieve.

You can expect an acknowledgement within a few days and an assessment shortly
after. If the report is valid you will be credited in the advisory unless you
prefer otherwise.

## Threat model

Genesis is a **local-first desktop application**. It binds `127.0.0.1` only,
contacts no external service, and stores everything under the user's own data
directory. The most valuable reports concern:

### Sandbox escape

Generated code is compiled and executed. It runs under Linux namespaces with
rlimits, an empty network namespace outside dependency installation, and an
environment that never inherits the host's — so no credential is visible to it.

Known and documented gaps, which are **not** vulnerabilities:

- The sandbox shares the host filesystem for reads. Writes are confined.
  Reported through `IsolationReport.Degraded`.
- Windows and macOS have no namespace isolation; that is a Linux facility. The
  isolation report says so rather than implying confinement it lacks.
- Node build steps run without an address-space ceiling, because any
  `RLIMIT_AS` breaks WebAssembly. Every other constraint still applies.

A way to *escape the constraints that are claimed* is a vulnerability. A
consequence of a limitation already documented is not.

### Authentication

Generated projects ship Argon2id password hashing, HS256 tokens verified
signature-first with one permitted algorithm, and rotating refresh tokens whose
replay revokes the whole session family.

Reports of interest: token forgery, algorithm confusion, a way to bypass route
protection, or a flaw in the rotation logic.

### Generated code

Genesis writes authentication and persistence into every project. A weakness in
what it *generates* affects every user's output and is treated with the same
seriousness as a flaw in Genesis itself.

## Out of scope

- Findings that require an attacker to already have local access — the
  application trusts the user running it
- Missing rate limits on a loopback-only interface
- Vulnerabilities in a generated project caused by user modification
- The documented limitations above

## What Genesis does not do

For clarity, because it narrows the attack surface considerably:

- No network calls to any external service
- No telemetry, analytics or crash reporting
- No account system, licence check or remote authentication
- No auto-update mechanism
