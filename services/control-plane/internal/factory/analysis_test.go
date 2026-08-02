package factory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
)

// The analyser must describe the project in front of it. A finding that would
// be identical for every project is a template, and templates masquerading as
// analysis are worse than no analysis at all.
func TestAnalysisReportsRealGapsInAGeneratedProject(t *testing.T) {
	workspace := generateProject(t, "Build a CRM for a sales team")

	analysis, err := factory.AnalyzeProject(context.Background(), workspace,
		factory.BlueprintFor(domain.CategoryCRM))
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}

	// The metrics must be measured, not assumed.
	if analysis.GoFiles < 10 {
		t.Errorf("only %d Go files were counted in a full project", analysis.GoFiles)
	}
	if analysis.Lines < 500 {
		t.Errorf("only %d lines were counted", analysis.Lines)
	}
	if analysis.Packages < 4 {
		t.Errorf("only %d packages were found", analysis.Packages)
	}
	if analysis.TestFiles == 0 {
		t.Error("the generated tests were not counted")
	}

	// Until v1.1 these two gaps were real and this test asserted they were
	// reported. The factory now generates PostgreSQL repositories and wires
	// them into registerRoutes, so the assertion is inverted: the analyser
	// looks for exactly these conditions, which makes their absence a
	// regression test for the persistence layer itself. If a future change
	// stops emitting repositories or stops mounting handlers, the analyser
	// will say so and this test will fail.
	titles := make([]string, 0, len(analysis.Findings))
	for _, finding := range analysis.Findings {
		titles = append(titles, finding.Title)
	}
	joined := strings.Join(titles, " | ")

	if strings.Contains(joined, "repository interfaces have no implementation") {
		t.Errorf("repositories are generated but reported missing: %s", joined)
	}
	if strings.Contains(joined, "never mounted") {
		t.Errorf("handlers are wired but reported unmounted: %s", joined)
	}

	// No finding of high severity should survive in a freshly generated
	// project. A generator that ships known-high defects is not finished.
	for _, finding := range analysis.Findings {
		if finding.Severity == factory.SeverityHigh {
			t.Errorf("a freshly generated project has a high-severity finding: %s — %s",
				finding.Title, finding.Detail)
		}
	}

	// Every finding must be actionable and attributed.
	for _, finding := range analysis.Findings {
		if finding.Action == "" {
			t.Errorf("finding %q has no action", finding.Title)
		}
		if finding.Detail == "" {
			t.Errorf("finding %q has no detail", finding.Title)
		}
		if finding.Category == "" {
			t.Errorf("finding %q has no category", finding.Title)
		}
	}
}

// Findings must reflect what is actually on disk, so adding the missing pieces
// must remove the corresponding findings.
func TestAnalysisRespondsToTheActualProject(t *testing.T) {
	workspace := t.TempDir()

	// A bare project missing everything operational.
	writeFile(t, workspace, "api/go.mod", "module x\n\ngo 1.23\n")
	writeFile(t, workspace, "api/main.go", "package main\n\nfunc main() {}\n")

	before, err := factory.AnalyzeProject(context.Background(), workspace, factory.Blueprint{})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}

	missing := findingTitles(before)
	for _, want := range []string{"container image", "CI pipeline", "database migrations"} {
		if !strings.Contains(missing, want) {
			t.Errorf("a missing %s was not reported: %s", want, missing)
		}
	}

	// Supply them, and the findings must disappear.
	writeFile(t, workspace, "api/Dockerfile", "FROM scratch\n")
	writeFile(t, workspace, ".github/workflows/ci.yml", "name: CI\n")
	writeFile(t, workspace, "migrations/0001_init.up.sql", "CREATE TABLE x ();\n")
	writeFile(t, workspace, "docker-compose.yml", "services: {}\n")
	writeFile(t, workspace, ".env.example", "KEY=value\n")

	after, err := factory.AnalyzeProject(context.Background(), workspace, factory.Blueprint{})
	if err != nil {
		t.Fatalf("re-analyse: %v", err)
	}

	remaining := findingTitles(after)
	for _, gone := range []string{"container image", "CI pipeline", "database migrations"} {
		if strings.Contains(remaining, gone) {
			t.Errorf("%q is still reported after being added: %s", gone, remaining)
		}
	}
}

func TestAnalysisDetectsUnparseableSource(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, workspace, "api/broken.go", "package main\n\nfunc broken( {\n")

	analysis, err := factory.AnalyzeProject(context.Background(), workspace, factory.Blueprint{})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}

	found := false
	for _, finding := range analysis.Findings {
		if strings.Contains(finding.Title, "does not parse") {
			found = true
			if finding.Severity != factory.SeverityHigh {
				t.Errorf("a syntax error should be high severity, got %s", finding.Severity)
			}
			if finding.File == "" {
				t.Error("the failing file was not identified")
			}
		}
	}
	if !found {
		t.Fatalf("an unparseable file was not reported: %v", findingTitles(analysis))
	}
}

func TestAnalysisCountsTodoMarkers(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, workspace, "api/a.go", "package a\n\n// TODO: implement this\n// FIXME: and this\nvar X = 1\n")

	analysis, err := factory.AnalyzeProject(context.Background(), workspace, factory.Blueprint{})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if analysis.TODOs != 2 {
		t.Fatalf("expected 2 markers, counted %d", analysis.TODOs)
	}
	if !strings.Contains(findingTitles(analysis), "TODO or FIXME") {
		t.Error("the markers were counted but not reported")
	}
}

func TestAnalysisOrdersFindingsBySeverity(t *testing.T) {
	workspace := generateProject(t, "Build a CRM system")

	analysis, err := factory.AnalyzeProject(context.Background(), workspace,
		factory.BlueprintFor(domain.CategoryCRM))
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if len(analysis.Findings) < 2 {
		t.Skip("not enough findings to check ordering")
	}

	rank := map[factory.Severity]int{
		factory.SeverityHigh: 4, factory.SeverityMedium: 3,
		factory.SeverityLow: 2, factory.SeverityInfo: 1,
	}
	for i := 1; i < len(analysis.Findings); i++ {
		if rank[analysis.Findings[i].Severity] > rank[analysis.Findings[i-1].Severity] {
			t.Fatalf("findings are not ordered by severity: %s after %s",
				analysis.Findings[i].Severity, analysis.Findings[i-1].Severity)
		}
	}
}

func TestAnalysisMarkdownIsUseful(t *testing.T) {
	analysis := factory.Analysis{
		Files: 80, GoFiles: 40, TestFiles: 8, Packages: 6, Lines: 3000, Endpoints: 24,
		Findings: []factory.Finding{
			{Severity: factory.SeverityHigh, Category: "completeness",
				Title: "Repositories are unimplemented", Detail: "Nothing persists data.",
				Action: "Implement them against PostgreSQL."},
			{Severity: factory.SeverityLow, Category: "testing",
				Title: "Coverage is thin", Detail: "Few tests.", Action: "Add more."},
		},
	}

	markdown := analysis.Markdown()
	for _, want := range []string{
		"Improvement Plan", "80 files", "40 Go", "24 registered endpoints",
		"1 high", "HIGH priority", "Repositories are unimplemented",
		"Do this:", "Implement them against PostgreSQL",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("the report is missing %q:\n%s", want, markdown)
		}
	}

	// A clean project must say so rather than inventing filler.
	clean := factory.Analysis{Files: 10, GoFiles: 5}.Markdown()
	if !strings.Contains(clean, "No structural gaps") {
		t.Errorf("a clean analysis did not say so:\n%s", clean)
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func findingTitles(analysis *factory.Analysis) string {
	titles := make([]string, 0, len(analysis.Findings))
	for _, finding := range analysis.Findings {
		titles = append(titles, finding.Title)
	}
	return strings.Join(titles, " | ")
}
