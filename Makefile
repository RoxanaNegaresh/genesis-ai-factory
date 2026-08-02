.DEFAULT_GOAL := help
SHELL := /bin/bash

VERSION    ?= 1.2.0
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

SERVER_PKG := github.com/genesis-ai-factory/control-plane/...
CLI_PKG    := github.com/genesis-ai-factory/cli/...
GO_PKGS    := $(SERVER_PKG) $(CLI_PKG)

BIN := $(CURDIR)/bin

.PHONY: help
help: ## Show available targets
	@echo ""
	@echo "  GENESIS AI FACTORY — v$(VERSION)"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo ""

# --- build ---------------------------------------------------------------

.PHONY: build
build: build-server build-cli ## Build every binary

.PHONY: build-server
build-server: ## Build the control plane
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/genesis-server \
		github.com/genesis-ai-factory/control-plane/cmd/genesis-server
	@chmod +x $(BIN)/genesis-server
	@echo "→ $(BIN)/genesis-server"

.PHONY: build-cli
build-cli: ## Build the genesis CLI
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/genesis \
		github.com/genesis-ai-factory/cli/cmd/genesis
	@chmod +x $(BIN)/genesis
	@echo "→ $(BIN)/genesis"

.PHONY: icons
icons: ## Regenerate the Tauri application icons (true 8-bit RGBA)
	python3 apps/desktop/src-tauri/icons/generate.py

.PHONY: desktop-deps
desktop-deps: ## Install the Linux system libraries Tauri needs
	@./scripts/desktop-deps.sh

SIDECAR_DIR := apps/desktop/src-tauri/binaries

.PHONY: sidecar
sidecar: build ## Stage the engine inside the desktop bundle
	@# Tauri looks for a sidecar named with the Rust target triple and strips
	@# the suffix again on extraction. Staging it here is what makes the
	@# installed application self-contained: no separate server to install, no
	@# PATH to configure, and nothing left running after the window closes.
	@mkdir -p $(SIDECAR_DIR)
	@triple=$$(rustc -vV | awk '/^host:/{print $$2}'); \
	ext=""; case "$$triple" in *windows*) ext=".exe";; esac; \
	cp $(BIN)/genesis-server$$ext $(SIDECAR_DIR)/genesis-server-$$triple$$ext; \
	chmod +x $(SIDECAR_DIR)/genesis-server-$$triple$$ext; \
	echo "→ $(SIDECAR_DIR)/genesis-server-$$triple$$ext"

.PHONY: build-desktop
build-desktop: build icons sidecar ## Build the desktop application bundle
	cd apps/desktop && npm install --no-audit --no-fund && npm run desktop:build

.PHONY: desktop
desktop: build icons sidecar ## Run the desktop app (builds the engine it launches)
	cd apps/desktop && npm install --no-audit --no-fund && npm run desktop

.PHONY: desktop-windows
desktop-windows: icons ## Cross-compile the Windows engine and bundle the app
	@# The Go engine cross-compiles cleanly; the Tauri shell does not, because
	@# it links the platform webview. Run this on Windows, or use the GitHub
	@# Actions workflow, which builds the installer on a Windows runner.
	@mkdir -p $(SIDECAR_DIR)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(SIDECAR_DIR)/genesis-server-x86_64-pc-windows-msvc.exe \
		github.com/genesis-ai-factory/control-plane/cmd/genesis-server
	@echo "→ $(SIDECAR_DIR)/genesis-server-x86_64-pc-windows-msvc.exe"
	cd apps/desktop && npm install --no-audit --no-fund && npm run desktop:build

.PHONY: cross
cross: ## Cross-compile server and CLI for Windows and Linux
	@mkdir -p $(BIN)
	@for target in "linux amd64" "linux arm64" "windows amd64"; do \
		set -- $$target; os=$$1; arch=$$2; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "→ $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(BIN)/genesis-server-$$os-$$arch$$ext \
			github.com/genesis-ai-factory/control-plane/cmd/genesis-server || exit 1; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(BIN)/genesis-$$os-$$arch$$ext \
			github.com/genesis-ai-factory/cli/cmd/genesis || exit 1; \
	done

# --- test ----------------------------------------------------------------

.PHONY: test
test: ## Run every test suite
	go test $(GO_PKGS)
	$(MAKE) test-ai
	cd apps/desktop && npm run typecheck

.PHONY: venv
venv: ## Create a Python virtual environment for the AI engine
	@# Ubuntu 23.04+ and Debian 12+ follow PEP 668 and refuse global pip
	@# installs with "error: externally-managed-environment". A virtual
	@# environment is the supported answer; --break-system-packages is not,
	@# because it can overwrite libraries the distribution's own tools need.
	@#
	@# The engine itself declares no dependencies and runs with `python3 -m`,
	@# so this is only needed to run its test suite.
	python3 -m venv services/ai-engine/.venv
	services/ai-engine/.venv/bin/pip install --quiet --upgrade pip pytest
	@echo "→ services/ai-engine/.venv"
	@echo "  activate: source services/ai-engine/.venv/bin/activate"

.PHONY: test-ai
test-ai: ## Run the AI engine tests
	@# Prefer the virtual environment when one exists, so this works on a
	@# PEP 668 system without any flags. Falls back to the system pytest.
	@if [ -x services/ai-engine/.venv/bin/pytest ]; then \
		cd services/ai-engine && .venv/bin/pytest tests/ -q; \
	elif python3 -c "import pytest" 2>/dev/null; then \
		cd services/ai-engine && python3 -m pytest tests/ -q; \
	else \
		echo "pytest is not available. Run: make venv"; exit 1; \
	fi

.PHONY: bench
bench: ## Measure generation quality across every product category
	go test github.com/genesis-ai-factory/control-plane/internal/factory \
		-run TestBenchmarkBaseline -v -timeout 20m -count=1

.PHONY: heal-demo
heal-demo: ## Demonstrate self-healing on a deliberately broken project
	go test github.com/genesis-ai-factory/control-plane/internal/factory \
		-run TestHealerRepairsABrokenProject -v -timeout 20m -count=1

.PHONY: bench-report
bench-report: ## Write a machine-comparable benchmark report to benchmark.json
	GENESIS_BENCHMARK_OUT=$(CURDIR)/benchmark.json go test \
		github.com/genesis-ai-factory/control-plane/internal/factory \
		-run TestBenchmarkBaseline -timeout 20m -count=1
	@echo "→ $(CURDIR)/benchmark.json"

.PHONY: test-go
test-go: ## Run Go tests only
	go test $(GO_PKGS)

.PHONY: test-race
test-race: ## Run Go tests with the race detector
	go test -race $(GO_PKGS)

.PHONY: test-cover
test-cover: ## Produce a coverage report
	go test -coverprofile=coverage.out -covermode=atomic $(GO_PKGS)
	go tool cover -func=coverage.out | tail -1
	@echo "HTML report: go tool cover -html=coverage.out"

.PHONY: test-postgres
test-postgres: ## Run the repository suite against PostgreSQL
	@test -n "$$GENESIS_TEST_POSTGRES_DSN" || \
		{ echo "set GENESIS_TEST_POSTGRES_DSN first"; exit 1; }
	go test -count=1 github.com/genesis-ai-factory/control-plane/internal/infra/sqlstore

# --- quality --------------------------------------------------------------

.PHONY: lint
lint: ## Static analysis
	go vet $(GO_PKGS)
	gofmt -l services apps/cli | (! grep .) || { echo "run: make fmt"; exit 1; }

.PHONY: fmt
fmt: ## Format Go sources
	gofmt -w services apps/cli

.PHONY: verify-persistence
verify-persistence: ## Prove a generated product stores data in real PostgreSQL
	./scripts/verify-persistence.sh

.PHONY: arch
arch: ## Verify the Clean Architecture dependency rule
	go test github.com/genesis-ai-factory/control-plane/internal/arch -v

.PHONY: vuln
vuln: ## Scan dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest $(GO_PKGS)

# --- run -----------------------------------------------------------------

.PHONY: run
run: ## Run the control plane (SQLite, no external services)
	go run github.com/genesis-ai-factory/control-plane/cmd/genesis-server

# --- local inference ------------------------------------------------------

.PHONY: models
models: ## Show which models fit this machine
	cd services/ai-engine && python3 -m genesis_ai.cli list

.PHONY: model-pull
model-pull: ## Download the recommended model
	cd services/ai-engine && python3 -m genesis_ai.cli pull

.PHONY: model-serve
model-serve: ## Start the local inference server
	cd services/ai-engine && python3 -m genesis_ai.cli serve

.PHONY: ai-doctor
ai-doctor: ## Check the local inference setup
	cd services/ai-engine && python3 -m genesis_ai.cli doctor

.PHONY: run-ai
run-ai: ## Run the control plane with local inference enabled
	GENESIS_LLM_URL=http://127.0.0.1:8791 \
		go run github.com/genesis-ai-factory/control-plane/cmd/genesis-server

.PHONY: dev
dev: build icons sidecar ## Run the desktop app against a live server
	cd apps/desktop && npm run desktop

.PHONY: web
web: ## Run the desktop UI in a browser
	cd apps/desktop && npm run dev

.PHONY: demo
demo: build ## Build, start the server and generate a sample product
	@$(BIN)/genesis-server & echo $$! > /tmp/genesis-demo.pid; \
	sleep 2; \
	$(BIN)/genesis create "Build a Jira competitor with kanban boards and sprints"; \
	kill $$(cat /tmp/genesis-demo.pid) 2>/dev/null || true

# --- infrastructure -------------------------------------------------------

.PHONY: up
up: ## Start Postgres, Redis and Qdrant
	docker compose up -d postgres redis qdrant

.PHONY: down
down: ## Stop the infrastructure stack
	docker compose down

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) coverage.out apps/desktop/dist
	go clean -cache -testcache
