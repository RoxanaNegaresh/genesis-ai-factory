package http

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"

	"github.com/genesis-ai-factory/control-plane/internal/adapter/ws"
	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// RouterConfig carries what the router needs from configuration.
type RouterConfig struct {
	CORSOrigins []string
	BodyLimit   int
	Version     string
	Commit      string
	ReadinessFn func(ctx context.Context) error
}

// NewRouter builds the Fiber application with the full middleware stack.
func NewRouter(
	h *Handlers,
	hub *ws.Hub,
	issuer port.TokenIssuer,
	cfg RouterConfig,
	log *slog.Logger,
) *fiber.App {
	if cfg.BodyLimit <= 0 {
		cfg.BodyLimit = 4 << 20
	}

	app := fiber.New(fiber.Config{
		AppName:               "Genesis AI Factory",
		DisableStartupMessage: true,
		ErrorHandler:          ErrorHandler(log),
		BodyLimit:             cfg.BodyLimit,
		ReadTimeout:           30 * time.Second,
		// WriteTimeout is deliberately generous: websocket upgrades and event
		// streams are long-lived, and a short write timeout would sever them.
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		StreamRequestBody: true,
	})

	app.Use(RequestID())
	app.Use(SecurityHeaders())
	app.Use(Logger(log))
	app.Use(Recover(log))
	app.Use(CORS(cfg.CORSOrigins))

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "version": cfg.Version})
	})

	// Readiness differs from liveness: it reports whether dependencies are
	// usable, so an orchestrator stops routing traffic instead of restarting a
	// healthy process whose database is briefly unavailable.
	app.Get("/ready", func(c *fiber.Ctx) error {
		if cfg.ReadinessFn != nil {
			ctx, cancel := context.WithTimeout(c.Context(), 3*time.Second)
			defer cancel()
			if err := cfg.ReadinessFn(ctx); err != nil {
				return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
					"status": "unavailable", "reason": "database unreachable"})
			}
		}
		return c.JSON(fiber.Map{"status": "ready"})
	})

	api := app.Group("/api/v1")
	api.Get("/meta", h.Meta)

	// Authentication endpoints are rate limited independently and more tightly
	// than the rest of the API: they are the credential-stuffing surface.
	authLimiter := NewRateLimit(20, time.Minute)
	auth := api.Group("/auth", authLimiter.Handler())
	auth.Post("/register", h.Register)
	auth.Post("/login", h.Login)
	auth.Post("/refresh", h.Refresh)
	auth.Post("/logout", h.Logout)
	auth.Get("/me", Authenticate(issuer), h.Me)

	protected := api.Group("", Authenticate(issuer))

	protected.Get("/agents", h.Agents)
	protected.Get("/models", h.Models)
	protected.Get("/blueprints", h.Blueprints)
	protected.Post("/classify", h.Classify)

	projects := protected.Group("/projects")
	projects.Get("/", h.ListProjects)
	projects.Post("/", h.CreateProject)
	projects.Get("/:id", h.GetProject)
	projects.Patch("/:id", h.UpdateProject)
	projects.Delete("/:id", RequireRole(domain.RoleMember), h.DeleteProject)
	projects.Get("/:id/artifacts", h.ProjectArtifacts)

	// IDE surface: the file tree, editing, search, history and rollback that
	// make generated code inspectable and reversible.
	projects.Get("/:id/files", h.WorkspaceTree)
	projects.Get("/:id/export", h.ExportWorkspace)
	projects.Get("/:id/file", h.ReadWorkspaceFile)
	projects.Put("/:id/file", h.WriteWorkspaceFile)
	projects.Get("/:id/search", h.SearchWorkspace)
	projects.Get("/:id/history", h.WorkspaceHistory)
	projects.Get("/:id/diff", h.WorkspaceDiff)
	projects.Get("/:id/vcs", h.WorkspaceStatus)
	projects.Post("/:id/rollback", RequireRole(domain.RoleMember), h.RollbackWorkspace)
	projects.Get("/:id/runs", h.ListRuns)
	projects.Post("/:id/runs", h.StartRun)

	runs := protected.Group("/runs")
	runs.Get("/:id", h.GetRun)
	runs.Post("/:id/cancel", h.CancelRun)
	runs.Get("/:id/events", h.RunEvents)
	runs.Get("/:id/tasks", h.RunTasks)
	runs.Get("/:id/artifacts", h.RunArtifacts)
	runs.Get("/:id/agents", h.RunAgents)

	protected.Get("/artifacts/:id", h.GetArtifact)

	// Websocket: authenticate before the upgrade so an unauthenticated client
	// never occupies a connection slot.
	app.Use("/ws", func(c *fiber.Ctx) error {
		if !websocket.IsWebSocketUpgrade(c) {
			return fiber.ErrUpgradeRequired
		}
		principal, err := issuer.Parse(bearerToken(c))
		if err != nil {
			return err
		}
		c.Locals("principal", principal)
		return c.Next()
	})
	app.Get("/ws", websocket.New(hub.Handle))

	app.Use(func(c *fiber.Ctx) error {
		return domain.NotFound("route")
	})

	return app
}
