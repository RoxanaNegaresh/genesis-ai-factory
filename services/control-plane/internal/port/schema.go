package port

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Validator checks a decoded JSON document against a JSON Schema subset.
//
// This lives in the port layer rather than in an infrastructure adapter because
// it is pure logic with no external dependency, and because the agent runtime
// in the application layer must be able to use it without importing infra —
// the dependency rule points inward, and this is genuinely an inner concern.
//
// Why a hand-written validator rather than a dependency: constrained decoding
// already guarantees well-formed JSON matching the grammar for providers that
// support it, so this is the *second* line of defence — it must run on every
// response, including from providers with no grammar support, and it must
// produce error messages a model can act on during repair. Generic validators
// emit machine-oriented pointers ("/properties/epics/items/2"); repair works far
// better with prose the model can follow. The supported subset is exactly what
// agent schemas use, and unknown keywords are ignored rather than rejected.
type Validator struct {
	schema map[string]any
}

// NewValidator compiles a schema document.
func NewValidator(raw json.RawMessage) (*Validator, error) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("invalid JSON schema: %w", err)
	}
	return &Validator{schema: schema}, nil
}

// ValidationIssue is one problem with a document.
type ValidationIssue struct {
	Path    string
	Problem string
}

func (i ValidationIssue) String() string {
	if i.Path == "" {
		return i.Problem
	}
	return i.Path + ": " + i.Problem
}

// ValidationError aggregates every problem found in one pass.
//
// Reporting all issues at once matters for repair: fixing one error per round
// trip would multiply latency by the number of mistakes.
type ValidationError struct {
	Issues []ValidationIssue
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		parts = append(parts, issue.String())
	}
	return "schema validation failed: " + strings.Join(parts, "; ")
}

// Instructions renders the issues as a correction prompt for the model.
func (e *ValidationError) Instructions() string {
	var sb strings.Builder
	sb.WriteString("Your previous response did not satisfy the required schema. ")
	sb.WriteString("Fix exactly these problems and return the complete corrected JSON:\n")
	for _, issue := range e.Issues {
		if issue.Path == "" {
			fmt.Fprintf(&sb, "- %s\n", issue.Problem)
			continue
		}
		fmt.Fprintf(&sb, "- at `%s`: %s\n", issue.Path, issue.Problem)
	}
	sb.WriteString("Return only the JSON document, with no commentary.")
	return sb.String()
}

// Validate checks a document, returning nil or a *ValidationError.
func (v *Validator) Validate(document any) error {
	issues := v.check("", v.schema, document)
	if len(issues) == 0 {
		return nil
	}
	// Deterministic ordering keeps repair prompts stable, which in turn keeps
	// caching and test assertions meaningful.
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Path != issues[j].Path {
			return issues[i].Path < issues[j].Path
		}
		return issues[i].Problem < issues[j].Problem
	})
	if len(issues) > 25 {
		issues = issues[:25]
	}
	return &ValidationError{Issues: issues}
}

// ValidateJSON parses and validates raw bytes.
func (v *Validator) ValidateJSON(raw []byte) (map[string]any, error) {
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, &ValidationError{Issues: []ValidationIssue{{
			Problem: "the response is not valid JSON: " + err.Error(),
		}}}
	}
	if err := v.Validate(document); err != nil {
		return nil, err
	}
	object, _ := document.(map[string]any)
	return object, nil
}

func (v *Validator) check(path string, schema map[string]any, value any) []ValidationIssue {
	var issues []ValidationIssue

	if enum, ok := schema["enum"].([]any); ok {
		if !containsValue(enum, value) {
			issues = append(issues, ValidationIssue{path, fmt.Sprintf(
				"must be one of %s", formatList(enum))})
			return issues
		}
	}

	declared, hasType := schema["type"].(string)
	if hasType {
		if problem := checkType(declared, value); problem != "" {
			return append(issues, ValidationIssue{path, problem})
		}
	}

	switch declared {
	case "object":
		issues = append(issues, v.checkObject(path, schema, value)...)
	case "array":
		issues = append(issues, v.checkArray(path, schema, value)...)
	case "string":
		issues = append(issues, checkString(path, schema, value)...)
	case "integer", "number":
		issues = append(issues, checkNumber(path, schema, value)...)
	}
	return issues
}

func (v *Validator) checkObject(path string, schema map[string]any, value any) []ValidationIssue {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	var issues []ValidationIssue

	properties, _ := schema["properties"].(map[string]any)

	if required, ok := schema["required"].([]any); ok {
		for _, item := range required {
			name, _ := item.(string)
			if name == "" {
				continue
			}
			if _, present := object[name]; !present {
				issues = append(issues, ValidationIssue{
					join(path, name), "this required field is missing"})
			}
		}
	}

	// additionalProperties:false is how agent schemas prevent a model from
	// inventing fields that downstream code will silently ignore.
	if allowed, ok := schema["additionalProperties"].(bool); ok && !allowed && properties != nil {
		for name := range object {
			if _, declared := properties[name]; !declared {
				issues = append(issues, ValidationIssue{
					join(path, name), "this field is not allowed by the schema"})
			}
		}
	}

	for name, rawSub := range properties {
		child, present := object[name]
		if !present {
			continue
		}
		sub, ok := rawSub.(map[string]any)
		if !ok {
			continue
		}
		issues = append(issues, v.check(join(path, name), sub, child)...)
	}
	return issues
}

func (v *Validator) checkArray(path string, schema map[string]any, value any) []ValidationIssue {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var issues []ValidationIssue

	if min, ok := numberOf(schema["minItems"]); ok && float64(len(items)) < min {
		issues = append(issues, ValidationIssue{path, fmt.Sprintf(
			"must contain at least %d items, but has %d", int(min), len(items))})
	}
	if max, ok := numberOf(schema["maxItems"]); ok && float64(len(items)) > max {
		issues = append(issues, ValidationIssue{path, fmt.Sprintf(
			"must contain at most %d items, but has %d", int(max), len(items))})
	}

	if sub, ok := schema["items"].(map[string]any); ok {
		for i, item := range items {
			issues = append(issues, v.check(fmt.Sprintf("%s[%d]", path, i), sub, item)...)
		}
	}
	return issues
}

func checkString(path string, schema map[string]any, value any) []ValidationIssue {
	s, ok := value.(string)
	if !ok {
		return nil
	}
	var issues []ValidationIssue

	if min, ok := numberOf(schema["minLength"]); ok && float64(len([]rune(s))) < min {
		issues = append(issues, ValidationIssue{path, fmt.Sprintf(
			"must be at least %d characters, but is %d", int(min), len([]rune(s)))})
	}
	if max, ok := numberOf(schema["maxLength"]); ok && float64(len([]rune(s))) > max {
		issues = append(issues, ValidationIssue{path, fmt.Sprintf(
			"must be at most %d characters, but is %d", int(max), len([]rune(s)))})
	}
	return issues
}

func checkNumber(path string, schema map[string]any, value any) []ValidationIssue {
	n, ok := numberOf(value)
	if !ok {
		return nil
	}
	var issues []ValidationIssue

	if min, ok := numberOf(schema["minimum"]); ok && n < min {
		issues = append(issues, ValidationIssue{path, fmt.Sprintf("must be at least %v", min)})
	}
	if max, ok := numberOf(schema["maximum"]); ok && n > max {
		issues = append(issues, ValidationIssue{path, fmt.Sprintf("must be at most %v", max)})
	}
	return issues
}

func checkType(declared string, value any) string {
	switch declared {
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return "must be an object, but is " + describe(value)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return "must be an array, but is " + describe(value)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return "must be a string, but is " + describe(value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return "must be a boolean, but is " + describe(value)
		}
	case "number":
		if _, ok := numberOf(value); !ok {
			return "must be a number, but is " + describe(value)
		}
	case "integer":
		n, ok := numberOf(value)
		if !ok {
			return "must be an integer, but is " + describe(value)
		}
		if n != math.Trunc(n) {
			return fmt.Sprintf("must be a whole number, but is %v", n)
		}
	case "null":
		if value != nil {
			return "must be null"
		}
	}
	return ""
}

func describe(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64, int, int64:
		return "a number"
	case []any:
		return fmt.Sprintf("an array of %d items", len(v))
	case map[string]any:
		return "an object"
	}
	return "an unexpected value"
}

func numberOf(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func containsValue(options []any, value any) bool {
	for _, option := range options {
		if fmt.Sprint(option) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

func formatList(options []any) string {
	parts := make([]string, 0, len(options))
	for _, option := range options {
		parts = append(parts, fmt.Sprintf("%q", fmt.Sprint(option)))
	}
	return strings.Join(parts, ", ")
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// ExtractJSON recovers a JSON document from a model response.
//
// Even with constrained decoding some providers wrap output in prose or a
// markdown fence. Recovering the document is strictly better than failing the
// whole agent over formatting the model was never asked about.
func ExtractJSON(response string) string {
	trimmed := strings.TrimSpace(response)

	if fenced := extractFenced(trimmed); fenced != "" {
		trimmed = fenced
	}

	start := strings.IndexAny(trimmed, "{[")
	if start < 0 {
		return trimmed
	}

	open := trimmed[start]
	close := byte('}')
	if open == '[' {
		close = ']'
	}

	// Scan for the matching delimiter, honouring string literals and escapes so
	// a brace inside a string cannot terminate the scan early.
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(trimmed); i++ {
		c := trimmed[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// nothing
		case c == open:
			depth++
		case c == close:
			depth--
			if depth == 0 {
				return trimmed[start : i+1]
			}
		}
	}
	return trimmed[start:]
}

func extractFenced(s string) string {
	const fence = "```"
	start := strings.Index(s, fence)
	if start < 0 {
		return ""
	}
	rest := s[start+len(fence):]
	if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
		// Drop an optional language tag such as ```json.
		if tag := strings.TrimSpace(rest[:newline]); tag == "" || !strings.ContainsAny(tag, " {[\"") {
			rest = rest[newline+1:]
		}
	}
	if end := strings.Index(rest, fence); end >= 0 {
		return strings.TrimSpace(rest[:end])
	}
	return strings.TrimSpace(rest)
}
