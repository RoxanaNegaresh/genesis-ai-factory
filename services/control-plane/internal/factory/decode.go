package factory

import "strings"

// Typed accessors over a validated JSON document.
//
// The document has already passed schema validation, so these deliberately
// return zero values instead of errors: re-checking types that the validator
// just guaranteed would add noise at every call site. They stay defensive
// anyway, because a schema change and a reader change can land out of order,
// and a nil map should degrade to a missing section rather than a panic.

func stringField(document map[string]any, key string) string {
	if document == nil {
		return ""
	}
	value, _ := document[key].(string)
	return strings.TrimSpace(value)
}

func floatField(document map[string]any, key string) float64 {
	if document == nil {
		return 0
	}
	switch v := document[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

func stringSlice(document map[string]any, key string) []string {
	if document == nil {
		return nil
	}
	raw, _ := document[key].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func objectSlice(document map[string]any, key string) []map[string]any {
	if document == nil {
		return nil
	}
	raw, _ := document[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}
