# Shipping Genesis AI Factory

How to produce the installers you distribute, and what a customer receives.

## What the customer gets

One installer. No prerequisites, no account, no API key, no network call.

| Platform | Artifact | Install |
|---|---|---|
| Windows | `Genesis AI Factory_1.2.0_x64-setup.exe` (NSIS) or `.msi` | Per-user, no admin prompt |
| macOS | `Genesis AI Factory_1.2.0_aarch64.dmg` | Drag to Applications |
| Linux | `.deb` or `.AppImage` | `dpkg -i`, or run the AppImage |

The engine is bundled **inside** the application as a Tauri sidecar. Launching
the app starts it on `127.0.0.1:8787`; closing the window stops it. Nothing is
installed as a service and nothing is left running.

## Building the installers

Tauri links the platform webview, so bundles cannot be cross-compiled — each
must be produced on its own OS. The GitHub Actions workflow does this for all
three:

```bash
git tag v1.2.0 && git push --tags     # builds Windows, macOS and Linux
```

Locally, for the current platform:

```bash
./scripts/desktop-deps.sh      # Linux only: GTK and WebKit headers
make build-desktop             # → apps/desktop/src-tauri/target/release/bundle/
```

`build-desktop` depends on `build`, `icons` and `sidecar`, so the engine
binary, the application icons and the staged sidecar cannot be missing.

### Windows specifically

On a Windows machine with Go, Node and Rust 1.85+:

```powershell
go build -o apps\desktop\src-tauri\binaries\genesis-server-x86_64-pc-windows-msvc.exe .\services\control-plane\cmd\genesis-server
python apps\desktop\src-tauri\icons\generate.py
cd apps\desktop
npm install
npm run desktop:build
```

The installer lands in
`src-tauri\target\release\bundle\nsis\` and `\msi\`.

## Code signing

Unsigned installers trigger SmartScreen on Windows and Gatekeeper on macOS.
Both warnings are severe enough to lose a sale, so signing is not optional for
a commercial release.

**Windows** — an OV or EV certificate. EV gets SmartScreen reputation
immediately; OV accumulates it over time.

```json
"windows": {
  "certificateThumbprint": "YOUR_THUMBPRINT",
  "digestAlgorithm": "sha256",
  "timestampUrl": "http://timestamp.digicert.com"
}
```

Timestamping matters: without it, every installer stops validating the day the
certificate expires, including ones already downloaded.

**macOS** — an Apple Developer ID plus notarisation.

```bash
export APPLE_CERTIFICATE=...          # base64 .p12
export APPLE_CERTIFICATE_PASSWORD=...
export APPLE_SIGNING_IDENTITY="Developer ID Application: Your Company (TEAMID)"
export APPLE_ID=... APPLE_PASSWORD=... APPLE_TEAM_ID=...
```

The workflow reads these from repository secrets when present and skips signing
when absent, so an unsigned build still succeeds for internal testing.

## Versioning

Three files carry the version and must agree:

```
Makefile                              VERSION ?= 1.2.0
apps/desktop/src-tauri/tauri.conf.json  "version": "1.2.0"
apps/desktop/package.json               "version": "1.2.0"
```

The Windows installer refuses to upgrade over a build with a higher version, so
the tag and these three must match.

## Pre-release checklist

```bash
make test                 # 10 Go packages
make bench                # expect 100% across 5 blueprints
make arch                 # Clean Architecture dependency rule
make verify-persistence   # generates a product, runs it against PostgreSQL
cd apps/desktop/src-tauri && cargo test --test packaging
```

Then verify the built application by hand:

- [ ] It launches with no engine on `PATH` — this is what makes it independent
- [ ] Minimise, maximise, close, and window snapping all work
- [ ] File / Edit / View / Engine / Help menus open, accelerators fire
- [ ] Copy and paste work inside the editor
- [ ] Light and dark themes both legible; toggle persists across restarts
- [ ] Generating a project succeeds with the machine offline
- [ ] Closing the window leaves no `genesis-server` process behind

## What to tell customers about cost

Genesis contacts no external service. There is no licence check, no telemetry,
no account and no API key. Verified by generating a full project inside a
network namespace with no route to the internet.

`GENESIS_LLM_API_KEY` exists for customers who point Genesis at their own
OpenAI-compatible endpoint that happens to require a key — a self-hosted vLLM
behind a gateway, say. It defaults to empty and is never required.

For local model reasoning, everything stays on the machine:

```bash
make model-pull    # Qwen2.5-0.5B GGUF, open weights, ~469 MB
make model-serve   # llama.cpp on 127.0.0.1:8791
```

## Support diagnostics

When a customer reports a problem, three things resolve most cases:

| Symptom | First check |
|---|---|
| "Engine not responding" | **Engine → Open Engine Log** in the menu |
| Anything data-related | **Engine → Open Data Folder** |
| Generated project will not build | The build log in the run view |

The engine log lives at `%APPDATA%\genesis\engine.log` on Windows,
`~/Library/Application Support/genesis/engine.log` on macOS and
`~/.config/genesis/engine.log` on Linux.

## Known limitations to disclose

State these in your documentation rather than letting a customer discover them:

1. **Generated projects need PostgreSQL to run.** The factory itself uses
   SQLite and needs nothing.
2. **Only Go and Node projects are verified end to end.** Python, Rust and C#
   generation exists but is not started and probed.
3. **Node build steps run without a memory ceiling.** Any `RLIMIT_AS` breaks
   WebAssembly, which `esbuild` uses; every other sandbox constraint applies.
4. **The sandbox shares the host filesystem for reads.** Writes, network and
   credentials are confined. Reported through `IsolationReport`.
5. **On Windows the sandbox has no namespace isolation** — that is a Linux
   facility. The report says so rather than implying confinement it lacks.
6. **A 0.5B local model is too small for real design work.** The critics detect
   the degenerate output and fall back to blueprints, which is the designed
   behaviour, but customers wanting model-authored design need a larger model.
