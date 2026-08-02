package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Blueprint synthesis.
//
// v0.1 shipped five blueprints and fell back to a generic SaaS template for
// anything else. That fallback is the weakest point in the product: a user who
// asks for a veterinary clinic system gets "Organization, User, Resource" and
// correctly concludes the system did not understand them.
//
// Synthesis derives a blueprint for the brief instead. The critical constraint
// is that a synthesised blueprint must be *structurally identical* to a
// built-in one — same entity shape, same reference integrity, same screen
// contract — because every downstream agent, the SQL generator and the code
// generator all assume those invariants. A model producing free-form structure
// here would break the entire pipeline, so its output is validated, repaired
// and normalised before it is ever used.

var blueprintSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "description", "entities", "screens", "epics", "roles"],
  "properties": {
    "name": {"type": "string", "minLength": 4, "maxLength": 60},
    "description": {"type": "string", "minLength": 30, "maxLength": 300},
    "epics": {
      "type": "array", "minItems": 3, "maxItems": 7,
      "items": {"type": "string", "minLength": 3, "maxLength": 50}
    },
    "roles": {
      "type": "array", "minItems": 2, "maxItems": 5,
      "items": {"type": "string", "minLength": 3, "maxLength": 24}
    },
    "entities": {
      "type": "array", "minItems": 4, "maxItems": 12,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "plural", "description", "fields"],
        "properties": {
          "name": {"type": "string", "minLength": 2, "maxLength": 32},
          "plural": {"type": "string", "minLength": 2, "maxLength": 40},
          "description": {"type": "string", "minLength": 10, "maxLength": 160},
          "fields": {
            "type": "array", "minItems": 1, "maxItems": 14,
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["name", "type", "required"],
              "properties": {
                "name": {"type": "string", "minLength": 2, "maxLength": 32},
                "type": {"type": "string", "enum": [
                  "text", "int", "decimal", "bool", "timestamp", "json", "enum", "ref"
                ]},
                "required": {"type": "boolean"},
                "ref": {"type": "string", "maxLength": 32},
                "enum": {"type": "array", "maxItems": 8, "items": {"type": "string", "maxLength": 24}}
              }
            }
          }
        }
      }
    },
    "screens": {
      "type": "array", "minItems": 3, "maxItems": 8,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "route", "purpose", "primary_data"],
        "properties": {
          "name": {"type": "string", "minLength": 3, "maxLength": 40},
          "route": {"type": "string", "minLength": 1, "maxLength": 60},
          "purpose": {"type": "string", "minLength": 10, "maxLength": 160},
          "primary_data": {"type": "string", "minLength": 2, "maxLength": 32}
        }
      }
    }
  }
}`)

// SynthesizeBlueprint derives a product template for a brief that matches no
// built-in category. It returns false when synthesis is unavailable or the
// result cannot be made structurally sound.
func SynthesizeBlueprint(
	ctx context.Context,
	tb Toolbelt,
	reasoning *Reasoning,
	prompt string,
) (Blueprint, bool) {
	if !reasoning.Enabled() {
		return Blueprint{}, false
	}

	instruction := `Design the data model and screens for this product.

Requirements:
- Every entity needs a name (singular, PascalCase), a snake_case plural, a description and fields.
- Do NOT include id, created_at, updated_at or deleted_at; those are added automatically.
- Use "ref" for a foreign key and set its "ref" property to another entity's name.
- Use "enum" only with an explicit list of allowed values.
- Use "decimal" for money and quantities, never "int" or a floating type.
- Include an entity named User with at least email and role.
- Every screen's primary_data must name one of your entities.
- Routes start with / and use :id for a detail view.

Model the actual domain in the brief. A generic "Resource" entity is a failure.`

	p := NewPrompt(reasoning.PromptBudget(ctx)).
		Add("Product brief", prompt, 0).
		Add("Your task", instruction, 0)

	document := reasoning.think(ctx, tb, domain.RoleArchitect, "a custom product blueprint",
		houseStyle+"\n\nYou are a domain modeller. You turn a description of a business into the entities and screens a working system needs.",
		p.String(), "product_blueprint", blueprintSchema, port.ClassReasoning, 0.3)
	if document == nil {
		return Blueprint{}, false
	}

	blueprint, problems := normalizeSynthesized(document, prompt)
	if len(problems) > 0 {
		tb.Emit(ctx, domain.LevelDebug,
			fmt.Sprintf("Repaired %d structural problems in the synthesised blueprint", len(problems)),
			map[string]any{"problems": problems})
	}

	if err := ValidateBlueprint(blueprint); err != nil {
		tb.Emit(ctx, domain.LevelWarn,
			"Discarding synthesised blueprint: "+err.Error()+"; using the generic template instead", nil)
		return Blueprint{}, false
	}

	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Synthesised a custom blueprint: %s (%d entities, %d screens)",
			blueprint.Name, len(blueprint.Entities), len(blueprint.Screens)),
		map[string]any{"entities": len(blueprint.Entities), "screens": len(blueprint.Screens)})

	return blueprint, true
}

// normalizeSynthesized converts a validated document into a Blueprint,
// repairing the structural mistakes models reliably make.
//
// Repairing rather than rejecting is deliberate: a blueprint that is 90%
// correct plus four fixable naming errors is far more useful than a fallback to
// a generic template, and every repair here is mechanical and safe.
func normalizeSynthesized(document map[string]any, prompt string) (Blueprint, []string) {
	var problems []string

	blueprint := Blueprint{
		Key:         "synthesized",
		Version:     "1.0.0",
		Category:    domain.CategoryCustom,
		Name:        stringField(document, "name"),
		Description: stringField(document, "description"),
		Epics:       stringSlice(document, "epics"),
		Roles:       stringSlice(document, "roles"),
		NFRs:        commonNFRs(),
	}
	if blueprint.Name == "" {
		blueprint.Name = domain.TitleFromPrompt(prompt)
	}

	declared := map[string]bool{}
	for _, raw := range objectSlice(document, "entities") {
		name := pascalIdentifier(stringField(raw, "name"))
		if name == "" {
			problems = append(problems, "dropped an entity with no usable name")
			continue
		}
		if declared[name] {
			problems = append(problems, "dropped duplicate entity "+name)
			continue
		}
		declared[name] = true
	}

	takenPlurals := map[string]bool{}
	for _, raw := range objectSlice(document, "entities") {
		name := pascalIdentifier(stringField(raw, "name"))
		if name == "" {
			continue
		}

		// The plural becomes a SQL table name, so it must be a bare identifier.
		// snakeIdentifier strips spaces and punctuation that toSnake alone
		// would pass through into invalid DDL.
		// A plural is only trusted when it plausibly belongs to its entity.
		// A model returning "name" for Animal is not offering a table name, it
		// is echoing the field label, and accepting it produces a schema no
		// human can read.
		plural := toSnake(snakeIdentifier(stringField(raw, "plural")))
		stem := toSnake(name)
		if plural == "" || plural == stem || !strings.Contains(plural, stem[:min(len(stem), 4)]) {
			plural = stem + "s"
		}
		// Two entities mapping to the same table would silently overwrite each
		// other's DDL. Observed against a real model, which returned the
		// literal word "name" as the plural for several entities. Deriving from
		// the entity name is always safe, because entity names are already
		// deduplicated above.
		if takenPlurals[plural] {
			problems = append(problems, fmt.Sprintf(
				"%s had table name %q which was already taken; derived one from the entity name", name, plural))
			plural = toSnake(name) + "s"
			for suffix := 2; takenPlurals[plural]; suffix++ {
				plural = fmt.Sprintf("%s%d", toSnake(name)+"s", suffix)
			}
		}
		takenPlurals[plural] = true

		entity := Entity{
			Name:        name,
			Plural:      plural,
			Description: stringField(raw, "description"),
			Fields:      baseFields(),
		}

		seen := map[string]bool{"id": true, "created_at": true, "updated_at": true}
		for _, rawField := range objectSlice(raw, "fields") {
			fieldName := toSnake(snakeIdentifier(stringField(rawField, "name")))
			if fieldName == "" || seen[fieldName] {
				continue
			}
			// Audit columns are added by the generator; a model that repeats
			// them would produce a duplicate struct field.
			if fieldName == "deleted_at" {
				continue
			}
			seen[fieldName] = true

			field := Field{
				Name:     fieldName,
				Type:     stringField(rawField, "type"),
				Required: boolField(rawField, "required"),
			}

			switch field.Type {
			case "ref":
				target := pascalIdentifier(stringField(rawField, "ref"))
				if target == "" || !declared[target] {
					// A dangling reference would generate a foreign key to a
					// table that does not exist, so it degrades to plain text.
					problems = append(problems,
						fmt.Sprintf("%s.%s referenced unknown entity %q; treated as text",
							name, fieldName, stringField(rawField, "ref")))
					field.Type = "text"
					field.Ref = ""
				} else {
					field.Ref = target
				}
			case "enum":
				values := stringSlice(rawField, "enum")
				cleaned := make([]string, 0, len(values))
				for _, v := range values {
					if v = toSnake(snakeIdentifier(v)); v != "" {
						cleaned = append(cleaned, v)
					}
				}
				if len(cleaned) < 2 {
					problems = append(problems,
						fmt.Sprintf("%s.%s was an enum with fewer than two values; treated as text", name, fieldName))
					field.Type = "text"
				} else {
					field.Enum = cleaned
					field.Required = true
				}
			}
			entity.Fields = append(entity.Fields, field)
		}
		blueprint.Entities = append(blueprint.Entities, entity)
	}

	// Every product needs an authenticated user; without one the generated
	// auth layer has nothing to attach to.
	if !declared["User"] {
		problems = append(problems, "added the missing User entity")
		blueprint.Entities = append(blueprint.Entities, entity("User", "users", "An authenticated account",
			f("email", "text", true), f("display_name", "text", true),
			f("password_hash", "text", true)))
		declared["User"] = true
	}

	routes := map[string]bool{}
	for _, raw := range objectSlice(document, "screens") {
		screen := Screen{
			Name:        stringField(raw, "name"),
			Route:       normalizeRoute(stringField(raw, "route")),
			Purpose:     stringField(raw, "purpose"),
			PrimaryData: pascalIdentifier(stringField(raw, "primary_data")),
			Components:  []string{"DataTable", "FilterBar", "DetailDrawer"},
		}
		if screen.Name == "" || routes[screen.Route] {
			continue
		}
		if screen.PrimaryData != "" && !declared[screen.PrimaryData] {
			problems = append(problems,
				fmt.Sprintf("screen %q referenced unknown entity %q", screen.Name, screen.PrimaryData))
			screen.PrimaryData = ""
		}
		routes[screen.Route] = true
		blueprint.Screens = append(blueprint.Screens, screen)
	}

	// A synthesised blueprint still needs personas for the PRD.
	if len(blueprint.Roles) > 0 {
		for _, role := range blueprint.Roles[:min(2, len(blueprint.Roles))] {
			blueprint.Personas = append(blueprint.Personas, Persona{
				Name:  titleize(role),
				Role:  toSnake(snakeIdentifier(role)),
				Goals: []string{"Complete their work in this product without friction"},
				Pains: []string{"Tools that do not match how this business actually operates"},
			})
		}
	}
	return blueprint, problems
}

// ValidateBlueprint enforces the invariants every downstream generator assumes.
//
// This is the gate that makes synthesis safe. If a blueprint passes here, the
// SQL generator, code generator and frontend generator can all treat it exactly
// like a built-in one.
func ValidateBlueprint(b Blueprint) error {
	if b.Name == "" || b.Description == "" {
		return fmt.Errorf("the blueprint has no name or description")
	}
	if len(b.Entities) < 3 {
		return fmt.Errorf("only %d entities were produced; at least 3 are required", len(b.Entities))
	}
	if len(b.Screens) < 2 {
		return fmt.Errorf("only %d screens were produced; at least 2 are required", len(b.Screens))
	}

	names := map[string]bool{}
	plurals := map[string]bool{}
	for _, e := range b.Entities {
		if e.Name == "" || e.Plural == "" {
			return fmt.Errorf("an entity is missing its name or plural")
		}
		if names[e.Name] {
			return fmt.Errorf("duplicate entity %s", e.Name)
		}
		if plurals[e.Plural] {
			// Two entities mapping to one table would silently collide.
			return fmt.Errorf("entities %s and another share the table name %s", e.Name, e.Plural)
		}
		names[e.Name] = true
		plurals[e.Plural] = true

		fields := map[string]bool{}
		for _, f := range e.Fields {
			if f.Name == "" {
				return fmt.Errorf("%s has a field with no name", e.Name)
			}
			if fields[f.Name] {
				return fmt.Errorf("%s has duplicate field %s", e.Name, f.Name)
			}
			fields[f.Name] = true

			if !validFieldType(f.Type) {
				return fmt.Errorf("%s.%s has unknown type %q", e.Name, f.Name, f.Type)
			}
			if f.Type == "enum" && len(f.Enum) < 2 {
				return fmt.Errorf("%s.%s is an enum with fewer than two values", e.Name, f.Name)
			}
		}
		for _, required := range []string{"id", "created_at", "updated_at"} {
			if !fields[required] {
				return fmt.Errorf("%s is missing the %s field", e.Name, required)
			}
		}
	}

	// References must resolve, or the generated DDL will not apply.
	for _, e := range b.Entities {
		for _, f := range e.Fields {
			if f.Type == "ref" && !names[f.Ref] {
				return fmt.Errorf("%s.%s references unknown entity %q", e.Name, f.Name, f.Ref)
			}
		}
	}

	for _, s := range b.Screens {
		if s.Route == "" || !strings.HasPrefix(s.Route, "/") {
			return fmt.Errorf("screen %q has an invalid route %q", s.Name, s.Route)
		}
		if s.PrimaryData != "" && !names[s.PrimaryData] {
			return fmt.Errorf("screen %q references unknown entity %q", s.Name, s.PrimaryData)
		}
	}
	return nil
}

func validFieldType(t string) bool {
	switch t {
	case "uuid", "text", "int", "decimal", "bool", "timestamp", "json", "enum", "ref":
		return true
	}
	return false
}

// pascalIdentifier sanitises a model-supplied entity name into a valid Go
// identifier in PascalCase.
func pascalIdentifier(s string) string {
	var parts []string
	var current []rune

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			current = append(current, r)
		default:
			if len(current) > 0 {
				parts = append(parts, string(current))
				current = nil
			}
		}
	}
	if len(current) > 0 {
		parts = append(parts, string(current))
	}

	var out strings.Builder
	for _, part := range parts {
		runes := []rune(part)
		if runes[0] >= '0' && runes[0] <= '9' && out.Len() == 0 {
			// An identifier cannot start with a digit.
			continue
		}
		if runes[0] >= 'a' && runes[0] <= 'z' {
			runes[0] -= 32
		}
		out.WriteString(string(runes))
	}
	return out.String()
}

// snakeIdentifier strips anything that cannot appear in a column name.
//
// Separators are collapsed rather than mapped one-to-one: "Registration
// Number" passed through toSnake afterwards would otherwise become
// "registration__number", because the space becomes an underscore and the
// capital N adds another.
func snakeIdentifier(s string) string {
	var (
		out     strings.Builder
		pending bool
	)
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			if pending && out.Len() > 0 {
				out.WriteByte('_')
			}
			pending = false
			out.WriteRune(r)
		default:
			// Any run of separators collapses to a single underscore, emitted
			// lazily so trailing separators leave nothing behind.
			pending = true
		}
	}
	return strings.Trim(out.String(), "_")
}

// normalizeRoute ensures a route is a valid, leading-slash path.
func normalizeRoute(route string) string {
	route = strings.TrimSpace(route)
	if route == "" {
		return "/"
	}
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	// Collapse whitespace which would otherwise produce an unreachable route.
	return strings.ReplaceAll(route, " ", "-")
}

func boolField(document map[string]any, key string) bool {
	if document == nil {
		return false
	}
	value, _ := document[key].(bool)
	return value
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// BlueprintSchemaForTest exposes the synthesis schema to the test package.
func BlueprintSchemaForTest() []byte { return blueprintSchema }
