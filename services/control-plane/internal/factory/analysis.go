package factory

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Static analysis of generated projects.
//
// The Improver agent has until now produced a backlog derived from what the
// factory *intended* to generate. That is a template dressed as an analysis: it
// says the same thing for every project, because it never looks at one.
//
// This analyses the actual code. Every finding is derived from the AST or the
// file tree, so it is true of this specific project or it is not reported at
// all — which is the difference between advice and noise.

// Severity ranks a finding.
type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
	SeverityInfo   Severity = "info"
)

// Finding is one observation about a generated project.
type Finding struct {
	Severity Severity `json:"severity"`
	Category string   `json:"category"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	// File and Line locate the finding when it is specific to one place.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	// Action is the concrete next step, not a restatement of the problem.
	Action string `json:"action"`
}

// Analysis is the result of inspecting a project.
type Analysis struct {
	Findings []Finding `json:"findings"`
	// Metrics measured from the source.
	Files       int `json:"files"`
	GoFiles     int `json:"go_files"`
	TestFiles   int `json:"test_files"`
	Lines       int `json:"lines"`
	Packages    int `json:"packages"`
	Entities    int `json:"entities"`
	Endpoints   int `json:"endpoints"`
	TODOs       int `json:"todos"`
	LongestFunc int `json:"longest_function"`
}

// Counts returns findings grouped by severity.
func (a Analysis) Counts() map[Severity]int {
	counts := map[Severity]int{}
	for _, finding := range a.Findings {
		counts[finding.Severity]++
	}
	return counts
}

// AnalyzeProject inspects a generated workspace.
func AnalyzeProject(ctx context.Context, root string, blueprint Blueprint) (*Analysis, error) {
	analysis := &Analysis{Entities: len(blueprint.Entities)}

	packages := map[string]bool{}
	// unimplemented tracks interfaces with no implementing type, which is the
	// single most consequential gap in what the factory currently produces.
	interfaces := map[string]string{}
	structs := map[string]bool{}
	handlerFiles := map[string]bool{}
	var routerFile string

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if skipAnalysisDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		analysis.Files++
		relative, _ := filepath.Rel(root, path)
		relative = filepath.ToSlash(relative)

		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(content)

		analysis.GoFiles++
		analysis.Lines += strings.Count(text, "\n")
		analysis.TODOs += strings.Count(text, "TODO") + strings.Count(text, "FIXME")
		if strings.HasSuffix(path, "_test.go") {
			analysis.TestFiles++
		}
		if strings.Contains(relative, "adapter/http") {
			handlerFiles[relative] = true
		}
		if strings.Contains(relative, "cmd/server") {
			routerFile = relative
		}

		file, err := parser.ParseFile(token.NewFileSet(), path, content, parser.AllErrors)
		if err != nil {
			analysis.Findings = append(analysis.Findings, Finding{
				Severity: SeverityHigh, Category: "correctness",
				Title:  "A source file does not parse",
				Detail: err.Error(), File: relative,
				Action: "Fix the syntax error; this file cannot compile.",
			})
			return nil
		}
		packages[file.Name.Name] = true

		// Count endpoints by route registration rather than by handler name,
		// which is how many are actually reachable.
		analysis.Endpoints += strings.Count(text, "g.Get(") + strings.Count(text, "g.Post(") +
			strings.Count(text, "g.Patch(") + strings.Count(text, "g.Delete(")

		ast.Inspect(file, func(node ast.Node) bool {
			switch declaration := node.(type) {
			case *ast.TypeSpec:
				switch declaration.Type.(type) {
				case *ast.InterfaceType:
					interfaces[declaration.Name.Name] = relative
				case *ast.StructType:
					structs[declaration.Name.Name] = true
				}
			case *ast.FuncDecl:
				if declaration.Body == nil {
					return true
				}
				// Function length is a proxy for reviewability; a 200-line
				// generated function is a maintenance problem regardless of
				// whether it works.
				length := declaration.Body.End() - declaration.Body.Pos()
				if int(length) > analysis.LongestFunc {
					analysis.LongestFunc = int(length)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	analysis.Packages = len(packages)

	analysis.Findings = append(analysis.Findings,
		analyseCompleteness(root, interfaces, structs, handlerFiles, routerFile)...)
	analysis.Findings = append(analysis.Findings, analyseTesting(analysis)...)
	analysis.Findings = append(analysis.Findings, analyseOperability(root)...)

	// Highest severity first, then alphabetically, so the report is stable
	// between runs and the important things are at the top.
	sort.SliceStable(analysis.Findings, func(i, j int) bool {
		left, right := analysis.Findings[i], analysis.Findings[j]
		if severityRank(left.Severity) != severityRank(right.Severity) {
			return severityRank(left.Severity) > severityRank(right.Severity)
		}
		return left.Title < right.Title
	})
	return analysis, nil
}

// analyseCompleteness reports what the generated project declares but does not
// implement. This is the most valuable analysis the factory can perform on
// itself, because it is exactly the gap between "scaffolding" and "product".
func analyseCompleteness(
	root string,
	interfaces map[string]string,
	structs map[string]bool,
	handlerFiles map[string]bool,
	routerFile string,
) []Finding {
	var findings []Finding

	// A repository interface with no implementing struct means the use cases
	// cannot be wired to a database.
	var unimplemented []string
	for name := range interfaces {
		if !strings.HasSuffix(name, "Repository") {
			continue
		}
		// A conventional implementation is named for the interface.
		base := strings.TrimSuffix(name, "Repository")
		if structs[base+"Repo"] || structs[base+"PostgresRepo"] || structs["Postgres"+base+"Repo"] {
			continue
		}
		unimplemented = append(unimplemented, name)
	}
	if len(unimplemented) > 0 {
		sort.Strings(unimplemented)
		shown := unimplemented
		if len(shown) > 6 {
			shown = append(shown[:6], fmt.Sprintf("and %d more", len(unimplemented)-6))
		}
		findings = append(findings, Finding{
			Severity: SeverityHigh, Category: "completeness",
			Title: fmt.Sprintf("%d repository interfaces have no implementation", len(unimplemented)),
			Detail: "The use cases are complete but nothing persists data: " +
				strings.Join(shown, ", ") + ".",
			Action: "Implement each repository against PostgreSQL using pgx, then wire them in cmd/server.",
		})
	}

	// Handlers that exist but are never mounted are unreachable, which is
	// indistinguishable from missing when the API is called.
	//
	// The signal is the route-registration function: if it constructs no
	// handler and calls no Register, nothing it declares is reachable. Checking
	// the function rather than the whole file avoids a false negative from an
	// unrelated mention of "Handler" elsewhere in main.
	if routerFile != "" && len(handlerFiles) > 0 {
		content, err := os.ReadFile(filepath.Join(root, routerFile))
		if err == nil && !routesAreWired(string(content)) {
			findings = append(findings, Finding{
				Severity: SeverityHigh, Category: "completeness",
				Title:  fmt.Sprintf("%d handler files are never mounted on the router", len(handlerFiles)),
				Detail: "Every resource handler is generated but registerRoutes wires none of them, so the API surface is unreachable.",
				File:   routerFile,
				Action: "Construct each handler in cmd/server and call its Register method on the /api/v1 group.",
			})
		}
	}
	return findings
}

func analyseTesting(analysis *Analysis) []Finding {
	var findings []Finding

	if analysis.GoFiles == 0 {
		return findings
	}
	// A ratio, not an absolute count: ten test files is excellent for a small
	// project and negligible for a large one.
	ratio := float64(analysis.TestFiles) / float64(analysis.GoFiles)
	switch {
	case ratio < 0.15:
		findings = append(findings, Finding{
			Severity: SeverityMedium, Category: "testing",
			Title: fmt.Sprintf("Test coverage is thin: %d test files for %d source files",
				analysis.TestFiles, analysis.GoFiles),
			Detail: "Most generated code has no automated verification, so a refactor cannot be made safely.",
			Action: "Add table-driven tests for each use case, and HTTP contract tests for each endpoint.",
		})
	case ratio < 0.3:
		findings = append(findings, Finding{
			Severity: SeverityLow, Category: "testing",
			Title:  "Test coverage could be deeper",
			Detail: fmt.Sprintf("%d test files cover %d source files.", analysis.TestFiles, analysis.GoFiles),
			Action: "Extend coverage to the use case and adapter layers.",
		})
	}

	if analysis.TODOs > 0 {
		findings = append(findings, Finding{
			Severity: SeverityLow, Category: "completeness",
			Title:  fmt.Sprintf("%d TODO or FIXME markers remain", analysis.TODOs),
			Detail: "Unfinished work is marked in the source but not tracked anywhere.",
			Action: "Resolve each marker or convert it into a tracked issue.",
		})
	}
	return findings
}

func analyseOperability(root string) []Finding {
	var findings []Finding

	// Absence of these is a real operational gap, and checking the filesystem
	// is more honest than assuming the generator produced them.
	expectations := []struct {
		path     string
		severity Severity
		title    string
		detail   string
		action   string
	}{
		{"api/Dockerfile", SeverityMedium, "The API has no container image",
			"The service cannot be deployed reproducibly.",
			"Add a multi-stage Dockerfile producing a distroless image."},
		{"docker-compose.yml", SeverityLow, "There is no compose stack",
			"A developer cannot start the full system with one command.",
			"Add a compose file wiring the API, database and cache."},
		{".github/workflows/ci.yml", SeverityMedium, "There is no CI pipeline",
			"Nothing verifies a change before it is merged.",
			"Add a workflow running build, vet and test on every pull request."},
		{"migrations", SeverityHigh, "There are no database migrations",
			"The schema exists only as documentation and cannot be applied.",
			"Generate versioned SQL migrations and apply them on boot."},
		{".env.example", SeverityLow, "Configuration is undocumented",
			"A new developer cannot tell which environment variables are required.",
			"Add an .env.example listing every variable with a safe default."},
	}

	for _, expectation := range expectations {
		if _, err := os.Stat(filepath.Join(root, expectation.path)); err == nil {
			continue
		}
		findings = append(findings, Finding{
			Severity: expectation.severity, Category: "operations",
			Title: expectation.title, Detail: expectation.detail,
			File: expectation.path, Action: expectation.action,
		})
	}
	return findings
}

func skipAnalysisDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "build", "vendor", "target":
		return true
	}
	return false
}

func severityRank(severity Severity) int {
	switch severity {
	case SeverityHigh:
		return 4
	case SeverityMedium:
		return 3
	case SeverityLow:
		return 2
	}
	return 1
}

// Markdown renders the analysis as a report.
func (a Analysis) Markdown() string {
	var sb strings.Builder
	sb.WriteString("# Improvement Plan\n\n")

	counts := a.Counts()
	sb.WriteString("## What was analysed\n\n")
	fmt.Fprintf(&sb, "%d files (%d Go, %d tests) across %d packages, %d lines. ",
		a.Files, a.GoFiles, a.TestFiles, a.Packages, a.Lines)
	fmt.Fprintf(&sb, "%d entities, %d registered endpoints.\n\n", a.Entities, a.Endpoints)
	fmt.Fprintf(&sb, "**Findings:** %d high, %d medium, %d low.\n\n",
		counts[SeverityHigh], counts[SeverityMedium], counts[SeverityLow])

	if len(a.Findings) == 0 {
		sb.WriteString("No structural gaps were found in this project.\n")
		return sb.String()
	}

	current := Severity("")
	for _, finding := range a.Findings {
		if finding.Severity != current {
			current = finding.Severity
			fmt.Fprintf(&sb, "## %s priority\n\n", strings.ToUpper(string(current)))
		}
		fmt.Fprintf(&sb, "### %s\n\n", finding.Title)
		if finding.File != "" {
			fmt.Fprintf(&sb, "`%s`", finding.File)
			if finding.Line > 0 {
				fmt.Fprintf(&sb, " line %d", finding.Line)
			}
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "%s\n\n**Do this:** %s\n\n", finding.Detail, finding.Action)
	}

	sb.WriteString("---\n\nEvery finding above was derived from this project's own source, ")
	sb.WriteString("not from a template. A finding that is not true of this code is a bug in the analyser.\n")
	return sb.String()
}

// routesAreWired reports whether the router actually mounts any handler.
func routesAreWired(source string) bool {
	// Look inside registerRoutes specifically; a mention of "Handler" in a
	// comment or an import does not make a route reachable.
	start := strings.Index(source, "func registerRoutes(")
	if start < 0 {
		// A project with a different structure: fall back to any Register call.
		return strings.Contains(source, ".Register(")
	}

	body := source[start:]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}
	return strings.Contains(body, ".Register(") || strings.Contains(body, "Handler(")
}
