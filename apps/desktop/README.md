# Genesis AI Factory — Desktop

A Tauri 2 shell around the React workspace. It launches `genesis-server` as a
child process, injects the loopback session token before the frontend boots,
and terminates the engine on close so the next launch finds port 8787 free.

## Quick start

```bash
# From the repository root
./scripts/desktop-deps.sh     # Linux system libraries (once per machine)
make desktop                  # builds the engine, generates icons, runs the app
```

`make desktop` is the whole story. It depends on `build` and `icons`, so the
engine binary and the application icons cannot be missing by the time Tauri
needs them.

## Requirements

| Tool | Version | Why |
|---|---|---|
| Rust | **1.85+** | Tauri 2's dependencies publish `edition2024` manifests; Cargo below 1.85 cannot parse them |
| Node | 20+ | Vite 5 |
| Go | 1.25+ | builds `genesis-server`, which the shell launches |

Linux additionally needs GTK 3, WebKit2GTK 4.1 and their headers — Tauri links
against the platform webview rather than shipping one. `./scripts/desktop-deps.sh`
installs them for Debian/Ubuntu, Fedora/RHEL, Arch and openSUSE.

## Commands

```bash
make desktop          # run the app (recommended)
make build-desktop    # produce a distributable bundle
make icons            # regenerate the application icons
npm run dev           # frontend only, in a browser at :1420
npm run typecheck     # TypeScript, no emit
```

## Troubleshooting

Each of these was hit on a real Ubuntu machine. The pattern is that the error
message names the wrong culprit, which is why they are worth listing.

### `Package 'glib-2.0' not found` while building `glib-sys`

The GTK development headers are missing. The message names a Rust crate, not a
distribution package, which sends people to crates.io instead of apt.

```bash
./scripts/desktop-deps.sh
```

### `error: proc macro panicked … failed to open icon`

`src-tauri/icons/` is empty. Tauri reads icons at compile time inside
`generate_context!`, so a missing file surfaces as a macro panic rather than a
missing-file error.

```bash
make icons
```

### `error: proc macro panicked … icon … is not RGBA`

The icons exist but are the wrong PNG subtype. `convert -resize` optimises
small images into `PaletteAlpha`, `GrayscaleAlpha` or `Bilevel` — all valid
PNG, none of them RGBA, and Tauri accepts only RGBA.

`icons/generate.py` writes the pixel buffer and the IHDR directly, so the
output format is a property of the code rather than of the installed
ImageMagick version.

```bash
make icons
file src-tauri/icons/32x32.png
# PNG image data, 32 x 32, 8-bit/color RGBA, non-interlaced
```

### `feature edition2024 is required … not stabilized in this version of Cargo`

Cargo is older than 1.85. If `rustc --version` already reports something newer,
a rustup **directory override** is pinning an old toolchain for this path:

```bash
rustup show active-toolchain     # look for "(directory override)"
rustup override unset
rustup update stable
```

A stale index can keep the old manifests around afterwards:

```bash
rm -rf ~/.cargo/registry/src/index.crates.io-*
rm -rf ~/.cargo/registry/cache/index.crates.io-*
cargo clean
```

### `error[E0597]: engine does not live long enough`

Fixed in v1.2.1. If you see it, you are on an older checkout.

The cause is worth understanding, because it was a latent deadlock and not
merely a compile error:

```rust
// Wrong: both temporaries — the State borrowed from `window` and the
// MutexGuard — live until the end of the `if let`, so the guard outlives
// the State it came from, and the lock is held across child.wait().
if let Some(mut child) = engine.0.lock().unwrap().take() { … }

// Right: the guard is dropped at the semicolon.
let child = engine.0.lock().unwrap().take();
if let Some(mut child) = child { … }
```

### "token expired"

**This is not a licence, subscription or billing problem.** Genesis is free,
runs entirely on your machine, and never contacts a paid service. The token in
question is your own local sign-in session, issued by the copy of
`genesis-server` running on 127.0.0.1.

Access tokens live 15 minutes on purpose: they cannot be revoked, so a short
life is the only bound on a stolen one. Before v1.2.1 nothing renewed them, so
the app worked for a quarter of an hour and then stopped.

Fixed in two places:

- The API client retries once on a 401, silently exchanging the refresh token
  for a new pair. Concurrent requests share one in-flight refresh, because a
  refresh token is single-use and spending it twice looks like theft to the
  server, which then revokes the whole family.
- The server rewrites `session.json` every `GENESIS_ACCESS_TTL / 3`, so any
  client that re-reads the file — the CLI included — always finds a live token.

If you are on an older build, restarting the app issues a fresh session. To
confirm you are on a fixed build:

```bash
cd services/control-plane
go test ./internal/adapter/http/ -run TestExpiredAccessTokenIsRecoverable -v
```

Sessions can be made longer for a trusted single-user machine:

```bash
GENESIS_ACCESS_TTL=8h ./bin/genesis-server
```

### Does Genesis need an API key or a paid account?

No. There is no account, no telemetry, no licence check and no paid API.

`GENESIS_LLM_API_KEY` exists only for people who point Genesis at an
OpenAI-*compatible* endpoint that happens to require a key — a self-hosted
vLLM behind a gateway, for instance. It defaults to empty, and the product
generates complete projects with no model at all, falling back to blueprints.

For local model reasoning, everything stays on your machine:

```bash
make model-pull    # Qwen2.5-0.5B GGUF, ~469 MB, open weights
make model-serve   # llama.cpp on 127.0.0.1:8791
make run-ai
```

### The window opens but the UI says the engine is unavailable

The shell writes the engine's output to `engine.log` in the data directory:

| Platform | Path |
|---|---|
| Linux | `~/.config/genesis/engine.log` |
| macOS | `~/Library/Application Support/genesis/engine.log` |
| Windows | `%APPDATA%\genesis\engine.log` |

The commonest cause after extracting an archive is a lost executable bit —
zip does not preserve Unix permissions, and workspace snapshots strip it too.
The app now detects this and says so, but the fix is:

```bash
chmod +x bin/genesis-server
```

### `error: externally-managed-environment` from pip

Debian 12+ and Ubuntu 23.04+ implement PEP 668 and refuse global pip installs.

The AI engine declares **no dependencies** and runs with `python3 -m`, so this
does not affect normal use. It only appears when installing `pytest` to run its
test suite:

```bash
make venv        # creates services/ai-engine/.venv
make test-ai     # uses the venv automatically when present
```

`--break-system-packages` is deliberately not recommended: it can overwrite
libraries the distribution's own tooling depends on.

## Layout

```
src/                   React application
  views/               Workspace, RunView, Editor (Monaco)
  components/ui/       Primitives
  lib/                 API client, event stream, helpers
src-tauri/
  src/main.rs          Shell: engine lifecycle, token injection, IPC
  icons/generate.py    Icon generator — writes true 8-bit RGBA
  tests/packaging.rs   Asserts icons exist, are RGBA, and the toolchain floor
  tauri.conf.json      Window, CSP, bundle configuration
```

## Regression tests

```bash
cd src-tauri && cargo test --test packaging
```

Four invariants, each covering a failure above: every configured icon exists,
every PNG is 8-bit RGBA, `icon.ico` is a real ICO container, and
`rust-version` admits `edition2024`. They were verified to fail when the
corresponding defect is reintroduced, rather than merely to pass today.
