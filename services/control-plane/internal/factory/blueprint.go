// Package factory contains the product generation engine: category
// classification, product blueprints, the agent implementations and the run
// driver that executes the autonomous development loop.
package factory

import (
	"sort"
	"strings"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
)

// Field is one attribute of a domain entity in a blueprint.
type Field struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // uuid|text|int|decimal|bool|timestamp|json|enum|ref
	Required bool     `json:"required"`
	Ref      string   `json:"ref,omitempty"`  // target entity for ref fields
	Enum     []string `json:"enum,omitempty"` // allowed values for enum fields
	Note     string   `json:"note,omitempty"`
}

// Entity is a first-class object in the generated product.
type Entity struct {
	Name        string   `json:"name"`
	Plural      string   `json:"plural"`
	Description string   `json:"description"`
	Fields      []Field  `json:"fields"`
	Indexes     []string `json:"indexes,omitempty"`
}

// Screen is a UI surface the frontend agent must build.
type Screen struct {
	Name        string   `json:"name"`
	Route       string   `json:"route"`
	Purpose     string   `json:"purpose"`
	Components  []string `json:"components"`
	PrimaryData string   `json:"primary_data,omitempty"`
}

// UserStory is a requirement with testable acceptance criteria. Stories without
// acceptance criteria are how specifications quietly become unverifiable, so
// the type makes them mandatory in the critic pass.
type UserStory struct {
	ID         string   `json:"id"`
	Persona    string   `json:"persona"`
	Want       string   `json:"want"`
	SoThat     string   `json:"so_that"`
	Acceptance []string `json:"acceptance"`
	Priority   string   `json:"priority"` // must|should|could
	Epic       string   `json:"epic"`
}

// Persona is a user archetype.
type Persona struct {
	Name  string   `json:"name"`
	Role  string   `json:"role"`
	Goals []string `json:"goals"`
	Pains []string `json:"pains"`
}

// Blueprint is a reusable product template: the accumulated knowledge of what a
// given class of software must contain. Blueprints make generation reliable —
// a model asked to invent a CRM from nothing omits things; a model filling in a
// blueprint does not.
type Blueprint struct {
	Key          string                 `json:"key"`
	Version      string                 `json:"version"`
	Category     domain.ProjectCategory `json:"category"`
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Personas     []Persona              `json:"personas"`
	Epics        []string               `json:"epics"`
	Entities     []Entity               `json:"entities"`
	Screens      []Screen               `json:"screens"`
	Roles        []string               `json:"roles"`
	Integrations []string               `json:"integrations,omitempty"`
	NFRs         []string               `json:"nfrs"`
	Keywords     []string               `json:"-"`
}

// baseFields are attached to every entity: identity, audit and soft delete are
// not optional in a product that people run in production.
func baseFields() []Field {
	return []Field{
		{Name: "id", Type: "uuid", Required: true},
		{Name: "created_at", Type: "timestamp", Required: true},
		{Name: "updated_at", Type: "timestamp", Required: true},
	}
}

func entity(name, plural, desc string, fields ...Field) Entity {
	return Entity{
		Name:        name,
		Plural:      plural,
		Description: desc,
		Fields:      append(baseFields(), fields...),
	}
}

func f(name, typ string, required bool) Field {
	return Field{Name: name, Type: typ, Required: required}
}

func ref(name, target string, required bool) Field {
	return Field{Name: name, Type: "ref", Ref: target, Required: required}
}

func enum(name string, values ...string) Field {
	return Field{Name: name, Type: "enum", Required: true, Enum: values}
}

// commonNFRs apply to every generated product.
func commonNFRs() []string {
	return []string{
		"Authenticated API with role-based authorization on every mutating endpoint",
		"p95 read latency under 200ms for list endpoints with 10k rows",
		"All list endpoints paginated and filterable",
		"Structured audit trail for create, update and delete operations",
		"Automated test suite covering domain rules and HTTP contracts",
		"Container image and compose stack for one-command local run",
	}
}

// registry holds the built-in blueprints.
var registry = []Blueprint{crmBlueprint(), pmBlueprint(), erpBlueprint(), marketplaceBlueprint(), saasBlueprint()}

// Blueprints returns every built-in template.
func Blueprints() []Blueprint {
	out := make([]Blueprint, len(registry))
	copy(out, registry)
	return out
}

// BlueprintFor returns the template for a category.
func BlueprintFor(category domain.ProjectCategory) Blueprint {
	for _, b := range registry {
		if b.Category == category {
			return b
		}
	}
	return saasBlueprint()
}

// Classification is the result of analysing a natural-language brief.
type Classification struct {
	Category   domain.ProjectCategory `json:"category"`
	Confidence float64                `json:"confidence"`
	Matched    []string               `json:"matched_signals"`
	Runners    []string               `json:"runner_up_categories"`
}

// Classify infers the product archetype from a brief.
//
// This is a deterministic weighted keyword classifier, not a model call. That
// is a deliberate choice for v0.1: classification must be instant, free,
// offline and reproducible, and a scored lexicon handles the overwhelming
// majority of real briefs ("build a Jira competitor", "CRM for my sales team").
// v0.2 keeps this as the fast path and escalates only low-confidence briefs to
// a model, which is both cheaper and more predictable than always inferring.
func Classify(prompt string) Classification {
	text := " " + strings.ToLower(prompt) + " "

	type scored struct {
		category domain.ProjectCategory
		score    float64
		matched  []string
	}

	results := make([]scored, 0, len(registry))
	for _, b := range registry {
		s := scored{category: b.Category}
		for _, kw := range b.Keywords {
			weight := 1.0
			// Multi-word signals ("sales pipeline") are far more discriminating
			// than single tokens ("sales"), so they score higher.
			if strings.Contains(kw, " ") {
				weight = 2.5
			}
			if strings.Contains(text, " "+kw) || strings.Contains(text, kw+" ") {
				s.score += weight
				s.matched = append(s.matched, kw)
			}
		}
		results = append(results, s)
	}

	sort.SliceStable(results, func(i, j int) bool { return results[i].score > results[j].score })

	best := results[0]
	if best.score == 0 {
		return Classification{Category: domain.CategoryCustom, Confidence: 0, Matched: nil}
	}

	// Confidence reflects how decisively the winner beat the field: a brief
	// matching both "crm" and "marketplace" signals should not be reported as
	// certain.
	var total float64
	for _, r := range results {
		total += r.score
	}
	confidence := best.score / total
	if len(results) > 1 && results[1].score > 0 {
		margin := (best.score - results[1].score) / best.score
		confidence = (confidence + margin) / 2
	}
	if confidence > 0.99 {
		confidence = 0.99
	}

	var runners []string
	for _, r := range results[1:] {
		if r.score > 0 {
			runners = append(runners, string(r.category))
		}
	}
	sort.Strings(best.matched)
	return Classification{
		Category:   best.category,
		Confidence: round2(confidence),
		Matched:    best.matched,
		Runners:    runners,
	}
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

// --- built-in blueprints -------------------------------------------------

func crmBlueprint() Blueprint {
	return Blueprint{
		Key: "crm", Version: "1.0.0", Category: domain.CategoryCRM,
		Name:        "Customer Relationship Management",
		Description: "Track contacts, qualify leads, manage a sales pipeline and report on revenue.",
		Keywords: []string{"crm", "customer relationship", "sales pipeline", "leads", "lead",
			"deals", "salesforce", "hubspot", "pipedrive", "contacts", "sales team", "prospect", "opportunity"},
		Personas: []Persona{
			{Name: "Sales Representative", Role: "sales_rep",
				Goals: []string{"Close more deals", "Never lose track of a follow-up"},
				Pains: []string{"Context scattered across email and spreadsheets"}},
			{Name: "Sales Manager", Role: "manager",
				Goals: []string{"Forecast revenue accurately", "Spot stalled deals early"},
				Pains: []string{"No reliable pipeline visibility"}},
		},
		Epics: []string{"Contact management", "Lead qualification", "Pipeline & deals", "Activities & tasks", "Reporting"},
		Roles: []string{"admin", "manager", "sales_rep", "viewer"},
		Entities: []Entity{
			entity("Company", "companies", "An organisation you sell to",
				f("name", "text", true), f("domain", "text", false), f("industry", "text", false),
				f("size", "int", false), f("annual_revenue", "decimal", false)),
			entity("Contact", "contacts", "A person at a company",
				f("first_name", "text", true), f("last_name", "text", true), f("email", "text", true),
				f("phone", "text", false), f("title", "text", false), ref("company_id", "Company", false),
				ref("owner_id", "User", true)),
			entity("Lead", "leads", "An unqualified potential customer",
				f("name", "text", true), f("email", "text", true), f("source", "text", false),
				enum("status", "new", "contacted", "qualified", "unqualified"),
				f("score", "int", false), ref("owner_id", "User", true)),
			entity("Deal", "deals", "A revenue opportunity moving through the pipeline",
				f("title", "text", true), f("value", "decimal", true), f("currency", "text", true),
				ref("stage_id", "PipelineStage", true), ref("company_id", "Company", false),
				ref("contact_id", "Contact", false), ref("owner_id", "User", true),
				f("expected_close_date", "timestamp", false), f("probability", "int", false),
				enum("status", "open", "won", "lost")),
			entity("PipelineStage", "pipeline_stages", "An ordered stage of the sales process",
				f("name", "text", true), f("position", "int", true), f("win_probability", "int", false)),
			entity("Activity", "activities", "A call, email, meeting or note attached to a record",
				enum("kind", "call", "email", "meeting", "note"), f("subject", "text", true),
				f("body", "text", false), f("due_at", "timestamp", false), f("completed_at", "timestamp", false),
				ref("owner_id", "User", true), f("related_type", "text", false), f("related_id", "uuid", false)),
			entity("User", "users", "An authenticated member of the sales organisation",
				f("email", "text", true), f("display_name", "text", true),
				enum("role", "admin", "manager", "sales_rep", "viewer"), f("password_hash", "text", true)),
		},
		Screens: []Screen{
			{Name: "Dashboard", Route: "/", Purpose: "Pipeline value, deals closing this month, activity feed",
				Components: []string{"MetricCard", "PipelineFunnel", "ActivityFeed"}, PrimaryData: "Deal"},
			{Name: "Contacts", Route: "/contacts", Purpose: "Searchable contact directory",
				Components: []string{"DataTable", "FilterBar", "ContactDrawer"}, PrimaryData: "Contact"},
			{Name: "Leads", Route: "/leads", Purpose: "Qualify inbound leads and convert them",
				Components: []string{"DataTable", "LeadScoreBadge", "ConvertDialog"}, PrimaryData: "Lead"},
			{Name: "Pipeline", Route: "/pipeline", Purpose: "Drag deals between stages",
				Components: []string{"KanbanBoard", "DealCard", "StageColumn"}, PrimaryData: "Deal"},
			{Name: "Deal Detail", Route: "/deals/:id", Purpose: "Full deal context and activity history",
				Components: []string{"DetailHeader", "Timeline", "ActivityComposer"}, PrimaryData: "Deal"},
			{Name: "Reports", Route: "/reports", Purpose: "Revenue forecast and win/loss analysis",
				Components: []string{"BarChart", "LineChart", "DateRangePicker"}, PrimaryData: "Deal"},
		},
		NFRs: append(commonNFRs(), "Pipeline board must remain responsive with 1000 open deals"),
	}
}

func pmBlueprint() Blueprint {
	return Blueprint{
		Key: "pm", Version: "1.0.0", Category: domain.CategoryPM,
		Name:        "Project & Issue Tracking",
		Description: "Plan work, track issues on boards and sprints, and coordinate a team.",
		Keywords: []string{"jira", "project management", "issue tracker", "issue tracking", "kanban",
			"scrum", "sprint", "backlog", "task management", "asana", "linear", "trello", "monday",
			"tickets", "agile", "epics", "story points", "task tracker"},
		Personas: []Persona{
			{Name: "Engineer", Role: "member",
				Goals: []string{"See exactly what to work on next", "Update status without ceremony"},
				Pains: []string{"Tools that demand more admin than they save"}},
			{Name: "Project Lead", Role: "manager",
				Goals: []string{"Track sprint progress", "Identify blockers early"},
				Pains: []string{"Status reports assembled by hand"}},
		},
		Epics: []string{"Projects & teams", "Issues & workflow", "Boards", "Sprints", "Notifications", "Permissions"},
		Roles: []string{"admin", "manager", "member", "viewer"},
		Entities: []Entity{
			entity("Project", "projects", "A container for work",
				f("key", "text", true), f("name", "text", true), f("description", "text", false),
				ref("lead_id", "User", false), enum("visibility", "private", "team", "public")),
			entity("Issue", "issues", "A unit of trackable work",
				f("key", "text", true), f("title", "text", true), f("description", "text", false),
				enum("type", "epic", "story", "task", "bug", "subtask"),
				enum("status", "backlog", "todo", "in_progress", "in_review", "done"),
				enum("priority", "lowest", "low", "medium", "high", "highest"),
				ref("project_id", "Project", true), ref("assignee_id", "User", false),
				ref("reporter_id", "User", true), ref("parent_id", "Issue", false),
				ref("sprint_id", "Sprint", false), f("story_points", "int", false),
				f("due_date", "timestamp", false), f("position", "decimal", true)),
			entity("Sprint", "sprints", "A time-boxed iteration",
				f("name", "text", true), ref("project_id", "Project", true),
				f("starts_at", "timestamp", false), f("ends_at", "timestamp", false),
				enum("state", "planned", "active", "completed"), f("goal", "text", false)),
			entity("Board", "boards", "A visual workflow over issues",
				f("name", "text", true), ref("project_id", "Project", true),
				enum("kind", "kanban", "scrum"), f("column_config", "json", true)),
			entity("Comment", "comments", "Discussion on an issue",
				ref("issue_id", "Issue", true), ref("author_id", "User", true), f("body", "text", true)),
			entity("Attachment", "attachments", "A file attached to an issue",
				ref("issue_id", "Issue", true), f("filename", "text", true),
				f("size_bytes", "int", true), f("content_type", "text", true), f("storage_path", "text", true)),
			entity("Label", "labels", "A tag applied to issues",
				f("name", "text", true), f("color", "text", true), ref("project_id", "Project", true)),
			entity("Team", "teams", "A group of collaborators",
				f("name", "text", true), f("description", "text", false)),
			entity("Notification", "notifications", "An in-app alert",
				ref("user_id", "User", true), f("kind", "text", true), f("payload", "json", true),
				f("read_at", "timestamp", false)),
			entity("User", "users", "An authenticated team member",
				f("email", "text", true), f("display_name", "text", true), f("avatar_url", "text", false),
				enum("role", "admin", "manager", "member", "viewer"), f("password_hash", "text", true)),
		},
		Screens: []Screen{
			{Name: "My Work", Route: "/", Purpose: "Issues assigned to the current user across projects",
				Components: []string{"IssueList", "QuickFilter", "StatusChip"}, PrimaryData: "Issue"},
			{Name: "Board", Route: "/projects/:key/board", Purpose: "Drag issues across workflow columns",
				Components: []string{"KanbanBoard", "IssueCard", "SwimLane", "SprintHeader"}, PrimaryData: "Issue"},
			{Name: "Backlog", Route: "/projects/:key/backlog", Purpose: "Rank and groom upcoming work",
				Components: []string{"SortableList", "SprintPlanner", "EstimateBadge"}, PrimaryData: "Issue"},
			{Name: "Issue Detail", Route: "/issues/:key", Purpose: "Full issue context, comments and history",
				Components: []string{"DetailHeader", "MarkdownEditor", "CommentThread", "ActivityLog"}, PrimaryData: "Issue"},
			{Name: "Sprint Report", Route: "/projects/:key/reports", Purpose: "Burndown and velocity",
				Components: []string{"BurndownChart", "VelocityChart"}, PrimaryData: "Sprint"},
			{Name: "Project Settings", Route: "/projects/:key/settings", Purpose: "Workflow, labels and members",
				Components: []string{"TabbedForm", "MemberTable", "WorkflowEditor"}, PrimaryData: "Project"},
		},
		NFRs: append(commonNFRs(),
			"Board drag-and-drop must apply optimistically and reconcile with the server",
			"Issue keys are stable, human-readable and unique per project"),
	}
}

func erpBlueprint() Blueprint {
	return Blueprint{
		Key: "erp", Version: "1.0.0", Category: domain.CategoryERP,
		Name:        "Enterprise Resource Planning",
		Description: "Run inventory, procurement, sales orders, production and basic accounting.",
		Keywords: []string{"erp", "enterprise resource", "inventory", "manufacturing", "warehouse",
			"purchase order", "procurement", "accounting", "bill of materials", "stock management",
			"supply chain", "sap", "odoo", "production planning", "goods receipt"},
		Personas: []Persona{
			{Name: "Warehouse Operator", Role: "operator",
				Goals: []string{"Record stock movements quickly and correctly"},
				Pains: []string{"Paper processes and stock discrepancies"}},
			{Name: "Procurement Officer", Role: "buyer",
				Goals: []string{"Reorder before stockouts", "Track supplier performance"},
				Pains: []string{"No visibility of incoming goods"}},
			{Name: "Controller", Role: "finance",
				Goals: []string{"Accurate inventory valuation", "Clean audit trail"},
				Pains: []string{"Manual reconciliation between systems"}},
		},
		Epics: []string{"Product & inventory", "Procurement", "Sales orders", "Production", "Accounting", "Reporting"},
		Roles: []string{"admin", "finance", "buyer", "operator", "viewer"},
		Entities: []Entity{
			entity("Product", "products", "A stock-keeping item",
				f("sku", "text", true), f("name", "text", true), f("description", "text", false),
				enum("type", "raw_material", "component", "finished_good", "service"),
				f("unit_of_measure", "text", true), f("cost_price", "decimal", true),
				f("sale_price", "decimal", true), f("reorder_point", "int", false)),
			entity("Warehouse", "warehouses", "A physical stock location",
				f("code", "text", true), f("name", "text", true), f("address", "text", false)),
			entity("StockLevel", "stock_levels", "Quantity of a product at a location",
				ref("product_id", "Product", true), ref("warehouse_id", "Warehouse", true),
				f("quantity_on_hand", "decimal", true), f("quantity_reserved", "decimal", true)),
			entity("StockMovement", "stock_movements", "An auditable change in stock",
				ref("product_id", "Product", true), ref("warehouse_id", "Warehouse", true),
				enum("kind", "receipt", "issue", "transfer", "adjustment"),
				f("quantity", "decimal", true), f("reference_type", "text", false),
				f("reference_id", "uuid", false), ref("performed_by", "User", true)),
			entity("Supplier", "suppliers", "A vendor you purchase from",
				f("name", "text", true), f("email", "text", false), f("phone", "text", false),
				f("payment_terms", "text", false), f("lead_time_days", "int", false)),
			entity("PurchaseOrder", "purchase_orders", "An order placed with a supplier",
				f("number", "text", true), ref("supplier_id", "Supplier", true),
				enum("status", "draft", "sent", "partially_received", "received", "cancelled"),
				f("ordered_at", "timestamp", false), f("expected_at", "timestamp", false),
				f("total_amount", "decimal", true), f("currency", "text", true)),
			entity("PurchaseOrderLine", "purchase_order_lines", "A line item on a purchase order",
				ref("purchase_order_id", "PurchaseOrder", true), ref("product_id", "Product", true),
				f("quantity", "decimal", true), f("unit_price", "decimal", true),
				f("received_quantity", "decimal", true)),
			entity("Customer", "customers", "An organisation you sell to",
				f("name", "text", true), f("email", "text", false), f("billing_address", "text", false),
				f("credit_limit", "decimal", false)),
			entity("SalesOrder", "sales_orders", "A confirmed customer order",
				f("number", "text", true), ref("customer_id", "Customer", true),
				enum("status", "draft", "confirmed", "picking", "shipped", "invoiced", "cancelled"),
				f("total_amount", "decimal", true), f("currency", "text", true),
				f("ordered_at", "timestamp", false)),
			entity("SalesOrderLine", "sales_order_lines", "A line item on a sales order",
				ref("sales_order_id", "SalesOrder", true), ref("product_id", "Product", true),
				f("quantity", "decimal", true), f("unit_price", "decimal", true)),
			entity("BillOfMaterials", "bills_of_materials", "Components required to build a product",
				ref("product_id", "Product", true), f("version", "text", true), f("is_active", "bool", true)),
			entity("BomLine", "bom_lines", "A component requirement",
				ref("bom_id", "BillOfMaterials", true), ref("component_id", "Product", true),
				f("quantity", "decimal", true)),
			entity("ProductionOrder", "production_orders", "A manufacturing job",
				f("number", "text", true), ref("product_id", "Product", true),
				f("quantity", "decimal", true),
				enum("status", "planned", "released", "in_progress", "completed", "cancelled"),
				f("scheduled_start", "timestamp", false), f("scheduled_end", "timestamp", false)),
			entity("Invoice", "invoices", "A financial document",
				f("number", "text", true), enum("kind", "customer", "supplier"),
				enum("status", "draft", "posted", "paid", "void"),
				f("amount", "decimal", true), f("tax_amount", "decimal", true),
				f("due_date", "timestamp", false), f("reference_id", "uuid", false)),
			entity("JournalEntry", "journal_entries", "A double-entry accounting record",
				f("entry_date", "timestamp", true), f("account_code", "text", true),
				f("debit", "decimal", true), f("credit", "decimal", true),
				f("description", "text", false), f("reference_id", "uuid", false)),
			entity("User", "users", "An authenticated employee",
				f("email", "text", true), f("display_name", "text", true),
				enum("role", "admin", "finance", "buyer", "operator", "viewer"), f("password_hash", "text", true)),
		},
		Screens: []Screen{
			{Name: "Operations Dashboard", Route: "/", Purpose: "Stockouts, open orders, production status",
				Components: []string{"MetricCard", "AlertList", "OrderTable"}, PrimaryData: "StockLevel"},
			{Name: "Products", Route: "/products", Purpose: "Item master with stock levels",
				Components: []string{"DataTable", "StockBadge", "ProductForm"}, PrimaryData: "Product"},
			{Name: "Inventory", Route: "/inventory", Purpose: "Stock by warehouse and movement history",
				Components: []string{"DataTable", "MovementTimeline", "AdjustmentDialog"}, PrimaryData: "StockLevel"},
			{Name: "Purchase Orders", Route: "/purchasing", Purpose: "Create and receive supplier orders",
				Components: []string{"DataTable", "OrderBuilder", "ReceiveGoodsDialog"}, PrimaryData: "PurchaseOrder"},
			{Name: "Sales Orders", Route: "/sales", Purpose: "Confirm, pick and ship customer orders",
				Components: []string{"DataTable", "OrderBuilder", "FulfilmentPanel"}, PrimaryData: "SalesOrder"},
			{Name: "Production", Route: "/production", Purpose: "Schedule and track manufacturing jobs",
				Components: []string{"GanttChart", "BomTree", "WorkOrderCard"}, PrimaryData: "ProductionOrder"},
			{Name: "Accounting", Route: "/accounting", Purpose: "Invoices and the general ledger",
				Components: []string{"DataTable", "LedgerView", "InvoiceForm"}, PrimaryData: "Invoice"},
		},
		NFRs: append(commonNFRs(),
			"Stock movements are append-only; corrections are compensating entries",
			"Inventory quantities must never go negative without an explicit override",
			"Financial figures use fixed-point decimal arithmetic, never floating point"),
	}
}

func marketplaceBlueprint() Blueprint {
	return Blueprint{
		Key: "marketplace", Version: "1.0.0", Category: domain.CategoryMarketplace,
		Name:        "Two-Sided Marketplace",
		Description: "Connect sellers and buyers with listings, bookings or orders, payments and reviews.",
		Keywords: []string{"marketplace", "airbnb", "uber", "etsy", "ebay", "two-sided", "sellers",
			"buyers", "listings", "booking", "e-commerce", "ecommerce", "online store", "shop",
			"vendors", "payments", "escrow", "peer to peer"},
		Personas: []Persona{
			{Name: "Seller", Role: "seller",
				Goals: []string{"List quickly", "Get paid reliably"},
				Pains: []string{"Opaque fees and slow payouts"}},
			{Name: "Buyer", Role: "buyer",
				Goals: []string{"Find the right listing fast", "Transact safely"},
				Pains: []string{"Fake listings and unclear pricing"}},
			{Name: "Marketplace Operator", Role: "admin",
				Goals: []string{"Grow supply and demand", "Keep fraud low"},
				Pains: []string{"Manual dispute handling"}},
		},
		Epics: []string{"Accounts & onboarding", "Listings & search", "Orders & booking", "Payments & payouts", "Reviews & trust", "Admin & moderation"},
		Roles: []string{"admin", "seller", "buyer", "support"},
		Entities: []Entity{
			entity("User", "users", "An account that can buy, sell or both",
				f("email", "text", true), f("display_name", "text", true), f("avatar_url", "text", false),
				enum("role", "admin", "seller", "buyer", "support"),
				f("password_hash", "text", true), f("is_verified", "bool", true)),
			entity("SellerProfile", "seller_profiles", "Public seller identity and payout details",
				ref("user_id", "User", true), f("business_name", "text", true), f("bio", "text", false),
				f("rating_average", "decimal", false), f("rating_count", "int", false),
				enum("payout_status", "unverified", "pending", "active", "suspended")),
			entity("Category", "categories", "A taxonomy node for listings",
				f("name", "text", true), f("slug", "text", true), ref("parent_id", "Category", false)),
			entity("Listing", "listings", "An item or service offered for sale",
				f("title", "text", true), f("description", "text", true), f("slug", "text", true),
				ref("seller_id", "User", true), ref("category_id", "Category", true),
				f("price", "decimal", true), f("currency", "text", true),
				enum("status", "draft", "active", "paused", "sold", "removed"),
				f("location", "text", false), f("inventory_count", "int", false)),
			entity("ListingImage", "listing_images", "A photo of a listing",
				ref("listing_id", "Listing", true), f("url", "text", true), f("position", "int", true)),
			entity("Order", "orders", "A purchase transaction",
				f("number", "text", true), ref("buyer_id", "User", true), ref("seller_id", "User", true),
				enum("status", "pending", "paid", "fulfilled", "cancelled", "refunded", "disputed"),
				f("subtotal", "decimal", true), f("platform_fee", "decimal", true),
				f("total", "decimal", true), f("currency", "text", true)),
			entity("OrderLine", "order_lines", "A purchased listing within an order",
				ref("order_id", "Order", true), ref("listing_id", "Listing", true),
				f("quantity", "int", true), f("unit_price", "decimal", true)),
			entity("Payment", "payments", "A payment attempt against an order",
				ref("order_id", "Order", true), f("provider", "text", true),
				f("provider_reference", "text", false),
				enum("status", "initiated", "authorized", "captured", "failed", "refunded"),
				f("amount", "decimal", true), f("currency", "text", true)),
			entity("Payout", "payouts", "A transfer of funds to a seller",
				ref("seller_id", "User", true), f("amount", "decimal", true), f("currency", "text", true),
				enum("status", "scheduled", "processing", "paid", "failed"), f("scheduled_for", "timestamp", false)),
			entity("Review", "reviews", "Feedback after a completed order",
				ref("order_id", "Order", true), ref("author_id", "User", true),
				ref("subject_id", "User", true), f("rating", "int", true), f("body", "text", false)),
			entity("Message", "messages", "Direct communication between buyer and seller",
				ref("thread_id", "Thread", true), ref("sender_id", "User", true), f("body", "text", true)),
			entity("Thread", "threads", "A conversation about a listing or order",
				ref("listing_id", "Listing", false), ref("order_id", "Order", false)),
			entity("Dispute", "disputes", "A contested order requiring resolution",
				ref("order_id", "Order", true), ref("opened_by", "User", true), f("reason", "text", true),
				enum("status", "open", "under_review", "resolved", "rejected"), f("resolution", "text", false)),
		},
		Screens: []Screen{
			{Name: "Home / Search", Route: "/", Purpose: "Discover listings with filters and sorting",
				Components: []string{"SearchBar", "FacetFilters", "ListingGrid", "MapView"}, PrimaryData: "Listing"},
			{Name: "Listing Detail", Route: "/listings/:slug", Purpose: "Full listing with seller info and purchase action",
				Components: []string{"ImageGallery", "PriceBox", "SellerCard", "ReviewList"}, PrimaryData: "Listing"},
			{Name: "Checkout", Route: "/checkout", Purpose: "Confirm and pay for an order",
				Components: []string{"OrderSummary", "PaymentForm", "AddressForm"}, PrimaryData: "Order"},
			{Name: "Seller Dashboard", Route: "/sell", Purpose: "Manage listings, orders and payouts",
				Components: []string{"MetricCard", "ListingTable", "PayoutTable"}, PrimaryData: "Listing"},
			{Name: "Orders", Route: "/orders", Purpose: "Buyer order history and status",
				Components: []string{"OrderList", "StatusTimeline"}, PrimaryData: "Order"},
			{Name: "Messages", Route: "/messages", Purpose: "Conversations between parties",
				Components: []string{"ThreadList", "MessagePane", "Composer"}, PrimaryData: "Thread"},
			{Name: "Admin Moderation", Route: "/admin", Purpose: "Review listings, users and disputes",
				Components: []string{"ModerationQueue", "UserTable", "DisputePanel"}, PrimaryData: "Dispute"},
		},
		NFRs: append(commonNFRs(),
			"Payment state transitions are idempotent and driven by provider webhooks",
			"Money is stored as minor units in fixed-point decimals, never floats",
			"Search returns in under 300ms for 100k active listings"),
	}
}

func saasBlueprint() Blueprint {
	return Blueprint{
		Key: "saas", Version: "1.0.0", Category: domain.CategorySaaS,
		Name:        "Multi-Tenant SaaS Application",
		Description: "A general web application with organisations, members, billing and a core resource.",
		Keywords: []string{"saas", "web app", "web application", "platform", "dashboard", "portal",
			"multi-tenant", "subscription", "internal tool", "admin panel"},
		Personas: []Persona{
			{Name: "End User", Role: "member",
				Goals: []string{"Get the core job done with minimal friction"},
				Pains: []string{"Slow, cluttered interfaces"}},
			{Name: "Workspace Admin", Role: "admin",
				Goals: []string{"Control access and billing"},
				Pains: []string{"No visibility into usage"}},
		},
		Epics: []string{"Authentication", "Organisations & members", "Core resource management", "Billing", "Settings & audit"},
		Roles: []string{"owner", "admin", "member", "viewer"},
		Entities: []Entity{
			entity("Organization", "organizations", "A tenant boundary",
				f("name", "text", true), f("slug", "text", true), enum("plan", "free", "pro", "enterprise")),
			entity("User", "users", "An authenticated account",
				f("email", "text", true), f("display_name", "text", true),
				f("password_hash", "text", true), f("avatar_url", "text", false)),
			entity("Membership", "memberships", "A user's role within an organisation",
				ref("organization_id", "Organization", true), ref("user_id", "User", true),
				enum("role", "owner", "admin", "member", "viewer")),
			entity("Resource", "resources", "The core object the product manages",
				ref("organization_id", "Organization", true), f("name", "text", true),
				f("description", "text", false), enum("status", "active", "archived"),
				ref("created_by", "User", true), f("metadata", "json", false)),
			entity("ApiKey", "api_keys", "A programmatic credential",
				ref("organization_id", "Organization", true), f("name", "text", true),
				f("key_hash", "text", true), f("last_used_at", "timestamp", false)),
			entity("AuditLog", "audit_logs", "A record of who changed what",
				ref("organization_id", "Organization", true), ref("actor_id", "User", true),
				f("action", "text", true), f("resource_type", "text", true),
				f("resource_id", "uuid", false), f("metadata", "json", false)),
			entity("Subscription", "subscriptions", "Billing state for an organisation",
				ref("organization_id", "Organization", true), f("provider_reference", "text", false),
				enum("status", "trialing", "active", "past_due", "canceled"),
				f("current_period_end", "timestamp", false)),
		},
		Screens: []Screen{
			{Name: "Dashboard", Route: "/", Purpose: "Overview of the workspace",
				Components: []string{"MetricCard", "RecentList", "EmptyState"}, PrimaryData: "Resource"},
			{Name: "Resources", Route: "/resources", Purpose: "Browse and manage core objects",
				Components: []string{"DataTable", "FilterBar", "CreateDialog"}, PrimaryData: "Resource"},
			{Name: "Resource Detail", Route: "/resources/:id", Purpose: "Inspect and edit one object",
				Components: []string{"DetailHeader", "TabbedPanel", "AuditTrail"}, PrimaryData: "Resource"},
			{Name: "Members", Route: "/settings/members", Purpose: "Invite and manage team access",
				Components: []string{"MemberTable", "InviteDialog", "RoleSelect"}, PrimaryData: "Membership"},
			{Name: "Billing", Route: "/settings/billing", Purpose: "Plan, usage and invoices",
				Components: []string{"PlanCard", "UsageChart", "InvoiceTable"}, PrimaryData: "Subscription"},
			{Name: "Settings", Route: "/settings", Purpose: "Organisation profile and API keys",
				Components: []string{"SettingsForm", "ApiKeyTable"}, PrimaryData: "Organization"},
		},
		NFRs: append(commonNFRs(),
			"Every query is scoped by organisation id; cross-tenant reads are impossible by construction"),
	}
}
