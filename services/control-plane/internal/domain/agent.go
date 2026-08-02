package domain

// AgentRole identifies a member of the simulated engineering organization.
// The roster is closed: adding a role is a deliberate design act, not a string
// literal typed at a call site.
type AgentRole string

const (
	RoleCEO       AgentRole = "ceo"
	RolePM        AgentRole = "product_manager"
	RoleUX        AgentRole = "ux_designer"
	RoleArchitect AgentRole = "system_architect"
	RoleBackend   AgentRole = "backend_engineer"
	RoleFrontend  AgentRole = "frontend_engineer"
	RoleDatabase  AgentRole = "database_engineer"
	RoleQA        AgentRole = "qa_engineer"
	RoleSecurity  AgentRole = "security_engineer"
	RoleDevOps    AgentRole = "devops_engineer"
	RoleImprover  AgentRole = "improver"
	RoleSystem    AgentRole = "system"
)

// AgentStatus is the live state of an agent within a run.
type AgentStatus string

const (
	AgentIdle    AgentStatus = "idle"
	AgentWorking AgentStatus = "working"
	AgentBlocked AgentStatus = "blocked"
	AgentDone    AgentStatus = "done"
	AgentFailed  AgentStatus = "failed"
)

// AgentProfile is static metadata about a role, used by the UI roster and by
// the orchestrator when it assigns tasks.
type AgentProfile struct {
	Role       AgentRole `json:"role"`
	Name       string    `json:"name"`
	Title      string    `json:"title"`
	Mission    string    `json:"mission"`
	Phase      Phase     `json:"phase"`
	Produces   []string  `json:"produces"`
	Consumes   []string  `json:"consumes"`
	ModelClass string    `json:"model_class"`
	Accent     string    `json:"accent"`
}

// agentRoster is the authoritative definition of the organization. The UI, the
// orchestrator and the CLI all read this one list, so they cannot disagree
// about who exists or what they do.
var agentRoster = []AgentProfile{
	{
		Role: RoleCEO, Name: "Atlas", Title: "Chief Executive",
		Mission:    "Turn a raw brief into a defensible product strategy: goal, audience, differentiators, success metrics and explicit scope guardrails.",
		Phase:      PhaseAnalyze,
		Produces:   []string{"product.vision"},
		Consumes:   []string{"user.brief"},
		ModelClass: "reasoning", Accent: "#8b5cf6",
	},
	{
		Role: RolePM, Name: "Nova", Title: "Product Manager",
		Mission:    "Convert strategy into personas, epics and user stories with testable acceptance criteria, then cut a defensible MVP.",
		Phase:      PhaseAnalyze,
		Produces:   []string{"product.prd"},
		Consumes:   []string{"product.vision"},
		ModelClass: "reasoning", Accent: "#3b82f6",
	},
	{
		Role: RoleUX, Name: "Iris", Title: "UX Designer",
		Mission:    "Design the user flows, information architecture, design tokens and screen inventory the frontend will implement.",
		Phase:      PhaseDesign,
		Produces:   []string{"design.system", "design.flows"},
		Consumes:   []string{"product.prd"},
		ModelClass: "reasoning", Accent: "#ec4899",
	},
	{
		Role: RoleArchitect, Name: "Vector", Title: "System Architect",
		Mission:    "Choose the stack, draw service boundaries, define API contracts and non-functional targets, and record the decisions.",
		Phase:      PhaseDesign,
		Produces:   []string{"arch.spec", "arch.adr"},
		Consumes:   []string{"product.prd", "product.vision"},
		ModelClass: "reasoning", Accent: "#06b6d4",
	},
	{
		Role: RoleDatabase, Name: "Strata", Title: "Database Engineer",
		Mission:    "Design the schema, indexes and migrations that make the product's access patterns fast and its data correct.",
		Phase:      PhaseDesign,
		Produces:   []string{"db.schema", "db.migrations"},
		Consumes:   []string{"arch.spec", "product.prd"},
		ModelClass: "code", Accent: "#f59e0b",
	},
	{
		Role: RoleBackend, Name: "Forge", Title: "Backend Engineer",
		Mission:    "Implement the server: domain model, use cases, HTTP layer, authentication, authorization and unit tests.",
		Phase:      PhaseBuild,
		Produces:   []string{"code.backend"},
		Consumes:   []string{"arch.spec", "db.schema"},
		ModelClass: "code", Accent: "#10b981",
	},
	{
		Role: RoleFrontend, Name: "Prism", Title: "Frontend Engineer",
		Mission:    "Implement the client: pages, components, state, routing and API integration against the design system.",
		Phase:      PhaseBuild,
		Produces:   []string{"code.frontend"},
		Consumes:   []string{"design.system", "design.flows", "arch.spec"},
		ModelClass: "code", Accent: "#6366f1",
	},
	{
		Role: RoleQA, Name: "Sentry", Title: "QA Engineer",
		Mission:    "Generate and execute tests, verify acceptance criteria, and produce reproducible failure reports.",
		Phase:      PhaseVerify,
		Produces:   []string{"qa.report"},
		Consumes:   []string{"code.backend", "code.frontend"},
		ModelClass: "code", Accent: "#14b8a6",
	},
	{
		Role: RoleSecurity, Name: "Aegis", Title: "Security Engineer",
		Mission:    "Audit authorization, injection surfaces, secret handling and dependencies; deliver findings with patches.",
		Phase:      PhaseVerify,
		Produces:   []string{"sec.report"},
		Consumes:   []string{"code.backend", "code.frontend", "arch.spec"},
		ModelClass: "reasoning", Accent: "#ef4444",
	},
	{
		Role: RoleDevOps, Name: "Relay", Title: "DevOps Engineer",
		Mission:    "Produce the container images, compose stack, CI pipeline, reverse proxy config and runbook needed to ship.",
		Phase:      PhaseShip,
		Produces:   []string{"ops.docker", "ops.ci", "docs.readme"},
		Consumes:   []string{"arch.spec", "code.backend", "code.frontend"},
		ModelClass: "code", Accent: "#0ea5e9",
	},
	{
		Role: RoleImprover, Name: "Kaizen", Title: "Improvement Agent",
		Mission:    "Analyse the shipped product for gaps, performance risks, security posture and UX friction; queue the next iteration.",
		Phase:      PhaseShip,
		Produces:   []string{"improve.plan"},
		Consumes:   []string{"qa.report", "sec.report"},
		ModelClass: "reasoning", Accent: "#a855f7",
	},
}

// AgentRoster returns a copy of the organization definition.
func AgentRoster() []AgentProfile {
	out := make([]AgentProfile, len(agentRoster))
	copy(out, agentRoster)
	return out
}

// AgentProfileFor looks up a role's static metadata.
func AgentProfileFor(role AgentRole) (AgentProfile, bool) {
	for _, p := range agentRoster {
		if p.Role == role {
			return p, true
		}
	}
	return AgentProfile{}, false
}

// AgentsForPhase returns the roles that participate in a phase, in roster order
// (which is also a valid execution order).
func AgentsForPhase(phase Phase) []AgentProfile {
	var out []AgentProfile
	for _, p := range agentRoster {
		if p.Phase == phase {
			out = append(out, p)
		}
	}
	return out
}

// DisplayName renders a role for humans.
func (r AgentRole) DisplayName() string {
	if p, ok := AgentProfileFor(r); ok {
		return p.Name + " · " + p.Title
	}
	if r == RoleSystem {
		return "Genesis · System"
	}
	return string(r)
}
