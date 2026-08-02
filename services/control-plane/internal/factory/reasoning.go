package factory

import (
	"context"
	"fmt"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Reasoning is the optional intelligence layer attached to a run.
//
// Every agent works with or without it. When a model is available the agent
// enriches its blueprint-derived output with model judgement; when it is not,
// the deterministic path still produces a complete, coherent document. This is
// not a fallback bolted on for robustness — it is the local-first requirement:
// the product must be useful on a machine with no model installed, and better
// on one with a model.
type Reasoning struct {
	reasoner *Reasoner
	memory   *MemoryService
	// budget tracks tokens consumed by this run so a single build cannot
	// silently exhaust an operator's compute.
	budget *TokenBudget
}

// Enabled reports whether model-backed reasoning is usable.
func (r *Reasoning) Enabled() bool {
	return r != nil && r.reasoner != nil && r.reasoner.Available() && !r.budget.Exhausted()
}

// TokenBudget bounds the tokens a run may consume.
type TokenBudget struct {
	limit int64
	used  int64
}

// NewTokenBudget constructs a budget. A non-positive limit means unbounded,
// which is only appropriate for tests.
func NewTokenBudget(limit int64) *TokenBudget { return &TokenBudget{limit: limit} }

// Spend records consumption and reports whether the budget still has room.
func (b *TokenBudget) Spend(tokens int) bool {
	if b == nil {
		return true
	}
	b.used += int64(tokens)
	return !b.Exhausted()
}

// Exhausted reports whether the budget is spent.
func (b *TokenBudget) Exhausted() bool {
	if b == nil || b.limit <= 0 {
		return false
	}
	return b.used >= b.limit
}

// Used reports consumption so far.
func (b *TokenBudget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used
}

// Remaining reports the tokens left, or -1 when unbounded.
func (b *TokenBudget) Remaining() int64 {
	if b == nil || b.limit <= 0 {
		return -1
	}
	if b.used >= b.limit {
		return 0
	}
	return b.limit - b.used
}

// think runs one constrained generation, recording cost and emitting progress.
//
// It never returns an error to the caller: a model failure degrades the
// artifact's richness, it does not fail the build. An agent that aborts a
// forty-file generation because a 0.5B model produced malformed JSON would be
// strictly worse than one that falls back to its blueprint.
func (r *Reasoning) think(
	ctx context.Context,
	tb Toolbelt,
	role domain.AgentRole,
	label string,
	system string,
	user string,
	schemaName string,
	schema []byte,
	class port.ModelClass,
	temperature float32,
) map[string]any {
	if !r.Enabled() {
		return nil
	}

	tb.Emit(ctx, domain.LevelDebug, "Consulting the model: "+label, map[string]any{
		"schema": schemaName, "class": string(class),
	})

	result, err := r.reasoner.Ask(ctx, system, user, schema, schemaName, class, temperature, 2)
	if err != nil {
		// Report the degradation honestly rather than pretending the richer
		// output was produced.
		tb.Emit(ctx, domain.LevelWarn,
			fmt.Sprintf("Model could not produce %s; using the deterministic blueprint instead", label),
			map[string]any{"error": err.Error(), "schema": schemaName})
		return nil
	}

	r.budget.Spend(result.TotalTokens())

	repairs := 0
	for _, attempt := range result.Attempts {
		if attempt.Repaired {
			repairs++
		}
	}
	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Model produced %s", label),
		map[string]any{
			"tokens":     result.TotalTokens(),
			"latency_ms": result.Latency.Milliseconds(),
			"model":      result.Model,
			"repairs":    repairs,
		})

	return result.Document
}

// PromptBudget reports how many prompt tokens this run may use, derived from
// the model's actual context window.
func (r *Reasoning) PromptBudget(ctx context.Context) int {
	if !r.Enabled() {
		return 2000
	}
	return r.reasoner.PromptBudget(ctx, 1500)
}

// critiqueEcho rejects output that merely restates its own input.
//
// A model under-powered for a task will happily satisfy a schema by copying
// fragments of the prompt into every field. That is indistinguishable from
// authored content by shape and worthless by substance, so it is checked
// separately from repetition: the architect case echoed the PRD's user stories
// into its "decisions" without repeating itself.
func critiqueEcho(outputs []string, source string) error {
	if strings.TrimSpace(source) == "" || len(outputs) == 0 {
		return nil
	}
	sourceTerms := tokenize(source)
	if len(sourceTerms) < 20 {
		return nil
	}

	echoed := 0
	considered := 0
	for _, out := range outputs {
		if len(strings.Fields(out)) < 6 {
			continue
		}
		considered++
		// Near-total containment in the source means nothing new was said.
		if similarity(out, source) > 0.9 {
			echoed++
		}
	}
	if considered > 0 && float64(echoed)/float64(considered) > 0.6 {
		return fmt.Errorf("output restates its input: %d of %d entries add nothing new", echoed, considered)
	}
	return nil
}

// critique rejects output that satisfies the schema but says nothing.
//
// Constrained decoding guarantees shape, not substance. Small models reliably
// produce documents that pass validation while being near-verbatim repetitions
// of the prompt or of each other. Shipping that is worse than falling back to
// the blueprint, because it looks authored and is not. This is the cheap,
// deterministic half of the critic pass described in the agent architecture.
func critique(fields []string) error {
	nonEmpty := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			nonEmpty = append(nonEmpty, f)
		}
	}
	if len(nonEmpty) < 2 {
		return nil
	}

	// Near-duplicate entries indicate the model looped rather than reasoned.
	duplicates := 0
	for i := 0; i < len(nonEmpty); i++ {
		for j := i + 1; j < len(nonEmpty); j++ {
			if similarity(nonEmpty[i], nonEmpty[j]) > 0.75 {
				duplicates++
			}
		}
	}
	pairs := len(nonEmpty) * (len(nonEmpty) - 1) / 2
	if pairs > 0 && float64(duplicates)/float64(pairs) > 0.4 {
		return fmt.Errorf("output is repetitive: %d of %d entry pairs are near-duplicates", duplicates, pairs)
	}
	return nil
}

// similarity is token-overlap between two strings, in [0,1].
func similarity(a, b string) float64 {
	ta, tb := tokenize(a), tokenize(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	shared := 0
	for term := range ta {
		if tb[term] > 0 {
			shared++
		}
	}
	smaller := len(ta)
	if len(tb) < smaller {
		smaller = len(tb)
	}
	return float64(shared) / float64(smaller)
}

// recall retrieves relevant long-term memory for a prompt.
func (r *Reasoning) recall(ctx context.Context, projectID domain.ID, query string, limit int) []port.MemoryHit {
	if r == nil || r.memory == nil {
		return nil
	}
	hits, err := r.memory.Recall(ctx, port.MemoryQuery{
		Text:      query,
		ProjectID: projectID.String(),
		Scopes:    []port.MemoryScope{port.ScopeProject, port.ScopeGlobal},
		Limit:     limit,
		MinScore:  0.25,
	})
	if err != nil {
		return nil
	}
	return hits
}

// remember stores a durable decision for future runs.
func (r *Reasoning) remember(ctx context.Context, projectID domain.ID, kind port.MemoryKind, title, content string) {
	if r == nil || r.memory == nil || strings.TrimSpace(content) == "" {
		return
	}
	_, _ = r.memory.Remember(ctx, port.Memory{
		Scope:      port.ScopeProject,
		ProjectID:  projectID.String(),
		Kind:       kind,
		Title:      title,
		Content:    content,
		Importance: 0.6,
	})
}

// memoryContext renders recalled memories as a prompt section.
func memoryContext(hits []port.MemoryHit) string {
	if len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Relevant decisions and preferences from earlier work. ")
	sb.WriteString("Stay consistent with these unless the brief contradicts them.\n\n")
	for _, hit := range hits {
		fmt.Fprintf(&sb, "- **%s**: %s\n", hit.Memory.Title, hit.Memory.Content)
	}
	return sb.String()
}

// --- shared prompt fragments ---------------------------------------------

// houseStyle is prepended to every system prompt.
//
// The instructions target the failure modes small local models actually
// exhibit: hedging, restating the question, inventing fields, and producing
// generic filler that passes a schema while saying nothing.
const houseStyle = `You are part of an autonomous software engineering organization that ships real products.

Rules:
- Answer only with the JSON document requested. No preamble, no markdown fences, no commentary.
- Be concrete and specific to this product. Generic statements that would apply to any software are worthless.
- Never invent fields that are not in the schema.
- Write in plain, direct professional English. No marketing language.`

// briefContext renders the product brief and detected category for a prompt.
func briefContext(bb *Blackboard) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Product name: %s\n", bb.Project.Name)
	fmt.Fprintf(&sb, "Original brief: %s\n", strings.TrimSpace(bb.Project.Prompt))
	fmt.Fprintf(&sb, "Detected category: %s (%s)\n", bb.Blueprint.Name, bb.Classification.Category)
	fmt.Fprintf(&sb, "Category description: %s\n", bb.Blueprint.Description)
	return sb.String()
}

// blueprintContext summarises the structural template the agents build on.
func blueprintContext(bb *Blackboard) string {
	bp := bb.Blueprint
	var sb strings.Builder

	sb.WriteString("The system has already selected a proven structural template for this category. ")
	sb.WriteString("Your job is to add product-specific judgement on top of it, not to redesign it.\n\n")

	sb.WriteString("Core entities: ")
	names := make([]string, 0, len(bp.Entities))
	for _, e := range bp.Entities {
		names = append(names, e.Name)
	}
	sb.WriteString(strings.Join(names, ", "))
	sb.WriteString("\n\nEpics: ")
	sb.WriteString(strings.Join(bp.Epics, ", "))
	sb.WriteString("\n\nScreens: ")
	screens := make([]string, 0, len(bp.Screens))
	for _, s := range bp.Screens {
		screens = append(screens, s.Name)
	}
	sb.WriteString(strings.Join(screens, ", "))
	sb.WriteString("\n")
	return sb.String()
}

// NewReasoningForTest constructs a reasoning layer directly.
//
// Exported for tests in the external factory_test package, which must be able
// to inject a scripted provider without exposing the unexported fields to
// production callers, who receive theirs from the driver.
func NewReasoningForTest(client port.LLM, memory *MemoryService, tokenBudget int64) *Reasoning {
	if client == nil {
		return nil
	}
	return &Reasoning{
		reasoner: NewReasoner(client, nil),
		memory:   memory,
		budget:   NewTokenBudget(tokenBudget),
	}
}

// CritiqueEchoForTest exposes the echo detector to the external test package.
func CritiqueEchoForTest(outputs []string, source string) error {
	return critiqueEcho(outputs, source)
}
