# Restoring this download

The archive contains the full source, all documentation, and the complete git
history. It deliberately does **not** contain a `.git` directory, build output,
or `node_modules` — see "Why" below.

## 1. Extract

```bash
unzip genesis-ai-factory-v1.2.0.zip
cd genesis-ai-factory
```

## 2. Restore the executable bit

Zip does not preserve Unix permissions, so the shell scripts arrive
non-executable:

```bash
chmod +x scripts/*.sh
```

## 3. Restore the git history (optional)

All six commits are in `genesis-history.bundle`:

```bash
git clone genesis-history.bundle ../genesis-restored
cd ../genesis-restored
git log --oneline        # 339b5c8 … edb387e
```

Or attach the history to the extracted copy in place:

```bash
git init -q
git config user.email "you@example.com"
git config user.name  "Your Name"
git fetch genesis-history.bundle 'refs/heads/*:refs/heads/*'
git checkout main
```

Once restored you can delete `genesis-history.bundle`.

## 4. Prerequisites

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.25+ | control plane, CLI, generated backends |
| Node | 20+ | desktop app, generated frontends |
| PostgreSQL | 14+ | generated products (SQLite is the factory's own default) |
| Python | 3.11+ | AI engine (optional) |

```bash
# Go, if not already installed
curl -sSL -o go.tgz https://go.dev/dl/go1.25.5.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go.tgz
export PATH=/usr/local/go/bin:$PATH
```

## 5. Build and verify

```bash
make build
make test          # 10 Go packages
make arch          # Clean Architecture dependency rule
make bench         # expect 100% across 5 blueprints
```

End-to-end proof that a generated product really works — generates a project,
applies its schema, builds it, runs its tests against a live server, then
exercises CRUD and the full authentication lifecycle over HTTP:

```bash
./scripts/verify-persistence.sh          # needs a reachable PostgreSQL
PGPORT=5433 ./scripts/verify-persistence.sh   # if yours is on another port
```

## 6. Run it

```bash
export GENESIS_SINGLE_USER=1
./bin/genesis-server &
./bin/genesis create "Build a Jira competitor"
```

---

## Why the archive is shaped this way

**No `.git` directory.** The workspace snapshotting layer treats `.git/config`
as a credential path and strips it. What survives is 328 files that look like a
repository but cannot be read as one, and that broken directory was 63% of the
file count. A `git bundle` carries the same history in a single 530 KB file
that nothing strips.

**No `bin/`.** Two compiled binaries were 20 MB against 3.7 MB of source, and
`make build` reproduces them exactly.

**No `node_modules/` or `dist/`.** Reproduced by `npm install` and `npm run
build`; shipping them would multiply the download for no benefit.
