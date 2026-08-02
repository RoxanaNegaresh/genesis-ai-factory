# Contributing

Thank you for considering a contribution.

## Before you open a pull request

```bash
make test    # every suite
make lint    # go vet + gofmt
make arch    # Clean Architecture dependency rule
```

All three must pass. CI runs the same commands, so a failure locally is a
failure in review.

## The architecture rule is enforced, not suggested

`internal/arch` parses the import graph and fails the build on a violation:

- `domain` imports nothing from the project — it is the innermost layer
- `usecase` depends only on `port` and `domain`
- infrastructure is reachable only through interfaces declared in `port`
- no package outside `infra` imports a database driver, an HTTP framework or
  a filesystem helper directly

If a change needs to break that rule, the rule is usually right and the design
needs another look. Open an issue describing the constraint you hit before
working around it.

## Generation quality is measured

`make bench` generates five product categories and scores each on whether it
compiles, tests, runs and serves. The baseline is enforced at 85%; the current
score is 100%. A change that lowers it fails CI.

If your change legitimately alters what is generated, run `make bench-report`
and include `benchmark.json` in the pull request so the difference is visible.

## Style

**Go** — `gofmt`, standard library idioms, errors wrapped with context.
Comments explain *why*, not *what*: a comment restating the code is noise, and
a comment recording a decision or a trap is worth its space.

**TypeScript** — strict mode, no `any`. `npm run typecheck` must be clean.

**Rust** — `cargo check` clean. The desktop shell is deliberately thin: it owns
the engine's lifecycle and OS access, and nothing else. Logic belongs in the
control plane where it is testable.

## Tests

New behaviour needs a test that fails without the change. Two patterns are used
throughout and are worth following:

- **Assert on observable behaviour, not implementation.** The export test reads
  the produced zip rather than checking that a function was called.
- **Prove the test catches the defect.** Several tests in this repository were
  verified by temporarily reintroducing the bug they guard against. If a test
  cannot fail, it is documentation with extra steps.

## Commit messages

Conventional Commits: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`.

The body matters more than the subject. Describe what was wrong, why the fix
works, and anything surprising you found. Several commits here record bugs
discovered while testing something else — that context is the most valuable
part of the history.

## Reporting bugs

Include:

- What you expected and what happened
- Operating system, and the output of `genesis doctor`
- For engine failures, the contents of `engine.log` (see the README's
  troubleshooting section for its location)
- For generation failures, the build log from the run view

## Security

Please do not open a public issue for a security problem. See
[SECURITY.md](SECURITY.md).
