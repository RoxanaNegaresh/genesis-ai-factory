package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

func domainArtifactVision() domain.ArtifactKind { return domain.ArtifactVision }
func domainArtifactPRD() domain.ArtifactKind    { return domain.ArtifactPRD }

// runPipelineWithModel executes the analysis phase with a scripted provider.
func runPipelineWithModel(t *testing.T, prompt string, client port.LLM) (*factory.Blackboard, string) {
	t.Helper()
	root := t.TempDir()

	project := &domain.Project{
		ID: domain.NewID(), OwnerID: domain.NewID(),
		Name: domain.TitleFromPrompt(prompt), Slug: domain.Slugify(domain.TitleFromPrompt(prompt)),
		Prompt: prompt, WorkspacePath: root, Settings: domain.DefaultProjectSettings(),
	}
	run, err := domain.NewRun(project.ID, project.OwnerID, domain.RunBuild, nil, 500000, time.Now())
	if err != nil {
		t.Fatalf("new run: %v", err)
	}

	bb := factory.NewBlackboard(project, run)
	bb.Classification = factory.Classify(prompt)
	bb.Blueprint = factory.BlueprintFor(bb.Classification.Category)
	bb.Reasoning = factory.NewReasoningForTest(client, nil, 500000)

	registry := factory.NewRegistry()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleSystem, nil, nil)

	for _, role := range []domain.AgentRole{domain.RoleCEO, domain.RolePM} {
		agent, ok := registry.Get(role)
		if !ok {
			t.Fatalf("agent %s missing", role)
		}
		if _, err := agent.Execute(context.Background(), bb, tb.For(role)); err != nil {
			t.Fatalf("agent %s failed: %v", role, err)
		}
	}
	return bb, root
}

func assertFileHasContent(t *testing.T, root, rel, needle string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("expected %s: %v", rel, err)
	}
	if !strings.Contains(string(raw), needle) {
		t.Fatalf("%s does not contain %q", rel, needle)
	}
}

// fakeLLM is a scripted inference provider.
//
// Tests must exercise the failure modes real models exhibit — malformed JSON,
// schema violations, truncation, prose wrappers, transport errors — because
// those paths are precisely where an agent runtime is either robust or a
// liability. A provider that always returns perfect output would test nothing.
type fakeLLM struct {
	mu        sync.Mutex
	responses []string
	errs      []error
	finish    []string
	calls     []port.CompletionRequest
	window    int
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) Ready(context.Context) error { return nil }

func (f *fakeLLM) Models(context.Context) ([]port.ModelInfo, error) {
	window := f.window
	if window == 0 {
		window = 8192
	}
	return []port.ModelInfo{{ID: "fake-model", Context: window, Classes: []port.ModelClass{port.ClassReasoning}}}, nil
}

func (f *fakeLLM) Complete(_ context.Context, req port.CompletionRequest) (*port.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	index := len(f.calls)
	f.calls = append(f.calls, req)

	if index < len(f.errs) && f.errs[index] != nil {
		return nil, f.errs[index]
	}
	if index >= len(f.responses) {
		return nil, errors.New("fake llm: no scripted response remaining")
	}

	reason := "stop"
	if index < len(f.finish) && f.finish[index] != "" {
		reason = f.finish[index]
	}
	return &port.CompletionResponse{
		Content:      f.responses[index],
		Model:        "fake-model",
		FinishReason: reason,
		Usage:        port.Usage{PromptTokens: 100, CompletionTokens: 50},
		Latency:      time.Millisecond,
	}, nil
}

func (f *fakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLLM) lastMessages() []port.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1].Messages
}

const testSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "items"],
  "properties": {
    "title": {"type": "string", "minLength": 5},
    "items": {"type": "array", "minItems": 2, "items": {"type": "string"}}
  }
}`

func ask(t *testing.T, llm *fakeLLM, repairs int) (*factory.Result, error) {
	t.Helper()
	reasoner := factory.NewReasoner(llm, nil)
	return reasoner.Ask(context.Background(), "system", "user",
		json.RawMessage(testSchema), "test", port.ClassReasoning, 0.2, repairs)
}

func TestReasonerAcceptsValidOutput(t *testing.T) {
	llm := &fakeLLM{responses: []string{`{"title":"A good title","items":["one","two"]}`}}

	result, err := ask(t, llm, 2)
	if err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}
	if result.Document["title"] != "A good title" {
		t.Fatalf("document not decoded: %+v", result.Document)
	}
	if llm.callCount() != 1 {
		t.Fatalf("expected exactly one call, got %d", llm.callCount())
	}
	if result.Usage.Total() != 150 {
		t.Fatalf("token accounting wrong: %+v", result.Usage)
	}
}

// The central guarantee of the agent runtime: invalid output is repaired by
// feeding the validator's own complaints back to the model.
func TestReasonerRepairsInvalidOutput(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		`{"title":"ok","items":[]}`,                      // too short, too few items
		`{"title":"A good title","items":["one","two"]}`, // corrected
	}}

	result, err := ask(t, llm, 2)
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if llm.callCount() != 2 {
		t.Fatalf("expected one repair call, got %d total calls", llm.callCount())
	}
	if len(result.Attempts) != 2 || !result.Attempts[1].Repaired {
		t.Fatalf("repair not recorded in attempts: %+v", result.Attempts)
	}
	// Cost must accumulate across attempts, or budgets would under-count.
	if result.Usage.Total() != 300 {
		t.Fatalf("expected tokens from both attempts, got %d", result.Usage.Total())
	}

	// The repair turn must actually tell the model what was wrong.
	messages := llm.lastMessages()
	if len(messages) < 4 {
		t.Fatalf("expected the failed answer and a correction in the conversation, got %d messages", len(messages))
	}
	correction := messages[len(messages)-1].Content
	for _, want := range []string{"title", "items", "corrected JSON"} {
		if !strings.Contains(correction, want) {
			t.Errorf("repair prompt is missing %q:\n%s", want, correction)
		}
	}
}

func TestReasonerGivesUpAfterRepairBudget(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		`{"title":"x"}`, `{"title":"y"}`, `{"title":"z"}`, `{"title":"w"}`,
	}}

	_, err := ask(t, llm, 2)
	if err == nil {
		t.Fatal("expected failure after exhausting repairs")
	}
	// One initial attempt plus two repairs; a fourth call would mean the bound
	// is not enforced, which is how a defect becomes an unbounded spend.
	if llm.callCount() != 3 {
		t.Fatalf("expected 3 calls (1 + 2 repairs), got %d", llm.callCount())
	}
	if !strings.Contains(err.Error(), "never satisfied") {
		t.Fatalf("unhelpful failure message: %v", err)
	}
}

func TestReasonerRecoversJSONFromProse(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		"Certainly! Here is the document:\n```json\n{\"title\":\"A good title\",\"items\":[\"one\",\"two\"]}\n```\nLet me know if you need changes.",
	}}

	result, err := ask(t, llm, 1)
	if err != nil {
		t.Fatalf("failed to recover JSON from a prose wrapper: %v", err)
	}
	if result.Document["title"] != "A good title" {
		t.Fatalf("wrong document: %+v", result.Document)
	}
	if llm.callCount() != 1 {
		t.Fatal("recovery should not have required a repair round trip")
	}
}

func TestReasonerHandlesTruncation(t *testing.T) {
	llm := &fakeLLM{
		responses: []string{`{"title":"A good title","items":["one"`, `{"title":"A good title","items":["one","two"]}`},
		finish:    []string{"length", "stop"},
	}

	result, err := ask(t, llm, 2)
	if err != nil {
		t.Fatalf("truncation was not recovered: %v", err)
	}
	if llm.callCount() != 2 {
		t.Fatalf("expected a retry after truncation, got %d calls", llm.callCount())
	}
	if result.Document["title"] != "A good title" {
		t.Fatalf("wrong document after recovery: %+v", result.Document)
	}

	// The retry must tell the model it was cut off, not merely repeat the ask.
	messages := llm.lastMessages()
	if !strings.Contains(messages[len(messages)-1].Content, "cut off") {
		t.Fatalf("retry prompt should explain the truncation:\n%s", messages[len(messages)-1].Content)
	}
}

// Transport failures are not repairable by rewording, so they must abort
// immediately rather than burning the repair budget.
func TestReasonerDoesNotRetryTransportErrors(t *testing.T) {
	llm := &fakeLLM{errs: []error{errors.New("connection refused")}}

	_, err := ask(t, llm, 3)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if llm.callCount() != 1 {
		t.Fatalf("transport errors must not be retried as repairs, got %d calls", llm.callCount())
	}
}

func TestReasonerConstrainsDecodingAndSeeds(t *testing.T) {
	llm := &fakeLLM{responses: []string{`{"title":"A good title","items":["one","two"]}`}}
	if _, err := ask(t, llm, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := llm.calls[0]
	if len(call.Schema) == 0 {
		t.Error("the schema must be sent so the server can constrain decoding")
	}
	if call.SchemaName != "test" {
		t.Errorf("schema name not propagated: %q", call.SchemaName)
	}
	// A fixed seed is what makes a run reproducible on providers that honour it.
	if call.Seed == nil {
		t.Error("a seed must be sent for reproducibility")
	}
	if call.MaxTokens <= 0 {
		t.Error("max tokens must be bounded")
	}
}

// Sizing output against the model's real window is what prevents the
// "request exceeds context size" failure observed against llama.cpp.
func TestReasonerSizesOutputToContextWindow(t *testing.T) {
	llm := &fakeLLM{
		window:    4096,
		responses: []string{`{"title":"A good title","items":["one","two"]}`},
	}
	if _, err := ask(t, llm, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := llm.calls[0].MaxTokens; got > 2048 {
		t.Fatalf("max tokens %d exceeds half of a 4096 window; the prompt plus reply will overflow", got)
	}

	reasoner := factory.NewReasoner(llm, nil)
	if window := reasoner.ContextWindow(context.Background()); window != 4096 {
		t.Fatalf("context window not discovered: %d", window)
	}
	budget := reasoner.PromptBudget(context.Background(), 1500)
	if budget <= 0 || budget >= 4096 {
		t.Fatalf("prompt budget %d must reserve room for the reply", budget)
	}
}

func TestPromptRespectsBudgetAndPriority(t *testing.T) {
	long := strings.Repeat("filler content. ", 500)

	prompt := factory.NewPrompt(200). // ~800 characters
						Add("Critical", "must survive truncation", 0).
						Add("Optional", long, 9).
						String()

	if !strings.Contains(prompt, "must survive truncation") {
		t.Fatal("the highest-priority section was dropped")
	}
	if len(prompt) > 900 {
		t.Fatalf("prompt exceeded its budget: %d characters", len(prompt))
	}
	if !strings.Contains(prompt, "## Critical") {
		t.Fatal("section headers are missing")
	}
}

func TestPromptSkipsEmptySections(t *testing.T) {
	prompt := factory.NewPrompt(1000).
		Add("Present", "content", 0).
		Add("Absent", "", 1).
		Add("Blank", "   \n  ", 2).
		String()

	if strings.Contains(prompt, "Absent") || strings.Contains(prompt, "Blank") {
		t.Fatalf("empty sections must not appear:\n%s", prompt)
	}
}

func TestTokenBudgetEnforcesLimit(t *testing.T) {
	budget := factory.NewTokenBudget(1000)

	if budget.Exhausted() {
		t.Fatal("a fresh budget must not be exhausted")
	}
	if !budget.Spend(400) {
		t.Fatal("spending within the limit should leave room")
	}
	if budget.Used() != 400 || budget.Remaining() != 600 {
		t.Fatalf("accounting wrong: used=%d remaining=%d", budget.Used(), budget.Remaining())
	}
	if budget.Spend(700) {
		t.Fatal("exceeding the limit must report exhaustion")
	}
	if !budget.Exhausted() {
		t.Fatal("budget should be exhausted after overspending")
	}

	// An unbounded budget is only for tests, but it must not report exhaustion.
	unbounded := factory.NewTokenBudget(0)
	unbounded.Spend(1 << 30)
	if unbounded.Exhausted() || unbounded.Remaining() != -1 {
		t.Fatal("a zero limit must mean unbounded")
	}

	// A nil budget must be safe: agents run without reasoning configured.
	var nilBudget *factory.TokenBudget
	if nilBudget.Exhausted() || !nilBudget.Spend(10) || nilBudget.Used() != 0 {
		t.Fatal("a nil budget must behave as unbounded and never panic")
	}
}

// --- agent behaviour with and without a model ----------------------------

const goodVisionJSON = `{
  "goal": "Give solar installers one place to track every lead from first call to signed contract, so no follow-up is lost.",
  "audience": [
    {"name": "Field sales representative", "need": "Log a site visit from a phone in under a minute"},
    {"name": "Operations manager", "need": "See which installations are scheduled and which are blocked on permits"}
  ],
  "differentiators": [
    "Permit tracking is a first-class stage, not a free-text note",
    "Quotes are generated from roof measurements captured during the site survey",
    "Installer scheduling is visible directly on the deal record"
  ],
  "success_metrics": [
    "Quote turnaround falls below 24 hours for 80 percent of qualified leads",
    "Fewer than 2 percent of deals stall for more than 30 days without an activity",
    "Installation schedule accuracy exceeds 90 percent against planned dates"
  ],
  "out_of_scope": [
    "Accounting and invoicing integrations",
    "Hardware inventory management"
  ]
}`

// The whole product must remain usable with no model installed. This is the
// local-first requirement, and it is verified rather than assumed.
func TestAgentsProduceCompleteOutputWithoutAModel(t *testing.T) {
	bb, root, artifacts := runAgentPipeline(t, "Build a CRM for a solar panel installation company")

	if bb.Reasoning.Enabled() {
		t.Fatal("this test must run with reasoning disabled")
	}
	if len(artifacts) < 10 {
		t.Fatalf("expected a full artifact set without a model, got %d", len(artifacts))
	}
	assertFileHasContent(t, root, "docs/product/VISION.md", "Product Vision")
	assertFileHasContent(t, root, "docs/product/PRD.md", "Acceptance criteria")
	assertFileHasContent(t, root, "docs/architecture/ARCHITECTURE.md", "Architecture Specification")
}

// With a model, its judgement must actually reach the artifact.
func TestCEOAgentUsesModelOutput(t *testing.T) {
	llm := &fakeLLM{responses: []string{goodVisionJSON}}
	bb, root := runPipelineWithModel(t, "Build a CRM for a solar panel installation company", llm)

	vision, ok := bb.Get(domainArtifactVision())
	if !ok {
		t.Fatal("no vision artifact produced")
	}
	if !strings.Contains(vision.Body, "Permit tracking is a first-class stage") {
		t.Fatalf("model differentiators did not reach the document:\n%s", vision.Body)
	}
	if !strings.Contains(vision.Body, "Quote turnaround falls below 24 hours") {
		t.Fatal("model success metrics did not reach the document")
	}
	if !strings.Contains(vision.Body, "Field sales representative") {
		t.Fatal("model audience did not reach the document")
	}
	// Attribution matters: a reader must know which parts were reasoned.
	if !strings.Contains(vision.Body, "authored by the reasoning model") {
		t.Fatal("model-authored content must be attributed")
	}
	assertFileHasContent(t, root, "docs/product/VISION.md", "Permit tracking")
}

// Schema-valid but degenerate output must be rejected in favour of the
// blueprint. Shipping repetitive filler that looks authored is worse than
// shipping an honest template.
func TestCriticRejectsDegenerateModelOutput(t *testing.T) {
	repetitive := `{
      "goal": "The system will track all customer interactions ensuring a seamless sales process.",
      "audience": [
        {"name": "Company", "need": "To manage their customer relationships and track sales progress"},
        {"name": "Customers", "need": "To manage their customer relationships and track sales progress"}
      ],
      "differentiators": [
        "The system will track all customer interactions including leads and deals ensuring a seamless sales process",
        "The system will track all customer interactions including leads and deals ensuring a seamless process",
        "The system will track all customer interactions including leads and deals ensuring seamless sales"
      ],
      "success_metrics": [
        "The system will track all customer interactions including leads and deals ensuring a seamless sales process",
        "The system will track all customer interactions including leads and deals ensuring seamless sales process",
        "The system will track all customer interactions including deals ensuring a seamless sales process"
      ],
      "out_of_scope": ["Accounting integrations", "Hardware inventory"]
    }`

	llm := &fakeLLM{responses: []string{repetitive}}
	bb, _ := runPipelineWithModel(t, "Build a CRM for a solar company", llm)

	vision, ok := bb.Get(domainArtifactVision())
	if !ok {
		t.Fatal("no vision artifact produced")
	}
	if strings.Contains(vision.Body, "authored by the reasoning model") {
		t.Fatal("degenerate model output was accepted; the critic did not reject it")
	}
	// The blueprint fallback must still yield a complete document.
	if !strings.Contains(vision.Body, "Success metrics") || len(vision.Body) < 500 {
		t.Fatalf("fallback document is incomplete:\n%s", vision.Body)
	}
}

// A model that fails entirely must degrade, never abort the build.
func TestBuildSurvivesTotalModelFailure(t *testing.T) {
	llm := &fakeLLM{errs: []error{
		errors.New("connection refused"), errors.New("connection refused"),
		errors.New("connection refused"), errors.New("connection refused"),
		errors.New("connection refused"), errors.New("connection refused"),
	}}

	bb, root := runPipelineWithModel(t, "Build a CRM system", llm)

	if _, ok := bb.Get(domainArtifactVision()); !ok {
		t.Fatal("the build must still produce a vision when the model is dead")
	}
	if _, ok := bb.Get(domainArtifactPRD()); !ok {
		t.Fatal("the build must still produce a PRD when the model is dead")
	}
	assertFileHasContent(t, root, "docs/product/PRD.md", "Acceptance criteria")
}

// An under-powered model may satisfy a schema by copying its input back. That
// is a distinct defect from repetition and must also be caught.
func TestCriticRejectsEchoedInput(t *testing.T) {
	// Distinct entries (so the repetition critic passes) that are all lifted
	// verbatim from the requirements the agent was given.
	echoed := `{
      "overview": "As a member I want to create view update and archive project records so that I can manage projects without leaving the product",
      "decisions": [
        {"title": "Projects", "choice": "As a member I want to create view update and archive project records", "rationale": "so that I can manage projects without leaving the product and track them", "tradeoff": "none identified for this approach at present"},
        {"title": "Issues", "choice": "As a member I want to create view update and archive issue records", "rationale": "so that I can manage issues without leaving the product and track them", "tradeoff": "none identified for this approach at present"},
        {"title": "Sprints", "choice": "As a member I want to create view update and archive sprint records", "rationale": "so that I can manage sprints without leaving the product and track them", "tradeoff": "none identified for this approach at present"}
      ],
      "risks": [
        {"risk": "Members cannot manage project records without leaving", "mitigation": "Allow members to create view update and archive records"},
        {"risk": "Members cannot manage issue records without leaving", "mitigation": "Allow members to create view update and archive issues"}
      ]
    }`

	source := `As a member, I want to create, view, update and archive project records so that I can
	manage projects without leaving the product. As a member, I want to create, view, update and
	archive issue records so that I can manage issues without leaving the product. As a member, I
	want to create, view, update and archive sprint records so that I can manage sprints without
	leaving the product and track them across the whole team and organisation.`

	if err := factory.CritiqueEchoForTest([]string{
		"As a member I want to create view update and archive project records so that I can manage projects without leaving the product",
		"As a member I want to create view update and archive issue records so that I can manage issues without leaving the product",
		"As a member I want to create view update and archive sprint records so that I can manage sprints without leaving the product",
	}, source); err == nil {
		t.Fatal("echoed content was accepted; the critic did not detect it")
	}

	// Genuinely new content must pass.
	if err := factory.CritiqueEchoForTest([]string{
		"Store issue ordering as a fractional rank so a drag updates one row instead of renumbering the column",
		"Keep sprint capacity in story points on the sprint record to avoid recomputing it on every board render",
		"Partition the activity log by month because it grows faster than every other table combined",
	}, source); err != nil {
		t.Fatalf("original content was wrongly rejected: %v", err)
	}
	_ = echoed
}
