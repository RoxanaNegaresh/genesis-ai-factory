// Package commands implements the genesis CLI verbs.
//
// The CLI is a first-class client, not a debugging afterthought: everything the
// desktop app can do is reachable from a terminal, because that is what makes
// the product scriptable and usable over SSH.
package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/genesis-ai-factory/cli/internal/client"
	"github.com/genesis-ai-factory/cli/internal/ui"
)

// Context carries shared CLI state.
type Context struct {
	Client *client.Client
	Out    io.Writer
	Err    io.Writer
	JSON   bool
}

// Create registers a product and, unless --no-watch is passed, streams the build.
func Create(ctx context.Context, c *Context, prompt, name string, watch bool) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("a product brief is required, for example: genesis create \"Build a CRM system\"")
	}

	// Show the interpretation before committing. A user who sees "classified as
	// marketplace" when they meant CRM can correct course in one second rather
	// than after a full build.
	classification, blueprint, suggested, err := c.Client.Classify(ctx, prompt)
	if err == nil {
		ui.Info(c.Out, "Interpreted as %s %s",
			ui.Bold(blueprint.Name),
			ui.Gray(fmt.Sprintf("(%s, %.0f%% confidence)", classification.Category, classification.Confidence*100)))
		if name == "" {
			name = suggested
		}
	}

	project, run, err := c.Client.CreateProject(ctx, prompt, name, true)
	if err != nil {
		return err
	}

	ui.Success(c.Out, "Created %s", ui.Bold(project.Name))
	ui.Field(c.Out, "project", project.ID)
	ui.Field(c.Out, "workspace", project.WorkspacePath)
	if run != nil {
		ui.Field(c.Out, "run", run.ID)
	}
	fmt.Fprintln(c.Out)

	if run == nil {
		return nil
	}
	if !watch {
		ui.Info(c.Out, "Follow the build with: %s", ui.Bold("genesis watch "+run.ID))
		return nil
	}
	return Watch(ctx, c, run.ID)
}

// Watch streams a run's event log until it reaches a terminal state.
func Watch(ctx context.Context, c *Context, runID string) error {
	// Ctrl-C must detach the viewer, not cancel the build. Killing a build
	// because someone closed a log window would be a serious usability defect.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		cursor    int64
		lastPhase string
		started   = time.Now()
	)

	for {
		events, next, err := c.Client.Events(ctx, runID, cursor)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(c.Out)
				ui.Info(c.Out, "Detached. The build continues in the background.")
				return nil
			}
			return err
		}
		cursor = next

		for _, e := range events {
			if phase, ok := e.Payload["phase"].(string); ok && e.Type == "phase.started" && phase != lastPhase {
				lastPhase = phase
				fmt.Fprintf(c.Out, "\n%s %s\n", ui.Magenta("▸"), ui.Bold(e.Message))
				continue
			}
			printEvent(c.Out, e)
		}

		run, err := c.Client.GetRun(ctx, runID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if run.Terminal() {
			fmt.Fprintln(c.Out)
			return summarise(ctx, c, run, time.Since(started))
		}

		select {
		case <-ctx.Done():
			fmt.Fprintln(c.Out)
			ui.Info(c.Out, "Detached. The build continues in the background.")
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func printEvent(w io.Writer, e client.Event) {
	// File writes are high-volume and low-signal; they are shown dimmed so the
	// narrative of the build stays readable.
	if e.Type == "file.written" {
		fmt.Fprintf(w, "  %s %s\n", ui.Gray("+"), ui.Gray(e.Message))
		return
	}

	agent := ""
	if e.AgentName != "" && e.AgentRole != "system" {
		agent = ui.Cyan(e.AgentName) + " "
	}

	prefix := "  " + ui.LevelBadge(e.Level) + " "
	switch e.Type {
	case "agent.assigned":
		fmt.Fprintf(w, "  %s %s%s\n", ui.Blue("▶"), agent, e.Message)
	case "agent.completed":
		fmt.Fprintf(w, "  %s %s%s\n", ui.Green("✔"), agent, ui.Gray(e.Message))
	case "agent.failed", "error.detected":
		fmt.Fprintf(w, "  %s %s%s\n", ui.Red("✘"), agent, ui.Red(e.Message))
	case "artifact.created":
		fmt.Fprintf(w, "  %s %s%s\n", ui.Yellow("◆"), agent, e.Message)
	case "run.completed":
		return // covered by the summary
	default:
		fmt.Fprintf(w, "%s%s%s\n", prefix, agent, e.Message)
	}
}

func summarise(ctx context.Context, c *Context, run *client.Run, elapsed time.Duration) error {
	switch run.Status {
	case "succeeded":
		ui.Success(c.Out, "%s in %s", ui.Bold("Build complete"), ui.Duration(elapsed))
	case "failed":
		ui.Error(c.Out, "Build failed after %s", ui.Duration(elapsed))
		if run.Error != nil {
			ui.Field(c.Out, "reason", run.Error.Message)
			ui.Field(c.Out, "phase", run.Error.Phase)
		}
	case "canceled":
		ui.Warn(c.Out, "Build canceled after %s", ui.Duration(elapsed))
	default:
		ui.Warn(c.Out, "Build ended as %s", run.Status)
	}

	if files, ok := run.Result["files_written"].(float64); ok {
		ui.Field(c.Out, "files", fmt.Sprintf("%.0f", files))
	}
	if artifacts, ok := run.Result["artifacts"].(float64); ok {
		ui.Field(c.Out, "artifacts", fmt.Sprintf("%.0f", artifacts))
	}
	if workspace, ok := run.Result["workspace"].(string); ok {
		ui.Field(c.Out, "workspace", workspace)
	}

	if run.Status == "succeeded" {
		fmt.Fprintln(c.Out)
		ui.Info(c.Out, "Inspect the plan:  %s", ui.Bold("genesis artifacts "+run.ID))
		if workspace, ok := run.Result["workspace"].(string); ok {
			ui.Info(c.Out, "Open the project:  %s", ui.Bold("cd "+workspace))
		}
	}
	_ = ctx
	return nil
}

// Projects lists the caller's projects.
func Projects(ctx context.Context, c *Context) error {
	projects, err := c.Client.ListProjects(ctx)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		ui.Info(c.Out, "No projects yet. Create one with: %s",
			ui.Bold(`genesis create "Build a CRM system"`))
		return nil
	}
	rows := make([][]string, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, []string{
			shortID(p.ID), p.Name, p.Category, ui.StatusBadge(p.Status),
			p.CreatedAt.Local().Format("2006-01-02 15:04"),
		})
	}
	ui.Table(c.Out, []string{"id", "name", "category", "status", "created"}, rows)
	return nil
}

// Status shows a run's phase-by-phase progress.
func Status(ctx context.Context, c *Context, runID string) error {
	run, err := c.Client.GetRun(ctx, runID)
	if err != nil {
		return err
	}

	fmt.Fprintf(c.Out, "\n  %s %s   %s\n\n",
		ui.Bold("Run"), shortID(run.ID), ui.StatusBadge(run.Status))
	fmt.Fprintf(c.Out, "  %s\n\n", ui.ProgressBar(run.Progress, 28))

	rows := make([][]string, 0, len(run.Phases))
	for _, p := range run.Phases {
		duration := ""
		if p.StartedAt != nil && p.FinishedAt != nil {
			duration = ui.Duration(p.FinishedAt.Sub(*p.StartedAt))
		}
		detail := ""
		if n, ok := p.Summary["artifacts"].(float64); ok && n > 0 {
			detail = fmt.Sprintf("%.0f artifacts", n)
		}
		if reason, ok := p.Summary["reason"].(string); ok {
			detail = reason
		}
		rows = append(rows, []string{p.Title, ui.StatusBadge(p.Status), duration, detail})
	}
	ui.Table(c.Out, []string{"phase", "status", "time", "detail"}, rows)

	if run.Error != nil {
		fmt.Fprintln(c.Out)
		ui.Error(c.Out, "%s (%s)", run.Error.Message, run.Error.Code)
	}
	return nil
}

// Agents shows either the static roster or the live board for a run.
func Agents(ctx context.Context, c *Context, runID string) error {
	if runID == "" {
		profiles, err := c.Client.Agents(ctx)
		if err != nil {
			return err
		}
		rows := make([][]string, 0, len(profiles))
		for _, p := range profiles {
			rows = append(rows, []string{p.Name, p.Title, p.Phase, p.ModelClass})
		}
		ui.Table(c.Out, []string{"agent", "role", "phase", "model"}, rows)
		return nil
	}

	board, err := c.Client.RunAgents(ctx, runID)
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(board))
	for _, a := range board {
		task := a.Task
		if len(task) > 52 {
			task = task[:49] + "…"
		}
		rows = append(rows, []string{
			a.Profile.Name, a.Profile.Title, ui.StatusBadge(a.Status),
			fmt.Sprint(a.Artifacts), task,
		})
	}
	ui.Table(c.Out, []string{"agent", "role", "status", "output", "last task"}, rows)
	return nil
}

// Artifacts lists a run's documents, or prints one when name is given.
func Artifacts(ctx context.Context, c *Context, runID, name string) error {
	items, err := c.Client.RunArtifacts(ctx, runID)
	if err != nil {
		return err
	}

	if name != "" {
		for _, a := range items {
			if strings.EqualFold(a.Name, name) || strings.EqualFold(a.Kind, name) {
				full, err := c.Client.Artifact(ctx, a.ID)
				if err != nil {
					return err
				}
				fmt.Fprintln(c.Out, full.Body)
				return nil
			}
		}
		return fmt.Errorf("no artifact named %q in this run", name)
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Kind < items[j].Kind })
	rows := make([][]string, 0, len(items))
	for _, a := range items {
		rows = append(rows, []string{a.Kind, a.Name, ui.Bytes(a.SizeBytes)})
	}
	ui.Table(c.Out, []string{"kind", "name", "size"}, rows)
	fmt.Fprintln(c.Out)
	ui.Info(c.Out, "Print one with: %s", ui.Bold("genesis artifacts "+shortID(runID)+" --name PRD.md"))
	return nil
}

// Analyze inspects a directory and reports what the factory sees.
func Analyze(ctx context.Context, c *Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("cannot analyse %s: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}

	counts := map[string]int{}
	var totalFiles int
	var totalBytes int64
	err = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "build", "vendor", "target":
				return filepath.SkipDir
			}
			return nil
		}
		totalFiles++
		if fi, err := d.Info(); err == nil {
			totalBytes += fi.Size()
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != "" {
			counts[ext]++
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(c.Out, "\n  %s %s\n\n", ui.Bold("Analysis of"), abs)
	ui.Field(c.Out, "files", fmt.Sprint(totalFiles))
	ui.Field(c.Out, "size", ui.Bytes(totalBytes))

	type kv struct {
		ext string
		n   int
	}
	sorted := make([]kv, 0, len(counts))
	for ext, n := range counts {
		sorted = append(sorted, kv{ext, n})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].n > sorted[j].n })

	fmt.Fprintln(c.Out)
	rows := make([][]string, 0, 10)
	for i, item := range sorted {
		if i >= 10 {
			break
		}
		rows = append(rows, []string{item.ext, fmt.Sprint(item.n), detectLanguage(item.ext)})
	}
	ui.Table(c.Out, []string{"ext", "files", "language"}, rows)

	markers := map[string]string{
		"go.mod":             "Go module",
		"package.json":       "Node/JavaScript project",
		"Cargo.toml":         "Rust crate",
		"pyproject.toml":     "Python project",
		"docker-compose.yml": "Docker Compose stack",
		"Dockerfile":         "Container image",
		"Makefile":           "Make build",
	}
	var found []string
	for file, description := range markers {
		if _, err := os.Stat(filepath.Join(abs, file)); err == nil {
			found = append(found, description)
		}
	}
	if len(found) > 0 {
		sort.Strings(found)
		fmt.Fprintln(c.Out)
		ui.Info(c.Out, "Detected: %s", strings.Join(found, ", "))
	}
	_ = ctx
	return nil
}

func detectLanguage(ext string) string {
	switch ext {
	case ".go":
		return "Go"
	case ".ts", ".tsx":
		return "TypeScript"
	case ".js", ".jsx":
		return "JavaScript"
	case ".py":
		return "Python"
	case ".rs":
		return "Rust"
	case ".cs":
		return "C#"
	case ".sql":
		return "SQL"
	case ".md":
		return "Markdown"
	case ".yaml", ".yml":
		return "YAML"
	case ".json":
		return "JSON"
	case ".css":
		return "CSS"
	case ".html":
		return "HTML"
	}
	return ""
}

// Blueprints lists the built-in product templates.
func Blueprints(ctx context.Context, c *Context) error {
	items, err := c.Client.Blueprints(ctx)
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, b := range items {
		rows = append(rows, []string{
			b.Key, b.Name, fmt.Sprint(b.Entities), fmt.Sprint(b.Screens), b.Description,
		})
	}
	ui.Table(c.Out, []string{"key", "name", "entities", "screens", "description"}, rows)
	return nil
}

// Models reports whether model-backed reasoning is active.
//
// This is the first thing to check when output looks generic: the difference
// between reasoned and blueprint-derived artifacts is invisible in the result
// but obvious here.
func Models(ctx context.Context, c *Context) error {
	status, err := c.Client.Models(ctx)
	if err != nil {
		return err
	}

	if !status.Enabled {
		ui.Warn(c.Out, "Reasoning is off — agents are using deterministic blueprints")
		if status.Reason != "" {
			ui.Field(c.Out, "reason", status.Reason)
		}
		fmt.Fprintln(c.Out)
		ui.Info(c.Out, "Enable it:")
		fmt.Fprintf(c.Out, "    %s\n", ui.Gray("genesis-ai pull          # download a model that fits"))
		fmt.Fprintf(c.Out, "    %s\n", ui.Gray("genesis-ai serve         # start the inference server"))
		fmt.Fprintf(c.Out, "    %s\n", ui.Gray("export GENESIS_LLM_URL=http://127.0.0.1:8791"))
		return nil
	}

	ui.Success(c.Out, "Reasoning is active via %s", ui.Bold(status.Provider))
	rows := make([][]string, 0, len(status.Data))
	for _, m := range status.Data {
		rows = append(rows, []string{m.ID, strings.Join(m.Classes, ","), fmt.Sprint(m.Context)})
	}
	ui.Table(c.Out, []string{"model", "classes", "context"}, rows)
	return nil
}

// Cancel stops a running build.
func Cancel(ctx context.Context, c *Context, runID string) error {
	run, err := c.Client.CancelRun(ctx, runID)
	if err != nil {
		return err
	}
	ui.Warn(c.Out, "Cancellation requested for run %s (status: %s)", shortID(run.ID), run.Status)
	return nil
}

// Login authenticates and stores a session.
func Login(ctx context.Context, c *Context, email, password string) error {
	session, err := c.Client.Login(ctx, email, password)
	if err != nil {
		return err
	}
	if err := client.SaveSession(session); err != nil {
		return fmt.Errorf("could not save the session: %w", err)
	}
	ui.Success(c.Out, "Signed in as %s", ui.Bold(session.Email))
	return nil
}

// Register creates an account and stores a session.
func Register(ctx context.Context, c *Context, email, password, name string) error {
	session, err := c.Client.Register(ctx, email, password, name)
	if err != nil {
		return err
	}
	if err := client.SaveSession(session); err != nil {
		return fmt.Errorf("could not save the session: %w", err)
	}
	ui.Success(c.Out, "Account created for %s", ui.Bold(session.Email))
	return nil
}

// Doctor checks that the environment is usable and reports what is wrong when
// it is not, which is far more useful than a connection error at first use.
func Doctor(ctx context.Context, c *Context) error {
	fmt.Fprintln(c.Out)
	ok := true

	meta, err := c.Client.Meta(ctx)
	if err != nil {
		ui.Error(c.Out, "Control plane unreachable at %s", c.Client.BaseURL)
		ui.Field(c.Out, "detail", err.Error())
		ok = false
	} else {
		ui.Success(c.Out, "Control plane %s (%s)", meta.Version, c.Client.BaseURL)
		if agents, found := meta.Capabilities["agents"].(float64); found {
			ui.Field(c.Out, "agents", fmt.Sprintf("%.0f", agents))
		}
		if blueprints, found := meta.Capabilities["blueprints"].(float64); found {
			ui.Field(c.Out, "blueprints", fmt.Sprintf("%.0f", blueprints))
		}
	}

	if c.Client.Token == "" {
		ui.Warn(c.Out, "No credentials found at %s", client.SessionPath())
		ui.Field(c.Out, "fix", "run: genesis login")
		ok = false
	} else {
		ui.Success(c.Out, "Credentials loaded")
	}

	if err == nil {
		if _, listErr := c.Client.ListProjects(ctx); listErr != nil {
			ui.Error(c.Out, "Authenticated request failed: %v", listErr)
			ok = false
		} else {
			ui.Success(c.Out, "Authenticated API access works")
		}
	}

	fmt.Fprintln(c.Out)
	if !ok {
		return errors.New("environment check failed")
	}
	ui.Success(c.Out, "%s", ui.Bold("Everything looks good"))
	return nil
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
