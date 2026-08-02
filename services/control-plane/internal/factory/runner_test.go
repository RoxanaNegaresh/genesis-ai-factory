package factory_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
	"github.com/genesis-ai-factory/control-plane/internal/infra/sandbox"
)

func newRunner(t *testing.T) *factory.Runner {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox is only implemented on Linux")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	return factory.NewRunner(sandbox.New(sandbox.DefaultConfig(), nil))
}

// generateProject runs the full agent pipeline into a temporary workspace.
func generateProject(t *testing.T, prompt string) string {
	t.Helper()
	root := t.TempDir()

	name := domain.TitleFromPrompt(prompt)
	project := &domain.Project{
		ID: domain.NewID(), OwnerID: domain.NewID(),
		Name: name, Slug: domain.Slugify(name),
		Prompt: prompt, WorkspacePath: root,
		Settings: domain.DefaultProjectSettings(),
	}
	run, _ := domain.NewRun(project.ID, project.OwnerID, domain.RunBuild, nil, 0, time.Now())

	bb := factory.NewBlackboard(project, run)
	bb.Classification = factory.Classify(prompt)
	bb.Blueprint = factory.BlueprintFor(bb.Classification.Category)

	registry := factory.NewRegistry()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleSystem, nil, nil)

	for _, role := range registry.Roles() {
		agent, ok := registry.Get(role)
		if !ok {
			continue
		}
		if _, err := agent.Execute(context.Background(), bb, tb.For(role)); err != nil {
			t.Fatalf("agent %s failed: %v", role, err)
		}
	}
	return root
}

// This is the v0.4 exit criterion: a generated project that starts and answers
// a real HTTP request, not merely one that compiles.
func TestGeneratedProjectRunsAndRespondsToRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping execution test in short mode")
	}
	runner := newRunner(t)

	workspace := generateProject(t, "Build a CRM for a sales team with leads and deals")
	tb := factory.NewWorkspaceToolbelt(workspace, domain.RoleQA, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	report, err := runner.Verify(ctx, tb, workspace, factory.GoToolchain())
	if err != nil {
		t.Fatalf("verification failed to run: %v", err)
	}

	for _, stage := range report.Stages {
		status := "ok"
		switch {
		case stage.Skipped:
			status = "skipped"
		case !stage.OK:
			status = "FAILED"
		}
		t.Logf("%-8s %-8s %-8s %s", stage.Stage, status,
			stage.Duration.Round(time.Millisecond), stage.Detail)
		if !stage.OK && !stage.Skipped && stage.Output != "" {
			t.Logf("  output:\n%s", stage.Output)
		}
	}
	t.Logf("summary: %s", report.Summary())

	if !report.Compiles {
		t.Fatal("the generated project does not compile")
	}
	if !report.TestsPass {
		t.Error("the generated project's own tests do not pass")
	}
	if !report.Starts {
		t.Fatal("the generated service does not start")
	}
	if !report.Responds {
		t.Fatal("the generated service does not answer an HTTP request")
	}
	if report.StatusCode != 200 {
		t.Errorf("health probe returned %d, expected 200", report.StatusCode)
	}
	if !report.Verified() {
		t.Fatal("the report does not consider the project verified")
	}
}

// A project that does not compile must fail at the build stage and must not
// attempt to start, which would produce a confusing second failure.
func TestVerificationStopsAtTheFirstFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping execution test in short mode")
	}
	runner := newRunner(t)

	workspace := generateProject(t, "Build a CRM system")

	// Break the code deliberately.
	broken := workspace + "/api/internal/domain/broken.go"
	if err := os.WriteFile(broken, []byte("package domain\n\nfunc Broken() { this is not go }\n"), 0o640); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tb := factory.NewWorkspaceToolbelt(workspace, domain.RoleQA, nil, nil)
	report, err := runner.Verify(ctx, tb, workspace, factory.GoToolchain())
	if err != nil {
		t.Fatalf("verification failed to run: %v", err)
	}

	if report.Compiles {
		t.Fatal("a project with a syntax error was reported as compiling")
	}
	if report.Starts || report.Responds {
		t.Fatal("verification attempted to start a project that does not compile")
	}

	// The failure must be diagnosable from the report alone.
	var buildStage *factory.StageResult
	for i := range report.Stages {
		if report.Stages[i].Stage == factory.StageBuild {
			buildStage = &report.Stages[i]
		}
	}
	if buildStage == nil {
		t.Fatal("no build stage was recorded")
	}
	if !strings.Contains(buildStage.Output, "broken.go") {
		t.Errorf("the build output does not identify the failing file:\n%s", buildStage.Output)
	}
	if report.Summary() == "" {
		t.Error("the report has no summary")
	}
}

func TestVerificationRejectsMissingProject(t *testing.T) {
	runner := newRunner(t)
	workspace := t.TempDir()
	tb := factory.NewWorkspaceToolbelt(workspace, domain.RoleQA, nil, nil)

	_, err := runner.Verify(context.Background(), tb, workspace, factory.GoToolchain())
	if err == nil {
		t.Fatal("verification of an empty workspace was accepted")
	}
}

func TestRunnerReportsUnavailableWithoutSandbox(t *testing.T) {
	runner := factory.NewRunner(nil)
	if runner.Available() {
		t.Fatal("a runner with no sandbox must not report itself available")
	}
	_, err := runner.Verify(context.Background(),
		factory.NewWorkspaceToolbelt(t.TempDir(), domain.RoleQA, nil, nil),
		t.TempDir(), factory.GoToolchain())
	if err == nil {
		t.Fatal("verification without a sandbox was accepted")
	}
}

func TestToolchainGrantsNetworkOnlyForDependencies(t *testing.T) {
	chain := factory.GoToolchain()

	// Dependency resolution genuinely needs the module proxy.
	if chain.Install == nil || chain.Install.Network != "host" {
		t.Error("dependency resolution should be permitted network access")
	}
	// Nothing else should have it. Generated code compiling or running with
	// egress is the failure mode the sandbox exists to prevent.
	if chain.Build != nil && chain.Build.Network != "none" {
		t.Error("compilation must not have network access")
	}
	if chain.Test != nil && chain.Test.Network != "none" {
		t.Error("tests must not have network access")
	}
	for _, step := range []*factory.Step{chain.Build, chain.Test, chain.Serve} {
		if step != nil && step.Timeout <= 0 {
			t.Error("every step must be time-bounded")
		}
	}
}

func TestVerificationSummaryReflectsProgress(t *testing.T) {
	cases := []struct {
		report factory.VerificationReport
		want   string
	}{
		{factory.VerificationReport{}, "does not compile"},
		{factory.VerificationReport{Compiles: true}, "tests failed"},
		{factory.VerificationReport{Compiles: true, TestsPass: true}, "did not start"},
		{factory.VerificationReport{Compiles: true, TestsPass: true, Starts: true}, "did not answer"},
		{factory.VerificationReport{Compiles: true, TestsPass: true, Starts: true, Responds: true}, "answers requests"},
	}
	for _, tc := range cases {
		if !strings.Contains(tc.report.Summary(), tc.want) {
			t.Errorf("summary %q does not mention %q", tc.report.Summary(), tc.want)
		}
	}
}

// The diagnostic excerpt must retain the head of the output, not only the tail.
//
// A Go toolchain prints the real error first and, if it crashed, a very long
// goroutine dump afterwards. Keeping only the tail discarded the sentence that
// explained the failure — every build error read as an unintelligible stack
// fragment, and the benchmark reported 35% while blaming the generated code
// for what was an address-space limit in the sandbox.
func TestTrimOutputKeepsTheExplanation(t *testing.T) {
	const explanation = "# github.com/jackc/pgx/v5/pgtype\nruntime: out of memory"
	noise := strings.Repeat("goroutine 42 [running]:\n", 500)
	excerpt := factory.TrimOutputForTest(explanation+"\n"+noise, 600)

	if !strings.Contains(excerpt, "out of memory") {
		t.Errorf("the excerpt dropped the explanation:\n%s", excerpt)
	}
	if len(excerpt) > 700 {
		t.Errorf("the excerpt is %d bytes; the limit was 600", len(excerpt))
	}
}

// Compiling a generated project must fit inside whatever ceiling the toolchain
// declares. The Go compiler reserves a large arena, so a ceiling sized for a
// small program kills it part-way through a dependency.
func TestGoToolchainRaisesTheBuildMemoryCeiling(t *testing.T) {
	chain := factory.GoToolchain()

	if chain.StepMemoryLimitBytes < 2<<30 {
		t.Errorf("the build ceiling is %d bytes; the Go compiler needs at least 2 GiB "+
			"of address space to compile pgx", chain.StepMemoryLimitBytes)
	}
	if chain.StepEnv["GOFLAGS"] == "" {
		t.Error("build parallelism is unbounded; concurrent compile actions multiply peak usage")
	}
}
