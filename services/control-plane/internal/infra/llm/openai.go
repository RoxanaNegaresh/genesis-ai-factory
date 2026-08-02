// Package llm implements inference providers behind port.LLM.
//
// The primary provider speaks the OpenAI chat-completions protocol, which is
// what llama.cpp's server, vLLM, Ollama, LM Studio and the hosted APIs all
// expose. Targeting one wire format rather than one vendor means the local-first
// requirement and optional cloud acceleration are the same code path.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Config configures an OpenAI-compatible provider.
type Config struct {
	// BaseURL is the API root, for example http://127.0.0.1:8791/v1.
	BaseURL string
	// APIKey is optional; local servers ignore it.
	APIKey string
	// Timeout bounds a single request. CPU inference of a long document is
	// slow, so this is generous by default.
	Timeout time.Duration
	// ModelMap pins a concrete model per capability class.
	ModelMap map[port.ModelClass]string
	// DefaultModel is used when a class has no mapping.
	DefaultModel string
	// Name identifies the provider in logs.
	Name string
}

// DefaultConfig points at a local llama.cpp server.
func DefaultConfig() Config {
	return Config{
		BaseURL:  "http://127.0.0.1:8791/v1",
		Timeout:  10 * time.Minute,
		ModelMap: map[port.ModelClass]string{},
		Name:     "llamacpp",
	}
}

// Provider is an OpenAI-compatible inference client.
type Provider struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger

	// modelsMu guards the discovered model list, which is cached because
	// llama.cpp serves a fixed set and re-querying on every call is wasteful.
	modelsMu  sync.RWMutex
	models    []port.ModelInfo
	modelsAt  time.Time
	modelsTTL time.Duration
}

// New constructs a provider.
func New(cfg Config, log *slog.Logger) *Provider {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.Name == "" {
		cfg.Name = "openai-compatible"
	}
	if cfg.ModelMap == nil {
		cfg.ModelMap = map[port.ModelClass]string{}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if log == nil {
		log = slog.Default()
	}
	return &Provider{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		log:       log,
		modelsTTL: time.Minute,
	}
}

var _ port.LLM = (*Provider)(nil)

// Name identifies the provider.
func (p *Provider) Name() string { return p.cfg.Name }

// --- wire types ----------------------------------------------------------

type chatRequest struct {
	Model          string          `json:"model,omitempty"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float32         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	Seed           *int            `json:"seed,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Stream         bool            `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *schemaSpec `json:"json_schema,omitempty"`
}

type schemaSpec struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete performs one inference call.
func (p *Provider) Complete(ctx context.Context, req port.CompletionRequest) (*port.CompletionResponse, error) {
	if len(req.Messages) == 0 {
		return nil, domain.Invalid("messages_required", "at least one message is required")
	}

	messages := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		messages = append(messages, chatMessage{Role: string(m.Role), Content: m.Content})
	}

	body := chatRequest{
		Model:       p.resolveModel(req),
		Messages:    messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		Seed:        req.Seed,
	}

	// Constrained decoding: the server enforces the grammar during sampling, so
	// the response is valid JSON matching the schema by construction rather
	// than by retrying until the model happens to comply.
	if len(req.Schema) > 0 {
		name := req.SchemaName
		if name == "" {
			name = "output"
		}
		body.ResponseFormat = &responseFormat{
			Type:       "json_schema",
			JSONSchema: &schemaSpec{Name: name, Strict: true, Schema: req.Schema},
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode completion request: %w", err)
	}

	started := time.Now()
	raw, err := p.post(ctx, "/chat/completions", encoded)
	if err != nil {
		return nil, err
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode completion response: %w", err)
	}
	if parsed.Error != nil {
		return nil, domain.Unavailable("inference_failed", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, domain.Unavailable("inference_empty", "the model returned no choices")
	}

	return &port.CompletionResponse{
		Content:      parsed.Choices[0].Message.Content,
		Model:        parsed.Model,
		FinishReason: parsed.Choices[0].FinishReason,
		Usage: port.Usage{
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
		},
		Latency: time.Since(started),
	}, nil
}

// resolveModel maps a capability class onto a concrete model id.
func (p *Provider) resolveModel(req port.CompletionRequest) string {
	if req.Model != "" {
		return req.Model
	}
	if name, ok := p.cfg.ModelMap[req.Class]; ok && name != "" {
		return name
	}
	if p.cfg.DefaultModel != "" {
		return p.cfg.DefaultModel
	}
	// llama.cpp serves whatever single model it was started with and ignores
	// this field, so an empty value is correct rather than an error.
	return ""
}

// Models lists what the provider can serve, with a short cache.
func (p *Provider) Models(ctx context.Context) ([]port.ModelInfo, error) {
	p.modelsMu.RLock()
	if time.Since(p.modelsAt) < p.modelsTTL && p.models != nil {
		cached := make([]port.ModelInfo, len(p.models))
		copy(cached, p.models)
		p.modelsMu.RUnlock()
		return cached, nil
	}
	p.modelsMu.RUnlock()

	raw, err := p.get(ctx, "/models")
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
			// llama.cpp reports context size under meta.
			Meta struct {
				NCtxTrain int   `json:"n_ctx_train"`
				Size      int64 `json:"size"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}

	out := make([]port.ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		out = append(out, port.ModelInfo{
			ID:      m.ID,
			Classes: classesFor(m.ID),
			Context: m.Meta.NCtxTrain,
		})
	}

	p.modelsMu.Lock()
	p.models = out
	p.modelsAt = time.Now()
	p.modelsMu.Unlock()

	return out, nil
}

// classesFor infers capability tiers from a model identifier.
//
// This is a heuristic over naming conventions rather than a registry lookup: a
// user may install any GGUF, and refusing to route an unrecognised model would
// be worse than an imperfect guess that can be overridden by configuration.
func classesFor(id string) []port.ModelClass {
	lower := strings.ToLower(id)
	var classes []port.ModelClass

	if strings.Contains(lower, "coder") || strings.Contains(lower, "code") ||
		strings.Contains(lower, "starcoder") || strings.Contains(lower, "deepseek") {
		classes = append(classes, port.ClassCode)
	}
	// Small models are fast but not strong reasoners.
	small := strings.Contains(lower, "0.5b") || strings.Contains(lower, "1b") ||
		strings.Contains(lower, "1.5b") || strings.Contains(lower, "3b")
	if small {
		classes = append(classes, port.ClassFast)
	} else {
		classes = append(classes, port.ClassReasoning)
	}
	if len(classes) == 0 {
		classes = []port.ModelClass{port.ClassReasoning, port.ClassCode, port.ClassFast}
	}
	return classes
}

// Ready reports whether inference is available.
func (p *Provider) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := p.get(ctx, "/models"); err != nil {
		return err
	}
	return nil
}

func (p *Provider) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return p.do(req)
}

func (p *Provider) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return p.do(req)
}

func (p *Provider) do(req *http.Request) ([]byte, error) {
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	res, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, domain.Unavailable("inference_timeout",
				"the model did not respond in time; try a smaller model or a shorter prompt")
		}
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		// A refused connection to a local engine is the single most common
		// failure, and a raw dial error tells the user nothing actionable.
		return nil, domain.Unavailable("inference_unreachable",
			fmt.Sprintf("cannot reach the model server at %s — is it running?", p.cfg.BaseURL))
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		return nil, domain.Unavailable("inference_error",
			fmt.Sprintf("model server returned %d: %s", res.StatusCode, truncate(string(raw), 300)))
	}
	return raw, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
