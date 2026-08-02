// Package client is a typed HTTP and websocket client for the Genesis control
// plane. It is shared by every CLI command.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to the control plane.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// APIError is a structured failure returned by the server.
type APIError struct {
	Status    int               `json:"-"`
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id"`
	Fields    map[string]string `json:"fields,omitempty"`
}

func (e *APIError) Error() string {
	if len(e.Fields) > 0 {
		parts := make([]string, 0, len(e.Fields))
		for k, v := range e.Fields {
			parts = append(parts, k+" "+v)
		}
		return fmt.Sprintf("%s (%s)", e.Message, strings.Join(parts, "; "))
	}
	return e.Message
}

// DefaultBaseURL is the loopback address the desktop sidecar uses.
const DefaultBaseURL = "http://127.0.0.1:8787"

// New constructs a client.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		// No global timeout: log streaming and long polls are legitimate.
		// Per-request deadlines come from the caller's context instead.
		HTTP: &http.Client{Timeout: 0},
	}
}

// Session is the credential file written by the server in single-user mode.
type Session struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	APIBase      string `json:"api_base,omitempty"`
}

// SessionPath returns the location of the credential file.
func SessionPath() string {
	if dir := os.Getenv("GENESIS_DATA_DIR"); dir != "" {
		return filepath.Join(dir, "session.json")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "genesis", "session.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".genesis", "session.json")
}

// LoadSession reads stored credentials.
//
// Reading the server's own session file is what lets `genesis` work with no
// login step on a single-user machine: the server already established an
// identity at boot.
func LoadSession() (*Session, error) {
	b, err := os.ReadFile(SessionPath())
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("session file is corrupt: %w", err)
	}
	return &s, nil
}

// SaveSession persists credentials with owner-only permissions.
func SaveSession(s *Session) error {
	path := SessionPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// FromEnvironment builds a client from environment variables and the stored
// session, preferring explicit configuration.
func FromEnvironment() *Client {
	base := os.Getenv("GENESIS_API")
	token := os.Getenv("GENESIS_TOKEN")

	if token == "" {
		if s, err := LoadSession(); err == nil {
			token = s.AccessToken
			if base == "" && s.APIBase != "" {
				base = s.APIBase
			}
		}
	}
	return New(base, token)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	err := c.attempt(ctx, method, path, body, out)

	// Access tokens are deliberately short-lived. Making the operator re-run a
	// command because fifteen minutes elapsed would be a self-inflicted wound,
	// so an expired token is refreshed once and the request retried.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized && c.refresh(ctx) {
		return c.attempt(ctx, method, path, body, out)
	}
	return err
}

// refresh exchanges the stored refresh token for a new session.
func (c *Client) refresh(ctx context.Context) bool {
	session, err := LoadSession()
	if err != nil || session.RefreshToken == "" {
		return false
	}

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		User         struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := c.attempt(ctx, http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": session.RefreshToken}, &out); err != nil {
		return false
	}
	if out.AccessToken == "" {
		return false
	}

	c.Token = out.AccessToken
	session.AccessToken = out.AccessToken
	// Refresh tokens rotate, so the stored copy must be replaced or the next
	// refresh will present a revoked token and trigger family revocation.
	session.RefreshToken = out.RefreshToken
	session.ExpiresAt = out.ExpiresAt
	_ = SaveSession(session)
	return true
}

func (c *Client) attempt(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	res, err := c.HTTP.Do(req)
	if err != nil {
		// A connection refused here almost always means the server is not
		// running; say so instead of surfacing a raw dial error.
		if strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("cannot reach the Genesis server at %s — is it running? (start it with: genesis-server)", c.BaseURL)
		}
		return err
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return err
	}

	if res.StatusCode >= 400 {
		var envelope struct {
			Error APIError `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
			envelope.Error.Status = res.StatusCode
			return &envelope.Error
		}
		return &APIError{Status: res.StatusCode, Code: "http_error",
			Message: fmt.Sprintf("%s: %s", res.Status, strings.TrimSpace(string(raw)))}
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// --- typed models --------------------------------------------------------

type Project struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Slug          string    `json:"slug"`
	Prompt        string    `json:"prompt"`
	Category      string    `json:"category"`
	Status        string    `json:"status"`
	WorkspacePath string    `json:"workspace_path"`
	CreatedAt     time.Time `json:"created_at"`
}

type Phase struct {
	Name       string         `json:"name"`
	Title      string         `json:"title"`
	Status     string         `json:"status"`
	Summary    map[string]any `json:"summary"`
	StartedAt  *time.Time     `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at"`
}

type RunError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Phase   string `json:"phase"`
}

type Run struct {
	ID           string         `json:"id"`
	ProjectID    string         `json:"project_id"`
	Kind         string         `json:"kind"`
	Status       string         `json:"status"`
	CurrentPhase string         `json:"current_phase"`
	Progress     float64        `json:"progress"`
	Result       map[string]any `json:"result"`
	Error        *RunError      `json:"error"`
	Phases       []Phase        `json:"phases"`
	CreatedAt    time.Time      `json:"created_at"`
}

// Terminal reports whether the run has finished.
func (r Run) Terminal() bool {
	switch r.Status {
	case "succeeded", "failed", "canceled", "interrupted":
		return true
	}
	return false
}

type Event struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	AgentRole string         `json:"agent_role"`
	AgentName string         `json:"agent_name"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
}

type Artifact struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Body      string `json:"body,omitempty"`
}

type AgentProfile struct {
	Role       string `json:"role"`
	Name       string `json:"name"`
	Title      string `json:"title"`
	Mission    string `json:"mission"`
	Phase      string `json:"phase"`
	ModelClass string `json:"model_class"`
}

type AgentStatus struct {
	Profile   AgentProfile `json:"profile"`
	Status    string       `json:"status"`
	Task      string       `json:"task"`
	Artifacts int          `json:"artifacts"`
}

type Classification struct {
	Category   string   `json:"category"`
	Confidence float64  `json:"confidence"`
	Matched    []string `json:"matched_signals"`
}

type BlueprintSummary struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Entities    int      `json:"entities"`
	Screens     int      `json:"screens"`
	Epics       []string `json:"epics"`
}

type Meta struct {
	Name         string         `json:"name"`
	Version      string         `json:"version"`
	Commit       string         `json:"commit"`
	Capabilities map[string]any `json:"capabilities"`
}

// --- operations ----------------------------------------------------------

func (c *Client) Meta(ctx context.Context) (*Meta, error) {
	var m Meta
	return &m, c.do(ctx, http.MethodGet, "/api/v1/meta", nil, &m)
}

func (c *Client) Login(ctx context.Context, email, password string) (*Session, error) {
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		User         struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": email, "password": password}, &out)
	if err != nil {
		return nil, err
	}
	return &Session{
		AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		ExpiresAt: out.ExpiresAt, Email: out.User.Email, UserID: out.User.ID,
		APIBase: c.BaseURL,
	}, nil
}

func (c *Client) Register(ctx context.Context, email, password, name string) (*Session, error) {
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		User         struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/auth/register",
		map[string]string{"email": email, "password": password, "display_name": name}, &out)
	if err != nil {
		return nil, err
	}
	return &Session{
		AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		ExpiresAt: out.ExpiresAt, Email: out.User.Email, UserID: out.User.ID,
		APIBase: c.BaseURL,
	}, nil
}

// CreateProject registers a product, optionally starting the build immediately.
func (c *Client) CreateProject(ctx context.Context, prompt, name string, start bool) (*Project, *Run, error) {
	body := map[string]any{"prompt": prompt, "start": start}
	if name != "" {
		body["name"] = name
	}
	var out struct {
		Project  Project `json:"project"`
		Run      *Run    `json:"run"`
		RunError string  `json:"run_error"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/projects", body, &out); err != nil {
		return nil, nil, err
	}
	if out.RunError != "" {
		return &out.Project, nil, fmt.Errorf("project created but the build did not start: %s", out.RunError)
	}
	return &out.Project, out.Run, nil
}

func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out struct {
		Data []Project `json:"data"`
	}
	return out.Data, c.do(ctx, http.MethodGet, "/api/v1/projects?limit=100", nil, &out)
}

func (c *Client) GetProject(ctx context.Context, id string) (*Project, error) {
	var p Project
	return &p, c.do(ctx, http.MethodGet, "/api/v1/projects/"+id, nil, &p)
}

func (c *Client) StartRun(ctx context.Context, projectID string) (*Run, error) {
	var r Run
	return &r, c.do(ctx, http.MethodPost, "/api/v1/projects/"+projectID+"/runs",
		map[string]string{"kind": "build"}, &r)
}

func (c *Client) GetRun(ctx context.Context, id string) (*Run, error) {
	var r Run
	return &r, c.do(ctx, http.MethodGet, "/api/v1/runs/"+id, nil, &r)
}

func (c *Client) ListRuns(ctx context.Context, projectID string) ([]Run, error) {
	var out struct {
		Data []Run `json:"data"`
	}
	return out.Data, c.do(ctx, http.MethodGet, "/api/v1/projects/"+projectID+"/runs", nil, &out)
}

func (c *Client) CancelRun(ctx context.Context, id string) (*Run, error) {
	var r Run
	return &r, c.do(ctx, http.MethodPost, "/api/v1/runs/"+id+"/cancel", nil, &r)
}

// Events fetches a page of the log after a cursor.
func (c *Client) Events(ctx context.Context, runID string, afterSeq int64) ([]Event, int64, error) {
	var out struct {
		Data    []Event `json:"data"`
		NextSeq int64   `json:"next_seq"`
	}
	q := url.Values{}
	q.Set("after_seq", fmt.Sprint(afterSeq))
	q.Set("limit", "500")
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID+"/events?"+q.Encode(), nil, &out)
	return out.Data, out.NextSeq, err
}

func (c *Client) RunArtifacts(ctx context.Context, runID string) ([]Artifact, error) {
	var out struct {
		Data []Artifact `json:"data"`
	}
	return out.Data, c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID+"/artifacts", nil, &out)
}

func (c *Client) Artifact(ctx context.Context, id string) (*Artifact, error) {
	var a Artifact
	return &a, c.do(ctx, http.MethodGet, "/api/v1/artifacts/"+id, nil, &a)
}

func (c *Client) RunAgents(ctx context.Context, runID string) ([]AgentStatus, error) {
	var out struct {
		Data []AgentStatus `json:"data"`
	}
	return out.Data, c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID+"/agents", nil, &out)
}

// ModelInfo describes a model the inference backend can serve.
type ModelInfo struct {
	ID      string   `json:"id"`
	Classes []string `json:"classes"`
	Context int      `json:"context"`
}

// ModelStatus reports whether reasoning is available.
type ModelStatus struct {
	Enabled  bool        `json:"enabled"`
	Provider string      `json:"provider"`
	Reason   string      `json:"reason"`
	Data     []ModelInfo `json:"data"`
}

func (c *Client) Models(ctx context.Context) (*ModelStatus, error) {
	var out ModelStatus
	return &out, c.do(ctx, http.MethodGet, "/api/v1/models", nil, &out)
}

func (c *Client) Agents(ctx context.Context) ([]AgentProfile, error) {
	var out struct {
		Data []AgentProfile `json:"data"`
	}
	return out.Data, c.do(ctx, http.MethodGet, "/api/v1/agents", nil, &out)
}

func (c *Client) Blueprints(ctx context.Context) ([]BlueprintSummary, error) {
	var out struct {
		Data []BlueprintSummary `json:"data"`
	}
	return out.Data, c.do(ctx, http.MethodGet, "/api/v1/blueprints", nil, &out)
}

// Classify previews how a brief will be interpreted.
func (c *Client) Classify(ctx context.Context, prompt string) (*Classification, *BlueprintSummary, string, error) {
	var out struct {
		Classification Classification   `json:"classification"`
		Blueprint      BlueprintSummary `json:"blueprint"`
		SuggestedName  string           `json:"suggested_name"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/classify", map[string]string{"prompt": prompt}, &out)
	if err != nil {
		return nil, nil, "", err
	}
	return &out.Classification, &out.Blueprint, out.SuggestedName, nil
}
