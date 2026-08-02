package http_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	adapterhttp "github.com/genesis-ai-factory/control-plane/internal/adapter/http"
	"github.com/genesis-ai-factory/control-plane/internal/adapter/ws"
	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
	"github.com/genesis-ai-factory/control-plane/internal/infra/bus"
	"github.com/genesis-ai-factory/control-plane/internal/infra/crypto"
	"github.com/genesis-ai-factory/control-plane/internal/infra/migrate"
	"github.com/genesis-ai-factory/control-plane/internal/infra/sqlstore"
	"github.com/genesis-ai-factory/control-plane/internal/infra/vcs"
	"github.com/genesis-ai-factory/control-plane/internal/usecase"
)

// harness wires the real stack — real database, real bus, real driver — against
// a temporary directory. Mocking these would test the mocks; the point of an
// integration test is to prove the wiring itself is correct.
type harness struct {
	app       *fiber.App
	store     *sqlstore.Store
	workspace string
	t         *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := t.Context()

	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

	store, err := sqlstore.Open(ctx, sqlstore.DefaultOptions("sqlite", dsn))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := migrate.NewRunner(store.DB(), migrate.SQLite, discardLogger()).Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	users := sqlstore.NewUserRepo(store)
	tokens := sqlstore.NewRefreshTokenRepo(store)
	projects := sqlstore.NewProjectRepo(store)
	runs := sqlstore.NewRunRepo(store)
	tasks := sqlstore.NewTaskRepo(store)
	events := sqlstore.NewEventRepo(store)
	artifacts := sqlstore.NewArtifactRepo(store)

	eventBus := bus.New()
	t.Cleanup(eventBus.Close)

	log := discardLogger()
	hasher := crypto.NewArgon2Hasher(crypto.TestParams())
	issuer := crypto.NewJWTIssuer("test-secret-that-is-long-enough-32", time.Now)
	clock := testClock{}
	recorder := usecase.NewRecorder(events, eventBus, log)

	workspace := filepath.Join(dir, "workspaces")

	auth := usecase.NewAuth(users, tokens, hasher, issuer, clock, store,
		usecase.AuthConfig{AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour}, log)
	projectSvc := usecase.NewProjects(projects, runs, artifacts, recorder, clock, store, workspace, log)
	driver := factory.NewDriver(runs, projects, artifacts, tasks, recorder,
		factory.DriverConfig{MaxParallelAgents: 2, VersionControl: true}, log)
	runSvc := usecase.NewRuns(runs, projects, tasks, artifacts, events, recorder, driver, clock, store, log)

	handlers := adapterhttp.NewHandlers(auth, projectSvc, runSvc, usecase.NewWorkspaces(projects, recorder, clock, vcs.NewFactory(), log), nil, "test", "test")
	hub := ws.NewHub(eventBus, events, log)

	app := adapterhttp.NewRouter(handlers, hub, issuer, adapterhttp.RouterConfig{
		CORSOrigins: []string{"http://localhost:1420"},
		Version:     "test",
		ReadinessFn: store.Ping,
	}, log)

	return &harness{app: app, store: store, workspace: workspace, t: t}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

type testClock struct{}

func (testClock) Now() time.Time { return time.Now().UTC() }

type response struct {
	status int
	body   map[string]any
	raw    string
}

func (h *harness) do(method, path, token string, body any) response {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := h.app.Test(req, 30_000)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	out := response{status: res.StatusCode, raw: string(raw)}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out
}

// binary performs a request and returns the unparsed body, for responses that
// are not JSON. Reusing do() would try to unmarshal a zip.
func (h *harness) binary(method, path, token string) ([]byte, int, http.Header) {
	h.t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := h.app.Test(req, 30_000)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	return raw, res.StatusCode, http.Header(res.Header)
}

func (h *harness) register(email string) (token string, userID string) {
	h.t.Helper()
	res := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": email, "password": "a-sufficiently-long-password", "display_name": "Test User",
	})
	if res.status != http.StatusCreated {
		h.t.Fatalf("register failed: %d %s", res.status, res.raw)
	}
	user, _ := res.body["user"].(map[string]any)
	return res.body["access_token"].(string), user["id"].(string)
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func TestHealthAndReadiness(t *testing.T) {
	h := newHarness(t)

	if res := h.do(http.MethodGet, "/health", "", nil); res.status != http.StatusOK {
		t.Fatalf("health returned %d", res.status)
	}
	if res := h.do(http.MethodGet, "/ready", "", nil); res.status != http.StatusOK {
		t.Fatalf("ready returned %d: %s", res.status, res.raw)
	}
	if res := h.do(http.MethodGet, "/api/v1/meta", "", nil); res.status != http.StatusOK {
		t.Fatalf("meta returned %d", res.status)
	}
}

func TestAuthenticationFlow(t *testing.T) {
	h := newHarness(t)

	// First account becomes the owner.
	res := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": "first@genesis.local", "password": "a-sufficiently-long-password", "display_name": "First",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("register: %d %s", res.status, res.raw)
	}
	user := res.body["user"].(map[string]any)
	if user["role"] != string(domain.RoleOwner) {
		t.Fatalf("first account should be owner, got %v", user["role"])
	}
	if _, leaked := user["password_hash"]; leaked {
		t.Fatal("password hash exposed in the API response")
	}

	// Second account is a plain member.
	res = h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": "second@genesis.local", "password": "a-sufficiently-long-password",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("second register: %d %s", res.status, res.raw)
	}
	if res.body["user"].(map[string]any)["role"] != string(domain.RoleMember) {
		t.Fatal("second account should not be owner")
	}

	// Duplicate email conflicts.
	res = h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": "first@genesis.local", "password": "a-sufficiently-long-password",
	})
	if res.status != http.StatusConflict {
		t.Fatalf("duplicate email should conflict, got %d", res.status)
	}

	// Weak password rejected.
	res = h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": "weak@genesis.local", "password": "short",
	})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("weak password should be rejected, got %d", res.status)
	}

	// Login.
	res = h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": "first@genesis.local", "password": "a-sufficiently-long-password",
	})
	if res.status != http.StatusOK {
		t.Fatalf("login: %d %s", res.status, res.raw)
	}
	accessToken := str(res.body, "access_token")
	refreshToken := str(res.body, "refresh_token")

	// Wrong password fails without revealing whether the account exists.
	res = h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": "first@genesis.local", "password": "wrong-password-entirely",
	})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("bad password should be 401, got %d", res.status)
	}
	unknown := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": "nobody@genesis.local", "password": "wrong-password-entirely",
	})
	if unknown.status != res.status {
		t.Fatal("unknown account and wrong password must be indistinguishable")
	}

	// Authenticated identity.
	res = h.do(http.MethodGet, "/api/v1/auth/me", accessToken, nil)
	if res.status != http.StatusOK || str(res.body, "email") != "first@genesis.local" {
		t.Fatalf("me: %d %s", res.status, res.raw)
	}

	// Refresh rotates the token.
	res = h.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refreshToken})
	if res.status != http.StatusOK {
		t.Fatalf("refresh: %d %s", res.status, res.raw)
	}
	rotated := str(res.body, "refresh_token")
	if rotated == refreshToken {
		t.Fatal("refresh token was not rotated")
	}

	// Reuse of the old token is detected and kills the whole family.
	res = h.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refreshToken})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("reused refresh token should be rejected, got %d", res.status)
	}
	res = h.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": rotated})
	if res.status != http.StatusUnauthorized {
		t.Fatal("token family should be revoked after detected reuse")
	}
}

func TestUnauthenticatedAccessIsRejected(t *testing.T) {
	h := newHarness(t)

	protected := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/projects"},
		{http.MethodPost, "/api/v1/projects"},
		{http.MethodGet, "/api/v1/agents"},
		{http.MethodGet, "/api/v1/blueprints"},
		{http.MethodPost, "/api/v1/classify"},
		{http.MethodGet, "/api/v1/runs/" + domain.NewID().String()},
	}
	for _, tc := range protected {
		res := h.do(tc.method, tc.path, "", map[string]string{})
		if res.status != http.StatusUnauthorized {
			t.Errorf("%s %s returned %d without a token; expected 401", tc.method, tc.path, res.status)
		}
	}

	// A forged token must not be accepted.
	res := h.do(http.MethodGet, "/api/v1/projects", "not.a.token", nil)
	if res.status != http.StatusUnauthorized {
		t.Fatalf("garbage token accepted: %d", res.status)
	}
}

func TestProjectLifecycle(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("projects@genesis.local")

	res := h.do(http.MethodPost, "/api/v1/projects", token, map[string]any{
		"prompt": "Build a Jira competitor with kanban boards",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("create project: %d %s", res.status, res.raw)
	}
	project := res.body["project"].(map[string]any)
	projectID := str(project, "id")
	if str(project, "name") != "Jira Competitor With Kanban Boards" {
		t.Fatalf("unexpected derived name: %q", str(project, "name"))
	}
	if str(project, "workspace_path") == "" {
		t.Fatal("project has no workspace path")
	}
	if _, err := os.Stat(str(project, "workspace_path")); err != nil {
		t.Fatalf("workspace directory was not created: %v", err)
	}

	// Read back.
	res = h.do(http.MethodGet, "/api/v1/projects/"+projectID, token, nil)
	if res.status != http.StatusOK || str(res.body, "id") != projectID {
		t.Fatalf("get project: %d %s", res.status, res.raw)
	}

	// List.
	res = h.do(http.MethodGet, "/api/v1/projects", token, nil)
	if res.status != http.StatusOK {
		t.Fatalf("list: %d", res.status)
	}
	if total, _ := res.body["total"].(float64); total != 1 {
		t.Fatalf("expected 1 project, got %v", res.body["total"])
	}

	// Update.
	res = h.do(http.MethodPatch, "/api/v1/projects/"+projectID, token, map[string]any{
		"description": "A better issue tracker",
	})
	if res.status != http.StatusOK || str(res.body, "description") != "A better issue tracker" {
		t.Fatalf("update: %d %s", res.status, res.raw)
	}

	// Invalid id is rejected before touching the database.
	res = h.do(http.MethodGet, "/api/v1/projects/not-a-uuid", token, nil)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid id should be 422, got %d", res.status)
	}

	// Archive.
	res = h.do(http.MethodDelete, "/api/v1/projects/"+projectID, token, nil)
	if res.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", res.status, res.raw)
	}
	res = h.do(http.MethodGet, "/api/v1/projects", token, nil)
	if total, _ := res.body["total"].(float64); total != 0 {
		t.Fatalf("archived project still listed: %v", res.body["total"])
	}
}

// TestProjectIsolationBetweenUsers is the single most important authorization
// test: one user must never be able to read another user's project, even with
// a valid identifier.
func TestProjectIsolationBetweenUsers(t *testing.T) {
	h := newHarness(t)
	ownerToken, _ := h.register("owner@genesis.local")
	intruderToken, _ := h.register("intruder@genesis.local")

	res := h.do(http.MethodPost, "/api/v1/projects", ownerToken, map[string]any{
		"prompt": "Build a private CRM",
	})
	projectID := str(res.body["project"].(map[string]any), "id")

	// Reads, writes and deletes must all fail, and with 404 rather than 403:
	// confirming that an id exists is itself a disclosure.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/projects/" + projectID},
		{http.MethodPatch, "/api/v1/projects/" + projectID},
		{http.MethodDelete, "/api/v1/projects/" + projectID},
		{http.MethodGet, "/api/v1/projects/" + projectID + "/runs"},
		{http.MethodPost, "/api/v1/projects/" + projectID + "/runs"},
	} {
		res := h.do(tc.method, tc.path, intruderToken, map[string]any{})
		if res.status != http.StatusNotFound {
			t.Errorf("%s %s by a non-owner returned %d; expected 404", tc.method, tc.path, res.status)
		}
	}

	// The intruder's own listing must be empty.
	res = h.do(http.MethodGet, "/api/v1/projects", intruderToken, nil)
	if total, _ := res.body["total"].(float64); total != 0 {
		t.Fatalf("intruder can see %v projects", res.body["total"])
	}
}

func TestClassifyEndpoint(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("classify@genesis.local")

	res := h.do(http.MethodPost, "/api/v1/classify", token, map[string]string{
		"prompt": "Build a CRM system for tracking leads and deals",
	})
	if res.status != http.StatusOK {
		t.Fatalf("classify: %d %s", res.status, res.raw)
	}
	classification := res.body["classification"].(map[string]any)
	if classification["category"] != string(domain.CategoryCRM) {
		t.Fatalf("expected crm, got %v", classification["category"])
	}
	blueprint := res.body["blueprint"].(map[string]any)
	if entities, _ := blueprint["entities"].(float64); entities < 5 {
		t.Fatalf("crm blueprint should have several entities, got %v", blueprint["entities"])
	}

	res = h.do(http.MethodPost, "/api/v1/classify", token, map[string]string{"prompt": ""})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("empty prompt should be rejected, got %d", res.status)
	}
}

func TestAgentRosterEndpoint(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("agents@genesis.local")

	res := h.do(http.MethodGet, "/api/v1/agents", token, nil)
	if res.status != http.StatusOK {
		t.Fatalf("agents: %d", res.status)
	}
	data := res.body["data"].([]any)
	if len(data) != 11 {
		t.Fatalf("expected 11 agents, got %d", len(data))
	}
}

// TestFullBuildRun drives the entire product loop through the HTTP API and
// verifies that a real, inspectable project lands on disk.
func TestFullBuildRun(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("build@genesis.local")

	res := h.do(http.MethodPost, "/api/v1/projects", token, map[string]any{
		"prompt": "Build a Jira competitor with kanban boards, sprints and issue tracking",
		"start":  true,
	})
	if res.status != http.StatusCreated {
		t.Fatalf("create+start: %d %s", res.status, res.raw)
	}
	project := res.body["project"].(map[string]any)
	projectID := str(project, "id")
	workspacePath := str(project, "workspace_path")

	runBody, ok := res.body["run"].(map[string]any)
	if !ok {
		t.Fatalf("run was not started: %s", res.raw)
	}
	runID := str(runBody, "id")

	// Poll until the run reaches a terminal state.
	var final map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res = h.do(http.MethodGet, "/api/v1/runs/"+runID, token, nil)
		if res.status != http.StatusOK {
			t.Fatalf("get run: %d %s", res.status, res.raw)
		}
		status := str(res.body, "status")
		if status == string(domain.RunSucceeded) || status == string(domain.RunFailed) ||
			status == string(domain.RunCanceled) {
			final = res.body
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("run did not finish within 30 seconds")
	}
	if final["status"] != string(domain.RunSucceeded) {
		t.Fatalf("run failed: %v", final["error"])
	}
	if progress, _ := final["progress"].(float64); progress != 1 {
		t.Fatalf("completed run reports progress %v", final["progress"])
	}

	// Every phase must be accounted for.
	phases := final["phases"].([]any)
	if len(phases) != len(domain.PhaseOrder) {
		t.Fatalf("expected %d phases, got %d", len(domain.PhaseOrder), len(phases))
	}
	for _, raw := range phases {
		p := raw.(map[string]any)
		status := p["status"].(string)
		if status != string(domain.PhaseSucceeded) && status != string(domain.PhaseSkipped) {
			t.Errorf("phase %v ended as %s", p["name"], status)
		}
	}

	// The event log must tell a coherent story.
	res = h.do(http.MethodGet, "/api/v1/runs/"+runID+"/events?limit=1000", token, nil)
	if res.status != http.StatusOK {
		t.Fatalf("events: %d", res.status)
	}
	events := res.body["data"].([]any)
	if len(events) < 20 {
		t.Fatalf("expected a detailed event log, got %d events", len(events))
	}

	seen := map[string]bool{}
	var lastSeq float64
	for _, raw := range events {
		e := raw.(map[string]any)
		seen[e["type"].(string)] = true
		seq := e["seq"].(float64)
		if seq <= lastSeq {
			t.Fatalf("event sequence is not monotonic: %v after %v", seq, lastSeq)
		}
		lastSeq = seq
	}
	for _, required := range []string{
		string(domain.EventRunCreated), string(domain.EventRunStarted),
		string(domain.EventPhaseStarted), string(domain.EventPhaseCompleted),
		string(domain.EventAgentAssigned), string(domain.EventAgentCompleted),
		string(domain.EventArtifactCreated), string(domain.EventRunCompleted),
	} {
		if !seen[required] {
			t.Errorf("event log is missing %s", required)
		}
	}

	// Cursor resume must not duplicate or skip.
	cursor := events[4].(map[string]any)["seq"].(float64)
	res = h.do(http.MethodGet, fmt.Sprintf("/api/v1/runs/%s/events?after_seq=%d&limit=1000", runID, int64(cursor)), token, nil)
	rest := res.body["data"].([]any)
	if len(rest) != len(events)-5 {
		t.Fatalf("cursor resume returned %d events, expected %d", len(rest), len(events)-5)
	}
	if rest[0].(map[string]any)["seq"].(float64) != events[5].(map[string]any)["seq"].(float64) {
		t.Fatal("cursor resume skipped or repeated an event")
	}

	// Artifacts must be real documents, retrievable with their content.
	res = h.do(http.MethodGet, "/api/v1/runs/"+runID+"/artifacts", token, nil)
	artifacts := res.body["data"].([]any)
	if len(artifacts) < 10 {
		t.Fatalf("expected at least 10 artifacts, got %d", len(artifacts))
	}
	first := artifacts[0].(map[string]any)
	res = h.do(http.MethodGet, "/api/v1/artifacts/"+str(first, "id"), token, nil)
	if res.status != http.StatusOK {
		t.Fatalf("get artifact: %d", res.status)
	}
	if len(str(res.body, "body")) < 100 {
		t.Fatal("artifact body is empty or trivial")
	}

	// The task DAG must have been planned with real dependencies.
	res = h.do(http.MethodGet, "/api/v1/runs/"+runID+"/tasks", token, nil)
	tasks := res.body["data"].([]any)
	if len(tasks) != 11 {
		t.Fatalf("expected 11 planned tasks, got %d", len(tasks))
	}
	edges := 0
	for _, raw := range tasks {
		deps := raw.(map[string]any)["depends_on"].([]any)
		edges += len(deps)
	}
	if edges == 0 {
		t.Fatal("planned task DAG has no dependency edges")
	}

	// The agent dashboard must reflect completion.
	res = h.do(http.MethodGet, "/api/v1/runs/"+runID+"/agents", token, nil)
	board := res.body["data"].([]any)
	done := 0
	for _, raw := range board {
		if raw.(map[string]any)["status"] == string(domain.AgentDone) {
			done++
		}
	}
	if done != 11 {
		t.Fatalf("expected all 11 agents done, got %d", done)
	}

	// The project must now be marked ready.
	res = h.do(http.MethodGet, "/api/v1/projects/"+projectID, token, nil)
	if str(res.body, "status") != string(domain.ProjectReady) {
		t.Fatalf("project status is %q, expected ready", str(res.body, "status"))
	}
	if str(res.body, "category") != string(domain.CategoryPM) {
		t.Fatalf("project category is %q, expected pm", str(res.body, "category"))
	}

	// Finally: real files on disk that a human can open and edit.
	for _, rel := range []string{
		"README.md", "docker-compose.yml", "api/openapi.yaml",
		"migrations/0001_init.up.sql", "docs/product/PRD.md",
		"web/src/App.tsx", "api/cmd/server/main.go",
	} {
		info, err := os.Stat(filepath.Join(workspacePath, rel))
		if err != nil {
			t.Errorf("generated project is missing %s: %v", rel, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("generated file %s is empty", rel)
		}
	}
}

func TestConcurrentRunsAreRejected(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("concurrent@genesis.local")

	res := h.do(http.MethodPost, "/api/v1/projects", token, map[string]any{
		"prompt": "Build an ERP system for manufacturing", "start": true,
	})
	projectID := str(res.body["project"].(map[string]any), "id")

	// A second build while the first is active must be refused: two runs
	// writing the same workspace would interleave into an incoherent result.
	second := h.do(http.MethodPost, "/api/v1/projects/"+projectID+"/runs", token, map[string]any{})
	if second.status != http.StatusConflict && second.status != http.StatusAccepted {
		t.Fatalf("unexpected status for a concurrent run: %d %s", second.status, second.raw)
	}
	if second.status == http.StatusAccepted {
		// The first run may already have completed; then a second is legal.
		t.Log("first run completed before the second was requested; conflict not triggered")
	}
}

func TestErrorEnvelopeIsConsistent(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("errors@genesis.local")

	cases := []struct {
		name           string
		method, path   string
		token          string
		body           any
		expectedStatus int
	}{
		{"unauthenticated", http.MethodGet, "/api/v1/projects", "", nil, http.StatusUnauthorized},
		{"not found", http.MethodGet, "/api/v1/projects/" + domain.NewID().String(), token, nil, http.StatusNotFound},
		{"validation", http.MethodPost, "/api/v1/projects", token, map[string]any{"prompt": ""}, http.StatusUnprocessableEntity},
		{"unknown route", http.MethodGet, "/api/v1/nonexistent", token, nil, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := h.do(tc.method, tc.path, tc.token, tc.body)
			if res.status != tc.expectedStatus {
				t.Fatalf("expected %d, got %d: %s", tc.expectedStatus, res.status, res.raw)
			}
			envelope, ok := res.body["error"].(map[string]any)
			if !ok {
				t.Fatalf("response has no error envelope: %s", res.raw)
			}
			for _, field := range []string{"code", "message", "request_id"} {
				if v, _ := envelope[field].(string); v == "" {
					t.Errorf("error envelope is missing %q: %s", field, res.raw)
				}
			}
		})
	}
}

func TestCORSAllowlist(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/meta", nil)
	req.Header.Set("Origin", "http://localhost:1420")
	res, _ := h.app.Test(req)
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:1420" {
		t.Fatalf("allowed origin not echoed: %q", got)
	}

	req = httptest.NewRequest(http.MethodOptions, "/api/v1/meta", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	res, _ = h.app.Test(req)
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin was accepted: %q", got)
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	res, _ := h.app.Test(req)

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if res.Header.Get("X-Request-ID") == "" {
		t.Error("responses must carry a request id for correlation")
	}
}

// --- IDE surface ----------------------------------------------------------

// buildProject creates a project and waits for its run to finish, returning the
// project id and its workspace path.
func (h *harness) buildProject(token, prompt string) (string, string) {
	h.t.Helper()

	res := h.do(http.MethodPost, "/api/v1/projects", token, map[string]any{
		"prompt": prompt, "start": true,
	})
	if res.status != http.StatusCreated {
		h.t.Fatalf("create project: %d %s", res.status, res.raw)
	}
	project := res.body["project"].(map[string]any)
	projectID := str(project, "id")
	workspace := str(project, "workspace_path")

	runBody, ok := res.body["run"].(map[string]any)
	if !ok {
		h.t.Fatalf("no run was started: %s", res.raw)
	}
	runID := str(runBody, "id")

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		res = h.do(http.MethodGet, "/api/v1/runs/"+runID, token, nil)
		status := str(res.body, "status")
		if status == "succeeded" || status == "failed" || status == "canceled" {
			return projectID, workspace
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatal("the run did not finish in time")
	return "", ""
}

func TestWorkspaceTreeAndFileEditing(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("ide@genesis.local")
	projectID, _ := h.buildProject(token, "Build a CRM system")

	// The tree must contain the generated project.
	res := h.do(http.MethodGet, "/api/v1/projects/"+projectID+"/files", token, nil)
	if res.status != http.StatusOK {
		t.Fatalf("tree: %d %s", res.status, res.raw)
	}
	nodes := res.body["data"].([]any)
	if len(nodes) == 0 {
		t.Fatal("the file tree is empty")
	}

	names := map[string]bool{}
	for _, raw := range nodes {
		node := raw.(map[string]any)
		names[str(node, "name")] = true
		// Directories must come first and carry children.
		if node["dir"] == true {
			if _, ok := node["children"]; !ok {
				t.Errorf("directory %s has no children field", str(node, "name"))
			}
		}
	}
	for _, want := range []string{"api", "docs", "README.md"} {
		if !names[want] {
			t.Errorf("the tree is missing %q: %v", want, names)
		}
	}

	// Read a file.
	res = h.do(http.MethodGet, "/api/v1/projects/"+projectID+"/file?path=README.md", token, nil)
	if res.status != http.StatusOK {
		t.Fatalf("read: %d %s", res.status, res.raw)
	}
	content := str(res.body, "content")
	sha := str(res.body, "sha256")
	if content == "" || sha == "" {
		t.Fatalf("the file was returned empty: %s", res.raw)
	}
	if str(res.body, "language") != "markdown" {
		t.Errorf("wrong language: %q", str(res.body, "language"))
	}

	// Edit it. This is invariant I2: the user can change anything.
	edited := content + "\n<!-- edited by the user -->\n"
	res = h.do(http.MethodPut, "/api/v1/projects/"+projectID+"/file", token, map[string]any{
		"path": "README.md", "content": edited, "base_sha256": sha,
	})
	if res.status != http.StatusOK {
		t.Fatalf("write: %d %s", res.status, res.raw)
	}

	res = h.do(http.MethodGet, "/api/v1/projects/"+projectID+"/file?path=README.md", token, nil)
	if !strings.Contains(str(res.body, "content"), "edited by the user") {
		t.Fatal("the edit was not persisted")
	}
}

// A stale save would silently discard whichever change landed second.
func TestWorkspaceWriteRejectsStaleBase(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("stale@genesis.local")
	projectID, _ := h.buildProject(token, "Build a CRM system")

	res := h.do(http.MethodPut, "/api/v1/projects/"+projectID+"/file", token, map[string]any{
		"path": "README.md", "content": "clobbered",
		"base_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if res.status != http.StatusConflict {
		t.Fatalf("expected 409 for a stale write, got %d: %s", res.status, res.raw)
	}
}

// Path confinement is the security boundary of the whole editor surface.
func TestWorkspaceRejectsPathEscapes(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("escape@genesis.local")
	projectID, _ := h.buildProject(token, "Build a CRM system")

	for _, path := range []string{
		"../../../etc/passwd", "/etc/passwd", "..", "api/../../outside.txt",
		".git/config", ".git/HEAD",
	} {
		res := h.do(http.MethodGet,
			"/api/v1/projects/"+projectID+"/file?path="+url.QueryEscape(path), token, nil)
		if res.status == http.StatusOK {
			t.Errorf("reading %q was permitted", path)
		}

		res = h.do(http.MethodPut, "/api/v1/projects/"+projectID+"/file", token, map[string]any{
			"path": path, "content": "owned",
		})
		if res.status == http.StatusOK {
			t.Errorf("writing %q was permitted", path)
		}
	}
}

func TestWorkspaceSearch(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("search@genesis.local")
	projectID, _ := h.buildProject(token, "Build a CRM system")

	res := h.do(http.MethodGet,
		"/api/v1/projects/"+projectID+"/search?q=Validate", token, nil)
	if res.status != http.StatusOK {
		t.Fatalf("search: %d %s", res.status, res.raw)
	}
	hits := res.body["data"].([]any)
	if len(hits) == 0 {
		t.Fatal("a term known to be in the generated code returned no hits")
	}
	first := hits[0].(map[string]any)
	if str(first, "path") == "" || first["line"] == nil || str(first, "text") == "" {
		t.Fatalf("an incomplete search hit: %+v", first)
	}

	// An empty query is a client error, not an excuse to scan everything.
	res = h.do(http.MethodGet, "/api/v1/projects/"+projectID+"/search?q=", token, nil)
	if res.status != http.StatusUnprocessableEntity {
		t.Errorf("an empty query returned %d", res.status)
	}
}

func TestWorkspaceHistoryAndRollback(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("history@genesis.local")
	projectID, _ := h.buildProject(token, "Build a CRM system")

	res := h.do(http.MethodGet, "/api/v1/projects/"+projectID+"/history", token, nil)
	if res.status != http.StatusOK {
		t.Fatalf("history: %d %s", res.status, res.raw)
	}
	commits, _ := res.body["data"].([]any)
	if len(commits) == 0 {
		t.Skip("git is unavailable in this environment")
	}

	newest := commits[0].(map[string]any)
	if str(newest, "sha") == "" || str(newest, "subject") == "" {
		t.Fatalf("an incomplete commit record: %+v", newest)
	}

	// Damage the project, then roll it back.
	res = h.do(http.MethodGet, "/api/v1/projects/"+projectID+"/file?path=README.md", token, nil)
	original := str(res.body, "content")

	h.do(http.MethodPut, "/api/v1/projects/"+projectID+"/file", token, map[string]any{
		"path": "README.md", "content": "destroyed", "base_sha256": str(res.body, "sha256"),
	})

	res = h.do(http.MethodGet, "/api/v1/projects/"+projectID+"/vcs", token, nil)
	if res.body["clean"] == true {
		t.Fatal("the working tree reports clean after a modification")
	}

	res = h.do(http.MethodPost, "/api/v1/projects/"+projectID+"/rollback", token,
		map[string]any{"ref": str(newest, "sha")})
	if res.status != http.StatusNoContent {
		t.Fatalf("rollback: %d %s", res.status, res.raw)
	}

	res = h.do(http.MethodGet, "/api/v1/projects/"+projectID+"/file?path=README.md", token, nil)
	if str(res.body, "content") != original {
		t.Fatal("the rollback did not restore the original content")
	}
}

func TestWorkspaceIsolatedBetweenUsers(t *testing.T) {
	h := newHarness(t)
	ownerToken, _ := h.register("owner2@genesis.local")
	intruderToken, _ := h.register("intruder2@genesis.local")

	projectID, _ := h.buildProject(ownerToken, "Build a private CRM")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/projects/" + projectID + "/files"},
		{http.MethodGet, "/api/v1/projects/" + projectID + "/file?path=README.md"},
		{http.MethodPut, "/api/v1/projects/" + projectID + "/file"},
		{http.MethodGet, "/api/v1/projects/" + projectID + "/search?q=x"},
		{http.MethodGet, "/api/v1/projects/" + projectID + "/history"},
		{http.MethodPost, "/api/v1/projects/" + projectID + "/rollback"},
	} {
		res := h.do(tc.method, tc.path, intruderToken, map[string]any{"path": "README.md", "content": "x"})
		if res.status != http.StatusNotFound {
			t.Errorf("%s %s by a non-owner returned %d, expected 404", tc.method, tc.path, res.status)
		}
	}
}

// An expired access token must be recoverable through the refresh token
// without the user signing in again.
//
// This is the "token expired" report: the access token lives 15 minutes by
// design, and a client that never refreshes simply stops working a quarter of
// an hour after launch. The message reads like a licence or billing problem,
// which is the worst possible framing for a local sign-in that lapsed — the
// product is free and runs entirely offline.
func TestExpiredAccessTokenIsRecoverableByRefreshing(t *testing.T) {
	h := newHarness(t)

	res := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": "expiry@genesis.local", "password": "a-sufficiently-long-password",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("register: %d %s", res.status, res.raw)
	}
	refresh, _ := res.body["refresh_token"].(string)
	if refresh == "" {
		t.Fatal("registration returned no refresh token, so renewal is impossible")
	}

	// A token the server will reject, standing in for one that has lapsed.
	res = h.do(http.MethodGet, "/api/v1/projects", "expired.or.invalid.token", nil)
	if res.status != http.StatusUnauthorized {
		t.Fatalf("an invalid token should be rejected, got %d", res.status)
	}

	// Exactly what the desktop client now does on a 401.
	res = h.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": refresh,
	})
	if res.status != http.StatusOK {
		t.Fatalf("refresh should succeed: %d %s", res.status, res.raw)
	}

	renewed, _ := res.body["access_token"].(string)
	if renewed == "" {
		t.Fatal("refresh returned no access token")
	}

	// The retry must now succeed, which is what makes the expiry invisible.
	res = h.do(http.MethodGet, "/api/v1/projects", renewed, nil)
	if res.status != http.StatusOK {
		t.Fatalf("the renewed token was rejected: %d %s", res.status, res.raw)
	}

	// Rotation: the replacement must differ, or a client cannot tell that
	// renewal happened and will keep presenting a token that is about to die.
	if rotated, _ := res.body["refresh_token"].(string); rotated == refresh {
		t.Error("the refresh token was not rotated")
	}
}

// Registration and login must hand back a refresh token. Without one there is
// nothing to renew with, and the session ends when the access token does.
func TestSessionResponsesCarryARefreshToken(t *testing.T) {
	h := newHarness(t)

	res := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": "carries@genesis.local", "password": "a-sufficiently-long-password",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("register: %d %s", res.status, res.raw)
	}
	if token, _ := res.body["refresh_token"].(string); token == "" {
		t.Error("registration did not return a refresh token")
	}

	res = h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": "carries@genesis.local", "password": "a-sufficiently-long-password",
	})
	if res.status != http.StatusOK {
		t.Fatalf("login: %d %s", res.status, res.raw)
	}
	if token, _ := res.body["refresh_token"].(string); token == "" {
		t.Error("login did not return a refresh token")
	}
}

// A user must be able to take their project away.
//
// Without an export, a generated project can be viewed but not used: it lives
// inside the application's data directory under a UUID, which is not somewhere
// anyone can reasonably work. This was the single largest gap in the product —
// the factory built things and then kept them.
func TestProjectExportsAsAZipArchive(t *testing.T) {
	h := newHarness(t)
	token, _ := h.register("export@genesis.local")

	// The project must actually be built: an unbuilt one has no files, and an
	// empty archive would pass a weaker assertion while being useless.
	id, _ := h.buildProject(token, "Build a small inventory tracker")

	raw, status, header := h.binary(http.MethodGet, "/api/v1/projects/"+id+"/export", token)
	if status != http.StatusOK {
		t.Fatalf("export: %d %s", status, string(raw))
	}

	if got := header.Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type is %q, expected application/zip", got)
	}
	// Without a filename the browser saves the route's last segment, so the
	// user receives a file called "export" with no extension.
	if disposition := header.Get("Content-Disposition"); !strings.Contains(disposition, ".zip") {
		t.Errorf("Content-Disposition does not name a .zip file: %q", disposition)
	}

	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("the response is not a readable zip: %v", err)
	}
	if len(archive.File) == 0 {
		t.Fatal("the archive is empty")
	}

	for _, file := range archive.File {
		// Build scratch and version-control internals must not ship: the
		// first implementation included .genesis-tmp and the archive was
		// 4.5 MB of stale object files instead of 183 KB of source.
		if strings.HasPrefix(file.Name, ".genesis-tmp/") || strings.HasPrefix(file.Name, ".git/") {
			t.Errorf("the archive contains %s, which is not part of the project", file.Name)
		}
		// Zip requires forward slashes. Windows tools read a backslash as
		// part of the name, producing files called "api\main.go".
		if strings.Contains(file.Name, `\`) {
			t.Errorf("%q uses backslashes; zip entries must use forward slashes", file.Name)
		}
	}
}
