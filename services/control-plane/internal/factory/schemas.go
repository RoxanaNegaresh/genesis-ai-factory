package factory

import "encoding/json"

// Agent output schemas.
//
// These are the contract between a non-deterministic model and deterministic
// downstream code. Two properties matter more than expressiveness:
//
//  1. `additionalProperties: false` everywhere — a model that invents a field
//     is telling us the prompt was ambiguous, and silently dropping it hides
//     the problem until someone wonders why data vanished.
//  2. `minItems` on every list that must not be empty — "produce epics" with no
//     lower bound reliably yields zero epics on a small model.
//
// Schemas are kept modest in depth on purpose: constrained decoding on a small
// local model degrades sharply with deeply nested grammars, and a flat document
// that validates beats a rich one that never completes.

var visionSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["goal", "audience", "differentiators", "success_metrics", "out_of_scope"],
  "properties": {
    "goal": {
      "type": "string", "minLength": 40, "maxLength": 600,
      "description": "What the product achieves for its users, in two or three sentences."
    },
    "audience": {
      "type": "array", "minItems": 2, "maxItems": 4,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "need"],
        "properties": {
          "name": {"type": "string", "minLength": 3, "maxLength": 60},
          "need": {"type": "string", "minLength": 15, "maxLength": 300}
        }
      }
    },
    "differentiators": {
      "type": "array", "minItems": 2, "maxItems": 5,
      "items": {"type": "string", "minLength": 15, "maxLength": 220}
    },
    "success_metrics": {
      "type": "array", "minItems": 3, "maxItems": 5,
      "items": {"type": "string", "minLength": 15, "maxLength": 200}
    },
    "out_of_scope": {
      "type": "array", "minItems": 2, "maxItems": 6,
      "items": {"type": "string", "minLength": 8, "maxLength": 160}
    }
  }
}`)

var prdSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["summary", "epics", "stories"],
  "properties": {
    "summary": {"type": "string", "minLength": 40, "maxLength": 700},
    "epics": {
      "type": "array", "minItems": 3, "maxItems": 8,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "outcome"],
        "properties": {
          "name": {"type": "string", "minLength": 3, "maxLength": 60},
          "outcome": {"type": "string", "minLength": 15, "maxLength": 250}
        }
      }
    },
    "stories": {
      "type": "array", "minItems": 4, "maxItems": 14,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["epic", "persona", "want", "so_that", "priority", "acceptance"],
        "properties": {
          "epic": {"type": "string", "minLength": 3, "maxLength": 60},
          "persona": {"type": "string", "minLength": 3, "maxLength": 40},
          "want": {"type": "string", "minLength": 10, "maxLength": 220},
          "so_that": {"type": "string", "minLength": 10, "maxLength": 220},
          "priority": {"type": "string", "enum": ["must", "should", "could"]},
          "acceptance": {
            "type": "array", "minItems": 2, "maxItems": 5,
            "items": {"type": "string", "minLength": 15, "maxLength": 260}
          }
        }
      }
    }
  }
}`)

var archSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["overview", "decisions", "risks"],
  "properties": {
    "overview": {"type": "string", "minLength": 60, "maxLength": 900},
    "decisions": {
      "type": "array", "minItems": 3, "maxItems": 7,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["title", "choice", "rationale", "tradeoff"],
        "properties": {
          "title": {"type": "string", "minLength": 5, "maxLength": 90},
          "choice": {"type": "string", "minLength": 5, "maxLength": 200},
          "rationale": {"type": "string", "minLength": 25, "maxLength": 400},
          "tradeoff": {"type": "string", "minLength": 15, "maxLength": 300}
        }
      }
    },
    "risks": {
      "type": "array", "minItems": 2, "maxItems": 5,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["risk", "mitigation"],
        "properties": {
          "risk": {"type": "string", "minLength": 15, "maxLength": 250},
          "mitigation": {"type": "string", "minLength": 15, "maxLength": 300}
        }
      }
    }
  }
}`)

var classifySchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["category", "confidence", "reasoning"],
  "properties": {
    "category": {"type": "string", "enum": ["crm", "erp", "pm", "marketplace", "saas", "custom"]},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "reasoning": {"type": "string", "minLength": 10, "maxLength": 300}
  }
}`)
