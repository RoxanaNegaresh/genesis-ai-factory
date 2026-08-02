package factory_test

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
	"github.com/genesis-ai-factory/control-plane/internal/infra/sandbox"
)

// benchmarkBlackboard builds a pipeline context for one benchmark case.
func benchmarkBlackboard(prompt, workspace string) (*factory.Blackboard, factory.Toolbelt) {
	name := domain.TitleFromPrompt(prompt)
	project := &domain.Project{
		ID: domain.NewID(), OwnerID: domain.NewID(),
		Name: name, Slug: domain.Slugify(name),
		Prompt: prompt, WorkspacePath: workspace,
		Settings: domain.DefaultProjectSettings(),
	}
	run, _ := domain.NewRun(project.ID, project.OwnerID, domain.RunBuild, nil, 0, time.Now())

	bb := factory.NewBlackboard(project, run)
	bb.Classification = factory.Classify(prompt)
	bb.Blueprint = factory.BlueprintFor(bb.Classification.Category)

	return bb, factory.NewWorkspaceToolbelt(workspace, domain.RoleSystem, nil, nil)
}

// TestBenchmarkBaseline establishes the quality floor with no model available.
//
// This is the number every future change is measured against. It must be a
// *test*, not a script someone remembers to run: a regression in generation
// quality should fail CI exactly like a regression in behaviour.
func TestBenchmarkBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark in short mode")
	}

	runner := factory.NewBenchmarkRunner()
	if !runner.Compile {
		t.Skip("go toolchain not available")
	}
	runner.Timeout = 3 * time.Minute
	// v0.4: measure execution, not just compilation. Every project is started
	// and probed over HTTP inside the sandbox.
	if runtime.GOOS == "linux" {
		runner.Runner = factory.NewRunner(sandbox.New(sandbox.DefaultConfig(), nil))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	report := runner.Run(ctx, factory.DefaultBenchmark(), benchmarkBlackboard)
	t.Logf("\n%s", report.Markdown())

	if len(report.Cases) != 5 {
		t.Fatalf("expected 5 benchmark cases, got %d", len(report.Cases))
	}

	// Every case must generate and compile. Anything less means the factory
	// ships broken repositories for a category, which is the failure this
	// harness exists to catch.
	for _, c := range report.Cases {
		if !c.Generated {
			t.Errorf("%s: produced no artifacts", c.Name)
		}
		if !c.Compiles {
			t.Errorf("%s: generated project does not compile: %v", c.Name, c.Errors)
		}
		if !c.TestsPass {
			t.Errorf("%s: generated tests fail: %v", c.Name, c.Errors)
		}
		// The v0.4 bar. A project that compiles but cannot serve a request is
		// not a working product.
		if runner.Runner != nil {
			if !c.Starts {
				t.Errorf("%s: the generated service does not start: %v", c.Name, c.Errors)
			}
			if !c.Responds {
				t.Errorf("%s: the generated service does not answer requests: %v", c.Name, c.Errors)
			}
		}
		if !c.HasSchema || !c.HasAPIContract || !c.HasDeployment {
			t.Errorf("%s: incomplete deliverables (schema=%v api=%v deploy=%v)",
				c.Name, c.HasSchema, c.HasAPIContract, c.HasDeployment)
		}
	}

	// The floor. Without a model the classifier and blueprints alone should
	// score highly on everything except the custom-domain case, which cannot
	// be modelled without synthesis.
	const floor = 0.85
	if report.Score < floor {
		t.Errorf("benchmark score %.1f%% is below the %.0f%% baseline", report.Score*100, floor*100)
	}

	// Persist the report so a human can diff runs. Failing to write it must not
	// fail the test; it is diagnostic output, not an assertion.
	if out := os.Getenv("GENESIS_BENCHMARK_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report.JSON()), 0o644); err != nil {
			t.Logf("could not write the benchmark report: %v", err)
		}
	}
}

func TestBenchmarkScoringIsHonest(t *testing.T) {
	// A case that generates nothing must not earn a passing score merely for
	// having been attempted.
	empty := factory.CaseResult{Name: "nothing"}
	factory.ComputeScoreForTest(&empty)
	if empty.Score > 0.15 {
		t.Fatalf("a case that produced nothing scored %.2f", empty.Score)
	}

	// Compilation must dominate documentation: a complete specification
	// attached to code that does not build is worth less than the reverse.
	docsOnly := factory.CaseResult{
		Generated: true, HasAcceptanceCriteria: true, HasSchema: true,
		HasAPIContract: true, HasDeployment: true,
	}
	compilingOnly := factory.CaseResult{Generated: true, Compiles: true, TestsPass: true, Starts: true, Responds: true}
	factory.ComputeScoreForTest(&docsOnly)
	factory.ComputeScoreForTest(&compilingOnly)

	if compilingOnly.Score <= docsOnly.Score {
		t.Fatalf("documentation (%.2f) outweighs working code (%.2f); the weights are wrong",
			docsOnly.Score, compilingOnly.Score)
	}

	// Running must be worth more than compiling, or the v0.4 bar is decorative.
	compilesOnly := factory.CaseResult{Generated: true, Compiles: true, TestsPass: true}
	runsToo := factory.CaseResult{Generated: true, Compiles: true, TestsPass: true, Starts: true, Responds: true}
	factory.ComputeScoreForTest(&compilesOnly)
	factory.ComputeScoreForTest(&runsToo)
	if runsToo.Score <= compilesOnly.Score {
		t.Fatalf("running (%.2f) is not scored above compiling (%.2f)", runsToo.Score, compilesOnly.Score)
	}

	// A perfect case must score 1.0, or the scale is not anchored.
	perfect := factory.CaseResult{
		Generated: true, Compiles: true, TestsPass: true, Starts: true, Responds: true,
		CategoryMatch: true, EntitiesFound: 3, EntitiesWanted: 3,
		HasAcceptanceCriteria: true, HasSchema: true, HasAPIContract: true, HasDeployment: true,
	}
	factory.ComputeScoreForTest(&perfect)
	if perfect.Score < 0.999 {
		t.Fatalf("a perfect case scored %.3f, not 1.0", perfect.Score)
	}
}

func TestBenchmarkReportRendersUsefully(t *testing.T) {
	report := factory.Report{
		Score: 0.87,
		Cases: []factory.CaseResult{
			{Name: "crm", Category: "crm", Generated: true, Compiles: true, TestsPass: true,
				EntitiesFound: 3, EntitiesWanted: 3, HasSchema: true, Score: 0.9},
			{Name: "broken", Category: "custom", Generated: true, Score: 0.15,
				Errors: []string{"compilation failed: undefined: Foo"}},
		},
	}

	markdown := report.Markdown()
	for _, want := range []string{"Generation Benchmark", "87.0%", "crm", "broken", "Failures", "undefined: Foo"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("report is missing %q:\n%s", want, markdown)
		}
	}

	// The JSON form must be machine-comparable between runs.
	if !strings.Contains(report.JSON(), `"score"`) {
		t.Error("JSON report is missing the score field")
	}
}

func TestDefaultBenchmarkCoversEveryCategory(t *testing.T) {
	cases := factory.DefaultBenchmark()
	if len(cases) < 5 {
		t.Fatalf("the benchmark must cover every built-in category, got %d cases", len(cases))
	}

	covered := map[string]bool{}
	for _, c := range cases {
		if c.Prompt == "" {
			t.Errorf("case %q has no prompt", c.Name)
		}
		covered[c.ExpectCategory] = true
	}
	for _, category := range []string{"crm", "pm", "erp", "marketplace"} {
		if !covered[category] {
			t.Errorf("no benchmark case covers the %s category", category)
		}
	}
	// A prompt outside every built-in category is what exercises synthesis.
	if !covered["custom"] {
		t.Error("the benchmark must include a domain no blueprint covers")
	}
}
