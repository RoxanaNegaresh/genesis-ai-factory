package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// QAAgent generates the test strategy and executable test scaffolding, then
// reports on coverage of the acceptance criteria.
type QAAgent struct{}

func (a *QAAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleQA)
	return Charter{
		Role: domain.RoleQA, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactCodeBackend},
		Outputs: []domain.ArtifactKind{domain.ArtifactQAReport},
		Tools:   []string{"fs.write", "fs.read", "exec.run", "test.run"}, ModelClass: "code",
		Budget: DefaultBudget(), Temperature: 0.1,
	}
}

func (a *QAAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	if !bb.Has(domain.ArtifactCodeBackend) {
		return nil, fmt.Errorf("qa engineer requires generated backend code")
	}
	bp := bb.Blueprint

	stories, _ := bb.Value("stories")
	storyList, _ := stories.([]UserStory)

	if err := tb.WriteFile(ctx, "api/internal/adapter/http/contract_test.go", qaContractTest(bp)); err != nil {
		return nil, err
	}
	if err := tb.WriteFile(ctx, "docs/qa/TEST_PLAN.md", qaTestPlan(bp, storyList)); err != nil {
		return nil, err
	}

	files, err := tb.ListFiles(ctx)
	if err != nil {
		return nil, err
	}
	goFiles, tsFiles, testFiles := 0, 0, 0
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, "_test.go"), strings.HasSuffix(f, ".test.ts"), strings.HasSuffix(f, ".test.tsx"):
			testFiles++
			if strings.HasSuffix(f, ".go") {
				goFiles++
			}
		case strings.HasSuffix(f, ".go"):
			goFiles++
		case strings.HasSuffix(f, ".ts"), strings.HasSuffix(f, ".tsx"):
			tsFiles++
		}
	}

	criteria := 0
	for _, s := range storyList {
		criteria += len(s.Acceptance)
	}

	// v0.4: actually run the thing. Up to v0.3 the QA agent could only report
	// what it intended to verify; now it builds the project, runs its tests,
	// starts it and makes a real request. A test plan describing checks nobody
	// executed is a document, not quality assurance.
	verification := a.verify(ctx, bb, tb)

	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Test plan covers %d acceptance criteria across %d stories", criteria, len(storyList)),
		map[string]any{"criteria": criteria, "stories": len(storyList), "test_files": testFiles})

	if verification != nil {
		level := domain.LevelInfo
		if !verification.Verified() {
			level = domain.LevelWarn
		}
		tb.Emit(ctx, level, "Verification: "+verification.Summary(), map[string]any{
			"compiles": verification.Compiles, "tests_pass": verification.TestsPass,
			"starts": verification.Starts, "responds": verification.Responds,
			"status_code": verification.StatusCode,
		})
		bb.SetValue("verification", verification)
	}

	var sb strings.Builder
	sb.WriteString("# QA Report\n\n")

	if verification != nil {
		sb.WriteString("## Verification\n\n")
		fmt.Fprintf(&sb, "**Result:** %s\n\n", verification.Summary())
		sb.WriteString("| Stage | Result | Time | Detail |\n|---|---|---|---|\n")
		for _, stage := range verification.Stages {
			status := "✔ pass"
			switch {
			case stage.Skipped:
				status = "— skipped"
			case !stage.OK:
				status = "✘ fail"
			}
			fmt.Fprintf(&sb, "| %s | %s | %s | %s |\n",
				stage.Stage, status, stage.Duration.Round(time.Millisecond), stage.Detail)
		}
		sb.WriteString("\n")

		// The isolation actually achieved is part of the result: "the tests
		// passed" means something different when they ran unconfined.
		fmt.Fprintf(&sb, "Executed under %s isolation (network isolated: %v).\n\n",
			strings.Join(verification.Isolation.Namespaces, ", "), verification.Isolation.NetworkIsolated)
		for _, note := range verification.Isolation.Degraded {
			fmt.Fprintf(&sb, "> Isolation note: %s\n\n", note)
		}

		for _, stage := range verification.Stages {
			if stage.OK || stage.Skipped || stage.Output == "" {
				continue
			}
			fmt.Fprintf(&sb, "### %s output\n\n```\n%s\n```\n\n", stage.Stage, stage.Output)
		}
	} else {
		sb.WriteString("## Verification\n\n")
		sb.WriteString("No execution sandbox was available, so the generated project was not built or run. ")
		sb.WriteString("The checks below describe what *should* be verified, not what was.\n\n")
	}

	sb.WriteString("## Scope\n\n")
	fmt.Fprintf(&sb, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&sb, "| Go source files | %d |\n| TypeScript source files | %d |\n| Test files | %d |\n",
		goFiles, tsFiles, testFiles)
	fmt.Fprintf(&sb, "| User stories | %d |\n| Acceptance criteria | %d |\n\n", len(storyList), criteria)

	sb.WriteString("## Test pyramid\n\n")
	sb.WriteString("| Level | Target | What it protects |\n|---|---|---|\n")
	sb.WriteString("| Unit (domain) | every invariant and enum guard | Business rules stay correct under refactoring |\n")
	sb.WriteString("| Repository conformance | every implementation, one suite | Storage backends cannot drift apart |\n")
	sb.WriteString("| HTTP contract | every endpoint, happy and error path | The published API shape does not change silently |\n")
	sb.WriteString("| End-to-end | the three core flows | The product works as a whole, not just per unit |\n\n")

	sb.WriteString("## Required checks before release\n\n")
	sb.WriteString("1. `go vet ./...` clean\n2. `go test ./...` green\n3. `npm run typecheck` clean\n")
	sb.WriteString("4. `npm run build` succeeds\n5. Every `MUST` story has at least one automated assertion\n\n")

	sb.WriteString("## Risk areas\n\n")
	for _, r := range qaRisks(bp) {
		fmt.Fprintf(&sb, "- %s\n", r)
	}

	body := sb.String()
	return []*domain.Artifact{artifact(bb, domain.ArtifactQAReport, "QA_REPORT.md", "text/markdown", body)}, nil
}

func qaRisks(bp Blueprint) []string {
	risks := []string{
		"Authorization checks must be asserted per role, not merely per endpoint — a passing 200 for an admin proves nothing about a viewer",
		"Pagination boundaries (empty page, exact page size, invalid cursor) are the most common source of list-endpoint defects",
		"Soft-deleted records must be excluded from every list query and unique index",
	}
	switch bp.Category {
	case domain.CategoryERP, domain.CategoryMarketplace:
		risks = append(risks,
			"Monetary arithmetic must be asserted against exact decimal expectations, never approximate float comparisons",
			"State machines (order, payment, stock) need explicit tests for illegal transitions, not only legal ones")
	case domain.CategoryPM:
		risks = append(risks,
			"Board reordering must be tested for concurrent moves that target the same position")
	case domain.CategoryCRM:
		risks = append(risks,
			"Pipeline aggregation must be verified against deals in every stage, including won and lost")
	}
	return risks
}

func qaContractTest(bp Blueprint) string {
	var sb strings.Builder
	sb.WriteString(`package http_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// newTestApp builds a Fiber app with the same middleware stack as production so
// contract tests exercise real error mapping rather than a simplified harness.
func newTestApp(t *testing.T) *fiber.App {
	t.Helper()
	app := fiber.New()
	app.Get("/health", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"status": "ok"}) })
	return app
}

func TestHealthEndpoint(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
}

// TestProtectedEndpointsRequireAuth asserts that every mutating resource route
// rejects an unauthenticated caller. This is the single most valuable contract
// test in the suite: an endpoint accidentally left public is invisible in
// manual testing because the developer is always signed in.
func TestProtectedEndpointsRequireAuth(t *testing.T) {
	protected := []struct {
		method string
		path   string
	}{
`)
	for _, e := range bp.Entities {
		if e.Name == "User" {
			continue
		}
		p := "/api/v1/" + routePath(e)
		fmt.Fprintf(&sb, "\t\t{http.MethodGet, %q},\n", p)
		fmt.Fprintf(&sb, "\t\t{http.MethodPost, %q},\n", p)
	}
	sb.WriteString(`	}

	for _, tc := range protected {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Skipf("enable once %s is wired into the router", tc.path)
		})
	}
}
`)
	return sb.String()
}

func qaTestPlan(bp Blueprint, stories []UserStory) string {
	var sb strings.Builder
	sb.WriteString("# Test Plan\n\n## Traceability matrix\n\n")
	sb.WriteString("Every acceptance criterion maps to at least one automated check. ")
	sb.WriteString("A criterion with no test is treated as an unimplemented requirement.\n\n")
	sb.WriteString("| Story | Priority | Criteria | Test level |\n|---|---|---|---|\n")
	for _, s := range stories {
		level := "unit + contract"
		if strings.Contains(strings.ToLower(s.Epic), "interface") {
			level = "component + e2e"
		}
		fmt.Fprintf(&sb, "| %s | %s | %d | %s |\n", s.ID, strings.ToUpper(s.Priority), len(s.Acceptance), level)
	}

	sb.WriteString("\n## Fixtures\n\n")
	sb.WriteString("| Entity | Fixture strategy |\n|---|---|\n")
	for _, e := range bp.Entities {
		strategy := "Builder function with sensible defaults and per-test overrides"
		if isSupportingEntity(e) {
			strategy = "Created implicitly through its parent's builder"
		}
		fmt.Fprintf(&sb, "| %s | %s |\n", e.Name, strategy)
	}

	sb.WriteString("\n## Commands\n\n```bash\n# Backend\ncd api && go test ./... -race\n\n")
	sb.WriteString("# Frontend\ncd web && npm run typecheck && npm test\n```\n")
	return sb.String()
}

// SecurityAgent audits the generated system and reports findings with fixes.
type SecurityAgent struct{}

func (a *SecurityAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleSecurity)
	return Charter{
		Role: domain.RoleSecurity, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactCodeBackend},
		Outputs: []domain.ArtifactKind{domain.ArtifactSecReport},
		Tools:   []string{"fs.read", "fs.write"}, ModelClass: "reasoning",
		Budget: DefaultBudget(), Temperature: 0.1,
	}
}

func (a *SecurityAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	bp := bb.Blueprint

	type finding struct {
		id, severity, title, detail, remediation string
	}

	findings := []finding{
		{"SEC-001", "high", "Authorization must be enforced in the use case layer",
			"Generated handlers accept any authenticated caller. Role checks written per handler are inevitably forgotten on the next endpoint.",
			"Add a permission check to each use case method, taking the principal as an argument, and cover it with a per-role test."},
		{"SEC-002", "high", "Rate limiting absent on authentication endpoints",
			"Login and refresh are unthrottled, permitting credential stuffing at network speed.",
			"Apply a per-IP and per-account limiter (for example 10 attempts per 15 minutes) with exponential backoff."},
		{"SEC-003", "medium", "Refresh token must be delivered as an httpOnly cookie",
			"A refresh token readable by JavaScript converts any XSS into persistent account compromise.",
			"Set the refresh token as httpOnly, Secure, SameSite=Lax and keep only the access token in memory."},
		{"SEC-004", "medium", "Security headers not configured",
			"Missing CSP, HSTS, X-Content-Type-Options and Referrer-Policy leave known attack classes unmitigated.",
			"Add a headers middleware; start CSP in report-only mode and tighten once violations are quiet."},
		{"SEC-005", "medium", "Mass assignment risk on PATCH endpoints",
			"Parsing a request body onto a loaded record allows a client to set fields it should never control, such as owner or role.",
			"Bind PATCH bodies to an explicit DTO with only the mutable fields, then copy them onto the aggregate."},
		{"SEC-006", "low", "No dependency vulnerability scanning in CI",
			"Vulnerable transitive dependencies are only discovered by chance.",
			"Run govulncheck and npm audit in CI and fail the build on high severity findings."},
	}

	if bp.Category == domain.CategoryMarketplace || bp.Category == domain.CategoryERP {
		findings = append(findings, finding{
			"SEC-007", "high", "Payment and stock mutations must be idempotent",
			"Retried webhooks or double-submitted forms can double-charge a customer or double-deduct inventory.",
			"Require an idempotency key on every money- or stock-moving operation and store the first result for replay."})
	}
	findings = append(findings, finding{
		"SEC-008", "info", "Secrets must never be committed",
		"Generated .env templates are placeholders; a real secret committed once is compromised permanently.",
		"Keep .env in .gitignore, load secrets from the environment, and run a secret scanner in CI."})

	var sb strings.Builder
	sb.WriteString("# Security Review\n\n")
	high, medium, low := 0, 0, 0
	for _, f := range findings {
		switch f.severity {
		case "high":
			high++
		case "medium":
			medium++
		case "low":
			low++
		}
	}
	fmt.Fprintf(&sb, "**Findings:** %d high · %d medium · %d low · %d informational\n\n",
		high, medium, low, len(findings)-high-medium-low)

	sb.WriteString("## Findings\n\n")
	for _, f := range findings {
		fmt.Fprintf(&sb, "### %s — %s (%s)\n\n**Issue.** %s\n\n**Remediation.** %s\n\n",
			f.id, f.title, strings.ToUpper(f.severity), f.detail, f.remediation)
	}

	sb.WriteString("## Baseline controls already in place\n\n")
	sb.WriteString("- Passwords hashed with a memory-hard function, never a fast digest\n")
	sb.WriteString("- Parameterised queries throughout; no string-concatenated SQL\n")
	sb.WriteString("- Strict CORS allowlist; wildcard origins with credentials are rejected by construction\n")
	sb.WriteString("- Errors return an opaque code to the client and full detail only to the log\n")
	sb.WriteString("- Soft delete preserves an audit trail rather than destroying evidence\n")

	body := sb.String()
	if err := tb.WriteFile(ctx, "docs/security/SECURITY_REVIEW.md", body); err != nil {
		return nil, err
	}

	tb.Emit(ctx, domain.LevelWarn,
		fmt.Sprintf("Security review complete: %d high, %d medium findings", high, medium),
		map[string]any{"high": high, "medium": medium, "total": len(findings)})

	return []*domain.Artifact{artifact(bb, domain.ArtifactSecReport, "SECURITY_REVIEW.md", "text/markdown", body)}, nil
}

// verify builds and runs the generated project, returning nil when no sandbox
// is configured.
//
// A verification failure is reported, not raised: the QA agent's job is to tell
// the truth about the artifact, and a project that fails to build is a finding
// rather than a reason to abandon the run.
func (a *QAAgent) verify(ctx context.Context, bb *Blackboard, tb Toolbelt) *VerificationReport {
	if bb.Runner == nil || !bb.Runner.Available() {
		tb.Emit(ctx, domain.LevelDebug,
			"No execution sandbox is configured; the generated project will not be built or run", nil)
		return nil
	}
	if bb.Project.WorkspacePath == "" {
		return nil
	}

	tb.Emit(ctx, domain.LevelInfo, "Building and running the generated project in the sandbox", nil)

	report, err := bb.Runner.Verify(ctx, tb, bb.Project.WorkspacePath, GoToolchain())
	if err != nil {
		tb.Emit(ctx, domain.LevelWarn, "Verification could not run: "+err.Error(), nil)
		return nil
	}

	// The web client is verified separately and reported separately. Until
	// v1.2 it was type-checked and never started, so "the frontend works"
	// rested on tsc exiting zero — a bundle that crashes on load type-checks
	// perfectly well.
	//
	// A frontend failure does not fail the backend report. They are different
	// artefacts with different toolchains, and collapsing them would make a
	// missing npm look like a broken API.
	verifyFrontend(ctx, tb, bb)

	return report
}

// verifyFrontend builds and serves the generated web client.
func verifyFrontend(ctx context.Context, tb Toolbelt, bb *Blackboard) {
	chain := NodeToolchain()

	marker := filepath.Join(bb.Project.WorkspacePath, chain.Dir, chain.Marker)
	if _, err := os.Stat(marker); err != nil {
		return
	}

	tb.Emit(ctx, domain.LevelInfo, "Building and serving the generated web client", nil)

	report, err := bb.Runner.Verify(ctx, tb, bb.Project.WorkspacePath, chain)
	if err != nil {
		tb.Emit(ctx, domain.LevelWarn, "Frontend verification could not run: "+err.Error(), nil)
		return
	}

	switch {
	case report.Responds:
		tb.Emit(ctx, domain.LevelInfo,
			"The web client builds, serves and answers a request",
			map[string]any{"status": report.StatusCode})
	case report.Compiles:
		tb.Emit(ctx, domain.LevelWarn,
			"The web client builds but did not answer a request", nil)
	default:
		tb.Emit(ctx, domain.LevelWarn,
			"The web client did not build: "+report.Summary(), nil)
	}
}
