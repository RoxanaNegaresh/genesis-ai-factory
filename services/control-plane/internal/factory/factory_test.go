package factory_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
)

func TestClassifyRecognisesEachCategory(t *testing.T) {
	cases := []struct {
		prompt string
		want   domain.ProjectCategory
	}{
		{"Build a Jira competitor", domain.CategoryPM},
		{"Create a project management system with kanban boards and sprints", domain.CategoryPM},
		{"Build a CRM system for my sales team to track leads and deals", domain.CategoryCRM},
		{"I need a CRM with a sales pipeline", domain.CategoryCRM},
		{"Create an ERP system for a manufacturing company with inventory and purchase orders", domain.CategoryERP},
		{"Build an Airbnb-like marketplace with listings and payments", domain.CategoryMarketplace},
		{"Build an e-commerce marketplace where sellers list products", domain.CategoryMarketplace},
		{"Build an internal admin dashboard web app", domain.CategorySaaS},
	}
	for _, tc := range cases {
		got := factory.Classify(tc.prompt)
		if got.Category != tc.want {
			t.Errorf("Classify(%q) = %s (confidence %.2f, matched %v), want %s",
				tc.prompt, got.Category, got.Confidence, got.Matched, tc.want)
		}
		if got.Confidence <= 0 {
			t.Errorf("Classify(%q) returned zero confidence for a recognised prompt", tc.prompt)
		}
	}
}

func TestClassifyFallsBackToCustom(t *testing.T) {
	got := factory.Classify("zxqv frobnicate the wibble")
	if got.Category != domain.CategoryCustom {
		t.Fatalf("expected custom category for an unrecognisable brief, got %s", got.Category)
	}
	if got.Confidence != 0 {
		t.Fatalf("expected zero confidence, got %.2f", got.Confidence)
	}
}

func TestClassifyIsDeterministic(t *testing.T) {
	const prompt = "Build a CRM with a sales pipeline and lead scoring"
	first := factory.Classify(prompt)
	for i := 0; i < 25; i++ {
		got := factory.Classify(prompt)
		if got.Category != first.Category || got.Confidence != first.Confidence {
			t.Fatalf("classification is not deterministic: %+v vs %+v", got, first)
		}
	}
}

func TestBlueprintsAreWellFormed(t *testing.T) {
	for _, bp := range factory.Blueprints() {
		t.Run(bp.Key, func(t *testing.T) {
			if bp.Name == "" || bp.Description == "" || bp.Version == "" {
				t.Fatal("blueprint metadata incomplete")
			}
			if len(bp.Entities) == 0 || len(bp.Screens) == 0 || len(bp.Epics) == 0 {
				t.Fatal("blueprint must define entities, screens and epics")
			}
			if len(bp.Personas) == 0 {
				t.Fatal("blueprint must define at least one persona")
			}

			names := map[string]bool{}
			for _, e := range bp.Entities {
				if names[e.Name] {
					t.Fatalf("duplicate entity %s", e.Name)
				}
				names[e.Name] = true
				if e.Plural == "" || e.Description == "" {
					t.Fatalf("entity %s is missing plural or description", e.Name)
				}
				// Audit fields are mandatory: a table without them cannot
				// support soft delete, ordering or debugging.
				for _, required := range []string{"id", "created_at", "updated_at"} {
					found := false
					for _, f := range e.Fields {
						if f.Name == required {
							found = true
						}
					}
					if !found {
						t.Errorf("entity %s is missing the %s field", e.Name, required)
					}
				}
			}

			// Every reference must resolve, or the generated SQL will not apply.
			for _, e := range bp.Entities {
				for _, f := range e.Fields {
					if f.Type == "ref" && f.Ref != "" && !names[f.Ref] {
						t.Errorf("entity %s field %s references unknown entity %s", e.Name, f.Name, f.Ref)
					}
					if f.Type == "enum" && len(f.Enum) == 0 {
						t.Errorf("entity %s field %s is an enum with no values", e.Name, f.Name)
					}
				}
			}

			routes := map[string]bool{}
			for _, s := range bp.Screens {
				if routes[s.Route] {
					t.Errorf("duplicate route %s", s.Route)
				}
				routes[s.Route] = true
				if len(s.Components) == 0 {
					t.Errorf("screen %s declares no components", s.Name)
				}
				if s.PrimaryData != "" && !names[s.PrimaryData] {
					t.Errorf("screen %s references unknown entity %s", s.Name, s.PrimaryData)
				}
			}
		})
	}
}

// --- toolbelt safety ------------------------------------------------------

func newToolbelt(t *testing.T) (*factory.WorkspaceToolbelt, string) {
	t.Helper()
	root := t.TempDir()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleBackend, nil, nil)
	return tb, root
}

func TestToolbeltConfinesWritesToWorkspace(t *testing.T) {
	tb, root := newToolbelt(t)
	ctx := context.Background()

	escapes := []string{
		"../escaped.txt",
		"../../etc/passwd",
		"/etc/passwd",
		"docs/../../outside.md",
		"~/secrets.txt",
		"",
		".",
	}
	for _, p := range escapes {
		if err := tb.WriteFile(ctx, p, "payload"); err == nil {
			t.Errorf("path %q was accepted; it must be rejected", p)
		}
	}

	// Nothing may have been created outside the workspace.
	parent := filepath.Dir(root)
	if _, err := os.Stat(filepath.Join(parent, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatal("a file escaped the workspace root")
	}
	if _, err := os.Stat(filepath.Join(parent, "outside.md")); !os.IsNotExist(err) {
		t.Fatal("a file escaped the workspace root via traversal")
	}
}

func TestToolbeltWriteReadList(t *testing.T) {
	tb, root := newToolbelt(t)
	ctx := context.Background()

	if err := tb.WriteFile(ctx, "docs/product/PRD.md", "# PRD"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tb.WriteFile(ctx, "api/main.go", "package main"); err != nil {
		t.Fatalf("write: %v", err)
	}

	content, err := tb.ReadFile(ctx, "docs/product/PRD.md")
	if err != nil || content != "# PRD" {
		t.Fatalf("read returned %q (err %v)", content, err)
	}

	files, err := tb.ListFiles(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	if files[0] != "api/main.go" || files[1] != "docs/product/PRD.md" {
		t.Fatalf("listing is not sorted or is wrong: %v", files)
	}

	// Content must actually be on disk, not only in memory.
	onDisk, err := os.ReadFile(filepath.Join(root, "api", "main.go"))
	if err != nil || string(onDisk) != "package main" {
		t.Fatalf("file was not persisted: %v", err)
	}

	if _, err := tb.ReadFile(ctx, "nope.txt"); err == nil {
		t.Fatal("reading a missing file should fail")
	}
}

func TestToolbeltBlocksSecrets(t *testing.T) {
	tb, _ := newToolbelt(t)
	ctx := context.Background()

	secrets := []string{
		"aws_secret_access_key = EXAMPLE_AWS_SECRET_KEY",
		"-----BEGIN RSA PRIVATE KEY-----\nEXAMPLE_KEY_DATA\n-----END RSA PRIVATE KEY-----",
		`api_key: "stripe_test_placeholder_key"`,
		"token = ghp_abcdefghijklmnopqrstuvwxyz0123456789",
	}
	for _, s := range secrets {
		if err := tb.WriteFile(ctx, "config.txt", s); err == nil {
			t.Errorf("secret-looking content was written: %q", truncate(s, 40))
		}
	}

	// Placeholders in templates must still be allowed, or every generated
	// .env.example would be blocked.
	safe := []string{
		"POSTGRES_PASSWORD=change-me",
		`api_key: "your-api-key-here"`,
		"JWT_SECRET=${JWT_SECRET}",
	}
	for _, s := range safe {
		if err := tb.WriteFile(ctx, "env.example", s); err != nil {
			t.Errorf("placeholder content was blocked: %q (%v)", s, err)
		}
	}
}

func TestToolbeltRejectsOversizedFiles(t *testing.T) {
	tb, _ := newToolbelt(t)
	huge := strings.Repeat("x", (2<<20)+1)
	if err := tb.WriteFile(context.Background(), "big.txt", huge); err == nil {
		t.Fatal("an oversized file was accepted")
	}
}

// --- agents ---------------------------------------------------------------

func runAgentPipeline(t *testing.T, prompt string) (*factory.Blackboard, string, []*domain.Artifact) {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()

	project := &domain.Project{
		ID: domain.NewID(), OwnerID: domain.NewID(),
		Name: domain.TitleFromPrompt(prompt), Slug: domain.Slugify(domain.TitleFromPrompt(prompt)),
		Prompt: prompt, WorkspacePath: root, Settings: domain.DefaultProjectSettings(),
	}
	run, err := domain.NewRun(project.ID, project.OwnerID, domain.RunBuild, nil, 0, time.Now())
	if err != nil {
		t.Fatalf("new run: %v", err)
	}

	bb := factory.NewBlackboard(project, run)
	bb.Classification = factory.Classify(prompt)
	bb.Blueprint = factory.BlueprintFor(bb.Classification.Category)

	registry := factory.NewRegistry()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleSystem, nil, nil)

	var produced []*domain.Artifact
	for _, role := range registry.Roles() {
		agent, ok := registry.Get(role)
		if !ok {
			t.Fatalf("agent %s not registered", role)
		}
		artifacts, err := agent.Execute(ctx, bb, tb.For(role))
		if err != nil {
			t.Fatalf("agent %s failed: %v", role, err)
		}
		if len(artifacts) == 0 {
			t.Fatalf("agent %s produced no artifacts", role)
		}
		produced = append(produced, artifacts...)
	}
	return bb, root, produced
}

func TestFullAgentPipelineProducesRealArtifacts(t *testing.T) {
	bb, root, artifacts := runAgentPipeline(t, "Build a Jira competitor with kanban boards and sprints")

	if bb.Classification.Category != domain.CategoryPM {
		t.Fatalf("expected pm category, got %s", bb.Classification.Category)
	}
	if len(artifacts) < 10 {
		t.Fatalf("expected at least 10 artifacts, got %d", len(artifacts))
	}

	// Every declared output kind must actually exist on the blackboard.
	required := []domain.ArtifactKind{
		domain.ArtifactVision, domain.ArtifactPRD, domain.ArtifactDesignSystem,
		domain.ArtifactDesignFlows, domain.ArtifactArchSpec, domain.ArtifactADR,
		domain.ArtifactDBSchema, domain.ArtifactMigrations, domain.ArtifactCodeBackend,
		domain.ArtifactCodeFrontend, domain.ArtifactQAReport, domain.ArtifactSecReport,
		domain.ArtifactDocker, domain.ArtifactCI, domain.ArtifactReadme, domain.ArtifactImprovePlan,
	}
	for _, kind := range required {
		a, ok := bb.Get(kind)
		if !ok {
			t.Errorf("missing artifact %s", kind)
			continue
		}
		if a.SizeBytes < 100 {
			t.Errorf("artifact %s is suspiciously small (%d bytes)", kind, a.SizeBytes)
		}
		if a.SHA256 == "" {
			t.Errorf("artifact %s has no content hash", kind)
		}
	}

	// The workspace must contain a real, coherent project on disk.
	expectedFiles := []string{
		"docs/product/VISION.md",
		"docs/product/PRD.md",
		"docs/design/DESIGN_SYSTEM.md",
		"docs/design/tokens.json",
		"docs/architecture/ARCHITECTURE.md",
		"docs/architecture/DATA_MODEL.md",
		"api/openapi.yaml",
		"migrations/0001_init.up.sql",
		"migrations/0001_init.down.sql",
		"api/go.mod",
		"api/cmd/server/main.go",
		"web/package.json",
		"web/src/App.tsx",
		"web/src/lib/api.ts",
		"docker-compose.yml",
		".github/workflows/ci.yml",
		"README.md",
		"Makefile",
		".gitignore",
	}
	for _, rel := range expectedFiles {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("expected generated file %s: %v", rel, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("generated file %s is empty", rel)
		}
	}
}

func TestGeneratedSQLIsCoherent(t *testing.T) {
	_, root, _ := runAgentPipeline(t, "Create an ERP system for a manufacturing company")

	raw, err := os.ReadFile(filepath.Join(root, "migrations", "0001_init.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)

	for _, want := range []string{"CREATE TABLE", "PRIMARY KEY", "REFERENCES", "CREATE INDEX", "NUMERIC(18,4)"} {
		if !strings.Contains(sql, want) {
			t.Errorf("generated DDL is missing %q", want)
		}
	}
	// Money must never be a binary float.
	if strings.Contains(sql, "DOUBLE PRECISION") || strings.Contains(sql, "FLOAT") {
		t.Error("generated DDL uses floating point, which is incorrect for monetary values")
	}

	// Foreign keys must only reference tables already created above them,
	// otherwise the migration cannot apply.
	created := map[string]bool{}
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "CREATE TABLE ") {
			name := strings.TrimSuffix(strings.Fields(trimmed)[2], " (")
			name = strings.TrimSuffix(name, "(")
			created[strings.TrimSpace(name)] = true
		}
		if idx := strings.Index(trimmed, "REFERENCES "); idx >= 0 {
			rest := strings.TrimSpace(trimmed[idx+len("REFERENCES "):])
			target := strings.TrimSpace(strings.Split(rest, " ")[0])
			target = strings.TrimSuffix(target, "(id)")
			target = strings.TrimSpace(target)
			if target != "" && !created[target] {
				t.Errorf("forward foreign key reference to %q before it is created", target)
			}
		}
	}
}

func TestGeneratedOpenAPICoversEveryEntity(t *testing.T) {
	bb, root, _ := runAgentPipeline(t, "Build a CRM system for a sales team")

	raw, err := os.ReadFile(filepath.Join(root, "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	spec := string(raw)

	if !strings.HasPrefix(spec, "openapi: 3.0.3") {
		t.Error("document does not declare an OpenAPI version")
	}
	for _, e := range bb.Blueprint.Entities {
		if e.Name == "User" {
			continue
		}
		// Paths are kebab-case, matching the routes the router actually
		// mounts. Asserting on e.Plural directly would pass for
		// single-word entities and quietly diverge for multi-word ones.
		if !strings.Contains(spec, "/"+routePathForTest(e)+":") {
			t.Errorf("openapi is missing the collection path for %s", e.Name)
		}
		if !strings.Contains(spec, "$ref: '#/components/schemas/"+e.Name+"'") {
			t.Errorf("openapi is missing a schema reference for %s", e.Name)
		}
	}
	// A password hash must never appear in a public contract.
	if strings.Contains(spec, "password_hash") {
		t.Error("openapi exposes password_hash")
	}
}

func TestGeneratedFrontendReferencesRealTypes(t *testing.T) {
	bb, root, _ := runAgentPipeline(t, "Build an Airbnb-like marketplace")

	types, err := os.ReadFile(filepath.Join(root, "web", "src", "lib", "types.ts"))
	if err != nil {
		t.Fatalf("read types: %v", err)
	}
	for _, e := range bb.Blueprint.Entities {
		if !strings.Contains(string(types), "export interface "+e.Name+" {") {
			t.Errorf("types.ts is missing an interface for %s", e.Name)
		}
	}
	if strings.Contains(string(types), "password_hash") {
		t.Error("types.ts exposes password_hash to the client")
	}

	// Every screen must have a page component and be routed.
	app, err := os.ReadFile(filepath.Join(root, "web", "src", "App.tsx"))
	if err != nil {
		t.Fatalf("read App.tsx: %v", err)
	}
	for _, s := range bb.Blueprint.Screens {
		if !strings.Contains(string(app), `path="`+s.Route+`"`) {
			t.Errorf("App.tsx does not route %s", s.Route)
		}
	}
}

func TestPipelineIsReproducible(t *testing.T) {
	const prompt = "Build a CRM system"
	_, rootA, artifactsA := runAgentPipeline(t, prompt)
	_, rootB, artifactsB := runAgentPipeline(t, prompt)

	if len(artifactsA) != len(artifactsB) {
		t.Fatalf("artifact count differs between runs: %d vs %d", len(artifactsA), len(artifactsB))
	}

	// Content hashes must match: a deterministic generator that produces
	// different bytes for identical input cannot support artifact
	// deduplication or meaningful diffs between runs.
	hashesA := map[domain.ArtifactKind]string{}
	for _, a := range artifactsA {
		hashesA[a.Kind] = a.SHA256
	}
	for _, b := range artifactsB {
		if want, ok := hashesA[b.Kind]; ok && want != b.SHA256 {
			t.Errorf("artifact %s is not reproducible across runs", b.Kind)
		}
	}

	filesA := listRelative(t, rootA)
	filesB := listRelative(t, rootB)
	if len(filesA) != len(filesB) {
		t.Fatalf("file count differs: %d vs %d", len(filesA), len(filesB))
	}
	for i := range filesA {
		if filesA[i] != filesB[i] {
			t.Fatalf("file set differs at %d: %s vs %s", i, filesA[i], filesB[i])
		}
	}
}

func TestAgentChartersDeclareTheirOutputs(t *testing.T) {
	registry := factory.NewRegistry()
	roles := registry.Roles()
	if len(roles) != 11 {
		t.Fatalf("expected 11 agents, got %d", len(roles))
	}
	for _, role := range roles {
		agent, _ := registry.Get(role)
		charter := agent.Charter()
		if charter.Role != role {
			t.Errorf("agent registered as %s reports role %s", role, charter.Role)
		}
		if charter.Mission == "" {
			t.Errorf("agent %s has no mission", role)
		}
		if len(charter.Outputs) == 0 {
			t.Errorf("agent %s declares no outputs", role)
		}
		if len(charter.Tools) == 0 {
			t.Errorf("agent %s declares no tools; capability must be explicit", role)
		}
		if charter.Budget.MaxDuration <= 0 || charter.Budget.MaxTokens <= 0 {
			t.Errorf("agent %s has no budget; unbounded agents are a defect", role)
		}
	}
}

func TestAgentsFailFastWithoutRequiredInputs(t *testing.T) {
	root := t.TempDir()
	project := &domain.Project{ID: domain.NewID(), Name: "x", Slug: "x", Prompt: "build a crm", WorkspacePath: root}
	run, _ := domain.NewRun(project.ID, domain.NewID(), domain.RunBuild, nil, 0, time.Now())

	bb := factory.NewBlackboard(project, run)
	bb.Classification = factory.Classify(project.Prompt)
	bb.Blueprint = factory.BlueprintFor(bb.Classification.Category)

	tb := factory.NewWorkspaceToolbelt(root, domain.RolePM, nil, nil)
	registry := factory.NewRegistry()

	// The product manager depends on the vision; running it first must fail
	// loudly rather than silently emitting an unfounded document.
	pm, _ := registry.Get(domain.RolePM)
	if _, err := pm.Execute(context.Background(), bb, tb); err == nil {
		t.Fatal("product manager ran without a vision artifact")
	}

	backend, _ := registry.Get(domain.RoleBackend)
	if _, err := backend.Execute(context.Background(), bb, tb); err == nil {
		t.Fatal("backend engineer ran without a database schema")
	}
}

func listRelative(t *testing.T, root string) []string {
	t.Helper()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleSystem, nil, nil)
	files, err := tb.ListFiles(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return files
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestGeneratedGoParsesForEveryBlueprint compiles the generated Go source with
// the standard parser and type-checks imports for every blueprint.
//
// This exists because a defect found only in one category ships broken code for
// that category. The marketplace blueprint exposed exactly this: entities made
// solely of references and enums produced an unused "strings" import, which is
// a compile error in Go. Testing one category would never have caught it.
func TestGeneratedGoParsesForEveryBlueprint(t *testing.T) {
	for _, bp := range factory.Blueprints() {
		bp := bp
		t.Run(bp.Key, func(t *testing.T) {
			prompt := "Build a " + bp.Name
			_, root, _ := runAgentPipeline(t, prompt)

			var goFiles []string
			err := filepath.WalkDir(filepath.Join(root, "api"), func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && strings.HasSuffix(path, ".go") {
					goFiles = append(goFiles, path)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk generated source: %v", err)
			}
			if len(goFiles) < 5 {
				t.Fatalf("expected generated Go sources, found %d", len(goFiles))
			}

			for _, file := range goFiles {
				src, err := os.ReadFile(file)
				if err != nil {
					t.Fatalf("read %s: %v", file, err)
				}
				fset := token.NewFileSet()
				parsed, err := parser.ParseFile(fset, file, src, parser.AllErrors)
				if err != nil {
					rel, _ := filepath.Rel(root, file)
					t.Errorf("generated file %s does not parse: %v", rel, err)
					continue
				}

				// Every import must be referenced. Go rejects unused imports,
				// so an unreferenced one means the file cannot compile.
				for _, imp := range parsed.Imports {
					if imp.Name != nil && (imp.Name.Name == "_" || imp.Name.Name == ".") {
						continue
					}
					path := strings.Trim(imp.Path.Value, `"`)
					if !identUsed(parsed, importQualifier(path, imp.Name)) {
						rel, _ := filepath.Rel(root, file)
						t.Errorf("generated file %s imports %q but never uses it (this will not compile)", rel, path)
					}
				}
			}
		})
	}
}

// importQualifier resolves the identifier an import is referenced by.
//
// The last path segment is not reliable: a versioned module path such as
// ".../fiber/v2" declares package "fiber", not "v2". Guessing wrong turns this
// check into a false-positive generator, so version suffixes are stripped and
// an explicit alias always wins.
func importQualifier(path string, alias *ast.Ident) string {
	if alias != nil {
		return alias.Name
	}
	segments := strings.Split(path, "/")
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if seg == "" {
			continue
		}
		// Skip a major-version element like "v2".
		if len(seg) > 1 && seg[0] == 'v' && strings.IndexFunc(seg[1:], func(r rune) bool {
			return r < '0' || r > '9'
		}) == -1 {
			continue
		}
		return seg
	}
	return path
}

// identUsed reports whether a package qualifier appears in a selector anywhere
// in the file body.
func identUsed(file *ast.File, pkg string) bool {
	used := false
	ast.Inspect(file, func(n ast.Node) bool {
		if used {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == pkg {
			used = true
			return false
		}
		return true
	})
	return used
}

// TestGeneratedProjectCompilesAndPasses is the strongest guarantee this suite
// can make: it invokes the real Go toolchain on a generated project and runs
// the tests the factory wrote.
//
// Parsing proves syntax; only compiling proves the code is valid, and only
// running the tests proves the generated assertions are satisfiable. This
// caught a generated test that failed by construction on entities with no
// required fields.
//
// Skipped when the toolchain or the module proxy is unavailable, because a
// hermetic CI without network access should not report a false failure.
func TestGeneratedProjectCompilesAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping toolchain test in short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}

	_, root, _ := runAgentPipeline(t, "Build an Airbnb-like marketplace with listings and payments")
	apiDir := filepath.Join(root, "api")

	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, goBin, args...)
		cmd.Dir = apiDir
		// GOWORK=off isolates the generated module from this repository's
		// workspace, which would otherwise refuse to resolve it.
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run("mod", "tidy"); err != nil {
		t.Skipf("cannot resolve dependencies (offline?): %v\n%s", err, out)
	}

	if out, err := run("build", "./..."); err != nil {
		t.Fatalf("generated project does not compile:\n%s", out)
	}

	out, err := run("test", "./...")
	if err != nil {
		t.Fatalf("generated tests fail:\n%s", out)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("expected passing generated tests, got:\n%s", out)
	}
}

// routePathForTest mirrors the generator's URL convention. It is duplicated
// here rather than exported because the test must fail if the generator's
// convention changes silently — a shared helper would move in lockstep and
// assert nothing.
func routePathForTest(e factory.Entity) string {
	var out strings.Builder
	for i, r := range e.Plural {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(r + 32)
			continue
		}
		if r == '_' {
			out.WriteByte('-')
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}
