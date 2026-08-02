package port

import (
	"context"
	"encoding/json"
	"time"
)

// Role identifies the author of a chat message.
type ChatRole string

const (
	ChatSystem    ChatRole = "system"
	ChatUser      ChatRole = "user"
	ChatAssistant ChatRole = "assistant"
)

// Message is one turn in a conversation.
type Message struct {
	Role    ChatRole `json:"role"`
	Content string   `json:"content"`
}

// ModelClass describes the capability tier an agent needs, rather than naming a
// specific model. The router maps a class onto whatever is actually installed,
// so a charter never has to know which weights are on the machine.
type ModelClass string

const (
	// ClassReasoning is for planning, analysis and design: quality matters more
	// than latency.
	ClassReasoning ModelClass = "reasoning"
	// ClassCode is for generating and editing source.
	ClassCode ModelClass = "code"
	// ClassFast is for classification and summarisation, where a small model is
	// sufficient and latency dominates.
	ClassFast ModelClass = "fast"
)

// Valid reports whether the class is known.
func (c ModelClass) Valid() bool {
	switch c {
	case ClassReasoning, ClassCode, ClassFast:
		return true
	}
	return false
}

// CompletionRequest is a single inference call.
type CompletionRequest struct {
	// Class selects the capability tier; the provider resolves the model.
	Class ModelClass
	// Model optionally pins an exact model, overriding Class.
	Model string

	Messages []Message

	// Schema, when set, constrains decoding so the output is valid JSON
	// matching this JSON Schema. This is the difference between parsing model
	// output hopefully and receiving a structurally guaranteed document.
	Schema json.RawMessage
	// SchemaName labels the schema for providers that require it.
	SchemaName string

	Temperature float32
	MaxTokens   int
	Stop        []string

	// Seed makes sampling reproducible where the provider supports it.
	Seed *int
}

// Usage reports token accounting for one call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Total returns the combined token count.
func (u Usage) Total() int { return u.PromptTokens + u.CompletionTokens }

// CompletionResponse is the result of an inference call.
type CompletionResponse struct {
	Content      string
	Model        string
	Usage        Usage
	Latency      time.Duration
	FinishReason string
}

// Truncated reports whether generation stopped because the token limit was
// reached, which usually means the output is unusable rather than merely short.
func (r CompletionResponse) Truncated() bool { return r.FinishReason == "length" }

// LLM is the inference boundary.
//
// The interface is deliberately narrow: one synchronous call plus a health
// probe. Everything richer (agents, validation, repair, budgets) is built on
// top in the application layer, so swapping llama.cpp for vLLM, an OpenAI
// endpoint or a stub in tests changes exactly one adapter.
type LLM interface {
	// Complete performs one inference call.
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
	// Models lists what the provider can currently serve.
	Models(ctx context.Context) ([]ModelInfo, error)
	// Ready reports whether inference is available right now.
	Ready(ctx context.Context) error
	// Name identifies the provider for logging and telemetry.
	Name() string
}

// ModelInfo describes an available model.
type ModelInfo struct {
	ID      string       `json:"id"`
	Classes []ModelClass `json:"classes"`
	Context int          `json:"context"`
	Size    string       `json:"size,omitempty"`
}

// Embedder produces vector representations for the memory system.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
	Ready(ctx context.Context) error
}

// MemoryScope bounds the visibility of a stored memory.
type MemoryScope string

const (
	ScopeGlobal  MemoryScope = "global"
	ScopeUser    MemoryScope = "user"
	ScopeProject MemoryScope = "project"
	ScopeRun     MemoryScope = "run"
)

// MemoryKind classifies what a memory records.
type MemoryKind string

const (
	KindPreference MemoryKind = "preference"
	KindDecision   MemoryKind = "decision"
	KindSnippet    MemoryKind = "snippet"
	KindLesson     MemoryKind = "lesson"
)

// Memory is a retrievable piece of long-term knowledge.
type Memory struct {
	ID         string         `json:"id"`
	Scope      MemoryScope    `json:"scope"`
	UserID     string         `json:"user_id,omitempty"`
	ProjectID  string         `json:"project_id,omitempty"`
	Kind       MemoryKind     `json:"kind"`
	Title      string         `json:"title"`
	Content    string         `json:"content"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Importance float32        `json:"importance"`
	Vector     []float32      `json:"-"`
	CreatedAt  time.Time      `json:"created_at"`
	LastUsedAt *time.Time     `json:"last_used_at,omitempty"`
	UseCount   int            `json:"use_count"`
}

// MemoryQuery retrieves relevant memories.
type MemoryQuery struct {
	Text      string
	Scopes    []MemoryScope
	Kinds     []MemoryKind
	UserID    string
	ProjectID string
	Limit     int
	// MinScore filters weak matches. Retrieval that returns everything is
	// retrieval that returns nothing useful once a project accumulates history.
	MinScore float32
}

// MemoryHit is a retrieved memory with its similarity score.
type MemoryHit struct {
	Memory Memory
	Score  float32
}

// MemoryStore persists and retrieves long-term memory.
type MemoryStore interface {
	Remember(ctx context.Context, m Memory) (Memory, error)
	Recall(ctx context.Context, q MemoryQuery) ([]MemoryHit, error)
	Forget(ctx context.Context, id string) error
	Count(ctx context.Context) (int, error)
}
