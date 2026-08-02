package factory_test

import (
	"context"
	"encoding/json"
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

func spec() factory.FunctionSpec {
	return factory.FunctionSpec{
		Name:      "Validate",
		Receiver:  "m *Deal",
		Signature: "() error",
		Doc:       "Validate checks the deal invariants.",
		Purpose:   "reject deals with a non-positive value",
		Fallback:  "return nil",
		Imports:   []string{"strings"},
	}
}

// The central safety property: a model must never be able to break the build.
func TestValidateBodyAcceptsCompilingCode(t *testing.T) {
	body := `v := NewValidation()
if strings.TrimSpace(m.Title) == "" {
	v.Add("title", "is required")
}
return v.Err()`

	if err := factory.ValidateBody(spec(), body); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
}

func TestValidateBodyRejectsSyntaxErrors(t *testing.T) {
	broken := map[string]string{
		"unbalanced brace":    "if m.Value <= 0 {\n\treturn errors.New(\"bad\")\n",
		"prose":               "Sure! Here is the implementation you asked for.",
		"unterminated string": `return errors.New("oops`,
		"stray keyword":       "return nil\n}\nfunc extra() {}",
	}
	for name, body := range broken {
		if err := factory.ValidateBody(spec(), body); err == nil {
			t.Errorf("%s: malformed body was accepted", name)
		}
	}
}

func TestValidateBodyRejectsForbiddenConstructs(t *testing.T) {
	cases := map[string]string{
		"panic":     `panic("unreachable")`,
		"os.Exit":   "os.Exit(1)",
		"imports":   "import \"fmt\"\nreturn nil",
		"package":   "package main\nreturn nil",
		"goroutine": "go func() { _ = m }()\nreturn nil",
		"exec":      `exec.Command("rm", "-rf", "/")`,
		"unsafe":    "_ = unsafe.Pointer(m)\nreturn nil",
	}
	for name, body := range cases {
		err := factory.ValidateBody(spec(), body)
		if err == nil {
			t.Errorf("%s: forbidden construct was accepted", name)
			continue
		}
		// The rejection must explain itself; a bare "invalid" teaches nobody.
		if len(err.Error()) < 20 {
			t.Errorf("%s: unhelpful rejection message %q", name, err)
		}
	}
}

func TestValidateBodyRejectsEmptyAndOversized(t *testing.T) {
	if err := factory.ValidateBody(spec(), "   \n  "); err == nil {
		t.Error("empty body accepted")
	}

	huge := strings.Repeat("_ = m\n", 200)
	err := factory.ValidateBody(spec(), huge)
	if err == nil {
		t.Fatal("oversized body accepted")
	}
	if !strings.Contains(err.Error(), "line limit") {
		t.Errorf("rejection should name the limit: %v", err)
	}
}

func TestValidateBodyRejectsWrongFunction(t *testing.T) {
	// A body that redefines something else would silently replace the wrong
	// declaration, which is worse than a compile error because it may compile.
	body := "return nil"
	other := spec()
	other.Name = "Validate"

	wrapped := "func somethingElse() error {\n\treturn nil\n}"
	if err := factory.ValidateBody(other, wrapped); err == nil {
		t.Fatal("a body defining a different function was accepted")
	}
	_ = body
}

func TestRenderProducesParseableDeclaration(t *testing.T) {
	rendered := spec().Render("v := NewValidation()\nreturn v.Err()")

	if !strings.Contains(rendered, "func (m *Deal) Validate() error {") {
		t.Fatalf("signature not rendered correctly:\n%s", rendered)
	}
	if !strings.Contains(rendered, "// Validate checks the deal invariants.") {
		t.Fatal("doc comment missing")
	}

	source := "package p\n" + rendered
	if _, err := parser.ParseFile(token.NewFileSet(), "x.go", source, 0); err != nil {
		t.Fatalf("rendered declaration does not parse: %v\n%s", err, rendered)
	}
}

func TestGoFileIsFormattedAndStable(t *testing.T) {
	file := factory.GoFile{
		Package: "domain",
		Doc:     "Package domain holds entities.",
		Imports: []string{"strings", "time"},
		Decls: []string{
			"type Deal struct {\nID string\nValue string\n}",
			"func (d *Deal) Empty() bool { return d.ID == \"\" }",
		},
	}

	rendered := file.String()
	if _, err := parser.ParseFile(token.NewFileSet(), "x.go", rendered, 0); err != nil {
		t.Fatalf("generated file does not parse: %v\n%s", err, rendered)
	}

	// gofmt output is canonical, so identical input must yield identical bytes.
	// Artifacts are content-addressed; unstable formatting would defeat dedup.
	if rendered != file.String() {
		t.Fatal("file rendering is not deterministic")
	}
	// Struct fields must have been aligned by gofmt, proving formatting ran.
	if !strings.Contains(rendered, "ID    string") {
		t.Fatalf("output was not gofmt-formatted:\n%s", rendered)
	}
}

func TestGoFileSurvivesUnparseableInput(t *testing.T) {
	// A malformed declaration must be emitted rather than swallowed, so the
	// defect is visible in the generated file instead of vanishing.
	file := factory.GoFile{Package: "p", Decls: []string{"func broken( {"}}
	rendered := file.String()
	if !strings.Contains(rendered, "func broken(") {
		t.Fatal("unparseable declaration was silently dropped")
	}
}

func TestRenderRuleCheckGeneratesCompilingCode(t *testing.T) {
	cases := []struct {
		rule  factory.BusinessRule
		field factory.Field
		want  string
	}{
		{
			factory.BusinessRule{Entity: "Deal", Field: "value", Rule: "positive", Message: "must be greater than zero"},
			factory.Field{Name: "value", Type: "decimal", Required: true},
			"strconv.ParseFloat",
		},
		{
			factory.BusinessRule{Entity: "Deal", Field: "quantity", Rule: "non_negative", Message: "cannot be negative"},
			factory.Field{Name: "quantity", Type: "int", Required: true},
			"< 0",
		},
		{
			factory.BusinessRule{Entity: "Deal", Field: "close_date", Rule: "future_date", Message: "must be in the future"},
			factory.Field{Name: "close_date", Type: "timestamp", Required: true},
			"Before(time.Now())",
		},
		{
			factory.BusinessRule{Entity: "Contact", Field: "email", Rule: "email", Message: "must be a valid address"},
			factory.Field{Name: "email", Type: "text", Required: true},
			"strings.Contains",
		},
	}

	for _, tc := range cases {
		check := factory.RenderRuleCheck(tc.rule, tc.field)
		if check == "" {
			t.Errorf("%s/%s produced no check", tc.rule.Entity, tc.rule.Rule)
			continue
		}
		if !strings.Contains(check, tc.want) {
			t.Errorf("%s check missing %q:\n%s", tc.rule.Rule, tc.want, check)
		}

		// Every rendered rule must compile inside a validation method.
		source := "package p\n" +
			"import (\n\"strconv\"\n\"strings\"\n\"time\"\n)\n" +
			"type m2 struct{}\n" +
			"func check(m *Deal, v *Validation) {\n" + check + "\n}\n" +
			"type Deal struct{ Value string; Quantity int64; CloseDate time.Time; Email string }\n" +
			"type Validation struct{}\nfunc (v *Validation) Add(a, b string) {}\n" +
			"var _ = strconv.Itoa\nvar _ = strings.TrimSpace\n"
		if _, err := parser.ParseFile(token.NewFileSet(), "x.go", source, 0); err != nil {
			t.Errorf("%s check does not parse: %v\n%s", tc.rule.Rule, err, check)
		}
	}
}

// Regression: `*m.Field.IsZero()` parses as `*(m.Field.IsZero())`, which
// dereferences a bool. Every optional-field rule must parenthesise.
func TestRenderRuleCheckParenthesisesDereference(t *testing.T) {
	check := factory.RenderRuleCheck(
		factory.BusinessRule{Entity: "Deal", Field: "close_date", Rule: "future_date", Message: "must be in the future"},
		factory.Field{Name: "close_date", Type: "timestamp", Required: false},
	)
	if strings.Contains(check, "*m.CloseDate.") {
		t.Fatalf("unparenthesised dereference before a method call:\n%s", check)
	}
	if !strings.Contains(check, "(*m.CloseDate)") {
		t.Fatalf("dereference is not parenthesised:\n%s", check)
	}

	source := "package p\nimport \"time\"\n" +
		"type Deal struct{ CloseDate *time.Time }\n" +
		"type Validation struct{}\nfunc (v *Validation) Add(a, b string) {}\n" +
		"func check(m *Deal, v *Validation) {\n" + check + "\n}\n"
	if _, err := parser.ParseFile(token.NewFileSet(), "x.go", source, 0); err != nil {
		t.Fatalf("optional timestamp rule does not parse: %v\n%s", err, check)
	}
}

func TestRenderRuleCheckGuardsOptionalFields(t *testing.T) {
	// An optional field is a pointer; dereferencing it unguarded is a nil panic
	// waiting for the first record that omits it.
	check := factory.RenderRuleCheck(
		factory.BusinessRule{Entity: "Deal", Field: "probability", Rule: "non_negative", Message: "cannot be negative"},
		factory.Field{Name: "probability", Type: "int", Required: false},
	)
	if !strings.Contains(check, "if m.Probability != nil {") {
		t.Fatalf("optional field is not nil-guarded:\n%s", check)
	}
	if !strings.Contains(check, "*m.Probability") {
		t.Fatalf("optional field is not dereferenced:\n%s", check)
	}
}

func TestRenderRuleCheckIgnoresTypeMismatches(t *testing.T) {
	// A model may propose "positive" on a text field. Emitting nothing is
	// correct; emitting a comparison that does not compile is not.
	check := factory.RenderRuleCheck(
		factory.BusinessRule{Entity: "Deal", Field: "title", Rule: "positive", Message: "x"},
		factory.Field{Name: "title", Type: "text", Required: true},
	)
	if check != "" {
		t.Fatalf("expected no check for a type mismatch, got:\n%s", check)
	}
}

// The decisive v0.3 test: model-derived domain rules must produce a project
// that still compiles and whose tests still pass.
//
// This is the whole risk of letting a model influence code generation. A rule
// referencing a mistyped field, or an optional field dereferenced without a nil
// check, would break every generated project for that category.
func TestModelDerivedRulesProduceCompilingCode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping toolchain test in short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}

	// A realistic rule set covering every supported rule type, every field
	// type, and both required and optional fields.
	rules := []factory.BusinessRule{
		{Entity: "Deal", Field: "value", Rule: "positive", Message: "deal value must be greater than zero"},
		{Entity: "Deal", Field: "probability", Rule: "non_negative", Message: "probability cannot be negative"},
		{Entity: "Deal", Field: "expected_close_date", Rule: "future_date", Message: "the close date must be in the future"},
		{Entity: "Contact", Field: "email", Rule: "email", Message: "must be a valid email address"},
		{Entity: "Contact", Field: "first_name", Rule: "min_length", Message: "a first name is required"},
		// A rule referencing a field that does not exist must be ignored rather
		// than generating a reference to a missing struct member.
		{Entity: "Deal", Field: "nonexistent_field", Rule: "positive", Message: "must be ignored by the generator"},
		// A rule whose type does not support it must also be skipped.
		{Entity: "Deal", Field: "title", Rule: "positive", Message: "must be ignored by the generator"},
	}

	root := t.TempDir()
	bb, tb := blackboardWithRules(t, root, rules)

	backend, ok := factory.NewRegistry().Get(domain.RoleBackend)
	if !ok {
		t.Fatal("backend agent missing")
	}
	if _, err := backend.Execute(context.Background(), bb, tb); err != nil {
		t.Fatalf("backend agent failed: %v", err)
	}

	apiDir := filepath.Join(root, "api")
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, goBin, args...)
		cmd.Dir = apiDir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run("mod", "tidy"); err != nil {
		t.Skipf("cannot resolve dependencies (offline?): %v\n%s", err, out)
	}
	if out, err := run("build", "./..."); err != nil {
		t.Fatalf("project with model-derived rules does not compile:\n%s", out)
	}
	if out, err := run("test", "./internal/domain/"); err != nil {
		t.Fatalf("generated domain tests fail with derived rules:\n%s", out)
	}

	// The rules must actually be present, not silently dropped.
	deal, err := os.ReadFile(filepath.Join(apiDir, "internal", "domain", "deal.go"))
	if err != nil {
		t.Fatalf("read generated entity: %v", err)
	}
	source := string(deal)
	if !strings.Contains(source, "deal value must be greater than zero") {
		t.Fatal("the derived positive-value rule did not reach the generated code")
	}
	if !strings.Contains(source, "if m.Probability != nil {") {
		t.Fatal("the optional field rule is not nil-guarded")
	}
	if strings.Contains(source, "nonexistent_field") {
		t.Fatal("a rule referencing an unknown field was emitted")
	}
}

// blackboardWithRules prepares a CRM blackboard whose PRD is already present,
// with a scripted model that returns the supplied rules.
func blackboardWithRules(t *testing.T, root string, rules []factory.BusinessRule) (*factory.Blackboard, factory.Toolbelt) {
	t.Helper()

	payload := map[string]any{"rules": rules}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode rules: %v", err)
	}

	client := &fakeLLM{responses: []string{string(encoded)}}

	project := &domain.Project{
		ID: domain.NewID(), OwnerID: domain.NewID(),
		Name: "Solar CRM", Slug: "solar-crm",
		Prompt:        "Build a CRM for a solar panel installation company",
		WorkspacePath: root, Settings: domain.DefaultProjectSettings(),
	}
	run, err := domain.NewRun(project.ID, project.OwnerID, domain.RunBuild, nil, 500000, time.Now())
	if err != nil {
		t.Fatalf("new run: %v", err)
	}

	bb := factory.NewBlackboard(project, run)
	bb.Classification = factory.Classify(project.Prompt)
	bb.Blueprint = factory.BlueprintFor(bb.Classification.Category)
	bb.Reasoning = factory.NewReasoningForTest(client, nil, 500000)

	// The backend agent requires the schema and reads the PRD.
	bb.Put(domain.NewArtifact(project.ID, run.ID, nil, domain.ArtifactDBSchema,
		"DATA_MODEL.md", "text/markdown", "# Data Model\n\nSchema present.", time.Now()))
	bb.Put(domain.NewArtifact(project.ID, run.ID, nil, domain.ArtifactPRD,
		"PRD.md", "text/markdown", "# PRD\n\nDeals must have a positive value.", time.Now()))

	tb := factory.NewWorkspaceToolbelt(root, domain.RoleBackend, nil, nil)
	return bb, tb
}

// Regression from a real run: the model returned acceptance criteria as
// validation messages, producing "email: Validates every required field and
// returns 201 with the created record" next to a form field.
func TestValidationMessagesMustBeUserFacing(t *testing.T) {
	rejected := []string{
		"Validates every required field and returns 201 with the created record",
		"GET /api/v1/deals supports pagination, sorting and filtering",
		"As a sales rep, I want to see my pipeline",
		"POST /api/v1/leads returns 422 on invalid input",
		"",
		strings.Repeat("word ", 20),
	}
	for _, message := range rejected {
		if factory.PlausibleValidationMessageForTest(message) {
			t.Errorf("accepted a message that is not validation copy: %q", message)
		}
	}

	accepted := []string{
		"must be greater than zero",
		"the close date must be in the future",
		"must be a valid email address",
		"cannot be negative",
	}
	for _, message := range accepted {
		if !factory.PlausibleValidationMessageForTest(message) {
			t.Errorf("rejected legitimate validation copy: %q", message)
		}
	}
}
