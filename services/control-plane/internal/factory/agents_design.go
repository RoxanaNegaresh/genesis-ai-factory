package factory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// UXDesignerAgent produces the design system and user flows the frontend agent
// implements against.
type UXDesignerAgent struct{}

func (a *UXDesignerAgent) Charter() Charter {
	p, _ := domain.AgentProfileFor(domain.RoleUX)
	return Charter{
		Role: domain.RoleUX, Mission: p.Mission,
		Inputs:  []domain.ArtifactKind{domain.ArtifactPRD},
		Outputs: []domain.ArtifactKind{domain.ArtifactDesignSystem, domain.ArtifactDesignFlows},
		Tools:   []string{"fs.write"}, ModelClass: "reasoning",
		Budget: DefaultBudget(), Temperature: 0.5,
	}
}

func (a *UXDesignerAgent) Execute(ctx context.Context, bb *Blackboard, tb Toolbelt) ([]*domain.Artifact, error) {
	if !bb.Has(domain.ArtifactPRD) {
		return nil, fmt.Errorf("ux designer requires the PRD")
	}
	bp := bb.Blueprint

	// Design tokens are emitted as JSON so they are machine-consumable by the
	// frontend agent and by Tailwind config generation, not just readable prose.
	tokens := `{
  "color": {
    "background":  { "light": "#ffffff", "dark": "#0b0d10" },
    "surface":     { "light": "#f7f8fa", "dark": "#14171c" },
    "surfaceAlt":  { "light": "#eef0f4", "dark": "#1b1f26" },
    "border":      { "light": "#e2e5ea", "dark": "#262b33" },
    "text":        { "light": "#0b0d10", "dark": "#e8eaed" },
    "textMuted":   { "light": "#5b6472", "dark": "#98a1af" },
    "primary":     { "light": "#4f46e5", "dark": "#7c74f5" },
    "success":     { "light": "#0f9d58", "dark": "#31c07a" },
    "warning":     { "light": "#d98324", "dark": "#f0a745" },
    "danger":      { "light": "#d93a3a", "dark": "#f2645f" }
  },
  "radius":  { "sm": "4px", "md": "8px", "lg": "12px", "full": "9999px" },
  "spacing": { "xs": "4px", "sm": "8px", "md": "12px", "lg": "16px", "xl": "24px", "2xl": "32px" },
  "font": {
    "sans": "Inter, -apple-system, Segoe UI, sans-serif",
    "mono": "JetBrains Mono, Menlo, Consolas, monospace",
    "size": { "xs": "11px", "sm": "12px", "md": "13px", "lg": "15px", "xl": "18px", "2xl": "22px" }
  },
  "elevation": {
    "sm": "0 1px 2px rgba(0,0,0,0.06)",
    "md": "0 4px 12px rgba(0,0,0,0.10)",
    "lg": "0 12px 32px rgba(0,0,0,0.16)"
  },
  "motion": { "fast": "120ms", "base": "180ms", "slow": "280ms", "easing": "cubic-bezier(0.16,1,0.3,1)" }
}
`
	if err := tb.WriteFile(ctx, "docs/design/tokens.json", tokens); err != nil {
		return nil, err
	}

	components := componentInventory(bp)

	var sb strings.Builder
	sb.WriteString("# Design System\n\n")
	sb.WriteString("## Principles\n\n")
	sb.WriteString("1. **Density over decoration.** This is a work tool; information per screen matters more than whitespace.\n")
	sb.WriteString("2. **One primary action per view.** Everything else is secondary or in a menu.\n")
	sb.WriteString("3. **Keyboard first.** Every frequent action has a shortcut and a visible focus state.\n")
	sb.WriteString("4. **Explicit states.** Loading, empty, error and permission-denied are designed, never accidental.\n")
	sb.WriteString("5. **Optimistic interaction.** Mutations render immediately and reconcile; latency is never exposed as a spinner where avoidable.\n\n")

	sb.WriteString("## Tokens\n\nSee `docs/design/tokens.json` — colour (light/dark), radius, spacing, type scale, elevation and motion.\n\n")

	sb.WriteString("## Component inventory\n\n| Component | Used by | Notes |\n|---|---|---|\n")
	for _, c := range components {
		fmt.Fprintf(&sb, "| `%s` | %s | %s |\n", c.name, strings.Join(c.screens, ", "), c.note)
	}

	sb.WriteString("\n## Layout\n\n")
	sb.WriteString("```\n")
	sb.WriteString("┌──────────────────────────────────────────────────────┐\n")
	sb.WriteString("│ TopBar: workspace switcher · search · user menu       │\n")
	sb.WriteString("├────────────┬─────────────────────────────────────────┤\n")
	sb.WriteString("│ SideNav    │ Page header: title · filters · actions   │\n")
	sb.WriteString("│ (collapsi- ├─────────────────────────────────────────┤\n")
	sb.WriteString("│  ble)      │ Content region                          │\n")
	sb.WriteString("│            │                                         │\n")
	sb.WriteString("└────────────┴─────────────────────────────────────────┘\n")
	sb.WriteString("```\n\n")
	sb.WriteString("Breakpoints: `sm 640` · `md 768` · `lg 1024` · `xl 1280`. ")
	sb.WriteString("The side navigation collapses to icons below `lg` and to a drawer below `md`.\n\n")

	sb.WriteString("## Accessibility\n\n")
	sb.WriteString("- Contrast ratio of at least 4.5:1 for body text in both themes\n")
	sb.WriteString("- Every interactive element is reachable and operable by keyboard\n")
	sb.WriteString("- Focus rings are visible and never removed without a replacement\n")
	sb.WriteString("- Live regions announce async results to screen readers\n")

	systemBody := sb.String()
	if err := tb.WriteFile(ctx, "docs/design/DESIGN_SYSTEM.md", systemBody); err != nil {
		return nil, err
	}

	var fb strings.Builder
	fb.WriteString("# User Flows & Screen Map\n\n")
	fb.WriteString("## Screen map\n\n| Screen | Route | Primary data | Purpose |\n|---|---|---|---|\n")
	for _, s := range bp.Screens {
		fmt.Fprintf(&fb, "| %s | `%s` | %s | %s |\n", s.Name, s.Route, s.PrimaryData, s.Purpose)
	}

	fb.WriteString("\n## Core flows\n\n")
	for i, flow := range coreFlows(bp) {
		fmt.Fprintf(&fb, "### Flow %d — %s\n\n", i+1, flow.name)
		for j, step := range flow.steps {
			fmt.Fprintf(&fb, "%d. %s\n", j+1, step)
		}
		fmt.Fprintf(&fb, "\n**Failure handling:** %s\n\n", flow.failure)
	}

	fb.WriteString("## Navigation structure\n\n")
	for _, s := range bp.Screens {
		depth := strings.Count(strings.Trim(s.Route, "/"), "/")
		fmt.Fprintf(&fb, "%s- %s (`%s`)\n", strings.Repeat("  ", depth), s.Name, s.Route)
	}

	flowsBody := fb.String()
	if err := tb.WriteFile(ctx, "docs/design/USER_FLOWS.md", flowsBody); err != nil {
		return nil, err
	}

	tb.Emit(ctx, domain.LevelInfo,
		fmt.Sprintf("Specified %d screens and %d shared components", len(bp.Screens), len(components)),
		map[string]any{"screens": len(bp.Screens), "components": len(components)})

	return []*domain.Artifact{
		artifact(bb, domain.ArtifactDesignSystem, "DESIGN_SYSTEM.md", "text/markdown", systemBody),
		artifact(bb, domain.ArtifactDesignFlows, "USER_FLOWS.md", "text/markdown", flowsBody),
	}, nil
}

type componentSpec struct {
	name    string
	screens []string
	note    string
}

// componentInventory inverts the screen list into a deduplicated component
// catalogue, which is what tells the frontend agent to build a component once
// and reuse it rather than per screen.
func componentInventory(bp Blueprint) []componentSpec {
	index := map[string][]string{}
	for _, s := range bp.Screens {
		for _, c := range s.Components {
			index[c] = append(index[c], s.Name)
		}
	}
	names := make([]string, 0, len(index))
	for n := range index {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]componentSpec, 0, len(names))
	for _, n := range names {
		note := "Single-purpose presentational component"
		if len(index[n]) > 1 {
			note = "Shared across screens — build once in `components/`"
		}
		if strings.Contains(n, "Table") || strings.Contains(n, "Board") || strings.Contains(n, "List") {
			note += "; must virtualise beyond 200 rows"
		}
		out = append(out, componentSpec{name: n, screens: index[n], note: note})
	}
	return out
}

type flow struct {
	name    string
	steps   []string
	failure string
}

func coreFlows(bp Blueprint) []flow {
	entityName := "record"
	entityPlural := "records"
	for _, e := range bp.Entities {
		if e.Name != "User" && !isSupportingEntity(e) {
			entityName = strings.ToLower(humanize(e.Name))
			entityPlural = e.Plural
			break
		}
	}
	primaryScreen := "the main screen"
	if len(bp.Screens) > 0 {
		primaryScreen = bp.Screens[0].Name
	}

	return []flow{
		{
			name: "First run and sign in",
			steps: []string{
				"User opens the application and is redirected to `/login`",
				"User submits email and password",
				"Server returns an access token (memory) and refresh token (httpOnly cookie)",
				fmt.Sprintf("User lands on %s with their workspace loaded", primaryScreen),
			},
			failure: "Invalid credentials show an inline error without clearing the email field; three failures add a short backoff.",
		},
		{
			name: fmt.Sprintf("Create a %s", entityName),
			steps: []string{
				fmt.Sprintf("User clicks the primary action on the %s screen", entityPlural),
				"A dialog opens with focus on the first field and required fields marked",
				"Client-side validation runs on blur; the submit button reflects validity",
				"On submit the row appears optimistically with a pending indicator",
				"The server response replaces the optimistic row with the canonical record",
			},
			failure: "A server rejection rolls the optimistic row back, restores the dialog with the entered values, and maps field errors onto inputs.",
		},
		{
			name: fmt.Sprintf("Find and edit a %s", entityName),
			steps: []string{
				"User types in the search field; results filter after a 200ms debounce",
				"User opens a record; the detail view loads with cached data first, then revalidates",
				"User edits a field inline; the change saves on blur",
				"A toast confirms the save and offers undo for 5 seconds",
			},
			failure: "A failed save reverts the field, keeps the edit in the input, and surfaces a retry action.",
		},
	}
}
