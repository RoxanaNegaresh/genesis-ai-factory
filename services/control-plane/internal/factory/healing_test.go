package factory_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
	"github.com/genesis-ai-factory/control-plane/internal/infra/sandbox"
)

// --- diagnosis ------------------------------------------------------------

func TestDiagnoseClassifiesCompilationFailure(t *testing.T) {
	report := &factory.VerificationReport{
		Stages: []factory.StageResult{
			{Stage: factory.StageInstall, OK: true},
			{Stage: factory.StageBuild, OK: false, Detail: "exit code 1", Output: `# github.com/genesis/app/internal/domain
internal/domain/deal.go:26:16: invalid operation: m.Email != "" (mismatched types *string and untyped string)
internal/domain/lead.go:31:9: undefined: strconv`},
		},
	}

	diagnosis := factory.Diagnose(report)
	if diagnosis == nil {
		t.Fatal("a failing build was not diagnosed")
	}
	if diagnosis.Category != "compile" {
		t.Errorf("wrong category: %q", diagnosis.Category)
	}
	if !diagnosis.Repairable {
		t.Error("a compilation error should be repairable")
	}

	// The files carrying the error are what the model must be shown.
	if len(diagnosis.Files) != 2 {
		t.Fatalf("expected 2 files, got %v", diagnosis.Files)
	}
	if !containsStr(diagnosis.Files, "internal/domain/deal.go") {
		t.Errorf("the failing file was not extracted: %v", diagnosis.Files)
	}
	if len(diagnosis.Details) != 2 {
		t.Errorf("expected 2 error details, got %v", diagnosis.Details)
	}
	if diagnosis.Signature == "" {
		t.Error("no signature was produced")
	}
}

// A signature must match the same error across projects, or lessons never
// transfer and memory is worthless.
func TestDiagnosisSignatureIsStableAcrossProjects(t *testing.T) {
	build := func(module, file string, line int) *factory.VerificationReport {
		return &factory.VerificationReport{Stages: []factory.StageResult{{
			Stage: factory.StageBuild, OK: false,
			Output: "# " + module + "\n" + file + ":" + itoa(line) +
				`:16: invalid operation: m.Email != "" (mismatched types *string and untyped string)`,
		}}}
	}

	first := factory.Diagnose(build("github.com/genesis/crm", "internal/domain/contact.go", 26))
	second := factory.Diagnose(build("github.com/genesis/erp", "internal/domain/supplier.go", 41))

	if first.Signature != second.Signature {
		t.Fatalf("the same error produced different signatures:\n  %s\n  %s",
			first.Signature, second.Signature)
	}

	// A genuinely different error must not collide.
	other := factory.Diagnose(&factory.VerificationReport{Stages: []factory.StageResult{{
		Stage: factory.StageBuild, OK: false,
		Output: "internal/domain/x.go:9:2: undefined: fmt",
	}}})
	if other.Signature == first.Signature {
		t.Fatal("different errors collapsed to the same signature")
	}
}

// Dependency failures are environmental; asking a model to patch source for
// them wastes an expensive call on a problem no edit can fix.
func TestDiagnoseMarksDependencyFailuresUnrepairable(t *testing.T) {
	diagnosis := factory.Diagnose(&factory.VerificationReport{
		Stages: []factory.StageResult{{
			Stage: factory.StageInstall, OK: false,
			Detail: "dial tcp: lookup proxy.golang.org: no such host",
		}},
	})
	if diagnosis == nil {
		t.Fatal("a failing install was not diagnosed")
	}
	if diagnosis.Repairable {
		t.Fatal("a dependency failure was marked repairable")
	}
}

func TestDiagnoseClassifiesEachStage(t *testing.T) {
	cases := map[factory.VerificationStage]string{
		factory.StageTest:  "test",
		factory.StageServe: "startup",
		factory.StageProbe: "health",
	}
	for stage, want := range cases {
		diagnosis := factory.Diagnose(&factory.VerificationReport{
			Stages: []factory.StageResult{
				{Stage: factory.StageBuild, OK: true},
				{Stage: stage, OK: false, Detail: "something went wrong", Output: "detail line"},
			},
		})
		if diagnosis == nil || diagnosis.Category != want {
			t.Errorf("stage %s produced %v, expected category %q", stage, diagnosis, want)
		}
	}
}

func TestDiagnoseReturnsNothingForSuccess(t *testing.T) {
	passing := &factory.VerificationReport{
		Compiles: true, TestsPass: true, Starts: true, Responds: true,
		Stages: []factory.StageResult{{Stage: factory.StageBuild, OK: true}},
	}
	if factory.Diagnose(passing) != nil {
		t.Fatal("a passing verification was diagnosed as broken")
	}
	if factory.Diagnose(nil) != nil {
		t.Fatal("a nil report was diagnosed")
	}
}

// --- the healing loop -----------------------------------------------------

func newHealingProject(t *testing.T) (string, *factory.Blackboard, *factory.WorkspaceToolbelt) {
	t.Helper()

	workspace := generateProject(t, "Build a CRM for a sales team")

	project := &domain.Project{
		ID: domain.NewID(), OwnerID: domain.NewID(),
		Name: "CRM", Slug: "crm", Prompt: "Build a CRM",
		WorkspacePath: workspace, Settings: domain.DefaultProjectSettings(),
	}
	run, _ := domain.NewRun(project.ID, project.OwnerID, domain.RunBuild, nil, 500000, time.Now())

	bb := factory.NewBlackboard(project, run)
	bb.Classification = factory.Classify(project.Prompt)
	bb.Blueprint = factory.BlueprintFor(bb.Classification.Category)

	return workspace, bb, factory.NewWorkspaceToolbelt(workspace, domain.RoleQA, nil, nil)
}

// The v0.6 exit criterion: a build that fails verification repairs itself and
// then passes.
func TestHealerRepairsABrokenProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping execution test in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox is only implemented on Linux")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	workspace, bb, tb := newHealingProject(t)
	runner := factory.NewRunner(sandbox.New(sandbox.DefaultConfig(), nil))

	// Break the project in a way a model can plausibly fix: a stray token in an
	// otherwise valid file.
	target := filepath.Join(workspace, "api", "internal", "domain", "deal.go")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Skipf("the generated project has no deal.go: %v", err)
	}
	broken := strings.Replace(string(original),
		"func (m *Deal) Archived() bool { return m.DeletedAt != nil }",
		"func (m *Deal) Archived() bool { return m.DeletedAt !== nil }", 1)
	if broken == string(original) {
		t.Skip("could not find the expected anchor to break")
	}
	if err := os.WriteFile(target, []byte(broken), 0o640); err != nil {
		t.Fatalf("break the project: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Confirm it really is broken before claiming a repair.
	initial, err := runner.Verify(ctx, tb, workspace, factory.GoToolchain())
	if err != nil {
		t.Fatalf("initial verification: %v", err)
	}
	if initial.Compiles {
		t.Fatal("the deliberately broken project still compiles")
	}

	// A scripted model returning the correct minimal repair. Using a real model
	// here would make the test non-deterministic; what is under test is the
	// loop — diagnose, patch, re-verify, accept — not the model's cleverness.
	repair := map[string]any{
		"diagnosis": "an invalid operator was used in the Archived method",
		"edits": []map[string]string{{
			"path":    "internal/domain/deal.go",
			"find":    "return m.DeletedAt !== nil",
			"replace": "return m.DeletedAt != nil",
			"why":     "Go has no !== operator",
		}},
	}
	encoded, _ := json.Marshal(repair)
	client := &fakeLLM{responses: []string{string(encoded)}}
	bb.Reasoning = factory.NewReasoningForTest(client, nil, 500000)

	healer := factory.NewHealer(runner, bb.Reasoning, 3)
	if !healer.Available() {
		t.Fatal("the healer reports itself unavailable with a runner and a model")
	}

	report := healer.Heal(ctx, tb, bb, initial)

	t.Logf("healing: %s", report.Summary())
	for _, attempt := range report.Attempts {
		t.Logf("  attempt %d: patched=%v improved=%v reverted=%v %s",
			attempt.Number, attempt.Patched, attempt.Improved, attempt.Reverted, attempt.Error)
	}

	if !report.Healed {
		t.Fatalf("the project was not repaired: %s", report.Final)
	}
	if len(report.Attempts) != 1 {
		t.Errorf("expected repair on the first attempt, took %d", len(report.Attempts))
	}

	// The fix must actually be in the file, not merely reported.
	repaired, _ := os.ReadFile(target)
	if strings.Contains(string(repaired), "!==") {
		t.Fatal("the invalid operator is still present")
	}

	// And the project must genuinely verify now.
	final, err := runner.Verify(ctx, tb, workspace, factory.GoToolchain())
	if err != nil || !final.Verified() {
		t.Fatalf("the repaired project does not verify: %v", final.Summary())
	}
}

// Monotonic progress: a repair that makes things worse must be undone, or the
// loop walks the project steadily downhill.
func TestHealerRevertsUnhelpfulRepairs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping execution test in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox is only implemented on Linux")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	workspace, bb, tb := newHealingProject(t)
	runner := factory.NewRunner(sandbox.New(sandbox.DefaultConfig(), nil))

	target := filepath.Join(workspace, "api", "internal", "domain", "deal.go")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Skipf("no deal.go: %v", err)
	}
	broken := strings.Replace(string(original),
		"func (m *Deal) Archived() bool { return m.DeletedAt != nil }",
		"func (m *Deal) Archived() bool { return m.DeletedAt !== nil }", 1)
	if broken == string(original) {
		t.Skip("could not find the expected anchor")
	}
	_ = os.WriteFile(target, []byte(broken), 0o640)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	initial, err := runner.Verify(ctx, tb, workspace, factory.GoToolchain())
	if err != nil {
		t.Fatalf("initial verification: %v", err)
	}

	// A model that "fixes" the error by introducing a different one.
	worse := map[string]any{
		"diagnosis": "replacing the operator",
		"edits": []map[string]string{{
			"path":    "internal/domain/deal.go",
			"find":    "return m.DeletedAt !== nil",
			"replace": "return m.DeletedAt !== nil && alsoUndefined()",
			"why":     "makes it worse",
		}},
	}
	encoded, _ := json.Marshal(worse)
	client := &fakeLLM{responses: []string{string(encoded), string(encoded), string(encoded)}}
	bb.Reasoning = factory.NewReasoningForTest(client, nil, 500000)

	report := factory.NewHealer(runner, bb.Reasoning, 2).Heal(ctx, tb, bb, initial)

	if report.Healed {
		t.Fatal("a repair that made things worse was reported as a success")
	}

	// The unhelpful change must have been rolled back to the state healing
	// started from, not left compounding.
	after, _ := os.ReadFile(target)
	if strings.Contains(string(after), "alsoUndefined") {
		t.Fatalf("an unhelpful repair was left in place:\n%s", after)
	}

	reverted := false
	for _, attempt := range report.Attempts {
		if attempt.Reverted {
			reverted = true
		}
	}
	if !reverted {
		t.Errorf("no attempt was recorded as reverted: %+v", report.Attempts)
	}
}

// Bounded: the loop must stop, or a defect becomes an unbounded spend.
func TestHealerRespectsAttemptBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping execution test in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox is only implemented on Linux")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	workspace, bb, tb := newHealingProject(t)
	runner := factory.NewRunner(sandbox.New(sandbox.DefaultConfig(), nil))

	target := filepath.Join(workspace, "api", "internal", "domain", "deal.go")
	original, _ := os.ReadFile(target)
	broken := strings.Replace(string(original),
		"func (m *Deal) Archived() bool { return m.DeletedAt != nil }",
		"func (m *Deal) Archived() bool { return m.DeletedAt !== nil }", 1)
	if broken == string(original) {
		t.Skip("could not find the expected anchor")
	}
	_ = os.WriteFile(target, []byte(broken), 0o640)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	initial, _ := runner.Verify(ctx, tb, workspace, factory.GoToolchain())

	// A model whose patches never apply: the anchor does not exist.
	useless := map[string]any{
		"diagnosis": "guessing",
		"edits": []map[string]string{{
			"path": "internal/domain/deal.go",
			"find": "this text is definitely not in the file at all", "replace": "x",
		}},
	}
	encoded, _ := json.Marshal(useless)
	client := &fakeLLM{responses: []string{
		string(encoded), string(encoded), string(encoded), string(encoded), string(encoded),
	}}
	bb.Reasoning = factory.NewReasoningForTest(client, nil, 500000)

	const budget = 2
	report := factory.NewHealer(runner, bb.Reasoning, budget).Heal(ctx, tb, bb, initial)

	if report.Healed {
		t.Fatal("a project was reported healed by patches that never applied")
	}
	if len(report.Attempts) > budget {
		t.Fatalf("the attempt budget was exceeded: %d attempts for a budget of %d",
			len(report.Attempts), budget)
	}
	if !strings.Contains(report.Summary(), "could not repair") {
		t.Errorf("the summary does not report the failure: %q", report.Summary())
	}
}

func TestHealerIsUnavailableWithoutDependencies(t *testing.T) {
	if factory.NewHealer(nil, nil, 3).Available() {
		t.Error("a healer with no runner or model reported itself available")
	}
	if factory.NewHealer(factory.NewRunner(nil), nil, 3).Available() {
		t.Error("a healer with no sandbox reported itself available")
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
