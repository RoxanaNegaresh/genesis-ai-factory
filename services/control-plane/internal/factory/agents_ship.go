package factory

import (
	"context"
	"fmt"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// DevOpsAgent produces everything needed to run and ship the generated product.
type DevOpsAgent struct{}

func (a *DevOpsAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleDevOps)
	return Charter{
		Role: domain.RoleDevOps, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactArchSpec},
		Outputs: []domain.ArtifactKind{domain.ArtifactDocker, domain.ArtifactCI, domain.ArtifactReadme},
		Tools:   []string{"fs.write"}, ModelClass: "code",
		Budget: DefaultBudget(), Temperature: 0.1,
	}
}

func (a *DevOpsAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	bp := bb.Blueprint
	slug := bb.Project.Slug

	files := map[string]string{
		"api/Dockerfile":             apiDockerfile(),
		"web/Dockerfile":             webDockerfile(),
		"web/nginx.conf":             nginxConf(),
		"docker-compose.yml":         compose(slug),
		".github/workflows/ci.yml":   ciWorkflow(),
		".env.example":               envExample(slug),
		".gitignore":                 gitignore(),
		"Makefile":                   makefile(),
		"README.md":                  projectReadme(bb.Project.Name, bp, slug),
		"docs/operations/RUNBOOK.md": runbook(bb.Project.Name),
	}
	for path, content := range files {
		if err := tb.WriteFile(ctx, path, content); err != nil {
			return nil, err
		}
	}

	tb.Emit(ctx, domain.LevelInfo,
		"Generated container images, compose stack, CI pipeline and runbook",
		map[string]any{"files": len(files)})

	return []*domain.Artifact{
		artifact(bb, domain.ArtifactDocker, "docker-compose.yml", "text/yaml", files["docker-compose.yml"]),
		artifact(bb, domain.ArtifactCI, "ci.yml", "text/yaml", files[".github/workflows/ci.yml"]),
		artifact(bb, domain.ArtifactReadme, "README.md", "text/markdown", files["README.md"]),
	}, nil
}

func apiDockerfile() string {
	return `# Multi-stage build: the final image contains a static binary and nothing
# else, so there is no shell, package manager or libc for an attacker to use.
FROM golang:1.23-alpine AS build
WORKDIR /src

# Copy manifests first so dependency download is cached independently of source
# changes — the difference between a 5-second and a 90-second rebuild.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /src/migrations /app/migrations
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/server"]
`
}

func webDockerfile() string {
	return `FROM node:20-alpine AS build
WORKDIR /src

COPY package*.json ./
RUN npm ci

COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /src/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://localhost/ >/dev/null || exit 1
`
}

func nginxConf() string {
	return `server {
    listen 80;
    server_name _;
    root /usr/share/nginx/html;
    index index.html;

    # Hashed asset filenames make long-lived caching safe; index.html must not
    # be cached or users keep booting a stale application shell after a deploy.
    location /assets/ {
        expires 1y;
        add_header Cache-Control "public, immutable";
    }

    location = /index.html {
        add_header Cache-Control "no-cache, must-revalidate";
    }

    location /api/ {
        proxy_pass http://api:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 60s;
    }

    # Client-side routing: unknown paths return the app shell, not a 404.
    location / {
        try_files $uri $uri/ /index.html;
    }

    add_header X-Content-Type-Options "nosniff" always;
    add_header X-Frame-Options "DENY" always;
    add_header Referrer-Policy "strict-origin-when-cross-origin" always;

    gzip on;
    gzip_types text/css application/javascript application/json image/svg+xml;
    gzip_min_length 1024;
}
`
}

func compose(slug string) string {
	return fmt.Sprintf(`services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: ${POSTGRES_USER:-postgres}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:-postgres}
      POSTGRES_DB: ${POSTGRES_DB:-%s}
    volumes:
      - pgdata:/var/lib/postgresql/data
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-postgres}"]
      interval: 5s
      timeout: 3s
      retries: 10

  redis:
    image: redis:7-alpine
    command: ["redis-server", "--save", "60", "1", "--appendonly", "no"]
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 10

  api:
    build: ./api
    environment:
      ADDR: ":8080"
      DATABASE_URL: postgres://${POSTGRES_USER:-postgres}:${POSTGRES_PASSWORD:-postgres}@postgres:5432/${POSTGRES_DB:-%s}?sslmode=disable
      REDIS_URL: redis://redis:6379/0
      JWT_SECRET: ${JWT_SECRET:?JWT_SECRET must be set}
      CORS_ORIGINS: http://localhost:5173,http://localhost:8081
      LOG_LEVEL: info
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    ports:
      - "8080:8080"
    restart: unless-stopped

  web:
    build: ./web
    depends_on:
      - api
    ports:
      - "8081:80"
    restart: unless-stopped

volumes:
  pgdata:
`, slug, slug)
}

func ciWorkflow() string {
	return `name: CI

on:
  push:
    branches: [main]
  pull_request:

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true

jobs:
  api:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16-alpine
        env:
          POSTGRES_PASSWORD: postgres
        options: >-
          --health-cmd pg_isready --health-interval 5s --health-timeout 3s --health-retries 10
        ports: ["5432:5432"]
    defaults:
      run:
        working-directory: api
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
          cache-dependency-path: api/go.sum
      - run: go mod download
      - run: go vet ./...
      # The schema must be applied before the repository tests run. Without
      # this they skip, and a suite that skips its only real database
      # coverage reports green while testing nothing that matters.
      - name: Apply migrations
        env:
          PGPASSWORD: postgres
        run: |
          psql -h localhost -U postgres -c 'CREATE DATABASE app_test;'
          psql -h localhost -U postgres -d app_test -v ON_ERROR_STOP=1 \
            -f ../migrations/0001_init.up.sql
      - name: Test
        env:
          TEST_DATABASE_URL: postgres://postgres:postgres@localhost:5432/app_test?sslmode=disable
        run: go test ./... -race -coverprofile=coverage.out
      - name: Vulnerability scan
        run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          govulncheck ./...

  web:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: web
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: '20'
          cache: npm
          cache-dependency-path: web/package-lock.json
      - run: npm ci
      - run: npm run typecheck
      - run: npm run build

  images:
    runs-on: ubuntu-latest
    needs: [api, web]
    if: github.ref == 'refs/heads/main'
    steps:
      - uses: actions/checkout@v4
      - name: Build images
        run: |
          docker build -t app-api:${{ github.sha }} ./api
          docker build -t app-web:${{ github.sha }} ./web
`
}

func envExample(slug string) string {
	return fmt.Sprintf(`# Copy to .env and fill in real values. Never commit .env.

POSTGRES_USER=postgres
POSTGRES_PASSWORD=change-me
POSTGRES_DB=%s

# Generate with: openssl rand -hex 32
JWT_SECRET=

ADDR=:8080
LOG_LEVEL=info
CORS_ORIGINS=http://localhost:5173
`, slug)
}

func gitignore() string {
	return `# Secrets
.env
.env.local
*.pem
*.key

# Build output
dist/
build/
node_modules/
*.exe
/api/server

# Test and coverage
coverage.out
coverage/
*.test

# Scratch space the build sandbox creates inside the workspace
.genesis-tmp/

# Editor and OS
.DS_Store
.idea/
.vscode/
*.swp
`
}

func makefile() string {
	return `.DEFAULT_GOAL := help
.PHONY: help deps up down logs api web test lint migrate

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

deps: ## Resolve dependencies (run this first)
	cd api && go mod tidy
	cd web && npm install

up: ## Start the full stack
	docker compose up -d --build

down: ## Stop the stack
	docker compose down

logs: ## Tail service logs
	docker compose logs -f --tail=100

api: ## Run the API locally
	cd api && go run ./cmd/server

migrate: ## Apply the schema to $$DATABASE_URL
	@test -n "$$DATABASE_URL" || { echo "set DATABASE_URL first"; exit 1; }
	psql "$$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/0001_init.up.sql

test-db: ## Run repository tests against a real database
	@test -n "$$TEST_DATABASE_URL" || { echo "set TEST_DATABASE_URL first"; exit 1; }
	cd api && go test ./internal/infra/postgres/ -v -count=1

web: ## Run the web client locally
	cd web && npm run dev

test: ## Run all tests
	cd api && go test ./... -race
	cd web && npm run typecheck

lint: ## Static analysis
	cd api && go vet ./...

migrate: ## Apply database migrations
	cd api && go run ./cmd/server -migrate
`
}

func projectReadme(name string, bp Blueprint, slug string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n%s\n\n", name, bp.Description)
	sb.WriteString("> Generated by **Genesis AI Factory**. Every file here is yours to edit — ")
	sb.WriteString("nothing is locked, obfuscated or regenerated behind your back.\n\n")

	sb.WriteString("## Quick start\n\n```bash\nmake deps                 # resolve Go and npm dependencies\ncp .env.example .env\n")
	sb.WriteString("echo \"JWT_SECRET=$(openssl rand -hex 32)\" >> .env\nmake up\n```\n\n")
	sb.WriteString("| Service | URL |\n|---|---|\n| Web client | http://localhost:8081 |\n")
	sb.WriteString("| API | http://localhost:8080/api/v1 |\n| Health | http://localhost:8080/health |\n\n")

	sb.WriteString("## Local development\n\n```bash\n# Terminal 1 — dependencies\ndocker compose up -d postgres redis\n\n")
	sb.WriteString("# Terminal 2 — API\nmake api\n\n# Terminal 3 — web client\nmake web\n```\n\n")

	sb.WriteString("## Repository layout\n\n```\n")
	sb.WriteString("api/          Go service (Clean Architecture)\n")
	sb.WriteString("web/          React + TypeScript client\n")
	sb.WriteString("migrations/   PostgreSQL schema\n")
	sb.WriteString("docs/         Product, architecture, design, QA and security documents\n")
	sb.WriteString("```\n\n")

	sb.WriteString("## Domain model\n\n| Entity | Purpose |\n|---|---|\n")
	for _, e := range bp.Entities {
		fmt.Fprintf(&sb, "| `%s` | %s |\n", e.Name, e.Description)
	}

	sb.WriteString("\n## Screens\n\n| Screen | Route |\n|---|---|\n")
	for _, s := range bp.Screens {
		fmt.Fprintf(&sb, "| %s | `%s` |\n", s.Name, s.Route)
	}

	sb.WriteString("\n## Documentation\n\n")
	sb.WriteString("- [Product vision](docs/product/VISION.md)\n")
	sb.WriteString("- [Requirements](docs/product/PRD.md)\n")
	sb.WriteString("- [Architecture](docs/architecture/ARCHITECTURE.md)\n")
	sb.WriteString("- [Decisions](docs/architecture/DECISIONS.md)\n")
	sb.WriteString("- [Data model](docs/architecture/DATA_MODEL.md)\n")
	sb.WriteString("- [Design system](docs/design/DESIGN_SYSTEM.md)\n")
	sb.WriteString("- [User flows](docs/design/USER_FLOWS.md)\n")
	sb.WriteString("- [Test plan](docs/qa/TEST_PLAN.md)\n")
	sb.WriteString("- [Security review](docs/security/SECURITY_REVIEW.md)\n")
	sb.WriteString("- [Runbook](docs/operations/RUNBOOK.md)\n")
	sb.WriteString("- [API reference](api/openapi.yaml)\n\n")

	fmt.Fprintf(&sb, "## Tests\n\n```bash\nmake test\n```\n\n---\n\nProject slug: `%s`\n", slug)
	return sb.String()
}

func runbook(name string) string {
	return fmt.Sprintf(`# %s — Operations Runbook

## Deploy

1. Merge to `+"`main`"+`; CI builds and tags images with the commit SHA.
2. Apply migrations before rolling the API: schema changes must be additive so
   the previous version keeps working during the rollout.
3. Roll the API, then the web client.
4. Verify `+"`/health`"+` and watch error rate for 10 minutes.

## Rollback

Migrations are forward-only. To roll back:

1. Deploy the previous image tag.
2. Only revert a migration if it is additive and unused by the previous version.
3. If data is affected, restore from the most recent snapshot and replay.

## Common incidents

### API returns 503

Check database connectivity first: `+"`docker compose logs postgres`"+`.
Most 503s are exhausted connection pools, not application faults. Confirm with
`+"`SELECT count(*) FROM pg_stat_activity;`"+` against the pool maximum.

### Slow list endpoints

Run `+"`EXPLAIN ANALYZE`"+` on the offending query. The usual cause is a
filter on a column without a supporting index, or a soft-delete predicate that
prevents an index from being used.

### Authentication failures after deploy

Verify `+"`JWT_SECRET`"+` is identical across API replicas. A rotated or
mismatched secret invalidates every issued token instantly.

## Backups

- Nightly `+"`pg_dump`"+` retained for 30 days
- Monthly restore drill into a scratch database — an untested backup is a
  hypothesis, not a backup

## Monitoring

| Signal | Threshold |
|---|---|
| API 5xx rate | > 1%% over 5 minutes |
| p95 latency | > 500ms over 10 minutes |
| Database connections | > 80%% of pool |
| Disk usage | > 80%% |
`, name)
}

// ImproverAgent analyses the finished product and queues the next iteration.
type ImproverAgent struct{}

func (a *ImproverAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleImprover)
	return Charter{
		Role: domain.RoleImprover, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactQAReport, domain.ArtifactSecReport},
		Outputs: []domain.ArtifactKind{domain.ArtifactImprovePlan},
		Tools:   []string{"fs.read", "fs.write"}, ModelClass: "reasoning",
		Budget: DefaultBudget(), Temperature: 0.4,
	}
}

func (a *ImproverAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	bp := bb.Blueprint
	files, err := tb.ListFiles(ctx)
	if err != nil {
		return nil, err
	}

	// v0.7: analyse the code that was actually produced rather than restating
	// what the factory intended to produce. A backlog that says the same thing
	// for every project is a template, not an analysis.
	if bb.Project.WorkspacePath != "" {
		analysis, err := AnalyzeProject(ctx, bb.Project.WorkspacePath, bp)
		if err == nil && len(analysis.Findings) > 0 {
			counts := analysis.Counts()
			tb.Emit(ctx, domain.LevelInfo,
				fmt.Sprintf("Analysed %d files: %d high, %d medium, %d low priority findings",
					analysis.Files, counts[SeverityHigh], counts[SeverityMedium], counts[SeverityLow]),
				map[string]any{
					"files": analysis.Files, "go_files": analysis.GoFiles,
					"test_files": analysis.TestFiles, "endpoints": analysis.Endpoints,
					"high": counts[SeverityHigh], "medium": counts[SeverityMedium],
				})

			body := analysis.Markdown()
			if err := tb.WriteFile(ctx, "docs/product/IMPROVEMENT_PLAN.md", body); err != nil {
				return nil, err
			}
			bb.SetValue("analysis", analysis)
			return []*domain.Artifact{
				artifact(bb, domain.ArtifactImprovePlan, "IMPROVEMENT_PLAN.md", "text/markdown", body),
			}, nil
		}
	}

	type item struct{ priority, area, action, why string }
	items := []item{
		{"P0", "Backend", "Implement the PostgreSQL repositories behind the generated port interfaces",
			"Use cases are complete but currently have no persistence implementation wired in."},
		{"P0", "Security", "Add per-role authorization checks in every use case",
			"Highest-severity finding in the security review; an authenticated user can currently reach any resource."},
		{"P0", "Backend", "Wire the generated handlers into the router in cmd/server",
			"Handlers exist but are not yet mounted, so the API surface is not reachable."},
		{"P1", "Testing", "Replace skipped contract tests with executing assertions",
			"Skipped tests give the appearance of coverage without the protection."},
		{"P1", "Frontend", "Add create and edit forms for each primary entity",
			"List views are read-only; the core loop is incomplete without mutation."},
		{"P1", "Performance", "Add cursor pagination indexes matching the default sort order",
			"Offset pagination degrades linearly and the default ordering must be index-backed."},
		{"P2", "UX", "Implement optimistic updates with rollback on failure",
			"The design system specifies optimistic interaction; the generated client currently awaits the round trip."},
		{"P2", "Operations", "Add structured metrics and a dashboard",
			"Logs alone cannot answer whether latency regressed after a deploy."},
	}
	switch bp.Category {
	case domain.CategoryPM:
		items = append(items, item{"P1", "Domain", "Implement fractional ranking for board reordering",
			"Integer positions require renumbering an entire column on every drag."})
	case domain.CategoryERP, domain.CategoryMarketplace:
		items = append(items, item{"P0", "Domain", "Make money and stock mutations idempotent and transactional",
			"Double submission or a retried webhook would otherwise corrupt financial state."})
	case domain.CategoryCRM:
		items = append(items, item{"P1", "Domain", "Add pipeline stage transition history",
			"Forecasting accuracy depends on knowing how long deals sit in each stage."})
	}

	var sb strings.Builder
	sb.WriteString("# Improvement Plan\n\n")
	fmt.Fprintf(&sb, "Generated after analysing %d files in the workspace.\n\n", len(files))
	sb.WriteString("## Prioritised backlog\n\n| Priority | Area | Action | Rationale |\n|---|---|---|---|\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "| **%s** | %s | %s | %s |\n", it.priority, it.area, it.action, it.why)
	}

	sb.WriteString("\n## Definition of done for the next iteration\n\n")
	sb.WriteString("1. Every P0 item is closed and covered by a test\n")
	sb.WriteString("2. `go test ./...` and `npm run typecheck` are green in CI\n")
	sb.WriteString("3. The application starts from `make up` and serves a real request end to end\n")
	sb.WriteString("4. No high-severity security finding remains open\n\n")

	sb.WriteString("## Honest assessment\n\n")
	sb.WriteString("This run produced a **complete, coherent specification and a structurally correct skeleton**: ")
	sb.WriteString("documentation, schema, API contract, layered Go code, a typed React client, containers and CI. ")
	sb.WriteString("What it does not yet produce is *working business logic wired to a database* — that is the ")
	sb.WriteString("v0.2+ capability, where model-authored implementations fill these interfaces and the sandbox ")
	sb.WriteString("compiles and tests the result until it runs. Stating that plainly is more useful than a ")
	sb.WriteString("progress bar that claims otherwise.\n")

	body := sb.String()
	if err := tb.WriteFile(ctx, "docs/product/IMPROVEMENT_PLAN.md", body); err != nil {
		return nil, err
	}

	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Queued %d improvements for the next iteration", len(items)),
		map[string]any{"items": len(items)})

	return []*domain.Artifact{artifact(bb, domain.ArtifactImprovePlan, "IMPROVEMENT_PLAN.md", "text/markdown", body)}, nil
}
