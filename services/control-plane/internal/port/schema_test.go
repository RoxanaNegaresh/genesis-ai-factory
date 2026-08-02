package port_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/genesis-ai-factory/control-plane/internal/port"
)

func mustValidator(t *testing.T, schema string) *port.Validator {
	t.Helper()
	v, err := port.NewValidator(json.RawMessage(schema))
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return v
}

const personSchema = `{
  "type": "object",
  "required": ["name", "age", "tags"],
  "additionalProperties": false,
  "properties": {
    "name": {"type": "string", "minLength": 2, "maxLength": 40},
    "age": {"type": "integer", "minimum": 0, "maximum": 150},
    "role": {"type": "string", "enum": ["admin", "member"]},
    "tags": {"type": "array", "minItems": 1, "maxItems": 3, "items": {"type": "string"}},
    "address": {
      "type": "object",
      "required": ["city"],
      "properties": {"city": {"type": "string"}, "zip": {"type": "string"}}
    }
  }
}`

func TestValidatorAcceptsValidDocument(t *testing.T) {
	v := mustValidator(t, personSchema)
	doc, err := v.ValidateJSON([]byte(`{
		"name":"Nova","age":34,"role":"admin","tags":["pm"],
		"address":{"city":"Berlin","zip":"10115"}}`))
	if err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	if doc["name"] != "Nova" {
		t.Fatalf("document not returned correctly: %+v", doc)
	}
}

func TestValidatorReportsEveryProblemAtOnce(t *testing.T) {
	v := mustValidator(t, personSchema)

	// A single pass must surface all four defects: repairing one per round trip
	// would multiply latency by the number of mistakes.
	_, err := v.ValidateJSON([]byte(`{"age":"old","role":"wizard","tags":[]}`))
	if err == nil {
		t.Fatal("invalid document accepted")
	}
	var ve *port.ValidationError
	if !asValidation(err, &ve) {
		t.Fatalf("expected a validation error, got %T", err)
	}
	if len(ve.Issues) < 4 {
		t.Fatalf("expected at least 4 issues, got %d: %v", len(ve.Issues), ve.Issues)
	}

	joined := ve.Error()
	for _, want := range []string{"name", "age", "role", "tags"} {
		if !strings.Contains(joined, want) {
			t.Errorf("issue list does not mention %q: %s", want, joined)
		}
	}
}

func TestValidatorRejectsUndeclaredFields(t *testing.T) {
	v := mustValidator(t, personSchema)
	_, err := v.ValidateJSON([]byte(`{"name":"Nova","age":30,"tags":["x"],"sneaky":true}`))
	if err == nil {
		t.Fatal("additionalProperties:false was not enforced")
	}
	if !strings.Contains(err.Error(), "sneaky") {
		t.Fatalf("error should name the offending field: %v", err)
	}
}

func TestValidatorEnforcesConstraints(t *testing.T) {
	v := mustValidator(t, personSchema)

	cases := map[string]string{
		`{"name":"N","age":30,"tags":["x"]}`:                    "at least 2 characters",
		`{"name":"Nova","age":200,"tags":["x"]}`:                "at most 150",
		`{"name":"Nova","age":30,"tags":["a","b","c","d"]}`:     "at most 3 items",
		`{"name":"Nova","age":30,"tags":[]}`:                    "at least 1 items",
		`{"name":"Nova","age":3.5,"tags":["x"]}`:                "whole number",
		`{"name":"Nova","age":30,"tags":["x"],"role":"wizard"}`: "must be one of",
		`{"name":"Nova","age":30,"tags":["x"],"address":{}}`:    "required field is missing",
		`{"name":"Nova","age":30,"tags":"not-an-array"}`:        "must be an array",
	}
	for document, want := range cases {
		_, err := v.ValidateJSON([]byte(document))
		if err == nil {
			t.Errorf("document should have been rejected: %s", document)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("for %s\n  expected message containing %q\n  got %v", document, want, err)
		}
	}
}

func TestValidatorNestedPaths(t *testing.T) {
	v := mustValidator(t, `{
		"type":"object","required":["epics"],
		"properties":{"epics":{"type":"array","items":{
			"type":"object","required":["title","stories"],
			"properties":{"title":{"type":"string"},"stories":{"type":"array","items":{"type":"string"}}}}}}}`)

	_, err := v.ValidateJSON([]byte(`{"epics":[{"title":"A","stories":["s"]},{"title":123}]}`))
	if err == nil {
		t.Fatal("nested defect not detected")
	}
	message := err.Error()
	// The path must locate the problem precisely enough for a model to fix it.
	if !strings.Contains(message, "epics[1]") {
		t.Fatalf("error should identify the failing array index: %s", message)
	}
	if !strings.Contains(message, "stories") {
		t.Fatalf("error should report the missing nested field: %s", message)
	}
}

func TestValidationInstructionsAreActionable(t *testing.T) {
	v := mustValidator(t, personSchema)
	_, err := v.ValidateJSON([]byte(`{"age":"old"}`))
	var ve *port.ValidationError
	if !asValidation(err, &ve) {
		t.Fatalf("expected validation error, got %v", err)
	}

	instructions := ve.Instructions()
	if !strings.Contains(instructions, "return the complete corrected JSON") {
		t.Fatal("repair instructions must tell the model what to produce")
	}
	if !strings.Contains(instructions, "age") {
		t.Fatal("repair instructions must name the failing fields")
	}
	if !strings.Contains(instructions, "only the JSON") {
		t.Fatal("repair instructions must forbid commentary")
	}
}

func TestValidatorRejectsMalformedJSON(t *testing.T) {
	v := mustValidator(t, personSchema)
	_, err := v.ValidateJSON([]byte(`{"name": "unterminated`))
	if err == nil {
		t.Fatal("malformed JSON accepted")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("unhelpful message for malformed input: %v", err)
	}
}

func TestExtractJSONRecoversFromWrapping(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                                    `{"a":1}`,
		"```json\n{\"a\":1}\n```":                    `{"a":1}`,
		"```\n{\"a\":1}\n```":                        `{"a":1}`,
		"Sure! Here is the document:\n{\"a\":1}":     `{"a":1}`,
		"{\"a\":1}\n\nLet me know if you need more.": `{"a":1}`,
		`[{"a":1}]`:                                  `[{"a":1}]`,
		// A brace inside a string must not terminate the scan.
		`{"note":"use } carefully","b":2}`: `{"note":"use } carefully","b":2}`,
		// An escaped quote must not confuse string tracking.
		`{"note":"say \"hi\"","b":2}`: `{"note":"say \"hi\"","b":2}`,
	}
	for input, want := range cases {
		if got := port.ExtractJSON(input); got != want {
			t.Errorf("ExtractJSON(%q)\n  = %q\n  want %q", input, got, want)
		}
	}
}

func TestExtractJSONProducesParseableOutput(t *testing.T) {
	// The recovered text must actually parse, which is the only property that
	// matters downstream.
	wrapped := "Here you go:\n```json\n{\n  \"epics\": [\"a\", \"b\"],\n  \"note\": \"contains } and ]\"\n}\n```\nHope that helps!"
	var parsed map[string]any
	if err := json.Unmarshal([]byte(port.ExtractJSON(wrapped)), &parsed); err != nil {
		t.Fatalf("recovered text does not parse: %v", err)
	}
	if len(parsed["epics"].([]any)) != 2 {
		t.Fatalf("recovered document is wrong: %+v", parsed)
	}
}

func asValidation(err error, target **port.ValidationError) bool {
	for err != nil {
		if ve, ok := err.(*port.ValidationError); ok {
			*target = ve
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
