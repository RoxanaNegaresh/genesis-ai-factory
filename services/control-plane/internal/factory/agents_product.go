package factory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// artifact is a helper that builds a project-scoped artifact and registers it
// on the blackboard in one step.
func artifact(bb *Blackboard, kind domain.ArtifactKind, name, mime, body string) *domain.Artifact {
	a := domain.NewArtifact(bb.Project.ID, bb.Run.ID, nil, kind, name, mime, body, time.Now())
	bb.Put(a)
	return a
}

// CEOAgent turns a raw brief into a product strategy.
type CEOAgent struct{}

func (a *CEOAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleCEO)
	return Charter{
		Role: domain.RoleCEO, Mission: p.Mission,
		Inputs: nil, Outputs: []domain.ArtifactKind{domain.ArtifactVision},
		Tools: []string{"fs.write", "memory.search"}, ModelClass: "reasoning",
		Budget: DefaultBudget(), Temperature: 0.4,
	}
}

func (a *CEOAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	bp := bb.Blueprint
	cls := bb.Classification

	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Classified the brief as %s (confidence %.0f%%)", bp.Name, cls.Confidence*100),
		map[string]any{"category": string(cls.Category), "signals": cls.Matched})

	// Ask the model for product judgement. When it answers, its output shapes
	// the document; when it does not, the blueprint-derived defaults below
	// still produce a complete vision.
	reasoned := a.reason(ctx, bb, tb)

	var sb strings.Builder
	sb.WriteString("# Product Vision\n\n")
	fmt.Fprintf(&sb, "**Project:** %s  \n", bb.Project.Name)
	fmt.Fprintf(&sb, "**Category:** %s (%s, confidence %.0f%%)\n\n", bp.Name, cls.Category, cls.Confidence*100)

	// Deliberately no generation timestamp in the body. Artifacts are
	// content-addressed, so embedding wall-clock time would give every
	// regeneration a new hash, defeating deduplication and turning every
	// re-run into a spurious diff. Provenance lives on the artifact record
	// (created_at, run_id), where it belongs.

	sb.WriteString("## Original brief\n\n> ")
	sb.WriteString(strings.ReplaceAll(strings.TrimSpace(bb.Project.Prompt), "\n", "\n> "))
	sb.WriteString("\n\n## Goal\n\n")
	if reasoned != nil && reasoned.Goal != "" {
		fmt.Fprintf(&sb, "%s\n\n", reasoned.Goal)
	} else {
		fmt.Fprintf(&sb, "%s\n\n", bp.Description)
	}

	sb.WriteString("## Target users\n\n")
	if reasoned != nil && len(reasoned.Audience) > 0 {
		for _, member := range reasoned.Audience {
			fmt.Fprintf(&sb, "### %s\n\n%s\n\n", member.Name, member.Need)
		}
	} else {
		for _, persona := range bp.Personas {
			fmt.Fprintf(&sb, "### %s (`%s`)\n\n", persona.Name, persona.Role)
			sb.WriteString("Goals:\n")
			for _, g := range persona.Goals {
				fmt.Fprintf(&sb, "- %s\n", g)
			}
			sb.WriteString("\nPain points:\n")
			for _, p := range persona.Pains {
				fmt.Fprintf(&sb, "- %s\n", p)
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString("## Strategy\n\n")
	if reasoned != nil && len(reasoned.Differentiators) > 0 {
		sb.WriteString("This product competes on:\n\n")
		for _, d := range reasoned.Differentiators {
			fmt.Fprintf(&sb, "- %s\n", d)
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("The product wins by being *complete on the fundamentals before it is clever*: ")
		sb.WriteString("the core object model, permissions and reporting must be correct and fast, ")
		sb.WriteString("because that is what makes a tool in this category trustworthy day to day.\n\n")
	}

	sb.WriteString("## Success metrics\n\n")
	metrics := successMetrics(cls.Category)
	if reasoned != nil && len(reasoned.SuccessMetrics) > 0 {
		metrics = reasoned.SuccessMetrics
	}
	for _, m := range metrics {
		fmt.Fprintf(&sb, "- %s\n", m)
	}

	sb.WriteString("\n## Scope guardrails\n\n")
	sb.WriteString("In scope for the first release:\n\n")
	for _, e := range bp.Epics {
		fmt.Fprintf(&sb, "- %s\n", e)
	}
	sb.WriteString("\nExplicitly out of scope for the first release:\n\n")
	excluded := outOfScope(cls.Category)
	if reasoned != nil && len(reasoned.OutOfScope) > 0 {
		excluded = reasoned.OutOfScope
	}
	for _, e := range excluded {
		fmt.Fprintf(&sb, "- %s\n", e)
	}

	if reasoned != nil {
		sb.WriteString("\n---\n\n*Product judgement in this document was authored by the reasoning model; ")
		sb.WriteString("structure and scope come from the category blueprint.*\n")
		bb.Reasoning.remember(ctx, bb.Project.ID, port.KindDecision,
			"Product goal for "+bb.Project.Name, reasoned.Goal)
	}

	body := sb.String()
	if err := tb.WriteFile(ctx, "docs/product/VISION.md", body); err != nil {
		return nil, err
	}
	return []*domain.Artifact{artifact(bb, domain.ArtifactVision, "VISION.md", "text/markdown", body)}, nil
}

func successMetrics(c domain.ProjectCategory) []string {
	switch c {
	case domain.CategoryCRM:
		return []string{
			"Weekly active sales reps / licensed seats above 70%",
			"Median time to log an activity under 15 seconds",
			"Pipeline forecast within 15% of actual closed revenue",
		}
	case domain.CategoryPM:
		return []string{
			"Median time to create and assign an issue under 20 seconds",
			"Sprint completion rate visible without manual reporting",
			"Board interactions feel instant (optimistic update under 100ms)",
		}
	case domain.CategoryERP:
		return []string{
			"Inventory accuracy above 98% against physical count",
			"Purchase-to-receipt cycle time reduced by 20%",
			"Month-end close completed without manual spreadsheet reconciliation",
		}
	case domain.CategoryMarketplace:
		return []string{
			"Listing-to-first-order conversion above 5%",
			"Payment success rate above 97%",
			"Dispute rate below 1% of completed orders",
		}
	default:
		return []string{
			"Time to first meaningful action under 2 minutes for a new user",
			"Weekly retention above 40% after four weeks",
			"p95 page interaction latency under 300ms",
		}
	}
}

func outOfScope(c domain.ProjectCategory) []string {
	common := []string{"Native mobile applications", "Third-party marketplace of plugins", "Multi-region data residency"}
	switch c {
	case domain.CategoryCRM:
		return append([]string{"Email campaign automation", "Telephony integration"}, common...)
	case domain.CategoryPM:
		return append([]string{"Time tracking and invoicing", "Custom scripting of workflow automation"}, common...)
	case domain.CategoryERP:
		return append([]string{"Payroll and HR", "Statutory tax filing per jurisdiction"}, common...)
	case domain.CategoryMarketplace:
		return append([]string{"Live logistics tracking", "In-house fraud scoring model"}, common...)
	default:
		return common
	}
}

// ProductManagerAgent converts strategy into a testable requirements document.
type ProductManagerAgent struct{}

func (a *ProductManagerAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RolePM)
	return Charter{
		Role: domain.RolePM, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactVision},
		Outputs: []domain.ArtifactKind{domain.ArtifactPRD},
		Tools:   []string{"fs.write", "memory.search"}, ModelClass: "reasoning",
		Budget: DefaultBudget(), Temperature: 0.3,
	}
}

func (a *ProductManagerAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	if !bb.Has(domain.ArtifactVision) {
		return nil, fmt.Errorf("product manager requires the product vision")
	}
	bp := bb.Blueprint

	// Blueprint stories guarantee complete CRUD and screen coverage; model
	// stories add the product-specific behaviour a template cannot know. Merging
	// both is deliberate: the model alone forgets systematic coverage, and the
	// template alone never says anything surprising about *this* product.
	stories := buildStories(bp)
	reasoned := a.reason(ctx, bb, tb)
	if len(reasoned) > 0 {
		stories = mergeStories(reasoned, stories)
	}
	bb.SetValue("stories", stories)

	source := "blueprint"
	if len(reasoned) > 0 {
		source = fmt.Sprintf("%d authored by the model, %d from the blueprint", len(reasoned), len(stories)-len(reasoned))
	}
	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Derived %d user stories across %d epics (%s)", len(stories), len(bp.Epics), source),
		map[string]any{"stories": len(stories), "epics": len(bp.Epics), "reasoned": len(reasoned)})

	var sb strings.Builder
	sb.WriteString("# Product Requirements Document\n\n")
	fmt.Fprintf(&sb, "**Project:** %s  \n**Category:** %s  \n**Blueprint:** %s v%s\n\n",
		bb.Project.Name, bp.Category, bp.Key, bp.Version)

	sb.WriteString("## 1. Personas\n\n| Persona | Role | Primary goal |\n|---|---|---|\n")
	for _, p := range bp.Personas {
		goal := ""
		if len(p.Goals) > 0 {
			goal = p.Goals[0]
		}
		fmt.Fprintf(&sb, "| %s | `%s` | %s |\n", p.Name, p.Role, goal)
	}

	sb.WriteString("\n## 2. Epics\n\n")
	for i, e := range bp.Epics {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, e)
	}

	sb.WriteString("\n## 3. User stories\n\n")
	currentEpic := ""
	for _, s := range stories {
		if s.Epic != currentEpic {
			currentEpic = s.Epic
			fmt.Fprintf(&sb, "\n### %s\n\n", currentEpic)
		}
		fmt.Fprintf(&sb, "**%s** (%s) — As a **%s**, I want to %s so that %s.\n\n",
			s.ID, strings.ToUpper(s.Priority), s.Persona, s.Want, s.SoThat)
		sb.WriteString("Acceptance criteria:\n")
		for _, ac := range s.Acceptance {
			fmt.Fprintf(&sb, "- %s\n", ac)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## 4. MVP cut\n\n")
	must, should, could := countByPriority(stories)
	fmt.Fprintf(&sb, "The MVP is every `MUST` story: **%d of %d** stories. "+
		"`SHOULD` (%d) follows in the first iteration, `COULD` (%d) is backlog.\n\n",
		must, len(stories), should, could)

	sb.WriteString("## 5. Roles and permissions\n\n| Role | Capability |\n|---|---|\n")
	for _, role := range bp.Roles {
		fmt.Fprintf(&sb, "| `%s` | %s |\n", role, permissionSummary(role))
	}

	sb.WriteString("\n## 6. Non-functional requirements\n\n")
	for _, n := range bp.NFRs {
		fmt.Fprintf(&sb, "- %s\n", n)
	}

	sb.WriteString("\n## 7. Roadmap\n\n")
	sb.WriteString("| Release | Content |\n|---|---|\n")
	sb.WriteString("| R1 (MVP) | All MUST stories, authentication, core CRUD, primary screens |\n")
	sb.WriteString("| R2 | SHOULD stories, reporting depth, bulk operations, notifications |\n")
	sb.WriteString("| R3 | COULD stories, integrations, advanced permissions, audit exports |\n")

	body := sb.String()
	if err := tb.WriteFile(ctx, "docs/product/PRD.md", body); err != nil {
		return nil, err
	}
	return []*domain.Artifact{artifact(bb, domain.ArtifactPRD, "PRD.md", "text/markdown", body)}, nil
}

// buildStories derives user stories from the blueprint's entities and screens.
// Each story carries acceptance criteria, because a requirement that cannot be
// verified cannot be built correctly or tested by the QA agent.
func buildStories(bp Blueprint) []UserStory {
	var stories []UserStory
	primaryPersona := "user"
	if len(bp.Personas) > 0 {
		primaryPersona = bp.Personas[0].Role
	}
	adminPersona := "admin"
	if len(bp.Roles) > 0 {
		adminPersona = bp.Roles[0]
	}

	n := 0
	next := func() string { n++; return fmt.Sprintf("US-%03d", n) }

	stories = append(stories,
		UserStory{ID: next(), Persona: primaryPersona, Epic: "Authentication",
			Want: "sign in with my email and password", SoThat: "my data stays private",
			Priority: "must",
			Acceptance: []string{
				"Valid credentials return an access token and a refresh token",
				"Invalid credentials return 401 without revealing whether the email exists",
				"Passwords are stored using a memory-hard hash, never plaintext or a fast digest",
				"An expired access token is rejected and can be renewed with a valid refresh token",
			}},
		UserStory{ID: next(), Persona: adminPersona, Epic: "Authentication",
			Want: "invite teammates and assign roles", SoThat: "the right people have the right access",
			Priority: "must",
			Acceptance: []string{
				"An invited user receives a single-use invitation link",
				"Role changes take effect on the next request without a re-login",
				"A user without permission receives 403 and the action is written to the audit log",
			}},
	)

	// Entity CRUD stories. Join/link tables get lower priority: they are
	// implementation detail rather than a user-facing capability.
	for _, e := range bp.Entities {
		if e.Name == "User" {
			continue
		}
		priority := "must"
		if isSupportingEntity(e) {
			priority = "should"
		}
		label := humanize(e.Name)
		stories = append(stories, UserStory{
			ID: next(), Persona: primaryPersona, Epic: epicFor(bp, e.Name),
			Want:     fmt.Sprintf("create, view, update and archive %s records", strings.ToLower(label)),
			SoThat:   fmt.Sprintf("I can manage %s without leaving the product", strings.ToLower(e.Plural)),
			Priority: priority,
			Acceptance: []string{
				fmt.Sprintf("POST /api/v1/%s validates every required field and returns 201 with the created record", routePath(e)),
				fmt.Sprintf("GET /api/v1/%s supports pagination, sorting and filtering", routePath(e)),
				fmt.Sprintf("PATCH /api/v1/%s/:id applies partial updates and rejects unknown fields", routePath(e)),
				fmt.Sprintf("DELETE /api/v1/%s/:id archives rather than destroys the record", routePath(e)),
				"All four endpoints require authentication and enforce role permissions",
			},
		})
	}

	for _, s := range bp.Screens {
		stories = append(stories, UserStory{
			ID: next(), Persona: primaryPersona, Epic: "User interface",
			Want:     fmt.Sprintf("use the %s screen", s.Name),
			SoThat:   strings.ToLower(strings.TrimSuffix(s.Purpose, ".")),
			Priority: "must",
			Acceptance: []string{
				fmt.Sprintf("Route %s renders without console errors", s.Route),
				fmt.Sprintf("The screen composes %s", strings.Join(s.Components, ", ")),
				"Loading, empty and error states are all handled explicitly",
				"The layout is usable at 1280px and degrades gracefully to 768px",
			},
		})
	}

	stories = append(stories, UserStory{
		ID: next(), Persona: adminPersona, Epic: "Settings & audit",
		Want: "review an audit trail of changes", SoThat: "I can investigate who changed what",
		Priority: "could",
		Acceptance: []string{
			"Every create, update and delete writes an audit entry with actor, timestamp and diff",
			"The audit list is filterable by actor, resource type and date range",
		},
	})
	return stories
}

func isSupportingEntity(e Entity) bool {
	name := strings.ToLower(e.Name)
	for _, suffix := range []string{"line", "image", "label", "attachment", "level", "movement", "member"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// epicFor picks the most plausible epic for an entity by name overlap, falling
// back to the first epic.
func epicFor(bp Blueprint, entityName string) string {
	lower := strings.ToLower(entityName)
	for _, e := range bp.Epics {
		el := strings.ToLower(e)
		if strings.Contains(el, lower) || strings.Contains(lower, strings.Split(el, " ")[0]) {
			return e
		}
	}
	if len(bp.Epics) > 0 {
		return bp.Epics[0]
	}
	return "Core"
}

func countByPriority(stories []UserStory) (must, should, could int) {
	for _, s := range stories {
		switch s.Priority {
		case "must":
			must++
		case "should":
			should++
		default:
			could++
		}
	}
	return
}

func permissionSummary(role string) string {
	switch role {
	case "admin", "owner":
		return "Full access including settings, members and destructive operations"
	case "manager", "finance":
		return "Read and write all records in scope, plus reporting and approvals"
	case "viewer", "support":
		return "Read-only access; no mutations"
	default:
		return "Create and edit own records, read team records"
	}
}

func humanize(s string) string {
	var out strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			out.WriteByte(' ')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// visionDraft is the typed projection of the CEO agent's schema.
type visionDraft struct {
	Goal            string
	Audience        []audienceMember
	Differentiators []string
	SuccessMetrics  []string
	OutOfScope      []string
}

type audienceMember struct {
	Name string
	Need string
}

// reason asks the model for product strategy, returning nil when unavailable.
func (a *CEOAgent) reason(ctx context.Context, bb *Blackboard, tb Toolbelt) *visionDraft {
	if !bb.Reasoning.Enabled() {
		return nil
	}

	prompt := NewPrompt(bb.Reasoning.PromptBudget(ctx)).
		Add("Product brief", briefContext(bb), 0).
		Add("Structural template", blueprintContext(bb), 1).
		Add("Prior context", memoryContext(bb.Reasoning.recall(ctx, bb.Project.ID, bb.Project.Prompt, 4)), 2).
		Add("Your task", `Define the product strategy.

- goal: what this product achieves for its users, specific to this brief.
- audience: the two to four groups who will use it, each with the concrete need it serves.
- differentiators: why someone would choose this over the incumbent tools in this category.
- success_metrics: measurable outcomes that prove it is working. Include numbers.
- out_of_scope: what the first release deliberately will not do.`, 0)

	document := bb.Reasoning.think(ctx, tb, domain.RoleCEO, "the product vision",
		houseStyle+"\n\nYou are the chief executive. You decide what the product is and, just as importantly, what it is not.",
		prompt.String(), "product_vision", visionSchema, port.ClassReasoning, 0.4)
	if document == nil {
		return nil
	}

	draft := &visionDraft{
		Goal:            stringField(document, "goal"),
		Differentiators: stringSlice(document, "differentiators"),
		SuccessMetrics:  stringSlice(document, "success_metrics"),
		OutOfScope:      stringSlice(document, "out_of_scope"),
	}
	if err := critique(append(append([]string{}, draft.Differentiators...), draft.SuccessMetrics...)); err != nil {
		tb.Emit(ctx, domain.LevelWarn,
			"Discarding model vision: "+err.Error()+"; using the blueprint instead", nil)
		return nil
	}
	for _, raw := range objectSlice(document, "audience") {
		draft.Audience = append(draft.Audience, audienceMember{
			Name: stringField(raw, "name"),
			Need: stringField(raw, "need"),
		})
	}
	return draft
}

// reason asks the model for product-specific user stories.
func (a *ProductManagerAgent) reason(ctx context.Context, bb *Blackboard, tb Toolbelt) []UserStory {
	if !bb.Reasoning.Enabled() {
		return nil
	}

	visionSummary := ""
	if artifact, ok := bb.Get(domain.ArtifactVision); ok {
		visionSummary = artifact.Body
	}

	prompt := NewPrompt(bb.Reasoning.PromptBudget(ctx)).
		Add("Product brief", briefContext(bb), 0).
		Add("Approved vision", visionSummary, 1).
		Add("Structural template", blueprintContext(bb), 2).
		Add("Your task", `Write the user stories that make this product distinctive.

The system already generates routine CRUD and screen-rendering stories automatically, so do NOT
write those. Write only the stories that require product judgement: workflows, rules, states,
permissions and interactions specific to this product.

Every story needs acceptance criteria that a tester could execute and objectively pass or fail.
Vague criteria such as "works correctly" or "is user friendly" are unacceptable.`, 0)

	document := bb.Reasoning.think(ctx, tb, domain.RolePM, "the requirements",
		houseStyle+"\n\nYou are the product manager. You turn strategy into requirements that engineers can build and testers can verify.",
		prompt.String(), "product_requirements", prdSchema, port.ClassReasoning, 0.3)
	if document == nil {
		return nil
	}

	// A model that echoes the prompt back as "user stories" passes the schema
	// and is useless. Check before adopting.
	var wants []string
	for _, raw := range objectSlice(document, "stories") {
		wants = append(wants, stringField(raw, "want"))
	}
	if err := critique(wants); err != nil {
		tb.Emit(ctx, domain.LevelWarn,
			"Discarding model stories: "+err.Error()+"; using blueprint coverage instead",
			map[string]any{"rejected": len(wants)})
		return nil
	}

	var stories []UserStory
	for i, raw := range objectSlice(document, "stories") {
		story := UserStory{
			ID:         fmt.Sprintf("US-M%02d", i+1),
			Epic:       stringField(raw, "epic"),
			Persona:    stringField(raw, "persona"),
			Want:       stringField(raw, "want"),
			SoThat:     stringField(raw, "so_that"),
			Priority:   stringField(raw, "priority"),
			Acceptance: stringSlice(raw, "acceptance"),
		}
		// A story with no acceptance criteria is not a requirement, it is a
		// wish. The schema enforces a minimum, but a defensive check here keeps
		// unverifiable work out of the plan if the schema is ever relaxed.
		if story.Want == "" || len(story.Acceptance) == 0 {
			continue
		}
		if story.Epic == "" {
			story.Epic = "Product behaviour"
		}
		if story.Priority == "" {
			story.Priority = "should"
		}
		stories = append(stories, story)
	}
	return stories
}

// mergeStories places model-authored stories first and renumbers the whole set
// so identifiers stay contiguous and stable within a run.
func mergeStories(reasoned, generated []UserStory) []UserStory {
	merged := make([]UserStory, 0, len(reasoned)+len(generated))
	merged = append(merged, reasoned...)
	merged = append(merged, generated...)
	for i := range merged {
		merged[i].ID = fmt.Sprintf("US-%03d", i+1)
	}
	return merged
}
