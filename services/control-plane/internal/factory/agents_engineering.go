package factory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// ArchitectAgent chooses the stack, draws boundaries and writes the API
// contract plus the decision record.
type ArchitectAgent struct{}

func (a *ArchitectAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleArchitect)
	return Charter{
		Role: domain.RoleArchitect, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactPRD},
		Outputs: []domain.ArtifactKind{domain.ArtifactArchSpec, domain.ArtifactADR},
		Tools:   []string{"fs.write"}, ModelClass: "reasoning",
		Budget: DefaultBudget(), Temperature: 0.2,
	}
}

func (a *ArchitectAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	if !bb.Has(domain.ArtifactPRD) {
		return nil, fmt.Errorf("architect requires the PRD")
	}
	bp := bb.Blueprint
	reasoned := a.reason(ctx, bb, tb)

	var sb strings.Builder
	sb.WriteString("# Architecture Specification\n\n")
	fmt.Fprintf(&sb, "**Project:** %s  \n**Category:** %s\n\n", bb.Project.Name, bp.Category)

	sb.WriteString("## 1. Stack\n\n| Layer | Choice | Rationale |\n|---|---|---|\n")
	sb.WriteString("| API | Go 1.23 + Fiber v2 | Static binary, low memory, high concurrency for list-heavy workloads |\n")
	sb.WriteString("| Architecture | Clean Architecture | Business rules independent of transport and storage; testable without a database |\n")
	sb.WriteString("| Database | PostgreSQL 16 | Relational integrity, JSONB for flexible attributes, strong indexing |\n")
	sb.WriteString("| Cache | Redis 7 | Session store, rate limiting, hot list caching |\n")
	sb.WriteString("| Frontend | React 18 + TypeScript + Vite | Fast HMR, strict typing against generated API types |\n")
	sb.WriteString("| Styling | TailwindCSS + shadcn/ui | Token-driven, no runtime CSS-in-JS cost |\n")
	sb.WriteString("| Auth | JWT access + rotating refresh | Stateless reads, revocable sessions |\n")
	sb.WriteString("| Container | Docker + Compose | One-command local run, parity with deployment |\n\n")

	if reasoned != nil && reasoned.Overview != "" {
		sb.WriteString("## 2. Overview\n\n")
		sb.WriteString(reasoned.Overview)
		sb.WriteString("\n\n")
	}

	sb.WriteString("## 3. Service boundaries\n\n```\n")
	sb.WriteString("┌─────────────┐   HTTPS    ┌──────────────────┐   SQL   ┌────────────┐\n")
	sb.WriteString("│  Web client │──────────►│   API service    │────────►│ PostgreSQL │\n")
	sb.WriteString("│  (React)    │◄──────────│   (Go/Fiber)     │◄────────│            │\n")
	sb.WriteString("└─────────────┘   JSON     └────────┬─────────┘         └────────────┘\n")
	sb.WriteString("                                    │ cache/session\n")
	sb.WriteString("                                    ▼\n")
	sb.WriteString("                              ┌───────────┐\n")
	sb.WriteString("                              │   Redis   │\n")
	sb.WriteString("                              └───────────┘\n```\n\n")
	sb.WriteString("A single deployable API service is correct at this scale: the domain is one ")
	sb.WriteString("transactional consistency boundary, and splitting it into services would add ")
	sb.WriteString("distributed-transaction complexity with no scaling benefit. The module structure ")
	sb.WriteString("below keeps extraction cheap if that changes.\n\n")

	sb.WriteString("## 3. Module structure\n\n```\n")
	sb.WriteString("api/\n├── cmd/server/main.go\n├── internal/\n")
	sb.WriteString("│   ├── domain/      entities and invariants (no external imports)\n")
	sb.WriteString("│   ├── usecase/     application services\n")
	sb.WriteString("│   ├── port/        repository and gateway interfaces\n")
	sb.WriteString("│   ├── adapter/http handlers, DTOs, middleware\n")
	sb.WriteString("│   └── infra/       postgres, redis, crypto implementations\n")
	sb.WriteString("├── migrations/\n└── openapi.yaml\n```\n\n")

	sb.WriteString("## 4. API contract\n\n")
	sb.WriteString("REST over JSON at `/api/v1`. Conventions:\n\n")
	sb.WriteString("- Plural, lower-case resource paths\n")
	sb.WriteString("- Cursor pagination (`?limit=&cursor=`) on every collection\n")
	sb.WriteString("- `PATCH` for partial updates; unknown fields are rejected, not ignored\n")
	sb.WriteString("- One error envelope: `{\"error\":{\"code\",\"message\",\"request_id\"}}`\n")
	sb.WriteString("- `Idempotency-Key` honoured on all `POST` endpoints\n\n")
	sb.WriteString("| Method | Path | Purpose |\n|---|---|---|\n")
	sb.WriteString("| POST | `/api/v1/auth/login` | Authenticate |\n")
	sb.WriteString("| POST | `/api/v1/auth/refresh` | Rotate tokens |\n")
	for _, e := range bp.Entities {
		if e.Name == "User" {
			continue
		}
		fmt.Fprintf(&sb, "| GET, POST | `/api/v1/%s` | List and create %s |\n", routePath(e), e.Plural)
		fmt.Fprintf(&sb, "| GET, PATCH, DELETE | `/api/v1/%s/{id}` | Read, update, archive a %s |\n",
			routePath(e), strings.ToLower(humanize(e.Name)))
	}

	sb.WriteString("\n## 5. Cross-cutting concerns\n\n")
	sb.WriteString("| Concern | Approach |\n|---|---|\n")
	sb.WriteString("| AuthN | JWT bearer, 15-minute access token, rotating refresh with reuse detection |\n")
	sb.WriteString("| AuthZ | Role check in the use case layer, never in the handler |\n")
	sb.WriteString("| Validation | At the DTO boundary; the domain re-asserts its own invariants |\n")
	sb.WriteString("| Errors | Typed sentinels mapped to HTTP status in one middleware |\n")
	sb.WriteString("| Logging | Structured JSON with request id correlation |\n")
	sb.WriteString("| Migrations | Embedded, forward-only, applied on boot |\n")
	sb.WriteString("| Testing | Domain unit tests, repository conformance tests, HTTP contract tests |\n\n")

	sb.WriteString("## 6. Non-functional targets\n\n")
	for _, n := range bp.NFRs {
		fmt.Fprintf(&sb, "- %s\n", n)
	}

	if reasoned != nil && len(reasoned.Risks) > 0 {
		sb.WriteString("\n## 7. Risks specific to this product\n\n")
		sb.WriteString("| Risk | Mitigation |\n|---|---|\n")
		for _, risk := range reasoned.Risks {
			fmt.Fprintf(&sb, "| %s | %s |\n", risk.Risk, risk.Mitigation)
		}
	}

	sb.WriteString("\n## 8. Scaling path\n\n")
	sb.WriteString("1. Vertical first — this workload is index-bound, not CPU-bound\n")
	sb.WriteString("2. Read replicas for reporting queries once they contend with writes\n")
	sb.WriteString("3. Redis-backed caching of hot list endpoints with event-driven invalidation\n")
	sb.WriteString("4. Extract the heaviest bounded context into its own service only when a team boundary demands it\n")

	specBody := sb.String()
	if err := tb.WriteFile(ctx, "docs/architecture/ARCHITECTURE.md", specBody); err != nil {
		return nil, err
	}

	adrBody := buildADRs(bp, reasoned)
	if err := tb.WriteFile(ctx, "docs/architecture/DECISIONS.md", adrBody); err != nil {
		return nil, err
	}

	openAPI := buildOpenAPI(bb.Project.Name, bp)
	if err := tb.WriteFile(ctx, "api/openapi.yaml", openAPI); err != nil {
		return nil, err
	}

	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Defined %d API resources and %d architecture decisions", len(bp.Entities)-1, 5),
		map[string]any{"entities": len(bp.Entities)})

	return []*domain.Artifact{
		artifact(bb, domain.ArtifactArchSpec, "ARCHITECTURE.md", "text/markdown", specBody),
		artifact(bb, domain.ArtifactADR, "DECISIONS.md", "text/markdown", adrBody),
	}, nil
}

func buildADRs(bp Blueprint, reasoned *archDraft) string {
	var sb strings.Builder
	sb.WriteString("# Architecture Decision Records\n\n")

	type adr struct{ title, context, decision, consequence string }
	adrs := []adr{
		{
			title:    "ADR-001: Modular monolith over microservices",
			context:  "The domain is a single transactional consistency boundary with a team size of one to five.",
			decision: "Ship one deployable API service with strict internal module boundaries.",
			consequence: "Simple deployment and local development, atomic transactions across the domain. " +
				"Cost: horizontal scaling is coarse-grained until a module is extracted.",
		},
		{
			title:    "ADR-002: PostgreSQL as the system of record",
			context:  "The data is highly relational with reporting requirements over joins.",
			decision: "PostgreSQL 16 with normalised schema plus JSONB for genuinely open attributes.",
			consequence: "Strong integrity and query flexibility. Cost: schema changes require migrations, " +
				"which is a feature rather than a defect at this level of data criticality.",
		},
		{
			title:       "ADR-003: Clean Architecture layering",
			context:     "Business rules must be testable without a database or HTTP server, and storage must be replaceable.",
			decision:    "Dependencies point inward: domain ← usecase ← adapter/infra, with ports owned by the inner layers.",
			consequence: "Fast unit tests and swappable infrastructure. Cost: more interfaces and explicit wiring.",
		},
		{
			title:    "ADR-004: JWT access tokens with rotating refresh tokens",
			context:  "Reads must scale without a session lookup, but sessions must remain revocable.",
			decision: "Short-lived signed access tokens plus opaque rotating refresh tokens with reuse detection.",
			consequence: "Stateless request authentication and a bounded compromise window. " +
				"Cost: refresh token state must be stored and rotated correctly.",
		},
		{
			title:       "ADR-005: Fixed-point decimals for monetary values",
			context:     "Financial arithmetic in binary floating point produces rounding errors that compound.",
			decision:    "Store money as NUMERIC in the database and as a decimal type in application code; never float64.",
			consequence: "Exact arithmetic and auditable totals. Cost: slightly more verbose arithmetic code.",
		},
	}
	if bp.Category == domain.CategoryPM {
		adrs = append(adrs, adr{
			title:       "ADR-006: Fractional ranking for board ordering",
			context:     "Dragging an issue between two others must not renumber the whole column.",
			decision:    "Store position as a decimal and insert at the midpoint between neighbours, rebalancing lazily.",
			consequence: "O(1) reordering with no write amplification. Cost: periodic rebalancing when precision is exhausted.",
		})
	}

	// Model-authored decisions follow the standing ones and are attributed, so a
	// reader can tell house policy from judgement made for this product.
	if reasoned != nil {
		for i, d := range reasoned.Decisions {
			adrs = append(adrs, adr{
				title:       fmt.Sprintf("ADR-%03d: %s", 100+i, d.Title),
				context:     "Identified during architecture review of this specific product.",
				decision:    d.Choice + " " + d.Rationale,
				consequence: d.Tradeoff,
			})
		}
	}

	for _, a := range adrs {
		fmt.Fprintf(&sb, "## %s\n\n**Status:** Accepted\n\n**Context.** %s\n\n**Decision.** %s\n\n**Consequences.** %s\n\n---\n\n",
			a.title, a.context, a.decision, a.consequence)
	}
	return sb.String()
}

// buildOpenAPI emits a real, parseable OpenAPI 3.0 document covering auth and
// full CRUD for every blueprint entity.
func buildOpenAPI(projectName string, bp Blueprint) string {
	var sb strings.Builder
	sb.WriteString("openapi: 3.0.3\n")
	sb.WriteString("info:\n")
	fmt.Fprintf(&sb, "  title: %s API\n", yamlEscape(projectName))
	sb.WriteString("  version: 1.0.0\n")
	fmt.Fprintf(&sb, "  description: %s\n", yamlEscape(bp.Description))
	sb.WriteString("servers:\n  - url: http://localhost:8080/api/v1\n")
	sb.WriteString("security:\n  - bearerAuth: []\n")
	sb.WriteString("paths:\n")

	sb.WriteString("  /auth/login:\n    post:\n      security: []\n      summary: Authenticate\n")
	sb.WriteString("      requestBody:\n        required: true\n        content:\n          application/json:\n")
	sb.WriteString("            schema:\n              type: object\n              required: [email, password]\n")
	sb.WriteString("              properties:\n                email: { type: string, format: email }\n")
	sb.WriteString("                password: { type: string, format: password }\n")
	sb.WriteString("      responses:\n        '200':\n          description: Authenticated\n")
	sb.WriteString("          content:\n            application/json:\n              schema:\n")
	sb.WriteString("                $ref: '#/components/schemas/Session'\n")
	sb.WriteString("        '401': { $ref: '#/components/responses/Unauthorized' }\n")

	// The rest of the authentication surface. Documenting only /auth/login
	// left generated clients unable to register, refresh or sign out, and made
	// the specification disagree with the router.
	sb.WriteString("  /auth/register:\n    post:\n      security: []\n      summary: Create an account\n")
	sb.WriteString("      requestBody:\n        required: true\n        content:\n          application/json:\n")
	sb.WriteString("            schema:\n              type: object\n              required: [email, display_name, password]\n")
	sb.WriteString("              properties:\n                email: { type: string, format: email }\n")
	sb.WriteString("                display_name: { type: string }\n")
	sb.WriteString("                password: { type: string, format: password, minLength: 12 }\n")
	sb.WriteString("      responses:\n        '201':\n          description: Registered\n")
	sb.WriteString("          content:\n            application/json:\n              schema:\n")
	sb.WriteString("                $ref: '#/components/schemas/Session'\n")
	sb.WriteString("        '422': { $ref: '#/components/responses/ValidationFailed' }\n")

	sb.WriteString("  /auth/refresh:\n    post:\n      security: []\n      summary: Rotate a refresh token\n")
	sb.WriteString("      requestBody:\n        required: true\n        content:\n          application/json:\n")
	sb.WriteString("            schema:\n              type: object\n              required: [refresh_token]\n")
	sb.WriteString("              properties:\n                refresh_token: { type: string }\n")
	sb.WriteString("      responses:\n        '200':\n          description: Rotated\n")
	sb.WriteString("        '401': { $ref: '#/components/responses/Unauthorized' }\n")

	sb.WriteString("  /auth/logout:\n    post:\n      security: []\n      summary: Revoke a session family\n")
	sb.WriteString("      requestBody:\n        required: true\n        content:\n          application/json:\n")
	sb.WriteString("            schema:\n              type: object\n              required: [refresh_token]\n")
	sb.WriteString("              properties:\n                refresh_token: { type: string }\n")
	sb.WriteString("      responses:\n        '204': { description: Signed out }\n")

	sb.WriteString("  /auth/me:\n    get:\n      summary: The authenticated principal\n")
	sb.WriteString("      responses:\n        '200':\n          description: The current user\n")
	sb.WriteString("        '401': { $ref: '#/components/responses/Unauthorized' }\n")

	for _, e := range bp.Entities {
		if e.Name == "User" {
			continue
		}
		lower := strings.ToLower(humanize(e.Name))
		fmt.Fprintf(&sb, "  /%s:\n", routePath(e))
		fmt.Fprintf(&sb, "    get:\n      summary: List %s\n      parameters:\n", e.Plural)
		sb.WriteString("        - { name: limit, in: query, schema: { type: integer, default: 50, maximum: 200 } }\n")
		sb.WriteString("        - { name: cursor, in: query, schema: { type: string } }\n")
		sb.WriteString("        - { name: q, in: query, schema: { type: string } }\n")
		sb.WriteString("      responses:\n        '200':\n          description: A page of results\n")
		sb.WriteString("          content:\n            application/json:\n              schema:\n")
		sb.WriteString("                type: object\n                properties:\n")
		sb.WriteString("                  data:\n                    type: array\n                    items:\n")
		fmt.Fprintf(&sb, "                      $ref: '#/components/schemas/%s'\n", e.Name)
		sb.WriteString("                  next_cursor: { type: string, nullable: true }\n")
		fmt.Fprintf(&sb, "    post:\n      summary: Create a %s\n      requestBody:\n        required: true\n", lower)
		sb.WriteString("        content:\n          application/json:\n            schema:\n")
		fmt.Fprintf(&sb, "              $ref: '#/components/schemas/%sInput'\n", e.Name)
		sb.WriteString("      responses:\n        '201':\n          description: Created\n")
		sb.WriteString("          content:\n            application/json:\n              schema:\n")
		fmt.Fprintf(&sb, "                $ref: '#/components/schemas/%s'\n", e.Name)
		sb.WriteString("        '422': { $ref: '#/components/responses/ValidationFailed' }\n")

		fmt.Fprintf(&sb, "  /%s/{id}:\n", routePath(e))
		sb.WriteString("    parameters:\n      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }\n")
		fmt.Fprintf(&sb, "    get:\n      summary: Fetch a %s\n      responses:\n", lower)
		sb.WriteString("        '200':\n          description: Found\n          content:\n            application/json:\n")
		fmt.Fprintf(&sb, "              schema:\n                $ref: '#/components/schemas/%s'\n", e.Name)
		sb.WriteString("        '404': { $ref: '#/components/responses/NotFound' }\n")
		fmt.Fprintf(&sb, "    patch:\n      summary: Update a %s\n      requestBody:\n        required: true\n", lower)
		sb.WriteString("        content:\n          application/json:\n            schema:\n")
		fmt.Fprintf(&sb, "              $ref: '#/components/schemas/%sInput'\n", e.Name)
		sb.WriteString("      responses:\n        '200':\n          description: Updated\n")
		sb.WriteString("          content:\n            application/json:\n              schema:\n")
		fmt.Fprintf(&sb, "                $ref: '#/components/schemas/%s'\n", e.Name)
		fmt.Fprintf(&sb, "    delete:\n      summary: Archive a %s\n      responses:\n        '204': { description: Archived }\n", lower)
	}

	sb.WriteString("components:\n  securitySchemes:\n    bearerAuth:\n      type: http\n      scheme: bearer\n      bearerFormat: JWT\n")
	sb.WriteString("  responses:\n")
	sb.WriteString("    Unauthorized:\n      description: Authentication required\n      content:\n        application/json:\n          schema: { $ref: '#/components/schemas/Error' }\n")
	sb.WriteString("    NotFound:\n      description: Resource not found\n      content:\n        application/json:\n          schema: { $ref: '#/components/schemas/Error' }\n")
	sb.WriteString("    ValidationFailed:\n      description: Request failed validation\n      content:\n        application/json:\n          schema: { $ref: '#/components/schemas/Error' }\n")
	sb.WriteString("  schemas:\n")
	sb.WriteString("    Error:\n      type: object\n      properties:\n        error:\n          type: object\n")
	sb.WriteString("          properties:\n            code: { type: string }\n            message: { type: string }\n            request_id: { type: string }\n")
	sb.WriteString("    Session:\n      type: object\n      properties:\n")
	sb.WriteString("        access_token: { type: string }\n        refresh_token: { type: string }\n        expires_at: { type: string, format: date-time }\n")

	for _, e := range bp.Entities {
		fmt.Fprintf(&sb, "    %s:\n      type: object\n      properties:\n", e.Name)
		for _, fld := range e.Fields {
			if fld.Name == "password_hash" {
				continue // never exposed over the API
			}
			fmt.Fprintf(&sb, "        %s: %s\n", fld.Name, openAPIType(fld))
		}
		fmt.Fprintf(&sb, "    %sInput:\n      type: object\n", e.Name)
		var required []string
		for _, fld := range e.Fields {
			if fld.Required && !isSystemField(fld.Name) && fld.Name != "password_hash" {
				required = append(required, fld.Name)
			}
		}
		if len(required) > 0 {
			fmt.Fprintf(&sb, "      required: [%s]\n", strings.Join(required, ", "))
		}
		sb.WriteString("      properties:\n")
		for _, fld := range e.Fields {
			if isSystemField(fld.Name) || fld.Name == "password_hash" {
				continue
			}
			fmt.Fprintf(&sb, "        %s: %s\n", fld.Name, openAPIType(fld))
		}
	}
	return sb.String()
}

func isSystemField(name string) bool {
	switch name {
	case "id", "created_at", "updated_at":
		return true
	}
	return false
}

func openAPIType(f Field) string {
	switch f.Type {
	case "uuid", "ref":
		return "{ type: string, format: uuid }"
	case "text":
		return "{ type: string }"
	case "int":
		return "{ type: integer }"
	case "decimal":
		return "{ type: string, description: 'decimal encoded as string to preserve precision' }"
	case "bool":
		return "{ type: boolean }"
	case "timestamp":
		return "{ type: string, format: date-time }"
	case "json":
		return "{ type: object, additionalProperties: true }"
	case "enum":
		return fmt.Sprintf("{ type: string, enum: [%s] }", strings.Join(f.Enum, ", "))
	}
	return "{ type: string }"
}

func yamlEscape(s string) string {
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// DatabaseAgent designs the schema and emits runnable migrations.
type DatabaseAgent struct{}

func (a *DatabaseAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleDatabase)
	return Charter{
		Role: domain.RoleDatabase, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactArchSpec},
		Outputs: []domain.ArtifactKind{domain.ArtifactDBSchema, domain.ArtifactMigrations},
		Tools:   []string{"fs.write"}, ModelClass: "code",
		Budget: DefaultBudget(), Temperature: 0.1,
	}
}

func (a *DatabaseAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	if !bb.Has(domain.ArtifactArchSpec) {
		return nil, fmt.Errorf("database engineer requires the architecture spec")
	}
	bp := bb.Blueprint

	ddl := buildDDL(bp)
	if err := tb.WriteFile(ctx, "migrations/0001_init.up.sql", ddl); err != nil {
		return nil, err
	}
	if err := tb.WriteFile(ctx, "migrations/0001_init.down.sql", buildDropDDL(bp)); err != nil {
		return nil, err
	}

	erd := buildERD(bp)
	if err := tb.WriteFile(ctx, "docs/architecture/DATA_MODEL.md", erd); err != nil {
		return nil, err
	}

	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Generated schema with %d tables and their indexes", len(bp.Entities)),
		map[string]any{"tables": len(bp.Entities)})

	return []*domain.Artifact{
		artifact(bb, domain.ArtifactDBSchema, "DATA_MODEL.md", "text/markdown", erd),
		artifact(bb, domain.ArtifactMigrations, "0001_init.up.sql", "application/sql", ddl),
	}, nil
}

// tableName converts an entity plural into a snake_case SQL identifier.
func tableName(e Entity) string { return toSnake(e.Plural) }

// routePath converts an entity plural into the URL segment for its collection.
//
// SQL identifiers and URL paths follow different conventions and must not
// share a helper. PostgreSQL folds unquoted identifiers to lower case and uses
// underscores, so "seller_profiles" is right for a table. URLs conventionally
// use hyphens, so "/api/v1/seller-profiles" is right for a route — and it is
// what every client library, style guide and reverse proxy expects.
func routePath(e Entity) string { return strings.ReplaceAll(toSnake(e.Plural), "_", "-") }

func toSnake(s string) string {
	var out strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			// Do not insert a separator where one already exists, or
			// "Registration_Number" becomes "registration__number".
			if i > 0 && s[i-1] != '_' {
				out.WriteByte('_')
			}
			out.WriteRune(r + 32)
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func sqlType(f Field) string {
	switch f.Type {
	case "uuid", "ref":
		return "UUID"
	case "text":
		return "TEXT"
	case "int":
		return "INTEGER"
	case "decimal":
		// NUMERIC, never DOUBLE PRECISION: binary floats cannot represent
		// monetary values exactly and the error compounds across aggregation.
		return "NUMERIC(18,4)"
	case "bool":
		return "BOOLEAN"
	case "timestamp":
		return "TIMESTAMPTZ"
	case "json":
		return "JSONB"
	case "enum":
		return "TEXT"
	}
	return "TEXT"
}

func buildDDL(bp Blueprint) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "-- %s — initial schema\n-- Generated by Genesis AI Factory (Database Engineer agent)\n\n", bp.Name)
	sb.WriteString("CREATE EXTENSION IF NOT EXISTS pgcrypto;\n\n")

	// Emit tables in dependency order so foreign keys always resolve.
	for _, e := range sortEntitiesByDependency(bp.Entities) {
		table := tableName(e)
		fmt.Fprintf(&sb, "-- %s\nCREATE TABLE %s (\n", e.Description, table)

		lines := make([]string, 0, len(e.Fields)+2)
		for _, f := range e.Fields {
			switch f.Name {
			case "id":
				lines = append(lines, "    id UUID PRIMARY KEY DEFAULT gen_random_uuid()")
				continue
			case "created_at":
				lines = append(lines, "    created_at TIMESTAMPTZ NOT NULL DEFAULT now()")
				continue
			case "updated_at":
				lines = append(lines, "    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()")
				continue
			}
			line := "    " + f.Name + " " + sqlType(f)
			if f.Required {
				line += " NOT NULL"
			}
			if f.Type == "enum" && len(f.Enum) > 0 {
				quoted := make([]string, len(f.Enum))
				for i, v := range f.Enum {
					quoted[i] = "'" + v + "'"
				}
				line += fmt.Sprintf(" CHECK (%s IN (%s))", f.Name, strings.Join(quoted, ", "))
			}
			if f.Type == "ref" && f.Ref != "" {
				if target, ok := findEntity(bp, f.Ref); ok {
					line += fmt.Sprintf(" REFERENCES %s (id)", tableName(target))
				}
			}
			lines = append(lines, line)
		}
		lines = append(lines, "    deleted_at TIMESTAMPTZ")
		sb.WriteString(strings.Join(lines, ",\n"))
		sb.WriteString("\n);\n\n")

		// Indexes: every foreign key, every enum used for filtering, plus a
		// recency index for the default listing order.
		for _, f := range e.Fields {
			if f.Type == "ref" {
				fmt.Fprintf(&sb, "CREATE INDEX ix_%s_%s ON %s (%s);\n", table, f.Name, table, f.Name)
			}
			if f.Type == "enum" {
				fmt.Fprintf(&sb, "CREATE INDEX ix_%s_%s ON %s (%s) WHERE deleted_at IS NULL;\n", table, f.Name, table, f.Name)
			}
			if f.Name == "email" || f.Name == "sku" || f.Name == "slug" || f.Name == "key" || f.Name == "number" || f.Name == "code" {
				fmt.Fprintf(&sb, "CREATE UNIQUE INDEX ux_%s_%s ON %s (%s) WHERE deleted_at IS NULL;\n", table, f.Name, table, f.Name)
			}
		}
		fmt.Fprintf(&sb, "CREATE INDEX ix_%s_created_at ON %s (created_at DESC) WHERE deleted_at IS NULL;\n\n", table, table)
	}

	sb.WriteString("-- Keep updated_at honest without relying on every writer to remember.\n")
	sb.WriteString("CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$\nBEGIN\n")
	sb.WriteString("    NEW.updated_at = now();\n    RETURN NEW;\nEND;\n$$ LANGUAGE plpgsql;\n\n")
	for _, e := range bp.Entities {
		table := tableName(e)
		fmt.Fprintf(&sb, "CREATE TRIGGER trg_%s_updated_at BEFORE UPDATE ON %s\n    FOR EACH ROW EXECUTE FUNCTION set_updated_at();\n", table, table)
	}

	// Sessions exist only where there are users to authenticate.
	if _, ok := authEntity(bp); ok {
		sb.WriteString(backendAuthMigration())
	}
	return sb.String()
}

func buildDropDDL(bp Blueprint) string {
	var sb strings.Builder
	sb.WriteString("-- Reverse of 0001_init.up.sql\n\n")
	entities := sortEntitiesByDependency(bp.Entities)
	for i := len(entities) - 1; i >= 0; i-- {
		fmt.Fprintf(&sb, "DROP TABLE IF EXISTS %s CASCADE;\n", tableName(entities[i]))
	}
	if _, ok := authEntity(bp); ok {
		sb.WriteString("DROP TABLE IF EXISTS sessions CASCADE;\n")
	}
	sb.WriteString("DROP FUNCTION IF EXISTS set_updated_at();\n")
	return sb.String()
}

// sortEntitiesByDependency topologically orders entities so a table is created
// after everything it references. Self-references are ignored (they are legal
// within a single CREATE TABLE), and cycles fall back to declaration order.
func sortEntitiesByDependency(entities []Entity) []Entity {
	index := map[string]Entity{}
	for _, e := range entities {
		index[e.Name] = e
	}

	var (
		ordered []Entity
		state   = map[string]int{} // 0 unvisited, 1 visiting, 2 done
		visit   func(name string)
	)
	visit = func(name string) {
		if state[name] != 0 {
			return
		}
		state[name] = 1
		e, ok := index[name]
		if !ok {
			state[name] = 2
			return
		}
		deps := make([]string, 0, 4)
		for _, f := range e.Fields {
			if f.Type == "ref" && f.Ref != "" && f.Ref != name {
				deps = append(deps, f.Ref)
			}
		}
		sort.Strings(deps)
		for _, d := range deps {
			if state[d] == 1 {
				continue // cycle: emit anyway, FK is deferrable in practice
			}
			visit(d)
		}
		state[name] = 2
		ordered = append(ordered, e)
	}

	// Deterministic entry order keeps generated migrations byte-stable across
	// runs, which matters for diffing and for artifact deduplication.
	for _, e := range entities {
		visit(e.Name)
	}
	return ordered
}

func findEntity(bp Blueprint, name string) (Entity, bool) {
	for _, e := range bp.Entities {
		if e.Name == name {
			return e, true
		}
	}
	return Entity{}, false
}

func buildERD(bp Blueprint) string {
	var sb strings.Builder
	sb.WriteString("# Data Model\n\n## Entity relationship diagram\n\n```mermaid\nerDiagram\n")
	for _, e := range bp.Entities {
		for _, f := range e.Fields {
			if f.Type == "ref" && f.Ref != "" {
				fmt.Fprintf(&sb, "    %s ||--o{ %s : \"%s\"\n", f.Ref, e.Name, f.Name)
			}
		}
	}
	sb.WriteString("```\n\n## Tables\n\n")
	for _, e := range bp.Entities {
		fmt.Fprintf(&sb, "### `%s`\n\n%s\n\n| Column | Type | Null | Notes |\n|---|---|---|---|\n",
			tableName(e), e.Description)
		for _, f := range e.Fields {
			null := "yes"
			if f.Required {
				null = "no"
			}
			note := ""
			switch {
			case f.Name == "id":
				note = "primary key"
			case f.Type == "ref":
				note = "→ " + toSnake(pluralOf(bp, f.Ref)) + ".id"
			case f.Type == "enum":
				note = "one of: " + strings.Join(f.Enum, ", ")
			case f.Type == "decimal":
				note = "fixed-point; never float"
			}
			fmt.Fprintf(&sb, "| `%s` | %s | %s | %s |\n", f.Name, sqlType(f), null, note)
		}
		fmt.Fprintf(&sb, "| `deleted_at` | TIMESTAMPTZ | yes | soft delete marker |\n\n")
	}

	sb.WriteString("## Indexing strategy\n\n")
	sb.WriteString("- Every foreign key is indexed; unindexed FKs turn joins and cascade checks into sequential scans\n")
	sb.WriteString("- Status/enum columns carry partial indexes filtered on `deleted_at IS NULL`, matching how the application queries them\n")
	sb.WriteString("- Natural keys (`email`, `sku`, `slug`, `key`, `number`, `code`) are uniquely indexed among live rows only, so archiving frees the value\n")
	sb.WriteString("- Each table has a descending `created_at` index supporting the default listing order\n\n")
	sb.WriteString("## Conventions\n\n")
	sb.WriteString("- Soft delete everywhere; destructive removal is an explicit administrative operation\n")
	sb.WriteString("- `updated_at` maintained by trigger rather than trusting every writer\n")
	sb.WriteString("- Monetary and quantity values use `NUMERIC`, never floating point\n")
	return sb.String()
}

func pluralOf(bp Blueprint, entityName string) string {
	if e, ok := findEntity(bp, entityName); ok {
		return e.Plural
	}
	return strings.ToLower(entityName) + "s"
}

// archDraft is the typed projection of the architect's schema.
type archDraft struct {
	Overview  string
	Decisions []archDecision
	Risks     []archRisk
}

type archDecision struct {
	Title     string
	Choice    string
	Rationale string
	Tradeoff  string
}

type archRisk struct {
	Risk       string
	Mitigation string
}

// reason asks the model for architecture judgement specific to this product.
func (a *ArchitectAgent) reason(ctx context.Context, bb *Blackboard, tb Toolbelt) *archDraft {
	if !bb.Reasoning.Enabled() {
		return nil
	}

	requirements := ""
	if artifact, ok := bb.Get(domain.ArtifactPRD); ok {
		requirements = artifact.Body
	}

	prompt := NewPrompt(bb.Reasoning.PromptBudget(ctx)).
		Add("Product brief", briefContext(bb), 0).
		Add("Requirements", requirements, 1).
		Add("Fixed stack", `The stack is already decided and is not open for debate:
Go 1.23 with Fiber, PostgreSQL 16, Redis, React 18 with TypeScript, Docker.
Do not propose alternatives to these.`, 0).
		Add("Prior decisions", memoryContext(bb.Reasoning.recall(ctx, bb.Project.ID, "architecture decision", 4)), 3).
		Add("Your task", `Provide the architecture judgement that is specific to THIS product.

- overview: how the pieces fit together for this product's particular workload and access patterns.
- decisions: the consequential choices within the fixed stack — data modelling, consistency,
  concurrency, caching, state machines. Each needs an honest trade-off, not just an advantage.
- risks: what is most likely to go wrong in this specific domain, and how to mitigate it.

Do not restate generic best practice. Everything you write must be traceable to this product.`, 0)

	document := bb.Reasoning.think(ctx, tb, domain.RoleArchitect, "the architecture analysis",
		houseStyle+"\n\nYou are the system architect. You are accountable for the decisions that are expensive to reverse.",
		prompt.String(), "architecture", archSchema, port.ClassReasoning, 0.2)
	if document == nil {
		return nil
	}

	draft := &archDraft{Overview: stringField(document, "overview")}
	for _, raw := range objectSlice(document, "decisions") {
		draft.Decisions = append(draft.Decisions, archDecision{
			Title:     stringField(raw, "title"),
			Choice:    stringField(raw, "choice"),
			Rationale: stringField(raw, "rationale"),
			Tradeoff:  stringField(raw, "tradeoff"),
		})
	}

	// Guard against both failure modes: repeating itself, and parroting the
	// requirements document back as if it were architecture.
	claims := make([]string, 0, len(draft.Decisions)+1)
	claims = append(claims, draft.Overview)
	for _, d := range draft.Decisions {
		claims = append(claims, d.Choice+" "+d.Rationale)
	}
	if err := critique(claims); err != nil {
		tb.Emit(ctx, domain.LevelWarn,
			"Discarding model architecture: "+err.Error()+"; using the blueprint instead", nil)
		return nil
	}
	if err := critiqueEcho(claims, requirements); err != nil {
		tb.Emit(ctx, domain.LevelWarn,
			"Discarding model architecture: "+err.Error()+"; using the blueprint instead", nil)
		return nil
	}
	for _, raw := range objectSlice(document, "risks") {
		draft.Risks = append(draft.Risks, archRisk{
			Risk:       stringField(raw, "risk"),
			Mitigation: stringField(raw, "mitigation"),
		})
	}

	// Architecture decisions are exactly the knowledge a later run must not
	// contradict, so they are written to long-term memory.
	for _, d := range draft.Decisions {
		bb.Reasoning.remember(ctx, bb.Project.ID, port.KindDecision, d.Title, d.Choice+" — "+d.Rationale)
	}
	return draft
}
