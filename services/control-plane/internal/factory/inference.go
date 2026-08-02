package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"

	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Reasoner turns a prompt plus a schema into a validated document.
//
// This is the single choke point through which every model interaction passes.
// Centralising it is what makes the determinism envelope real: budgets, repair,
// accounting and the "no unvalidated output escapes" rule are enforced in one
// place rather than re-implemented, and forgotten, in eleven agents.
type Reasoner struct {
	llm   port.LLM
	log   *slog.Logger
	clock func() time.Time

	// contextWindow is the model's real capacity, discovered once. Budgeting
	// against a guess is how prompts silently overflow: the server rejects the
	// request and the agent degrades for a reason that looks like a model
	// failure but is actually an accounting bug on our side.
	ctxOnce   sync.Once
	ctxWindow int
}

// NewReasoner constructs the inference helper.
func NewReasoner(client port.LLM, log *slog.Logger) *Reasoner {
	if log == nil {
		log = slog.Default()
	}
	return &Reasoner{llm: client, log: log, clock: time.Now}
}

// fallbackContextWindow is assumed when the provider does not report one.
// Deliberately conservative: overflowing is a hard failure, under-using is not.
const fallbackContextWindow = 4096

// ContextWindow reports the usable prompt budget in tokens, reserving room for
// the response.
func (r *Reasoner) ContextWindow(ctx context.Context) int {
	r.ctxOnce.Do(func() {
		r.ctxWindow = fallbackContextWindow
		models, err := r.llm.Models(ctx)
		if err != nil {
			return
		}
		for _, m := range models {
			if m.Context > 0 && m.Context > r.ctxWindow {
				r.ctxWindow = m.Context
			}
		}
		// Cap the assumed window: a model trained with 128k context on a CPU
		// with 2 GB of RAM cannot actually serve it, and llama.cpp is usually
		// started with a much smaller -c.
		if r.ctxWindow > 16384 {
			r.ctxWindow = 16384
		}
	})
	return r.ctxWindow
}

// PromptBudget returns the tokens available for the prompt, after reserving
// space for the model's reply and a safety margin for chat-template overhead.
func (r *Reasoner) PromptBudget(ctx context.Context, maxOutput int) int {
	budget := r.ContextWindow(ctx) - maxOutput - 256
	if budget < 512 {
		budget = 512
	}
	return budget
}

// Available reports whether inference can be attempted at all.
func (r *Reasoner) Available() bool { return r != nil && r.llm != nil }

// Attempt records one inference round trip for observability and accounting.
type Attempt struct {
	Number   int
	Model    string
	Usage    port.Usage
	Latency  time.Duration
	Repaired bool
	Err      error
}

// Result is a validated model response together with its cost.
type Result struct {
	Document map[string]any
	Raw      string
	Attempts []Attempt
	Usage    port.Usage
	Model    string
	Latency  time.Duration
}

// TotalTokens reports the tokens consumed across every attempt.
func (r Result) TotalTokens() int { return r.Usage.Total() }

// Ask performs constrained generation with bounded repair.
//
// The loop is: generate → extract → validate → on failure, feed the validator's
// own complaints back as a correction turn. Repair is capped because a model
// that cannot satisfy a schema in three tries will not satisfy it in thirty,
// and an unbounded loop is how an agent quietly consumes an entire budget.
func (r *Reasoner) Ask(
	ctx context.Context,
	system string,
	user string,
	schema json.RawMessage,
	schemaName string,
	class port.ModelClass,
	temperature float32,
	maxRepairs int,
) (*Result, error) {
	if !r.Available() {
		return nil, domain.Unavailable("llm_unavailable", "no inference provider is configured")
	}
	if maxRepairs < 0 {
		maxRepairs = 0
	}

	validator, err := port.NewValidator(schema)
	if err != nil {
		return nil, fmt.Errorf("agent schema is invalid: %w", err)
	}

	messages := []port.Message{
		{Role: port.ChatSystem, Content: system},
		{Role: port.ChatUser, Content: user},
	}

	result := &Result{}
	var lastErr error
	seed := deterministicSeed

	for attempt := 0; attempt <= maxRepairs; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Reserve output space inside the window rather than asking for a fixed
		// 4096 tokens the server may not have room for.
		maxOutput := defaultMaxTokens
		if window := r.ContextWindow(ctx); maxOutput > window/2 {
			maxOutput = window / 2
		}

		response, err := r.llm.Complete(ctx, port.CompletionRequest{
			Class:       class,
			Messages:    messages,
			Schema:      schema,
			SchemaName:  schemaName,
			Temperature: temperature,
			MaxTokens:   maxOutput,
			// A fixed seed makes a run reproducible on providers that honour
			// it, which turns "the model was weird that time" into a
			// debuggable, repeatable event.
			Seed: &seed,
		})
		if err != nil {
			// Transport and availability failures are not repairable by
			// rewording the prompt, so they abort immediately.
			result.Attempts = append(result.Attempts, Attempt{Number: attempt + 1, Err: err})
			return nil, err
		}

		result.Usage.PromptTokens += response.Usage.PromptTokens
		result.Usage.CompletionTokens += response.Usage.CompletionTokens
		result.Latency += response.Latency
		result.Model = response.Model
		result.Raw = response.Content

		record := Attempt{
			Number:   attempt + 1,
			Model:    response.Model,
			Usage:    response.Usage,
			Latency:  response.Latency,
			Repaired: attempt > 0,
		}

		// Truncation is reported explicitly: a document cut off mid-array is
		// usually schema-invalid anyway, but saying why saves a confusing hunt.
		if response.Truncated() {
			record.Err = errors.New("generation hit the token limit")
			result.Attempts = append(result.Attempts, record)
			lastErr = domain.Unavailable("output_truncated",
				"the model ran out of output tokens; the response is incomplete")
			messages = append(messages,
				port.Message{Role: port.ChatAssistant, Content: response.Content},
				port.Message{Role: port.ChatUser, Content: "Your response was cut off. Return a shorter, complete JSON document that satisfies the schema."})
			continue
		}

		extracted := port.ExtractJSON(response.Content)
		document, validationErr := validator.ValidateJSON([]byte(extracted))
		if validationErr == nil {
			result.Attempts = append(result.Attempts, record)
			result.Document = document
			return result, nil
		}

		record.Err = validationErr
		result.Attempts = append(result.Attempts, record)
		lastErr = validationErr

		var ve *port.ValidationError
		if !errors.As(validationErr, &ve) {
			return nil, validationErr
		}

		r.log.Debug("model output failed validation; repairing",
			"attempt", attempt+1, "issues", len(ve.Issues), "schema", schemaName)

		messages = append(messages,
			port.Message{Role: port.ChatAssistant, Content: response.Content},
			port.Message{Role: port.ChatUser, Content: ve.Instructions()})
	}

	return nil, fmt.Errorf("model output never satisfied the %s schema after %d attempts: %w",
		schemaName, maxRepairs+1, lastErr)
}

// deterministicSeed is shared by every call so two runs of the same prompt
// produce the same plan on providers that support seeding. It is a constant;
// the local copy below exists only because the wire format needs a pointer to
// distinguish "unset" from "zero".
const deterministicSeed = 42

// defaultMaxTokens bounds a single generation. Agent documents are structured
// and rarely long; a runaway generation is far more likely to be a defect than
// a genuinely large answer.
const defaultMaxTokens = 4096

// Prompt assembles a token-budgeted instruction from labelled sections.
//
// Naive prompt building concatenates everything and silently overflows the
// context window, which manifests as the model ignoring the earliest (and
// usually most important) instructions. Sections are therefore added in
// priority order and truncated from the least important end.
type Prompt struct {
	sections []promptSection
	budget   int
}

type promptSection struct {
	label    string
	body     string
	priority int
}

// NewPrompt starts an assembler with a character budget.
//
// Characters rather than tokens: an exact tokeniser would have to match the
// model's own, which varies per model and is unavailable locally. Roughly four
// characters per token is a safe, conservative approximation for budgeting.
func NewPrompt(tokenBudget int) *Prompt {
	if tokenBudget <= 0 {
		tokenBudget = 6000
	}
	return &Prompt{budget: tokenBudget * 4}
}

// Add appends a section. Lower priority numbers are kept first under pressure.
func (p *Prompt) Add(label, body string, priority int) *Prompt {
	body = strings.TrimSpace(body)
	if body == "" {
		return p
	}
	p.sections = append(p.sections, promptSection{label: label, body: body, priority: priority})
	return p
}

// String renders the prompt, dropping or trimming low-priority sections until
// it fits the budget.
func (p *Prompt) String() string {
	ordered := make([]promptSection, len(p.sections))
	copy(ordered, p.sections)
	// Stable sort by priority preserves the caller's ordering within a tier.
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j].priority < ordered[j-1].priority; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}

	var (
		sb    strings.Builder
		spent int
	)
	for _, section := range ordered {
		header := "\n## " + section.label + "\n\n"
		cost := len(header) + len(section.body)

		if spent+cost <= p.budget {
			sb.WriteString(header)
			sb.WriteString(section.body)
			sb.WriteString("\n")
			spent += cost
			continue
		}

		remaining := p.budget - spent - len(header)
		// Only include a truncated section if enough survives to be useful.
		if remaining > 400 {
			sb.WriteString(header)
			sb.WriteString(section.body[:remaining])
			sb.WriteString("\n…(truncated to fit the context window)\n")
			spent = p.budget
		}
		break
	}
	return strings.TrimSpace(sb.String())
}

// jsonList renders a slice as a compact bulleted list for prompts.
func jsonList(items []string) string {
	var sb strings.Builder
	for _, item := range items {
		sb.WriteString("- ")
		sb.WriteString(item)
		sb.WriteString("\n")
	}
	return sb.String()
}
