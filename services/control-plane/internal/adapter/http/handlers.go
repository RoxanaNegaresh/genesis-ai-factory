package http

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
	"github.com/genesis-ai-factory/control-plane/internal/port"
	"github.com/genesis-ai-factory/control-plane/internal/usecase"
)

// Handlers holds the use cases the transport layer exposes.
type Handlers struct {
	auth       *usecase.Auth
	projects   *usecase.Projects
	runs       *usecase.Runs
	workspaces *usecase.Workspaces
	inference  port.LLM
	version    string
	commit     string
}

// NewHandlers constructs the handler set. inference may be nil.
func NewHandlers(
	auth *usecase.Auth,
	projects *usecase.Projects,
	runs *usecase.Runs,
	workspaces *usecase.Workspaces,
	inference port.LLM,
	version, commit string,
) *Handlers {
	return &Handlers{auth: auth, projects: projects, runs: runs, workspaces: workspaces,
		inference: inference, version: version, commit: commit}
}

func clientInfo(c *fiber.Ctx) usecase.ClientInfo {
	return usecase.ClientInfo{UserAgent: c.Get("User-Agent"), IP: c.IP()}
}

// parseID converts an untrusted path parameter into a validated identifier.
func parseID(c *fiber.Ctx, name string) (domain.ID, error) {
	id, err := domain.ParseID(c.Params(name))
	if err != nil {
		return domain.Nil, domain.Invalid("id_invalid", "the supplied identifier is not valid")
	}
	return id, nil
}

// --- meta ----------------------------------------------------------------

// Meta reports build information and capabilities so a client can adapt to the
// server it is talking to instead of assuming.
func (h *Handlers) Meta(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"name":    "Genesis AI Factory",
		"version": h.version,
		"commit":  h.commit,
		"capabilities": fiber.Map{
			"agents":      len(domain.AgentRoster()),
			"blueprints":  len(factory.Blueprints()),
			"phases":      domain.PhaseOrder,
			"llm_backend": h.inference != nil,
			"sandbox":     true,
			"git":         h.workspaces != nil,
			"editor":      h.workspaces != nil,
		},
	})
}

// Models reports the inference backend and what it can serve, so a client can
// show the operator whether reasoning is active rather than guessing.
func (h *Handlers) Models(c *fiber.Ctx) error {
	if h.inference == nil {
		return c.JSON(fiber.Map{
			"enabled": false,
			"reason":  "no inference server configured (set GENESIS_LLM_URL)",
			"data":    []any{},
		})
	}

	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
	defer cancel()

	if err := h.inference.Ready(ctx); err != nil {
		return c.JSON(fiber.Map{
			"enabled":  false,
			"provider": h.inference.Name(),
			"reason":   "the inference server is not responding",
			"data":     []any{},
		})
	}

	models, err := h.inference.Models(ctx)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"enabled": true, "provider": h.inference.Name(), "data": models})
}

// Agents returns the static organization roster.
func (h *Handlers) Agents(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"data": domain.AgentRoster()})
}

// Blueprints returns the built-in product templates.
func (h *Handlers) Blueprints(c *fiber.Ctx) error {
	items := factory.Blueprints()
	summaries := make([]fiber.Map, 0, len(items))
	for _, b := range items {
		summaries = append(summaries, fiber.Map{
			"key": b.Key, "name": b.Name, "category": b.Category,
			"description": b.Description, "entities": len(b.Entities),
			"screens": len(b.Screens), "epics": b.Epics,
		})
	}
	return c.JSON(fiber.Map{"data": summaries})
}

// Classify previews how a brief would be categorised, letting a user see the
// system's interpretation before committing to a build.
func (h *Handlers) Classify(c *fiber.Ctx) error {
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	if strings.TrimSpace(body.Prompt) == "" {
		return domain.Invalid("prompt_required", "a product brief is required")
	}
	result := factory.Classify(body.Prompt)
	bp := factory.BlueprintFor(result.Category)
	return c.JSON(fiber.Map{
		"classification": result,
		"blueprint": fiber.Map{
			"key": bp.Key, "name": bp.Name, "description": bp.Description,
			"entities": len(bp.Entities), "screens": len(bp.Screens), "epics": bp.Epics,
		},
		"suggested_name": domain.TitleFromPrompt(body.Prompt),
	})
}

// --- auth ----------------------------------------------------------------

// Register creates an account.
func (h *Handlers) Register(c *fiber.Ctx) error {
	var body registerRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	session, err := h.auth.Register(c.Context(), body.Email, body.Password, body.DisplayName, clientInfo(c))
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(toSession(session))
}

// Login authenticates an account.
func (h *Handlers) Login(c *fiber.Ctx) error {
	var body loginRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	session, err := h.auth.Login(c.Context(), body.Email, body.Password, clientInfo(c))
	if err != nil {
		return err
	}
	return c.JSON(toSession(session))
}

// Refresh rotates a refresh token.
func (h *Handlers) Refresh(c *fiber.Ctx) error {
	var body refreshRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	session, err := h.auth.Refresh(c.Context(), body.RefreshToken, clientInfo(c))
	if err != nil {
		return err
	}
	return c.JSON(toSession(session))
}

// Logout revokes a refresh token.
func (h *Handlers) Logout(c *fiber.Ctx) error {
	var body refreshRequest
	_ = c.BodyParser(&body)
	if err := h.auth.Logout(c.Context(), body.RefreshToken); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// Me returns the authenticated account.
func (h *Handlers) Me(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	user, err := h.auth.Me(c.Context(), principal.UserID)
	if err != nil {
		return err
	}
	return c.JSON(toUser(user))
}

// --- projects ------------------------------------------------------------

// CreateProject registers a product and optionally starts building it.
func (h *Handlers) CreateProject(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	var body createProjectRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}

	project, err := h.projects.Create(c.Context(), principal, usecase.CreateInput{
		Name: body.Name, Prompt: body.Prompt, Settings: body.Settings,
	})
	if err != nil {
		return err
	}

	response := fiber.Map{"project": toProject(project)}
	if body.Start {
		run, err := h.runs.Start(c.Context(), principal, project.ID, usecase.StartInput{Kind: domain.RunBuild})
		if err != nil {
			// The project exists and is valid; report it with the start failure
			// rather than discarding successful work.
			response["run_error"] = err.Error()
		} else {
			response["run"] = toRun(run)
		}
	}
	return c.Status(fiber.StatusCreated).JSON(response)
}

// ListProjects returns a page of the caller's projects.
func (h *Handlers) ListProjects(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	filter := port.ProjectFilter{
		Query:  c.Query("q"),
		Limit:  c.QueryInt("limit", 50),
		Offset: c.QueryInt("offset", 0),
	}
	if s := c.Query("status"); s != "" {
		filter.Status = domain.ProjectStatus(s)
	}
	if cat := c.Query("category"); cat != "" {
		filter.Category = domain.ProjectCategory(cat)
	}

	items, total, err := h.projects.List(c.Context(), principal, filter)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": toProjects(items), "total": total,
		"limit": filter.Limit, "offset": filter.Offset})
}

// GetProject returns one project.
func (h *Handlers) GetProject(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	project, err := h.projects.Get(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(toProject(project))
}

// UpdateProject applies a partial change.
func (h *Handlers) UpdateProject(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	var body updateProjectRequest
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	project, err := h.projects.Update(c.Context(), principal, id, usecase.UpdateInput{
		Name: body.Name, Description: body.Description, Status: body.Status,
		Category: body.Category, Settings: body.Settings,
	})
	if err != nil {
		return err
	}
	return c.JSON(toProject(project))
}

// DeleteProject archives a project.
func (h *Handlers) DeleteProject(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	if err := h.projects.Delete(c.Context(), principal, id); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ProjectArtifacts lists documents produced for a project.
func (h *Handlers) ProjectArtifacts(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	items, err := h.projects.Artifacts(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": toArtifacts(items)})
}

// --- runs ----------------------------------------------------------------

// StartRun begins a build.
func (h *Handlers) StartRun(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	var body startRunRequest
	_ = c.BodyParser(&body)

	run, err := h.runs.Start(c.Context(), principal, id, usecase.StartInput{
		Kind: body.Kind, Prompt: body.Prompt,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(toRun(run))
}

// ListRuns returns a project's builds.
func (h *Handlers) ListRuns(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	items, total, err := h.runs.List(c.Context(), principal, id,
		c.QueryInt("limit", 50), c.QueryInt("offset", 0))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": toRuns(items), "total": total})
}

// GetRun returns a build with its phases.
func (h *Handlers) GetRun(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	run, err := h.runs.Get(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(toRun(run))
}

// CancelRun requests cancellation.
func (h *Handlers) CancelRun(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	run, err := h.runs.Cancel(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(toRun(run))
}

// RunEvents returns a page of the event log after a cursor.
func (h *Handlers) RunEvents(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	afterSeq, _ := strconv.ParseInt(c.Query("after_seq", "0"), 10, 64)

	events, err := h.runs.Events(c.Context(), principal, id, afterSeq, c.QueryInt("limit", 200))
	if err != nil {
		return err
	}
	next := afterSeq
	if len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	return c.JSON(fiber.Map{"data": toEvents(events), "next_seq": next})
}

// RunTasks returns the work DAG.
func (h *Handlers) RunTasks(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	tasks, err := h.runs.Tasks(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": toTasks(tasks)})
}

// RunArtifacts lists the documents a build produced.
func (h *Handlers) RunArtifacts(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	items, err := h.runs.Artifacts(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": toArtifacts(items)})
}

// RunAgents returns the live agent dashboard for a build.
func (h *Handlers) RunAgents(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	board, err := h.runs.AgentBoard(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": board})
}

// GetArtifact returns one artifact including its body.
func (h *Handlers) GetArtifact(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	a, err := h.runs.Artifact(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(toArtifact(a, true))
}

// --- workspace (IDE) ------------------------------------------------------

// WorkspaceTree returns the file tree of a project.
func (h *Handlers) WorkspaceTree(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	tree, err := h.workspaces.Tree(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": tree})
}

// ExportWorkspace streams the project as a zip download.
//
// Content-Disposition with a filename is what makes a browser save the file
// under a recognisable name instead of the route's last path segment, which
// would arrive as "export" with no extension.
func (h *Handlers) ExportWorkspace(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}

	// The archive is written straight to the response. Buffering it first
	// would hold an entire project in memory and delay the first byte, which
	// a user experiences as the button doing nothing.
	var buf bytes.Buffer
	filename, err := h.workspaces.ExportArchive(c.Context(), principal, id, &buf)
	if err != nil {
		return err
	}

	c.Set(fiber.HeaderContentType, "application/zip")
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+filename+`"`)
	return c.Send(buf.Bytes())
}

// ReadWorkspaceFile returns one file's content for the editor.
func (h *Handlers) ReadWorkspaceFile(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	// The path arrives as a query parameter rather than a path segment: a file
	// path contains slashes, and encoding them into a route makes both the
	// client and the router harder to get right.
	file, err := h.workspaces.ReadFile(c.Context(), principal, id, c.Query("path"))
	if err != nil {
		return err
	}
	return c.JSON(file)
}

// WriteWorkspaceFile saves a user's manual edit.
func (h *Handlers) WriteWorkspaceFile(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		BaseSHA string `json:"base_sha256"`
	}
	if err := c.BodyParser(&body); err != nil {
		return domain.Invalid("body_invalid", "request body is not valid JSON")
	}
	file, err := h.workspaces.WriteFile(c.Context(), principal, id, body.Path, body.Content, body.BaseSHA)
	if err != nil {
		return err
	}
	return c.JSON(file)
}

// SearchWorkspace finds text across a project.
func (h *Handlers) SearchWorkspace(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	hits, err := h.workspaces.Search(c.Context(), principal, id, c.Query("q"), c.QueryInt("limit", 100))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": hits})
}

// WorkspaceHistory returns the git log.
func (h *Handlers) WorkspaceHistory(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	commits, err := h.workspaces.History(c.Context(), principal, id, c.QueryInt("limit", 50))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"data": commits})
}

// WorkspaceDiff returns a commit's diff, or the working tree's.
func (h *Handlers) WorkspaceDiff(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	diff, err := h.workspaces.Diff(c.Context(), principal, id, c.Query("ref"))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"diff": diff})
}

// WorkspaceStatus reports the git working tree state.
func (h *Handlers) WorkspaceStatus(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	status, err := h.workspaces.VCSStatus(c.Context(), principal, id)
	if err != nil {
		return err
	}
	return c.JSON(status)
}

// RollbackWorkspace restores a project to an earlier commit.
func (h *Handlers) RollbackWorkspace(c *fiber.Ctx) error {
	principal, err := principalOf(c)
	if err != nil {
		return err
	}
	id, err := parseID(c, "id")
	if err != nil {
		return err
	}
	var body struct {
		Ref string `json:"ref"`
	}
	_ = c.BodyParser(&body)

	if err := h.workspaces.Rollback(c.Context(), principal, id, body.Ref); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}
