package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// logicSchema constrains a batch of generated function bodies.
//
// Bodies are requested in one batch rather than one call each. On CPU inference
// a round trip costs tens of seconds, and the prompt context (the entity, its
// fields, the house rules) is identical for every function in a file — paying
// that cost once per file instead of once per function is the difference
// between a usable build and an unusable one.
var logicSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["functions"],
  "properties": {
    "functions": {
      "type": "array", "minItems": 1, "maxItems": 6,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "body"],
        "properties": {
          "name": {"type": "string", "minLength": 2, "maxLength": 60},
          "body": {"type": "string", "minLength": 10, "maxLength": 4000},
          "note": {"type": "string", "maxLength": 200}
        }
      }
    }
  }
}`)

// BusinessRule is a domain constraint the model derives from the requirements
// and implements as a validation clause.
type BusinessRule struct {
	Entity  string `json:"entity"`
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// rulesSchema constrains derived business rules.
var rulesSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["rules"],
  "properties": {
    "rules": {
      "type": "array", "minItems": 1, "maxItems": 12,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["entity", "field", "rule", "message"],
        "properties": {
          "entity": {"type": "string", "minLength": 2, "maxLength": 40},
          "field": {"type": "string", "minLength": 1, "maxLength": 40},
          "rule": {"type": "string", "enum": [
            "required", "positive", "non_negative", "future_date", "past_date",
            "email", "url", "unique", "max_length", "min_length", "range"
          ]},
          "message": {"type": "string", "minLength": 8, "maxLength": 160}
        }
      }
    }
  }
}`)

// LogicGenerator asks a model to author business-logic bodies, validating every
// result before it reaches disk.
type LogicGenerator struct {
	reasoning *Reasoning
	// rejected records why bodies were discarded, so the run can report the
	// truth about how much of the code was model-authored.
	rejected []BodyRejection
	accepted int
}

// NewLogicGenerator constructs a generator. A nil reasoning layer is valid and
// means every function uses its fallback.
func NewLogicGenerator(reasoning *Reasoning) *LogicGenerator {
	return &LogicGenerator{reasoning: reasoning}
}

// Enabled reports whether model generation will be attempted.
func (g *LogicGenerator) Enabled() bool { return g != nil && g.reasoning.Enabled() }

// Stats reports how many bodies were accepted and rejected.
func (g *LogicGenerator) Stats() (accepted int, rejected []BodyRejection) {
	if g == nil {
		return 0, nil
	}
	return g.accepted, g.rejected
}

// Generate requests bodies for a batch of specs and returns a name→body map
// containing only bodies that parsed and passed the rules. Callers use their
// fallback for anything absent, so a partial result is always usable.
func (g *LogicGenerator) Generate(
	ctx context.Context,
	tb Toolbelt,
	role domain.AgentRole,
	subject string,
	contextText string,
	specs []FunctionSpec,
) map[string]string {
	accepted := map[string]string{}
	if !g.Enabled() || len(specs) == 0 {
		return accepted
	}

	var request strings.Builder
	request.WriteString("Write the body of each function listed below. ")
	request.WriteString("Return the statements only: no signature, no braces around the whole body, no package or import lines.\n\n")
	for _, spec := range specs {
		fmt.Fprintf(&request, "### %s\n", spec.Name)
		if spec.Receiver != "" {
			fmt.Fprintf(&request, "Receiver: `%s`\n", spec.Receiver)
		}
		fmt.Fprintf(&request, "Signature: `func %s%s`\n", spec.Name, spec.Signature)
		fmt.Fprintf(&request, "Purpose: %s\n\n", spec.Purpose)
	}

	prompt := NewPrompt(g.reasoning.PromptBudget(ctx)).
		Add("Subject", contextText, 0).
		Add("Functions to implement", request.String(), 0).
		Add("Rules", `- Use only the packages already imported by the file. Do not add imports.
- Return errors; never panic, never call os.Exit.
- Keep each body under 40 lines.
- Do not write comments explaining what the code obviously does.
- If a function cannot be meaningfully implemented from the information given,
  return a body that is a correct minimal implementation rather than a guess.`, 1)

	document := g.reasoning.think(ctx, tb, role,
		fmt.Sprintf("business logic for %s", subject),
		houseStyle+"\n\nYou are a senior Go engineer. You write correct, compiling, idiomatic code and nothing else.",
		prompt.String(), "generated_logic", logicSchema, port.ClassCode, 0.1)
	if document == nil {
		return accepted
	}

	byName := map[string]FunctionSpec{}
	for _, spec := range specs {
		byName[spec.Name] = spec
	}

	for _, raw := range objectSlice(document, "functions") {
		name := stringField(raw, "name")
		body := stringField(raw, "body")

		spec, known := byName[name]
		if !known {
			// A body for a function nobody asked for cannot be placed anywhere.
			g.rejected = append(g.rejected, BodyRejection{name, "no such function was requested"})
			continue
		}

		body = stripFunctionWrapper(body, spec)
		if err := ValidateBody(spec, body); err != nil {
			var rejection BodyRejection
			if ok := asRejection(err, &rejection); ok {
				g.rejected = append(g.rejected, rejection)
			} else {
				g.rejected = append(g.rejected, BodyRejection{name, err.Error()})
			}
			continue
		}

		accepted[name] = body
		g.accepted++
	}

	if len(g.rejected) > 0 {
		tb.Emit(ctx, domain.LevelWarn,
			fmt.Sprintf("Rejected %d generated function bodies for %s; using safe defaults", len(g.rejected), subject),
			map[string]any{"rejected": rejectionSummary(g.rejected), "accepted": len(accepted)})
	}
	return accepted
}

func asRejection(err error, target *BodyRejection) bool {
	if r, ok := err.(BodyRejection); ok {
		*target = r
		return true
	}
	return false
}

func rejectionSummary(rejections []BodyRejection) []string {
	out := make([]string, 0, len(rejections))
	for _, r := range rejections {
		out = append(out, r.Error())
	}
	return out
}

// stripFunctionWrapper removes a signature and outer braces if the model
// included them despite being asked for statements only.
//
// Instructing a model not to do something is a request, not a guarantee. This
// is cheaper and more reliable than a repair round trip for a purely
// syntactic deviation.
func stripFunctionWrapper(body string, spec FunctionSpec) string {
	trimmed := strings.TrimSpace(body)

	// Strip a markdown fence if one survived extraction.
	if strings.HasPrefix(trimmed, "```") {
		if newline := strings.IndexByte(trimmed, '\n'); newline >= 0 {
			trimmed = trimmed[newline+1:]
		}
		trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
		trimmed = strings.TrimSpace(trimmed)
	}

	if !strings.HasPrefix(trimmed, "func ") {
		return trimmed
	}

	open := strings.Index(trimmed, "{")
	close := strings.LastIndex(trimmed, "}")
	if open < 0 || close <= open {
		return trimmed
	}

	inner := strings.TrimSpace(trimmed[open+1 : close])
	// Only accept the unwrapped form if it still mentions the function we asked
	// for, or has no signature line at all; otherwise the model rewrote
	// something else entirely and validation should reject it.
	if spec.Name != "" && !strings.Contains(trimmed[:open], spec.Name) {
		return trimmed
	}
	return dedent(inner)
}

// dedent removes the common leading indentation from a block.
func dedent(block string) string {
	lines := strings.Split(block, "\n")

	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		count := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent < 0 || count < indent {
			indent = count
		}
	}
	if indent <= 0 {
		return block
	}

	for i, line := range lines {
		if len(line) >= indent {
			lines[i] = line[indent:]
		} else {
			lines[i] = strings.TrimLeft(line, " \t")
		}
	}
	return strings.Join(lines, "\n")
}

// DeriveRules asks the model for domain constraints implied by the
// requirements but absent from the blueprint's structural schema.
//
// This is where product knowledge genuinely lives: a blueprint knows a Deal has
// a `value`, only the requirements imply that it must be positive and that a
// won deal needs a close date.
func (g *LogicGenerator) DeriveRules(
	ctx context.Context,
	tb Toolbelt,
	bb *Blackboard,
	requirements string,
) []BusinessRule {
	if !g.Enabled() {
		return nil
	}

	entities := make([]string, 0, len(bb.Blueprint.Entities))
	for _, e := range bb.Blueprint.Entities {
		fields := make([]string, 0, len(e.Fields))
		for _, f := range e.Fields {
			if isSystemField(f.Name) {
				continue
			}
			fields = append(fields, f.Name+" ("+f.Type+")")
		}
		entities = append(entities, fmt.Sprintf("%s: %s", e.Name, strings.Join(fields, ", ")))
	}

	prompt := NewPrompt(g.reasoning.PromptBudget(ctx)).
		Add("Product", briefContext(bb), 0).
		Add("Entities and fields", strings.Join(entities, "\n"), 0).
		Add("Requirements", requirements, 2).
		Add("Your task", `List the validation rules this product needs that are not obvious from the field types alone.

Only rules that follow from the product domain. Do not restate that a required field is required —
the generator already handles that. Focus on constraints a domain expert would insist on:
values that must be positive, dates that must be in the future, states that require other fields.

Every rule must name an entity and field that appear in the list above.`, 0)

	document := g.reasoning.think(ctx, tb, domain.RoleDatabase, "the domain validation rules",
		houseStyle+"\n\nYou are a domain expert reviewing a data model for missing constraints.",
		prompt.String(), "business_rules", rulesSchema, port.ClassReasoning, 0.2)
	if document == nil {
		return nil
	}

	// Rules referring to fields that do not exist would generate code that does
	// not compile, so they are filtered against the real schema.
	valid := map[string]map[string]bool{}
	for _, e := range bb.Blueprint.Entities {
		fields := map[string]bool{}
		for _, f := range e.Fields {
			fields[f.Name] = true
		}
		valid[e.Name] = fields
	}

	// The message becomes user-facing validation text. A model that echoes the
	// requirements into it produces "email: Validates every required field and
	// returns 201 with the created record" on a form field — observed in a real
	// run. Reject the batch rather than shipping nonsense to end users.
	messages := make([]string, 0, 12)
	for _, raw := range objectSlice(document, "rules") {
		messages = append(messages, stringField(raw, "message"))
	}
	if err := critique(messages); err != nil {
		tb.Emit(ctx, domain.LevelWarn,
			"Discarding derived rules: "+err.Error(), nil)
		return nil
	}
	if err := critiqueEcho(messages, requirements); err != nil {
		tb.Emit(ctx, domain.LevelWarn,
			"Discarding derived rules: "+err.Error(), nil)
		return nil
	}

	var rules []BusinessRule
	var dropped int
	for _, raw := range objectSlice(document, "rules") {
		rule := BusinessRule{
			Entity:  stringField(raw, "entity"),
			Field:   stringField(raw, "field"),
			Rule:    stringField(raw, "rule"),
			Message: stringField(raw, "message"),
		}
		fields, known := valid[rule.Entity]
		if !known || !fields[rule.Field] {
			dropped++
			continue
		}
		// A validation message is shown next to a form field. Sentences lifted
		// from an API specification are not that.
		if !plausibleValidationMessage(rule.Message) {
			dropped++
			continue
		}
		rules = append(rules, rule)
	}

	// Reporting the drop rate honestly matters: when a synthesised blueprint is
	// rejected the model reasons about the domain it proposed while the code is
	// generated from the fallback, so every rule can legitimately be discarded.
	// Silently returning nothing would look like the model failed.
	if dropped > 0 {
		level := domain.LevelDebug
		message := fmt.Sprintf("Dropped %d validation rules that referenced fields not in the schema", dropped)
		if len(rules) == 0 {
			level = domain.LevelWarn
			message = fmt.Sprintf(
				"All %d derived rules referenced entities outside the active blueprint; none were applied", dropped)
		}
		tb.Emit(ctx, level, message, map[string]any{"dropped": dropped, "kept": len(rules)})
	}
	return rules
}

// RulesFor returns the derived rules that apply to an entity.
func RulesFor(rules []BusinessRule, entity string) []BusinessRule {
	var out []BusinessRule
	for _, rule := range rules {
		if rule.Entity == entity {
			out = append(out, rule)
		}
	}
	return out
}

// RenderRuleCheck emits the Go validation clause for a derived rule.
//
// Code is generated from a closed set of rule types rather than from
// model-written text. The model decides *which* constraint applies where — a
// judgement call — while the compiler-facing output stays under our control and
// is guaranteed to compile.
func RenderRuleCheck(rule BusinessRule, field Field) string {
	name := goFieldName(rule.Field)
	optional := !field.Required && field.Type != "json"

	// A dereference must be parenthesised. Without it, `*m.Date.IsZero()`
	// parses as `*(m.Date.IsZero())` — dereferencing a bool — which is a
	// compile error the generator would otherwise emit into every project
	// with an optional timestamp rule.
	deref := "m." + name
	guard := ""
	if optional {
		guard = fmt.Sprintf("if m.%s != nil {\n", name)
		deref = "(*m." + name + ")"
	}

	var check string
	switch rule.Rule {
	case "positive":
		switch field.Type {
		case "int":
			check = fmt.Sprintf("if %s <= 0 {\n\tv.Add(%q, %q)\n}", deref, rule.Field, rule.Message)
		case "decimal":
			check = fmt.Sprintf("if parsed, err := strconv.ParseFloat(%s, 64); err == nil && parsed <= 0 {\n\tv.Add(%q, %q)\n}",
				deref, rule.Field, rule.Message)
		}
	case "non_negative":
		switch field.Type {
		case "int":
			check = fmt.Sprintf("if %s < 0 {\n\tv.Add(%q, %q)\n}", deref, rule.Field, rule.Message)
		case "decimal":
			check = fmt.Sprintf("if parsed, err := strconv.ParseFloat(%s, 64); err == nil && parsed < 0 {\n\tv.Add(%q, %q)\n}",
				deref, rule.Field, rule.Message)
		}
	case "future_date":
		if field.Type == "timestamp" {
			check = fmt.Sprintf("if !%s.IsZero() && %s.Before(time.Now()) {\n\tv.Add(%q, %q)\n}",
				deref, deref, rule.Field, rule.Message)
		}
	case "past_date":
		if field.Type == "timestamp" {
			check = fmt.Sprintf("if !%s.IsZero() && %s.After(time.Now()) {\n\tv.Add(%q, %q)\n}",
				deref, deref, rule.Field, rule.Message)
		}
	case "email":
		if field.Type == "text" {
			check = fmt.Sprintf("if %s != \"\" && !strings.Contains(%s, \"@\") {\n\tv.Add(%q, %q)\n}",
				deref, deref, rule.Field, rule.Message)
		}
	case "url":
		if field.Type == "text" {
			check = fmt.Sprintf("if %s != \"\" && !strings.HasPrefix(%s, \"http\") {\n\tv.Add(%q, %q)\n}",
				deref, deref, rule.Field, rule.Message)
		}
	case "max_length":
		if field.Type == "text" {
			check = fmt.Sprintf("if len(%s) > 2000 {\n\tv.Add(%q, %q)\n}", deref, rule.Field, rule.Message)
		}
	case "min_length":
		if field.Type == "text" {
			check = fmt.Sprintf("if len(strings.TrimSpace(%s)) < 2 {\n\tv.Add(%q, %q)\n}", deref, rule.Field, rule.Message)
		}
	}

	if check == "" {
		// The rule does not apply to this field's type. Silently skipping is
		// correct: the model proposed something the type system already covers.
		return ""
	}
	if guard != "" {
		return guard + indentLines(check, "\t") + "\n}"
	}
	return check
}

func indentLines(block, prefix string) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// ruleImports reports the extra imports a set of rendered rules requires.
func ruleImports(rules []BusinessRule, entity Entity) []string {
	fields := map[string]Field{}
	for _, f := range entity.Fields {
		fields[f.Name] = f
	}

	needed := map[string]bool{}
	for _, rule := range rules {
		field, ok := fields[rule.Field]
		if !ok {
			continue
		}
		switch rule.Rule {
		case "positive", "non_negative":
			if field.Type == "decimal" {
				needed["strconv"] = true
			}
		case "future_date", "past_date":
			if field.Type == "timestamp" {
				needed["time"] = true
			}
		case "email", "url", "min_length":
			if field.Type == "text" {
				needed["strings"] = true
			}
		}
	}

	var out []string
	for _, candidate := range []string{"strconv", "strings", "time"} {
		if needed[candidate] {
			out = append(out, candidate)
		}
	}
	return out
}

// RulesSchemaForTest exposes the rules schema to the external test package.
func RulesSchemaForTest() []byte { return rulesSchema }

// plausibleValidationMessage rejects text that is clearly not user-facing
// validation copy. The checks are narrow on purpose: the goal is to catch
// wholesale echoes of the specification, not to police wording.
func plausibleValidationMessage(message string) bool {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" || len(strings.Fields(trimmed)) > 14 {
		return false
	}
	lowered := strings.ToLower(trimmed)
	for _, tell := range []string{
		"returns 20", "http", "endpoint", "api/", "get ", "post ", "patch ", "delete ",
		"pagination", "acceptance criteria", "user story", "as a ",
	} {
		if strings.Contains(lowered, tell) {
			return false
		}
	}
	return true
}

// PlausibleValidationMessageForTest exposes the message gate to the test package.
func PlausibleValidationMessageForTest(message string) bool {
	return plausibleValidationMessage(message)
}
