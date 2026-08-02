package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The benchmark harness.
//
// Up to v0.2 the honest answer to "did that change improve output quality?" was
// "it looks better to me". That is not a basis for engineering decisions about
// prompts, schemas or models. This harness turns quality into a number by
// asking the only questions that cannot be argued with:
//
//	does it generate?  does it compile?  do its tests pass?  is it complete?
//
// Scoring deliberately excludes anything subjective. Whether a PRD is
// "well-written" is a judgement; whether it contains acceptance criteria for
// every story is a fact.

// BenchmarkCase is one prompt to evaluate.
type BenchmarkCase struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// ExpectCategory, when set, asserts the classifier's answer.
	ExpectCategory string `json:"expect_category,omitempty"`
	// ExpectEntities are entity names the generated model must contain.
	ExpectEntities []string `json:"expect_entities,omitempty"`
}

// DefaultBenchmark is the standing evaluation set: one prompt per built-in
// category plus a domain deliberately outside all of them, which is the case
// blueprint synthesis exists to handle.
func DefaultBenchmark() []BenchmarkCase {
	return []BenchmarkCase{
		{
			Name:           "crm",
			Prompt:         "Build a CRM for a solar panel installation company with leads, deals and a sales pipeline",
			ExpectCategory: "crm",
			ExpectEntities: []string{"Lead", "Deal", "Contact"},
		},
		{
			Name:           "project-management",
			Prompt:         "Build a Jira competitor with kanban boards, sprints and issue tracking",
			ExpectCategory: "pm",
			ExpectEntities: []string{"Issue", "Sprint", "Board"},
		},
		{
			Name:           "erp",
			Prompt:         "Create an ERP system for a manufacturing company with inventory and purchase orders",
			ExpectCategory: "erp",
			ExpectEntities: []string{"Product", "PurchaseOrder", "StockLevel"},
		},
		{
			Name:           "marketplace",
			Prompt:         "Build an Airbnb-like marketplace with listings, bookings and payments",
			ExpectCategory: "marketplace",
			ExpectEntities: []string{"Listing", "Order", "Payment"},
		},
		{
			Name:           "custom-domain",
			Prompt:         "Build a management system for a veterinary clinic tracking animals, owners and appointments",
			ExpectCategory: "custom",
		},
	}
}

// CaseResult records what happened for one prompt.
type CaseResult struct {
	Name     string        `json:"name"`
	Prompt   string        `json:"prompt"`
	Category string        `json:"category"`
	Duration time.Duration `json:"duration_ms"`

	Generated bool `json:"generated"`
	Compiles  bool `json:"compiles"`
	TestsPass bool `json:"tests_pass"`
	// Starts and Responds are the v0.4 bar: does the generated service actually
	// run and answer a request, rather than merely compile.
	Starts         bool `json:"starts"`
	Responds       bool `json:"responds"`
	CategoryMatch  bool `json:"category_match"`
	EntitiesFound  int  `json:"entities_found"`
	EntitiesWanted int  `json:"entities_wanted"`

	Files     int `json:"files"`
	Artifacts int `json:"artifacts"`
	// Documentation completeness, checked structurally.
	HasAcceptanceCriteria bool `json:"has_acceptance_criteria"`
	HasSchema             bool `json:"has_schema"`
	HasAPIContract        bool `json:"has_api_contract"`
	HasDeployment         bool `json:"has_deployment"`

	Score  float64  `json:"score"`
	Errors []string `json:"errors,omitempty"`
}

// Report aggregates a benchmark run.
type Report struct {
	Cases     []CaseResult  `json:"cases"`
	Score     float64       `json:"score"`
	Duration  time.Duration `json:"duration_ms"`
	Reasoning bool          `json:"reasoning_enabled"`
}

// weights determine what "quality" means here. Compilation dominates on
// purpose: a beautiful specification attached to code that does not build is
// worth less than a plain one attached to code that does.
var weights = struct {
	Generated, Compiles, TestsPass, Runs, Category, Entities, Documentation float64
}{
	Generated: 0.10,
	Compiles:  0.22,
	TestsPass: 0.18,
	// Running and answering a request is weighted highest of any single
	// measure. Compiling proves the syntax; only serving a response proves the
	// product exists.
	Runs:          0.25,
	Category:      0.08,
	Entities:      0.07,
	Documentation: 0.10,
}

// score computes a case's weighted result in [0,1].
func (r *CaseResult) computeScore() {
	var total float64

	if r.Generated {
		total += weights.Generated
	}
	if r.Compiles {
		total += weights.Compiles
	}
	if r.TestsPass {
		total += weights.TestsPass
	}
	// Credit is split between starting and answering: a service that boots but
	// serves nothing has done most of the work and none of the point.
	if r.Starts {
		total += weights.Runs * 0.4
	}
	if r.Responds {
		total += weights.Runs * 0.6
	}
	if r.CategoryMatch {
		total += weights.Category
	}
	if r.EntitiesWanted > 0 {
		total += weights.Entities * float64(r.EntitiesFound) / float64(r.EntitiesWanted)
	} else {
		// No expectation declared: do not penalise the case for it.
		total += weights.Entities
	}

	docs := 0
	for _, present := range []bool{r.HasAcceptanceCriteria, r.HasSchema, r.HasAPIContract, r.HasDeployment} {
		if present {
			docs++
		}
	}
	total += weights.Documentation * float64(docs) / 4

	r.Score = total
}

// BenchmarkRunner evaluates prompts end to end.
type BenchmarkRunner struct {
	// Compile controls whether the generated project is built and tested. It is
	// the slowest and most valuable check.
	Compile bool
	// GoBinary is the toolchain used for compilation checks.
	GoBinary string
	// Timeout bounds a single compile-and-test cycle.
	Timeout time.Duration
	// Runner, when set, additionally starts each generated project and probes
	// it over HTTP. This is the v0.4 measure and supersedes the direct
	// toolchain invocation below.
	Runner *Runner
}

// NewBenchmarkRunner constructs a runner with sensible defaults.
func NewBenchmarkRunner() *BenchmarkRunner {
	binary, _ := exec.LookPath("go")
	return &BenchmarkRunner{Compile: binary != "", GoBinary: binary, Timeout: 4 * time.Minute}
}

// Run evaluates every case against a factory pipeline.
//
// The runner drives the agents directly rather than the HTTP API: a benchmark
// that depends on a running server measures the server too, and the question
// here is specifically about generation quality.
func (b *BenchmarkRunner) Run(
	ctx context.Context,
	cases []BenchmarkCase,
	newBlackboard func(prompt, workspace string) (*Blackboard, Toolbelt),
) Report {
	started := time.Now()
	report := Report{Cases: make([]CaseResult, 0, len(cases))}

	for _, testCase := range cases {
		report.Cases = append(report.Cases, b.runCase(ctx, testCase, newBlackboard))
	}

	for _, result := range report.Cases {
		report.Score += result.Score
	}
	if len(report.Cases) > 0 {
		report.Score /= float64(len(report.Cases))
	}
	report.Duration = time.Since(started)
	return report
}

func (b *BenchmarkRunner) runCase(
	ctx context.Context,
	testCase BenchmarkCase,
	newBlackboard func(prompt, workspace string) (*Blackboard, Toolbelt),
) CaseResult {
	started := time.Now()
	result := CaseResult{
		Name:           testCase.Name,
		Prompt:         testCase.Prompt,
		EntitiesWanted: len(testCase.ExpectEntities),
	}

	workspace, err := os.MkdirTemp("", "genesis-bench-*")
	if err != nil {
		result.Errors = append(result.Errors, "workspace: "+err.Error())
		return result
	}
	defer os.RemoveAll(workspace)

	bb, tb := newBlackboard(testCase.Prompt, workspace)
	result.Category = string(bb.Classification.Category)
	result.CategoryMatch = testCase.ExpectCategory == "" || result.Category == testCase.ExpectCategory

	registry := NewRegistry()
	for _, role := range registry.Roles() {
		agent, ok := registry.Get(role)
		if !ok {
			continue
		}
		artifacts, err := agent.Execute(ctx, bb, tb)
		if err != nil {
			result.Errors = append(result.Errors, string(role)+": "+err.Error())
			continue
		}
		result.Artifacts += len(artifacts)
	}
	result.Generated = result.Artifacts > 0

	files, _ := tb.ListFiles(ctx)
	result.Files = len(files)

	result.HasSchema = containsPath(files, "migrations/0001_init.up.sql")
	result.HasAPIContract = containsPath(files, "api/openapi.yaml")
	result.HasDeployment = containsPath(files, "docker-compose.yml")
	if prd, err := os.ReadFile(filepath.Join(workspace, "docs/product/PRD.md")); err == nil {
		result.HasAcceptanceCriteria = strings.Contains(string(prd), "Acceptance criteria")
	}

	for _, want := range testCase.ExpectEntities {
		for _, entity := range bb.Blueprint.Entities {
			if entity.Name == want {
				result.EntitiesFound++
				break
			}
		}
	}

	switch {
	case b.Runner != nil && b.Runner.Available() && result.Generated:
		// Full verification: build, test, start, probe.
		report, err := b.Runner.Verify(ctx, tb, workspace, GoToolchain())
		if err != nil {
			result.Errors = append(result.Errors, "verification: "+err.Error())
			break
		}
		result.Compiles = report.Compiles
		result.TestsPass = report.TestsPass
		result.Starts = report.Starts
		result.Responds = report.Responds
		for _, stage := range report.Stages {
			if !stage.OK && !stage.Skipped {
				result.Errors = append(result.Errors,
					string(stage.Stage)+" failed: "+firstLines(stage.Detail+" "+stage.Output, 2))
			}
		}
	case b.Compile && result.Generated:
		compiles, testsPass, errs := b.compileAndTest(ctx, filepath.Join(workspace, "api"))
		result.Compiles = compiles
		result.TestsPass = testsPass
		result.Errors = append(result.Errors, errs...)
	}

	result.Duration = time.Since(started)
	result.computeScore()
	return result
}

// compileAndTest builds the generated project and runs its domain tests.
func (b *BenchmarkRunner) compileAndTest(ctx context.Context, apiDir string) (bool, bool, []string) {
	if _, err := os.Stat(apiDir); err != nil {
		return false, false, []string{"no api directory was generated"}
	}

	run := func(args ...string) (string, error) {
		cmdCtx, cancel := context.WithTimeout(ctx, b.Timeout)
		defer cancel()
		cmd := exec.CommandContext(cmdCtx, b.GoBinary, args...)
		cmd.Dir = apiDir
		// GOWORK=off isolates the generated module from the factory's own
		// workspace, which would otherwise refuse to resolve it.
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run("mod", "tidy"); err != nil {
		return false, false, []string{"dependency resolution failed: " + firstLines(out, 3)}
	}
	if out, err := run("build", "./..."); err != nil {
		return false, false, []string{"compilation failed: " + firstLines(out, 5)}
	}
	if out, err := run("test", "./internal/domain/"); err != nil {
		return true, false, []string{"generated tests failed: " + firstLines(out, 5)}
	}
	return true, true, nil
}

func containsPath(files []string, want string) bool {
	for _, f := range files {
		if f == want {
			return true
		}
	}
	return false
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}

// Markdown renders a human-readable report.
func (r Report) Markdown() string {
	var sb strings.Builder
	sb.WriteString("# Generation Benchmark\n\n")
	fmt.Fprintf(&sb, "**Overall score:** %.1f%%  \n", r.Score*100)
	fmt.Fprintf(&sb, "**Reasoning:** %s  \n", enabledLabel(r.Reasoning))
	fmt.Fprintf(&sb, "**Duration:** %s\n\n", r.Duration.Round(time.Millisecond))

	sb.WriteString("| Case | Category | Gen | Compiles | Tests | Runs | Serves | Entities | Docs | Score |\n")
	sb.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")

	cases := make([]CaseResult, len(r.Cases))
	copy(cases, r.Cases)
	sort.SliceStable(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })

	for _, c := range cases {
		entities := "—"
		if c.EntitiesWanted > 0 {
			entities = fmt.Sprintf("%d/%d", c.EntitiesFound, c.EntitiesWanted)
		}
		docs := 0
		for _, present := range []bool{c.HasAcceptanceCriteria, c.HasSchema, c.HasAPIContract, c.HasDeployment} {
			if present {
				docs++
			}
		}
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s | %s | %s | %d/4 | %.0f%% |\n",
			c.Name, c.Category, tick(c.Generated), tick(c.Compiles), tick(c.TestsPass),
			tick(c.Starts), tick(c.Responds), entities, docs, c.Score*100)
	}

	var failures []CaseResult
	for _, c := range cases {
		if len(c.Errors) > 0 {
			failures = append(failures, c)
		}
	}
	if len(failures) > 0 {
		sb.WriteString("\n## Failures\n\n")
		for _, c := range failures {
			fmt.Fprintf(&sb, "**%s**\n", c.Name)
			for _, e := range c.Errors {
				fmt.Fprintf(&sb, "- %s\n", e)
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// JSON renders the report for machine comparison between runs.
func (r Report) JSON() string {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func tick(ok bool) string {
	if ok {
		return "✔"
	}
	return "✘"
}

func enabledLabel(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled (blueprints only)"
}

// ComputeScoreForTest exposes scoring to the external test package so the
// weighting can be asserted directly rather than inferred from a full run.
func ComputeScoreForTest(r *CaseResult) { r.computeScore() }
