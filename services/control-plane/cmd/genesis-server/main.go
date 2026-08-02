// Command genesis-server is the Genesis AI Factory control plane.
//
// It is the composition root: the only place that knows how every component is
// wired together. Everything else receives its dependencies through interfaces.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	adapterhttp "github.com/genesis-ai-factory/control-plane/internal/adapter/http"
	"github.com/genesis-ai-factory/control-plane/internal/adapter/ws"
	"github.com/genesis-ai-factory/control-plane/internal/config"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
	"github.com/genesis-ai-factory/control-plane/internal/infra/bus"
	"github.com/genesis-ai-factory/control-plane/internal/infra/crypto"
	"github.com/genesis-ai-factory/control-plane/internal/infra/llm"
	"github.com/genesis-ai-factory/control-plane/internal/infra/migrate"
	"github.com/genesis-ai-factory/control-plane/internal/infra/sandbox"
	"github.com/genesis-ai-factory/control-plane/internal/infra/sqlstore"
	"github.com/genesis-ai-factory/control-plane/internal/infra/vcs"
	"github.com/genesis-ai-factory/control-plane/internal/port"
	"github.com/genesis-ai-factory/control-plane/internal/usecase"
)

// Build metadata, injected at link time:
//
//	go build -ldflags "-X main.version=0.1.0 -X main.commit=$(git rev-parse --short HEAD)"
var (
	version   = "1.2.0"
	commit    = "dev"
	buildDate = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "genesis-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		migrateOnly = flag.Bool("migrate", false, "apply migrations and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("genesis-server %s (%s, built %s)\n", version, commit, buildDate)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	cfg.Version, cfg.Commit, cfg.BuildDate = version, commit, buildDate

	logger := newLogger(cfg)
	slog.SetDefault(logger)
	logger.Info("starting genesis control plane", "config", cfg.Redacted())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- storage ---------------------------------------------------------
	store, err := sqlstore.Open(ctx, sqlstore.DefaultOptions(cfg.DBDriver, cfg.DBDSN))
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer func() { _ = store.Close() }()

	if cfg.AutoMigrate || *migrateOnly {
		runner := migrate.NewRunner(store.DB(), migrate.Driver(cfg.DBDriver), logger)
		applied, err := runner.Up(ctx)
		if err != nil {
			return fmt.Errorf("migrations: %w", err)
		}
		if applied > 0 {
			logger.Info("schema migrated", "migrations_applied", applied)
		}
	}
	if *migrateOnly {
		logger.Info("migrations complete; exiting as requested")
		return nil
	}

	users := sqlstore.NewUserRepo(store)
	tokens := sqlstore.NewRefreshTokenRepo(store)
	projects := sqlstore.NewProjectRepo(store)
	runs := sqlstore.NewRunRepo(store)
	tasks := sqlstore.NewTaskRepo(store)
	events := sqlstore.NewEventRepo(store)
	artifacts := sqlstore.NewArtifactRepo(store)

	// --- infrastructure ---------------------------------------------------
	eventBus := bus.New()
	defer eventBus.Close()

	hasher := crypto.NewArgon2Hasher(crypto.DefaultParams())
	issuer := crypto.NewJWTIssuer(cfg.JWTSecret, time.Now)
	clock := systemClock{}
	recorder := usecase.NewRecorder(events, eventBus, logger)

	// --- application ------------------------------------------------------
	authService := usecase.NewAuth(users, tokens, hasher, issuer, clock, store,
		usecase.AuthConfig{
			AccessTTL:  cfg.AccessTokenTTL,
			RefreshTTL: cfg.RefreshTokenTTL,
			SingleUser: cfg.SingleUser,
		}, logger)

	projectService := usecase.NewProjects(projects, runs, artifacts, recorder, clock, store,
		cfg.WorkspaceRoot(), logger)

	// --- inference (optional) ---------------------------------------------
	//
	// The factory is fully functional without a model. When one is configured
	// the agents add real judgement on top of their blueprints; when it is
	// absent or unreachable, they fall back deterministically. Probing at boot
	// means the operator learns immediately, not thirty seconds into a build.
	var (
		inference port.LLM
		memory    *factory.MemoryService
	)
	if cfg.LLMBaseURL != "" {
		provider := llm.New(llm.Config{
			BaseURL:      cfg.LLMBaseURL,
			APIKey:       cfg.LLMAPIKey,
			Timeout:      cfg.LLMTimeout,
			DefaultModel: cfg.LLMModel,
			Name:         "openai-compatible",
		}, logger)

		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		err := provider.Ready(probeCtx)
		probeCancel()

		if err != nil {
			logger.Warn("inference server is not reachable; agents will use deterministic blueprints",
				"url", cfg.LLMBaseURL, "error", err)
		} else {
			inference = provider
			models, _ := provider.Models(ctx)
			names := make([]string, 0, len(models))
			for _, m := range models {
				names = append(names, m.ID)
			}
			logger.Info("inference enabled", "url", cfg.LLMBaseURL, "models", names)
		}
	} else {
		logger.Info("no inference server configured; agents will use deterministic blueprints",
			"hint", "set GENESIS_LLM_URL to enable model-backed reasoning")
	}

	memory = factory.NewMemoryService(factory.NewInMemoryStore(), nil)

	// --- execution sandbox --------------------------------------------------
	//
	// Generated code is built and run under OS-level isolation. Probing at boot
	// means an operator learns immediately whether isolation is real on this
	// host, rather than discovering after the fact that untrusted code ran
	// unconfined.
	executor := sandbox.New(sandbox.DefaultConfig(), logger)
	caps := executor.Capabilities()
	if caps.Complete() {
		logger.Info("execution sandbox ready",
			"namespaces", caps.Namespaces,
			"network_isolated", caps.NetworkIsolated,
			"memory_limited", caps.MemoryLimited)
	} else {
		logger.Warn("execution sandbox is degraded; generated code will run with reduced isolation",
			"degraded", caps.Degraded, "namespaces", caps.Namespaces)
	}

	driver := factory.NewDriver(runs, projects, artifacts, tasks, recorder,
		factory.DriverConfig{
			MaxParallelAgents: cfg.MaxParallelAgents,
			LLM:               inference,
			Memory:            memory,
			Sandbox:           executor,
			VersionControl:    true,
		}, logger)

	if vcs.Available() {
		logger.Info("version control enabled; every phase is snapshotted and reversible")
	} else {
		logger.Warn("git is not installed; generated projects will have no history and cannot be rolled back")
	}

	runService := usecase.NewRuns(runs, projects, tasks, artifacts, events, recorder,
		driver, clock, store, logger)

	workspaceService := usecase.NewWorkspaces(projects, recorder, clock, vcs.NewFactory(), logger)

	// A run marked "running" in the database with no goroutine executing it is
	// a lie the UI cannot detect. Reconcile at boot.
	if recovered, err := runService.RecoverInterrupted(ctx); err != nil {
		logger.Warn("failed to reconcile interrupted runs", "error", err)
	} else if recovered > 0 {
		logger.Warn("marked interrupted runs from a previous process", "count", recovered)
	}

	if cfg.SingleUser {
		owner, err := authService.EnsureLocalOwner(ctx)
		if err != nil {
			return fmt.Errorf("bootstrap local owner: %w", err)
		}
		session, err := authService.IssueFor(ctx, owner, usecase.ClientInfo{UserAgent: "genesis-desktop", IP: "127.0.0.1"})
		if err != nil {
			return fmt.Errorf("issue local session: %w", err)
		}
		// The desktop app and CLI read this file to authenticate without a
		// login prompt on a single-user machine.
		if err := writeLocalSession(cfg.DataDir, session); err != nil {
			logger.Warn("could not persist the local session token", "error", err)
		} else {
			logger.Info("local owner session ready", "user", owner.Email,
				"token_file", localSessionPath(cfg.DataDir))
		}

		// Keep the file fresh.
		//
		// Access tokens live 15 minutes: they cannot be revoked, so a short
		// life is the only bound on a stolen one. That is right for a token,
		// and wrong for a file that local clients read once at startup — the
		// desktop app and CLI would work for a quarter of an hour and then
		// report "token expired", which reads like a licence problem and is
		// in fact a lapsed local sign-in.
		//
		// Reissuing on a timer well inside the lifetime means a client that
		// re-reads the file always finds a usable token. Clients that hold one
		// in memory refresh over HTTP instead; this covers the ones that do
		// not, and it costs one cheap write per interval on a single-user
		// machine.
		go func() {
			interval := cfg.AccessTokenTTL / 3
			if interval < time.Minute {
				interval = time.Minute
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					renewed, err := authService.IssueFor(ctx, owner,
						usecase.ClientInfo{UserAgent: "genesis-desktop", IP: "127.0.0.1"})
					if err != nil {
						logger.Warn("could not renew the local session", "error", err)
						continue
					}
					if err := writeLocalSession(cfg.DataDir, renewed); err != nil {
						logger.Warn("could not persist the renewed local session", "error", err)
					}
				}
			}
		}()
	}

	// --- transport --------------------------------------------------------
	hub := ws.NewHub(eventBus, events, logger)
	handlers := adapterhttp.NewHandlers(authService, projectService, runService, workspaceService, inference, version, commit)

	app := adapterhttp.NewRouter(handlers, hub, issuer, adapterhttp.RouterConfig{
		CORSOrigins: cfg.CORSOrigins,
		BodyLimit:   cfg.RequestLimit,
		Version:     version,
		Commit:      commit,
		ReadinessFn: store.Ping,
	}, logger)

	// Background maintenance: expired refresh tokens accumulate forever
	// otherwise, and a growing table slowly degrades every login.
	go reapTokens(ctx, tokens, logger)

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "version", version)
		if err := app.Listen(cfg.Addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return fmt.Errorf("server: %w", err)
	case sig := <-stop:
		logger.Info("shutdown signal received", "signal", sig.String())
	}

	// Graceful shutdown: stop accepting, drain in-flight requests, then cancel
	// background work. Killing runs first would leave the API answering with
	// half-torn-down state.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer shutdownCancel()

	if active := driver.ActiveCount(); active > 0 {
		logger.Info("waiting for active builds to stop", "count", active)
	}
	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown incomplete", "error", err)
	}
	cancel()
	eventBus.Close()
	logger.Info("shutdown complete")
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{
		Level: level,
		// Redact anything that looks like a credential regardless of where it
		// came from: a single careless log line can leak a token permanently.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case "password", "token", "secret", "access_token", "refresh_token",
				"password_hash", "jwt_secret", "authorization":
				return slog.String(a.Key, "[redacted]")
			}
			return a
		},
	}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func reapTokens(ctx context.Context, tokens interface {
	DeleteExpired(context.Context, time.Time) (int64, error)
}, log *slog.Logger) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := tokens.DeleteExpired(ctx, time.Now().UTC())
			if err != nil {
				log.Warn("token cleanup failed", "error", err)
				continue
			}
			if n > 0 {
				log.Info("expired refresh tokens removed", "count", n)
			}
		}
	}
}
