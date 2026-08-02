package factory

import (
	"context"
	"fmt"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// BackendAgent generates the Go service: domain entities, ports, use cases,
// HTTP handlers, routing and tests.
//
// v0.1 generates from deterministic templates driven by the blueprint. This is
// not a placeholder for "real" generation — template-driven structure plus
// model-authored business logic is the correct division of labour, because
// structure must be *consistent* (a model that invents a different layout per
// file produces an unmaintainable repository) while logic must be *contextual*.
// v0.2 fills the logic bodies via the AI engine behind this same interface.
type BackendAgent struct{}

func (a *BackendAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleBackend)
	return Charter{
		Role: domain.RoleBackend, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactArchSpec, domain.ArtifactDBSchema},
		Outputs: []domain.ArtifactKind{domain.ArtifactCodeBackend},
		Tools:   []string{"fs.write", "fs.read", "exec.run", "git.commit"}, ModelClass: "code",
		Budget: DefaultBudget(), Temperature: 0.1,
	}
}

func (a *BackendAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	if !bb.Has(domain.ArtifactDBSchema) {
		return nil, fmt.Errorf("backend engineer requires the database schema")
	}
	bp := bb.Blueprint
	module := "github.com/genesis/" + bb.Project.Slug

	// v0.3: the model derives domain constraints the blueprint cannot know —
	// that a deal value must be positive, that a close date lies in the future.
	// Structure still comes from templates; only the semantics are authored.
	generator := NewLogicGenerator(bb.Reasoning)
	requirements := ""
	if artifact, ok := bb.Get(domain.ArtifactPRD); ok {
		requirements = artifact.Body
	}
	rules := generator.DeriveRules(ctx, tb, bb, requirements)
	if len(rules) > 0 {
		tb.Emit(ctx, domain.LevelInfo,
			fmt.Sprintf("Derived %d domain validation rules from the requirements", len(rules)),
			map[string]any{"rules": len(rules)})
		bb.SetValue("business_rules", rules)
	}

	// Entities that receive a full stack. User is excluded: authentication is
	// generated separately and owns its own persistence.
	resources := make([]Entity, 0, len(bp.Entities))
	for _, e := range bp.Entities {
		if e.Name == "User" {
			continue
		}
		resources = append(resources, e)
	}

	// Authentication is generated only when the blueprint carries a User
	// entity to authenticate against.
	userEntity, hasAuth := authEntity(bp)

	files := map[string]string{
		"api/go.mod":                       backendGoMod(module),
		"api/cmd/server/main.go":           backendMain(module, bb.Project.Name, resources, hasAuth),
		"api/internal/config/config.go":    backendConfig(),
		"api/internal/domain/errors.go":    backendDomainErrors(),
		"api/internal/httpx/errors.go":     backendHTTPErrors(module),
		"api/internal/httpx/middleware.go": backendMiddleware(module),
		"api/README.md":                    backendReadme(bb.Project.Name, bp),

		// v1.1: the persistence layer. Until now the ports had no
		// implementation, so a generated product could not store a row.
		"api/internal/infra/postgres/pool.go":          backendPostgresPool(),
		"api/internal/infra/postgres/cursor.go":        backendPostgresCursor(),
		"api/internal/infra/postgres/errors.go":        backendPostgresErrors(),
		"api/internal/infra/postgres/domain_errors.go": backendPostgresErrorBridge(module),
		"api/internal/infra/postgres/cursor_test.go":   backendCursorTest(),
		"api/internal/infra/postgres/contract_test.go": backendRepositoryContractTest(module, bp.Entities),

		// v1.2: the transaction boundary. Repository methods are each atomic,
		// which does not compose; a use case writing through two of them needs
		// both to commit or neither.
		"api/internal/port/unit_of_work.go":      backendPortUnitOfWork(),
		"api/internal/infra/postgres/tx.go":      backendPostgresUnitOfWork(module),
		"api/internal/infra/postgres/tx_test.go": backendUnitOfWorkTest(module, resources),
	}

	// v1.2: authentication. The schema always carried a users table and the
	// config always demanded a JWT_SECRET, but nothing issued or checked a
	// token and every resource route was public.
	if hasAuth {
		files["api/internal/domain/user.go"] = backendAuthDomain(userEntity)
		files["api/internal/port/auth.go"] = backendAuthPort(module)
		files["api/internal/infra/authcrypto/crypto.go"] = backendAuthCrypto(module)
		files["api/internal/infra/authcrypto/crypto_test.go"] = backendAuthCryptoTest()
		files["api/internal/infra/postgres/auth_repository.go"] = backendAuthRepository(module)
		files["api/internal/usecase/auth_service.go"] = backendAuthUseCase(module, defaultRole(userEntity))
		files["api/internal/usecase/auth_tokens.go"] = backendAuthTokenBridge(module)
		files["api/internal/adapter/http/auth_handler.go"] = backendAuthHandler(module)
		files["api/internal/adapter/http/auth_middleware.go"] = backendAuthMiddleware(module)
		files["api/internal/adapter/http/auth_test.go"] = backendAuthContractTest(module, resources)
		files["api/internal/infra/postgres/auth_service_test.go"] = backendAuthServiceTest(module)
	}

	// One integration test, for one entity that can be inserted without
	// fixtures. Emitting it per entity would duplicate the shared helpers in
	// the same package; one real round trip against a real server proves the
	// generated SQL works, and the seed helpers make extending it mechanical.
	if probe, ok := standaloneEntity(resources); ok {
		files["api/internal/infra/postgres/integration_test.go"] =
			backendPostgresIntegrationTest(module, probe)
	}

	// One domain file, one repository port, one use case and one handler set
	// per entity: the same shape every time, so a human can navigate any
	// generated project after learning one.
	for _, e := range bp.Entities {
		if e.Name == "User" {
			continue
		}
		lower := toSnake(e.Name)
		files["api/internal/domain/"+lower+".go"] = backendEntity(e, RulesFor(rules, e.Name))
		files["api/internal/port/"+lower+"_repository.go"] = backendPort(module, e)
		files["api/internal/usecase/"+lower+"_service.go"] = backendUseCase(module, e)
		files["api/internal/adapter/http/"+lower+"_handler.go"] = backendHandler(module, e)
		files["api/internal/domain/"+lower+"_test.go"] = backendEntityTest(e)
		files["api/internal/infra/postgres/"+lower+"_repository.go"] = backendPostgresRepo(module, e)
		files["api/internal/infra/postgres/"+lower+"_seed_test.go"] = backendPostgresSeed(module, e)
	}

	for path, content := range files {
		if err := tb.WriteFile(ctx, path, content); err != nil {
			return nil, err
		}
	}

	accepted, rejected := generator.Stats()
	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Generated %d Go files across domain, port, usecase and adapter layers", len(files)),
		map[string]any{
			"files": len(files), "module": module,
			"model_authored_bodies": accepted, "rejected_bodies": len(rejected),
			"domain_rules": len(rules),
		})

	summary := backendSummary(bp, module, len(files), rules, accepted, rejected)
	return []*domain.Artifact{
		artifact(bb, domain.ArtifactCodeBackend, "backend-manifest.md", "text/markdown", summary),
	}, nil
}

// backendGoMod emits the module manifest.
//
// No go.sum is generated: the checksums cannot be invented, they must be
// computed from the actual module downloads. The generated Makefile and README
// therefore make `go mod tidy` an explicit first step rather than shipping a
// fabricated file that would fail verification.
func backendGoMod(module string) string {
	return fmt.Sprintf(`module %s

go 1.23

require (
	github.com/gofiber/fiber/v2 v2.52.6
	github.com/jackc/pgx/v5 v5.7.2
	golang.org/x/crypto v0.31.0
)
`, module)
}

func backendMain(module, projectName string, resources []Entity, hasAuth bool) string {
	var imports strings.Builder
	if len(resources) > 0 {
		// The adapter package is named http, which collides with net/http in
		// this file. Aliasing it is clearer than renaming the package, whose
		// name is correct in every other context.
		fmt.Fprintf(&imports, "\n\n\tapphttp %q\n\t%q\n\t%q",
			module+"/internal/adapter/http",
			module+"/internal/infra/postgres",
			module+"/internal/usecase")
		if hasAuth {
			fmt.Fprintf(&imports, "\n\t%q\n\t%q",
				module+"/internal/infra/authcrypto", module+"/internal/port")
		}
	}

	// The composition root: the one place that knows every concrete type.
	// Constructing the graph here, rather than letting each layer reach for
	// its own dependencies, is what keeps the dependency rule enforceable —
	// every inner layer receives an interface it cannot see the source of.
	var wiring strings.Builder
	if len(resources) > 0 {
		wiring.WriteString("\n\t// Persistence. The pool is created without dialling; a database that\n")
		wiring.WriteString("\t// is briefly unreachable must not prevent the process from starting.\n")
		wiring.WriteString("\tpool, err := postgres.NewPool(ctx, cfg.DatabaseURL)\n")
		wiring.WriteString("\tif err != nil {\n\t\treturn err\n\t}\n")
		wiring.WriteString("\tdefer pool.Close()\n\n")
		wiring.WriteString("\tif err := postgres.Ping(ctx, pool); err != nil {\n")
		wiring.WriteString("\t\tlogger.Warn(\"database unreachable at startup; will retry on demand\", \"error\", err)\n")
		wiring.WriteString("\t}\n\n")
		wiring.WriteString("\t// The transaction boundary. Hand this to any use case that must write\n")
		wiring.WriteString("\t// through more than one repository atomically.\n")
		wiring.WriteString("\tuow := postgres.NewUnitOfWork(pool)\n")
		if hasAuth {
			wiring.WriteString("\n\t// Authentication. The refresh-token primitives are injected rather\n")
			wiring.WriteString("\t// than imported so the use case layer does not depend on crypto.\n")
			wiring.WriteString("\tusecase.SetRefreshTokenFuncs(authcrypto.NewRefreshToken, authcrypto.HashRefreshToken)\n")
			wiring.WriteString("\ttokens := authcrypto.NewHMACIssuer(cfg.JWTSecret)\n")
			wiring.WriteString("\tauth := usecase.NewAuthService(\n")
			wiring.WriteString("\t\tpostgres.NewUserRepo(pool),\n")
			wiring.WriteString("\t\tpostgres.NewSessionRepo(pool),\n")
			wiring.WriteString("\t\tauthcrypto.NewArgon2Hasher(),\n")
			wiring.WriteString("\t\ttokens,\n")
			wiring.WriteString("\t\tuow,\n")
			wiring.WriteString("\t)\n")
		} else {
			wiring.WriteString("\t_ = uow\n")
		}
	}

	var routes strings.Builder
	if len(resources) == 0 {
		routes.WriteString("\t_ = r\n")
	} else {
		indent := "\t"
		if hasAuth {
			routes.WriteString("\t// Public: a caller cannot present a token before obtaining one.\n")
			routes.WriteString("\tapphttp.NewAuthHandler(auth).Register(r)\n\n")
			routes.WriteString("\t// Everything below requires a valid access token. Protection is\n")
			routes.WriteString("\t// applied to the group, not listed per route: opt-in protection\n")
			routes.WriteString("\t// fails open the day someone forgets it on a new endpoint.\n")
			routes.WriteString("\tguarded := r.Group(\"\", apphttp.RequireAuth(tokens))\n")
			routes.WriteString("\tapphttp.NewAuthHandler(auth).RegisterProtected(guarded)\n\n")
			routes.WriteString("\t// Repository -> service -> handler, one chain per resource.\n")
			indent = "\t"
			for _, e := range resources {
				fmt.Fprintf(&routes,
					"%sapphttp.New%sHandler(usecase.New%sService(postgres.New%sRepo(db))).Register(guarded)\n",
					indent, e.Name, e.Name, e.Name)
			}
		} else {
			routes.WriteString("\t// Repository -> service -> handler, one chain per resource.\n")
			for _, e := range resources {
				fmt.Fprintf(&routes,
					"\tapphttp.New%sHandler(usecase.New%sService(postgres.New%sRepo(db))).Register(r)\n",
					e.Name, e.Name, e.Name)
			}
		}
	}

	routerSig := "func registerRoutes(r fiber.Router) {"
	routerCall := "registerRoutes(api)"
	if len(resources) > 0 && hasAuth {
		routerSig = "func registerRoutes(\n\tr fiber.Router,\n\tdb *postgres.DB,\n\tauth *usecase.AuthService,\n\ttokens port.TokenIssuer,\n) {"
		routerCall = "registerRoutes(api, pool, auth, tokens)"
	} else if len(resources) > 0 {
		routerSig = "func registerRoutes(r fiber.Router, db *postgres.DB) {"
		routerCall = "registerRoutes(api, pool)"
	}

	return fmt.Sprintf(`// Command server starts the %s API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"

	"%s/internal/config"
	"%s/internal/httpx"%s
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
%s
	app := fiber.New(fiber.Config{
		AppName:               %q,
		DisableStartupMessage: true,
		ReadTimeout:           15 * time.Second,
		WriteTimeout:          30 * time.Second,
		IdleTimeout:           60 * time.Second,
		BodyLimit:             4 * 1024 * 1024,
		ErrorHandler:          httpx.ErrorHandler,
	})

	app.Use(httpx.RequestID())
	app.Use(httpx.Logger(logger))
	app.Use(httpx.Recover(logger))
	app.Use(httpx.CORS(cfg.CORSOrigins))

	// Liveness: is this process running? It must not touch the database, or a
	// database outage will cause the orchestrator to kill healthy processes
	// and turn a recoverable incident into an outage of its own.
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})
%s
	api := app.Group("/api/v1")
	%s

	// Serve until interrupted, then drain in-flight requests before exiting so
	// a deploy does not sever active connections.
	errCh := make(chan error, 1)
	go func() {
		if err := app.Listen(cfg.Addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	logger.Info("server listening", "addr", cfg.Addr)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case <-stop:
		logger.Info("shutdown requested")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()
	return app.ShutdownWithContext(shutdownCtx)
}

// registerRoutes wires the generated resource handlers onto the API group.
%s
%s}
`, projectName, module, module, imports.String(), wiring.String(), projectName, readiness(len(resources) > 0), routerCall, routerSig, routes.String())
}

// readiness emits the readiness endpoint, which exists only when there is a
// database to report on. Separating it from liveness is the whole point: this
// one is allowed to fail while the process stays alive.
func readiness(hasDB bool) string {
	if !hasDB {
		return ""
	}
	return `
	// Readiness: can this process serve traffic? Unlike liveness, this is
	// allowed to fail while the process keeps running.
	app.Get("/ready", func(c *fiber.Ctx) error {
		if err := postgres.Ping(c.Context(), pool); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"status": "degraded", "database": "unreachable"})
		}
		return c.JSON(fiber.Map{"status": "ready", "database": "ok"})
	})
`
}

func backendConfig() string {
	return `// Package config resolves runtime configuration from the environment.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Config is the validated runtime configuration.
type Config struct {
	Addr        string
	DatabaseURL string
	RedisURL    string
	JWTSecret   string
	Env         string
	Level       string
	CORSOrigins []string
}

// Load reads configuration, applying defaults suitable for local development.
func Load() (Config, error) {
	c := Config{
		Addr:        env("ADDR", ":8080"),
		DatabaseURL: env("DATABASE_URL", ""),
		RedisURL:    env("REDIS_URL", ""),
		JWTSecret:   env("JWT_SECRET", ""),
		Env:         env("ENV", "development"),
		Level:       env("LOG_LEVEL", "info"),
		CORSOrigins: strings.Split(env("CORS_ORIGINS", "http://localhost:5173"), ","),
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	// A weak signing key silently undermines every authentication guarantee,
	// so it is rejected at boot rather than discovered in an incident.
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("JWT_SECRET must be at least 32 characters")
	}
	return c, nil
}

// LogLevel maps the configured level onto slog.
func (c Config) LogLevel() slog.Level {
	switch strings.ToLower(c.Level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}
`
}

func backendDomainErrors() string {
	return `package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors are the vocabulary the transport layer translates into
// status codes. Business code returns these; only the HTTP layer knows what a
// 404 is.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrValidation   = errors.New("validation failed")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

// Error carries a machine-readable code alongside a human message.
type Error struct {
	Code    string
	Message string
	Kind    error
	Fields  map[string]string
}

func (e *Error) Error() string  { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
func (e *Error) Unwrap() error  { return e.Kind }

// NotFound builds a not-found error for a resource.
func NotFound(resource string) *Error {
	return &Error{Code: resource + "_not_found", Message: resource + " does not exist", Kind: ErrNotFound}
}

// Invalid builds a validation error.
func Invalid(code, message string) *Error {
	return &Error{Code: code, Message: message, Kind: ErrValidation}
}

// Conflict builds a conflict error.
func Conflict(code, message string) *Error {
	return &Error{Code: code, Message: message, Kind: ErrConflict}
}

// Unauthorized builds an authentication error: the caller is not identified.
func Unauthorized(code, message string) *Error {
	return &Error{Code: code, Message: message, Kind: ErrUnauthorized}
}

// Forbidden builds an authorization error: the caller is identified but not
// permitted. The distinction matters — 401 tells a client to authenticate,
// 403 tells it not to bother.
func Forbidden(code, message string) *Error {
	return &Error{Code: code, Message: message, Kind: ErrForbidden}
}

// Validation accumulates field-level problems so a client can correct an entire
// form in one round trip.
type Validation struct{ Fields map[string]string }

// NewValidation starts an empty accumulator.
func NewValidation() *Validation { return &Validation{Fields: map[string]string{}} }

// Add records a field problem.
func (v *Validation) Add(field, problem string) *Validation {
	v.Fields[field] = problem
	return v
}

// Err returns nil when everything passed, otherwise an aggregate error.
func (v *Validation) Err() error {
	if len(v.Fields) == 0 {
		return nil
	}
	return &Error{Code: "validation_failed", Message: "one or more fields are invalid",
		Kind: ErrValidation, Fields: v.Fields}
}
`
}

func backendHTTPErrors(module string) string {
	return fmt.Sprintf(`// Package httpx holds transport concerns shared by every handler.
package httpx

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v2"

	"%s/internal/domain"
)

// ErrorHandler is the single place where domain errors become HTTP responses.
// Centralising it means no handler can invent its own error shape, and the
// client only ever parses one envelope.
func ErrorHandler(c *fiber.Ctx, err error) error {
	requestID, _ := c.Locals("request_id").(string)

	var de *domain.Error
	if errors.As(err, &de) {
		body := fiber.Map{"code": de.Code, "message": de.Message, "request_id": requestID}
		if len(de.Fields) > 0 {
			body["fields"] = de.Fields
		}
		return c.Status(statusFor(de.Kind)).JSON(fiber.Map{"error": body})
	}

	var fe *fiber.Error
	if errors.As(err, &fe) {
		return c.Status(fe.Code).JSON(fiber.Map{"error": fiber.Map{
			"code": "http_error", "message": fe.Message, "request_id": requestID}})
	}

	// Unexpected errors are logged in full and reported opaquely: internal
	// details are useful to us and useful to an attacker.
	slog.Error("unhandled error", "error", err, "path", c.Path(), "request_id", requestID)
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fiber.Map{
		"code": "internal_error", "message": "an unexpected error occurred", "request_id": requestID}})
}

func statusFor(kind error) int {
	switch {
	case errors.Is(kind, domain.ErrNotFound):
		return fiber.StatusNotFound
	case errors.Is(kind, domain.ErrConflict):
		return fiber.StatusConflict
	case errors.Is(kind, domain.ErrValidation):
		return fiber.StatusUnprocessableEntity
	case errors.Is(kind, domain.ErrUnauthorized):
		return fiber.StatusUnauthorized
	case errors.Is(kind, domain.ErrForbidden):
		return fiber.StatusForbidden
	}
	return fiber.StatusInternalServerError
}
`, module)
}

func backendMiddleware(module string) string {
	return `package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// RequestID attaches a correlation id to every request and echoes it back, so
// a user-reported error can be traced to exact log lines.
func RequestID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Get("X-Request-ID")
		if id == "" {
			buf := make([]byte, 8)
			_, _ = rand.Read(buf)
			id = hex.EncodeToString(buf)
		}
		c.Locals("request_id", id)
		c.Set("X-Request-ID", id)
		return c.Next()
	}
}

// Logger emits one structured line per request.
func Logger(log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		log.Info("request",
			"method", c.Method(),
			"path", c.Path(),
			"status", c.Response().StatusCode(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", c.Locals("request_id"),
		)
		return err
	}
}

// Recover converts a panic into a 500 instead of killing the worker.
func Recover(log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered", "panic", r, "path", c.Path())
				err = fiber.NewError(fiber.StatusInternalServerError, "internal error")
			}
		}()
		return c.Next()
	}
}

// CORS applies a strict allowlist. Wildcard origins are deliberately not
// supported: credentials plus "*" is an exfiltration primitive.
func CORS(origins []string) fiber.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return func(c *fiber.Ctx) error {
		origin := c.Get("Origin")
		if origin != "" && allowed[origin] {
			c.Set("Access-Control-Allow-Origin", origin)
			c.Set("Access-Control-Allow-Credentials", "true")
			c.Set("Vary", "Origin")
			c.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, Idempotency-Key")
			c.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if c.Method() == fiber.MethodOptions {
			return c.SendStatus(fiber.StatusNoContent)
		}
		return c.Next()
	}
}
`
}

// goType maps a blueprint field onto a Go type. Decimals are strings at the
// boundary to avoid float rounding; nullable columns become pointers so "unset"
// and "zero" remain distinguishable.
func goType(f Field) string {
	base := "string"
	switch f.Type {
	case "int":
		base = "int64"
	case "decimal":
		base = "string"
	case "bool":
		base = "bool"
	case "timestamp":
		base = "time.Time"
	case "json":
		base = "map[string]any"
	}
	if !f.Required && f.Type != "json" {
		return "*" + base
	}
	return base
}

func goFieldName(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		switch p {
		case "id":
			parts[i] = "ID"
		case "url":
			parts[i] = "URL"
		case "sku":
			parts[i] = "SKU"
		case "api":
			parts[i] = "API"
		default:
			if p != "" {
				parts[i] = strings.ToUpper(p[:1]) + p[1:]
			}
		}
	}
	return strings.Join(parts, "")
}

// entityHasValidatableFields reports whether the generated Validate method
// contains any check that an empty record would fail.
func entityHasValidatableFields(e Entity) bool {
	for _, f := range e.Fields {
		if isSystemField(f.Name) || !f.Required {
			continue
		}
		switch f.Type {
		case "text", "enum", "uuid", "ref":
			return true
		}
	}
	return false
}

// entityNeedsStrings reports whether the generated Validate method will emit
// any strings.* call. Go rejects an unused import, so the import block must be
// computed from what the body will actually contain rather than assumed —
// entities consisting only of references and enums use no string helpers.
func entityNeedsStrings(e Entity) bool {
	for _, f := range e.Fields {
		if isSystemField(f.Name) {
			continue
		}
		if f.Required && f.Type == "text" {
			return true
		}
		if f.Name == "email" {
			return true
		}
	}
	return false
}

func backendEntity(e Entity, rules []BusinessRule) string {
	var sb strings.Builder
	sb.WriteString("package domain\n\n")

	// Imports are computed from what the generated body will actually contain,
	// including any model-derived rules: Go rejects an unused import, so this
	// cannot be approximated.
	imports := map[string]bool{"time": true}
	if entityNeedsStrings(e) {
		imports["strings"] = true
	}
	for _, extra := range ruleImports(rules, e) {
		imports[extra] = true
	}
	ordered := make([]string, 0, len(imports))
	for _, candidate := range []string{"strconv", "strings", "time"} {
		if imports[candidate] {
			ordered = append(ordered, candidate)
		}
	}
	if len(ordered) == 1 {
		fmt.Fprintf(&sb, "import %q\n\n", ordered[0])
	} else {
		sb.WriteString("import (\n")
		for _, imp := range ordered {
			fmt.Fprintf(&sb, "\t%q\n", imp)
		}
		sb.WriteString(")\n\n")
	}
	fmt.Fprintf(&sb, "// %s %s\ntype %s struct {\n", e.Name, lowerFirst(e.Description), e.Name)
	for _, f := range e.Fields {
		fmt.Fprintf(&sb, "\t%s %s `json:%q`\n", goFieldName(f.Name), goType(f), f.Name)
	}
	sb.WriteString("\tDeletedAt *time.Time `json:\"deleted_at,omitempty\"`\n}\n\n")

	// Enum constants make invalid states unrepresentable at the type level
	// rather than relying on every call site to remember the allowed strings.
	for _, f := range e.Fields {
		if f.Type != "enum" || len(f.Enum) == 0 {
			continue
		}
		typeName := e.Name + goFieldName(f.Name)
		fmt.Fprintf(&sb, "// %s enumerates the allowed values of %s.%s.\ntype %s = string\n\nconst (\n",
			typeName, e.Name, goFieldName(f.Name), typeName)
		for _, v := range f.Enum {
			fmt.Fprintf(&sb, "\t%s%s %s = %q\n", typeName, goFieldName(v), typeName, v)
		}
		sb.WriteString(")\n\n")
		fmt.Fprintf(&sb, "// Valid%s reports whether v is an allowed %s value.\nfunc Valid%s(v string) bool {\n\tswitch v {\n\tcase ",
			typeName, f.Name, typeName)
		quoted := make([]string, len(f.Enum))
		for i, v := range f.Enum {
			quoted[i] = fmt.Sprintf("%q", v)
		}
		sb.WriteString(strings.Join(quoted, ", "))
		sb.WriteString(":\n\t\treturn true\n\t}\n\treturn false\n}\n\n")
	}

	// Validate enforces the invariants the database also enforces, so a bad
	// request fails fast with a useful message instead of a constraint error.
	fmt.Fprintf(&sb, "// Validate checks the invariants of a %s before persistence.\nfunc (m *%s) Validate() error {\n\tv := NewValidation()\n",
		e.Name, e.Name)
	for _, f := range e.Fields {
		if isSystemField(f.Name) {
			continue
		}
		name := goFieldName(f.Name)
		switch {
		case f.Required && f.Type == "text":
			fmt.Fprintf(&sb, "\tif strings.TrimSpace(m.%s) == \"\" {\n\t\tv.Add(%q, \"is required\")\n\t}\n", name, f.Name)
		case f.Required && f.Type == "enum":
			fmt.Fprintf(&sb, "\tif !Valid%s%s(m.%s) {\n\t\tv.Add(%q, \"is not an allowed value\")\n\t}\n",
				e.Name, name, name, f.Name)
		case f.Required && (f.Type == "uuid" || f.Type == "ref"):
			fmt.Fprintf(&sb, "\tif m.%s == \"\" {\n\t\tv.Add(%q, \"is required\")\n\t}\n", name, f.Name)
		}
		if f.Name == "email" {
			// An optional field is a *string. Comparing or passing it as a
			// string does not compile, and this shipped broken ERP and
			// marketplace projects until the benchmark caught it.
			if f.Required {
				fmt.Fprintf(&sb, "\tif m.%s != \"\" && !strings.Contains(m.%s, \"@\") {\n\t\tv.Add(%q, \"must be a valid email address\")\n\t}\n",
					name, name, f.Name)
			} else {
				fmt.Fprintf(&sb, "\tif m.%s != nil && *m.%s != \"\" && !strings.Contains(*m.%s, \"@\") {\n\t\tv.Add(%q, \"must be a valid email address\")\n\t}\n",
					name, name, name, f.Name)
			}
		}
	}
	// Model-derived domain rules follow the structural checks. Each was
	// rendered from a closed set of templates, so it is guaranteed to compile;
	// the model chose which constraint applies where, not the code that
	// implements it.
	if len(rules) > 0 {
		fields := map[string]Field{}
		for _, f := range e.Fields {
			fields[f.Name] = f
		}
		emitted := 0
		for _, rule := range rules {
			field, known := fields[rule.Field]
			if !known {
				continue
			}
			check := RenderRuleCheck(rule, field)
			if check == "" {
				continue
			}
			if emitted == 0 {
				sb.WriteString("\n\t// Domain rules derived from the product requirements.\n")
			}
			sb.WriteString(indentLines(check, "\t"))
			sb.WriteString("\n")
			emitted++
		}
	}

	sb.WriteString("\treturn v.Err()\n}\n\n")

	fmt.Fprintf(&sb, "// Archived reports whether the %s has been soft deleted.\nfunc (m *%s) Archived() bool { return m.DeletedAt != nil }\n",
		strings.ToLower(humanize(e.Name)), e.Name)
	return sb.String()
}

func backendEntityTest(e Entity) string {
	var sb strings.Builder
	sb.WriteString("package domain\n\nimport \"testing\"\n\n")

	// Only assert rejection when Validate can actually reject something.
	// An entity whose fields are all optional (a pure join table, for example)
	// has nothing to invalidate, and asserting otherwise would generate a test
	// that fails by construction — worse than no test, because it trains people
	// to ignore a red suite.
	if entityHasValidatableFields(e) {
		fmt.Fprintf(&sb, "func Test%sValidateRejectsEmptyRequiredFields(t *testing.T) {\n", e.Name)
		fmt.Fprintf(&sb, "\tm := &%s{}\n\tif err := m.Validate(); err == nil {\n", e.Name)
		sb.WriteString("\t\tt.Fatal(\"expected validation to reject an empty record\")\n\t}\n}\n\n")
	} else {
		fmt.Fprintf(&sb, "// %s has no required scalar fields, so an empty record is valid by design.\n", e.Name)
		fmt.Fprintf(&sb, "func Test%sAcceptsEmptyRecord(t *testing.T) {\n", e.Name)
		fmt.Fprintf(&sb, "\tm := &%s{}\n\tif err := m.Validate(); err != nil {\n", e.Name)
		fmt.Fprintf(&sb, "\t\tt.Fatalf(\"expected an empty %s to be valid, got %%v\", err)\n\t}\n}\n\n", e.Name)
	}

	fmt.Fprintf(&sb, "func Test%sArchivedReflectsDeletedAt(t *testing.T) {\n", e.Name)
	fmt.Fprintf(&sb, "\tm := &%s{}\n\tif m.Archived() {\n\t\tt.Fatal(\"a new record must not be archived\")\n\t}\n}\n", e.Name)

	for _, f := range e.Fields {
		if f.Type != "enum" || len(f.Enum) == 0 {
			continue
		}
		typeName := e.Name + goFieldName(f.Name)
		fmt.Fprintf(&sb, "\nfunc Test%sAcceptsOnlyKnownValues(t *testing.T) {\n", typeName)
		fmt.Fprintf(&sb, "\tfor _, v := range []string{%s} {\n", quoteList(f.Enum))
		fmt.Fprintf(&sb, "\t\tif !Valid%s(v) {\n\t\t\tt.Fatalf(\"%%q should be valid\", v)\n\t\t}\n\t}\n", typeName)
		fmt.Fprintf(&sb, "\tif Valid%s(\"definitely-not-valid\") {\n\t\tt.Fatal(\"unknown value accepted\")\n\t}\n}\n", typeName)
		break
	}
	return sb.String()
}

func quoteList(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(out, ", ")
}

func backendPort(module string, e Entity) string {
	return fmt.Sprintf(`package port

import (
	"context"

	"%s/internal/domain"
)

// %sFilter narrows a listing query.
type %sFilter struct {
	Query  string
	Limit  int
	Cursor string
}

// %sRepository persists %s. It is declared here, in the inner layer, and
// implemented by infrastructure: the dependency points inward.
type %sRepository interface {
	Create(ctx context.Context, m *domain.%s) error
	Update(ctx context.Context, m *domain.%s) error
	ByID(ctx context.Context, id string) (*domain.%s, error)
	List(ctx context.Context, f %sFilter) (items []*domain.%s, nextCursor string, err error)
	Archive(ctx context.Context, id string) error
}
`, module, e.Name, e.Name, e.Name, e.Plural, e.Name, e.Name, e.Name, e.Name, e.Name, e.Name)
}

func backendUseCase(module string, e Entity) string {
	lower := strings.ToLower(humanize(e.Name))
	return fmt.Sprintf(`package usecase

import (
	"context"

	"%s/internal/domain"
	"%s/internal/port"
)

// %sService implements the application logic for %s.
type %sService struct {
	repo port.%sRepository
}

// New%sService constructs the service.
func New%sService(repo port.%sRepository) *%sService {
	return &%sService{repo: repo}
}

// Create validates and persists a new %s.
func (s *%sService) Create(ctx context.Context, m *domain.%s) (*domain.%s, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// Get returns a single %s.
func (s *%sService) Get(ctx context.Context, id string) (*domain.%s, error) {
	return s.repo.ByID(ctx, id)
}

// List returns a page of %s.
func (s *%sService) List(ctx context.Context, f port.%sFilter) ([]*domain.%s, string, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	return s.repo.List(ctx, f)
}

// Update applies changes after re-validating the aggregate.
func (s *%sService) Update(ctx context.Context, m *domain.%s) (*domain.%s, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := s.repo.Update(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// Archive soft deletes a %s. Records are never destroyed by an API call.
func (s *%sService) Archive(ctx context.Context, id string) error {
	return s.repo.Archive(ctx, id)
}
`, module, module,
		e.Name, e.Plural, e.Name, e.Name,
		e.Name, e.Name, e.Name, e.Name, e.Name,
		lower, e.Name, e.Name, e.Name,
		lower, e.Name, e.Name,
		e.Plural, e.Name, e.Name, e.Name,
		e.Name, e.Name, e.Name,
		lower, e.Name)
}

func backendHandler(module string, e Entity) string {
	return fmt.Sprintf(`package http

import (
	"github.com/gofiber/fiber/v2"

	"%s/internal/domain"
	"%s/internal/port"
	"%s/internal/usecase"
)

// %sHandler exposes %s over HTTP.
type %sHandler struct {
	svc *usecase.%sService
}

// New%sHandler constructs the handler.
func New%sHandler(svc *usecase.%sService) *%sHandler {
	return &%sHandler{svc: svc}
}

// Register mounts the routes on a router group.
func (h *%sHandler) Register(r fiber.Router) {
	g := r.Group("/%s")
	g.Get("/", h.list)
	g.Post("/", h.create)
	g.Get("/:id", h.get)
	g.Patch("/:id", h.update)
	g.Delete("/:id", h.archive)
}

func (h *%sHandler) list(c *fiber.Ctx) error {
	items, next, err := h.svc.List(c.Context(), port.%sFilter{
		Query:  c.Query("q"),
		Limit:  c.QueryInt("limit", 50),
		Cursor: c.Query("cursor"),
	})
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": items, "next_cursor": next})
}

func (h *%sHandler) create(c *fiber.Ctx) error {
	var body domain.%s
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	created, err := h.svc.Create(c.Context(), &body)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *%sHandler) get(c *fiber.Ctx) error {
	item, err := h.svc.Get(c.Context(), c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(item)
}

func (h *%sHandler) update(c *fiber.Ctx) error {
	existing, err := h.svc.Get(c.Context(), c.Params("id"))
	if err != nil {
		return err
	}
	// Parse onto the loaded record so absent fields keep their stored values:
	// PATCH must not silently blank out what the client did not send.
	if err := c.BodyParser(existing); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	updated, err := h.svc.Update(c.Context(), existing)
	if err != nil {
		return err
	}
	return c.JSON(updated)
}

func (h *%sHandler) archive(c *fiber.Ctx) error {
	if err := h.svc.Archive(c.Context(), c.Params("id")); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}
`, module, module, module,
		e.Name, e.Plural, e.Name, e.Name,
		e.Name, e.Name, e.Name, e.Name, e.Name,
		e.Name, routePath(e),
		e.Name, e.Name,
		e.Name, e.Name,
		e.Name,
		e.Name,
		e.Name)
}

func backendReadme(projectName string, bp Blueprint) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — API\n\n%s\n\n", projectName, bp.Description)
	sb.WriteString("## Layout\n\n```\ncmd/server              entry point and composition root\ninternal/domain         entities and invariants\ninternal/port           repository interfaces\ninternal/usecase        application services\ninternal/adapter/http   HTTP handlers\ninternal/infra/postgres repository implementations (pgx)\ninternal/httpx          middleware and error mapping\nmigrations              SQL schema\n```\n\n")
	sb.WriteString("## Run\n\nThe schema must exist before the server can serve anything: the application\ndoes not create tables at boot, because a process that mutates the schema on\nstartup will race with itself the moment you run two of them.\n\n```bash\n# 1. dependencies (no go.sum is shipped; checksums must be computed locally)\ngo mod tidy\n\n# 2. database\ncreatedb app\npsql -d app -f ../migrations/0001_init.up.sql\n\n# 3. configuration\nexport DATABASE_URL=postgres://postgres:postgres@localhost:5432/app?sslmode=disable\nexport JWT_SECRET=$(openssl rand -hex 32)\n\n# 4. run\ngo run ./cmd/server\n```\n\nCheck it is alive and connected:\n\n```bash\ncurl localhost:8080/health   # process is up\ncurl localhost:8080/ready    # database is reachable\n```\n\n")
	sb.WriteString("## Test\n\n```bash\ngo test ./...\n```\n\nRepository tests need a real server and skip without one. Point them at a\nthrowaway database — they write rows:\n\n```bash\ncreatedb app_test\npsql -d app_test -f ../migrations/0001_init.up.sql\nTEST_DATABASE_URL=postgres://postgres@localhost:5432/app_test?sslmode=disable \\\n  go test ./internal/infra/postgres/ -v\n```\n\n## Endpoints\n\nEvery resource exposes the same five operations. `DELETE` archives rather\nthan destroys, and archived records disappear from reads.\n\n```\nGET    /api/v1/{resource}          list    (?q= search, ?limit= , ?cursor= )\nPOST   /api/v1/{resource}          create\nGET    /api/v1/{resource}/{id}     read\nPATCH  /api/v1/{resource}/{id}     partial update\nDELETE /api/v1/{resource}/{id}     archive\n```\n\n")
	for _, e := range bp.Entities {
		if e.Name == "User" {
			continue
		}
		fmt.Fprintf(&sb, "- `/api/v1/%s` — %s\n", routePath(e), e.Description)
	}
	return sb.String()
}

func backendSummary(bp Blueprint, module string, fileCount int, rules []BusinessRule, accepted int, rejected []BodyRejection) string {
	var sb strings.Builder
	sb.WriteString("# Backend Generation Manifest\n\n")
	fmt.Fprintf(&sb, "**Module:** `%s`  \n**Files generated:** %d  \n**Stack:** Go 1.23, Fiber v2, PostgreSQL\n\n", module, fileCount)

	// Attribution is reported honestly, including what was rejected. A manifest
	// that claims more authorship than occurred is worse than none.
	sb.WriteString("## Authorship\n\n")
	if len(rules) == 0 && accepted == 0 {
		sb.WriteString("All code in this run was generated from deterministic templates. ")
		sb.WriteString("No reasoning model was available, so no product-specific logic was inferred.\n\n")
	} else {
		fmt.Fprintf(&sb, "- %d domain validation rules were derived from the requirements by the reasoning model\n", len(rules))
		if accepted > 0 {
			fmt.Fprintf(&sb, "- %d function bodies were authored by the model and passed compilation checks\n", accepted)
		}
		if len(rejected) > 0 {
			fmt.Fprintf(&sb, "- %d generated bodies were rejected and replaced with safe defaults:\n", len(rejected))
			for _, r := range rejected {
				fmt.Fprintf(&sb, "  - %s\n", r.Error())
			}
		}
		sb.WriteString("\nEverything else is template-generated structure.\n\n")
	}

	if len(rules) > 0 {
		sb.WriteString("## Derived domain rules\n\n| Entity | Field | Rule | Message |\n|---|---|---|---|\n")
		for _, rule := range rules {
			fmt.Fprintf(&sb, "| %s | `%s` | %s | %s |\n", rule.Entity, rule.Field, rule.Rule, rule.Message)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("## Layers\n\n")
	sb.WriteString("| Layer | Contents |\n|---|---|\n")
	sb.WriteString("| `internal/domain` | Entities with validation and enum guards, plus unit tests |\n")
	sb.WriteString("| `internal/port` | Repository interfaces owned by the inner layer |\n")
	sb.WriteString("| `internal/usecase` | Application services orchestrating validation and persistence |\n")
	sb.WriteString("| `internal/adapter/http` | Fiber handlers translating HTTP to use cases |\n")
	sb.WriteString("| `internal/httpx` | Request id, logging, recovery, CORS, error mapping |\n\n")
	sb.WriteString("## Resources\n\n")
	for _, e := range bp.Entities {
		if e.Name == "User" {
			continue
		}
		fmt.Fprintf(&sb, "- **%s** (`/api/v1/%s`) — %d fields\n", e.Name, routePath(e), len(e.Fields))
	}
	return sb.String()
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
