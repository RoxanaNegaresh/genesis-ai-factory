package factory

import (
	"context"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// Charter is the declarative configuration of an agent: what it is for, what it
// consumes and produces, which tools it may use, and the budget it operates
// under. Charters are data, so tightening a tool allowlist or changing a model
// class is a configuration change, not a code change.
type Charter struct {
	Role        domain.AgentRole
	Mission     string
	Inputs      []domain.ArtifactKind
	Outputs     []domain.ArtifactKind
	Tools       []string
	ModelClass  string
	Budget      Budget
	Temperature float32
}

// Budget bounds an agent so a defect cannot become an unbounded resource burn.
type Budget struct {
	MaxTokens    int64
	MaxDuration  time.Duration
	MaxToolCalls int
	MaxRetries   int
}

// DefaultBudget is the conservative envelope applied when a charter omits one.
func DefaultBudget() Budget {
	return Budget{MaxTokens: 120_000, MaxDuration: 5 * time.Minute, MaxToolCalls: 64, MaxRetries: 2}
}

// Blackboard is the shared, typed workspace for one run. Agents never exchange
// natural language directly: they read and write artifacts here. A typed
// blackboard is inspectable, testable and cacheable in a way that a chat
// transcript between agents never is.
type Blackboard struct {
	Project        *domain.Project
	Run            *domain.Run
	Blueprint      Blueprint
	Classification Classification
	// Reasoning is the optional model-backed intelligence layer. It is nil when
	// no inference provider is configured, and every agent must still work.
	Reasoning *Reasoning
	// Runner verifies generated projects by building and running them inside
	// the sandbox. Nil when no execution sandbox is available, in which case
	// agents report that verification did not happen rather than implying it did.
	Runner    *Runner
	artifacts map[domain.ArtifactKind]*domain.Artifact
	values    map[string]any
}

// NewBlackboard creates an empty blackboard for a run.
func NewBlackboard(project *domain.Project, run *domain.Run) *Blackboard {
	return &Blackboard{
		Project:   project,
		Run:       run,
		artifacts: map[domain.ArtifactKind]*domain.Artifact{},
		values:    map[string]any{},
	}
}

// Put records an artifact.
func (b *Blackboard) Put(a *domain.Artifact) {
	if a != nil {
		b.artifacts[a.Kind] = a
	}
}

// Get retrieves an artifact by kind.
func (b *Blackboard) Get(kind domain.ArtifactKind) (*domain.Artifact, bool) {
	a, ok := b.artifacts[kind]
	return a, ok
}

// Has reports whether an artifact kind has been produced.
func (b *Blackboard) Has(kind domain.ArtifactKind) bool {
	_, ok := b.artifacts[kind]
	return ok
}

// Artifacts returns everything produced so far.
func (b *Blackboard) Artifacts() []*domain.Artifact {
	out := make([]*domain.Artifact, 0, len(b.artifacts))
	for _, a := range b.artifacts {
		out = append(out, a)
	}
	return out
}

// SetValue stores structured state that is not itself a deliverable.
func (b *Blackboard) SetValue(key string, v any) { b.values[key] = v }

// Value reads structured state.
func (b *Blackboard) Value(key string) (any, bool) {
	v, ok := b.values[key]
	return v, ok
}

// Toolbelt is the audited capability surface available to an agent. Every side
// effect the factory can have passes through this interface, which is what
// makes sandboxing, secret scanning and rollback enforceable rather than
// aspirational.
type Toolbelt interface {
	// WriteFile creates or replaces a file in the project workspace.
	WriteFile(ctx context.Context, relPath, content string) error
	// ReadFile reads a file from the project workspace.
	ReadFile(ctx context.Context, relPath string) (string, error)
	// ListFiles enumerates the workspace.
	ListFiles(ctx context.Context) ([]string, error)
	// Emit publishes a progress event attributed to the calling agent.
	Emit(ctx context.Context, level domain.Level, message string, fields map[string]any)
}

// Agent is a member of the simulated engineering organization.
//
// Note what this interface is not: it is not "a prompt". An agent is a program
// that may call a model, validate its output, and act through the toolbelt. In
// v0.1 the implementations are deterministic generators driven by blueprints;
// v0.2 swaps their bodies for LLM calls behind this identical contract, so the
// orchestrator, event stream, persistence and UI need no changes.
type Agent interface {
	Charter() Charter
	Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error)
}

// Registry maps roles to implementations.
type Registry struct {
	agents map[domain.AgentRole]Agent
}

// NewRegistry builds the default organization.
func NewRegistry() *Registry {
	r := &Registry{agents: map[domain.AgentRole]Agent{}}
	for _, a := range []Agent{
		&CEOAgent{}, &ProductManagerAgent{}, &UXDesignerAgent{}, &ArchitectAgent{},
		&DatabaseAgent{}, &BackendAgent{}, &FrontendAgent{}, &QAAgent{},
		&SecurityAgent{}, &DevOpsAgent{}, &ImproverAgent{},
	} {
		r.agents[a.Charter().Role] = a
	}
	return r
}

// Get returns the agent for a role.
func (r *Registry) Get(role domain.AgentRole) (Agent, bool) {
	a, ok := r.agents[role]
	return a, ok
}

// Roles returns every registered role in canonical phase order.
func (r *Registry) Roles() []domain.AgentRole {
	var out []domain.AgentRole
	for _, phase := range domain.PhaseOrder {
		for _, profile := range domain.AgentsForPhase(phase) {
			if _, ok := r.agents[profile.Role]; ok {
				out = append(out, profile.Role)
			}
		}
	}
	return out
}
