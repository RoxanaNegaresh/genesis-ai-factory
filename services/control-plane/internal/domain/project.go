package domain

import (
	"regexp"
	"strings"
	"time"
	"unicode"
)

// ProjectCategory is the product archetype detected from the brief. It selects
// the blueprint used by the Product Generation Engine (v0.3).
type ProjectCategory string

const (
	CategoryCRM         ProjectCategory = "crm"
	CategoryERP         ProjectCategory = "erp"
	CategoryPM          ProjectCategory = "pm"
	CategoryMarketplace ProjectCategory = "marketplace"
	CategorySaaS        ProjectCategory = "saas"
	CategoryCustom      ProjectCategory = "custom"
)

func (c ProjectCategory) Valid() bool {
	switch c {
	case CategoryCRM, CategoryERP, CategoryPM, CategoryMarketplace, CategorySaaS, CategoryCustom:
		return true
	}
	return false
}

// ProjectStatus is the lifecycle state of a generated product.
type ProjectStatus string

const (
	ProjectDraft    ProjectStatus = "draft"
	ProjectBuilding ProjectStatus = "building"
	ProjectReady    ProjectStatus = "ready"
	ProjectFailed   ProjectStatus = "failed"
	ProjectArchived ProjectStatus = "archived"
)

func (s ProjectStatus) Valid() bool {
	switch s {
	case ProjectDraft, ProjectBuilding, ProjectReady, ProjectFailed, ProjectArchived:
		return true
	}
	return false
}

// AutonomyLevel controls how much the factory may do without human approval.
type AutonomyLevel string

const (
	AutonomySupervised   AutonomyLevel = "supervised"   // approve every write
	AutonomyCheckpointed AutonomyLevel = "checkpointed" // approve at phase boundaries
	AutonomyFull         AutonomyLevel = "autonomous"   // approve only destructive ops
)

func (a AutonomyLevel) Valid() bool {
	switch a {
	case AutonomySupervised, AutonomyCheckpointed, AutonomyFull:
		return true
	}
	return false
}

// Project is the aggregate root for one product being built.
type Project struct {
	ID            ID
	OwnerID       ID
	Name          string
	Slug          string
	Prompt        string
	Description   string
	Category      ProjectCategory
	Status        ProjectStatus
	WorkspacePath string
	Stack         Settings
	Settings      ProjectSettings
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
}

// ProjectSettings holds the operational envelope for runs of this project.
type ProjectSettings struct {
	Autonomy           AutonomyLevel     `json:"autonomy"`
	TokenBudget        int64             `json:"token_budget"`
	MaxParallelAgents  int               `json:"max_parallel_agents"`
	MaxHealAttempts    int               `json:"max_heal_attempts"`
	PreferredLanguages []string          `json:"preferred_languages,omitempty"`
	ModelOverrides     map[string]string `json:"model_overrides,omitempty"`
}

// DefaultProjectSettings returns the settings applied when a client does not
// specify any. They are conservative on purpose: a first-time user should not
// be able to burn an unbounded amount of compute by accident.
func DefaultProjectSettings() ProjectSettings {
	return ProjectSettings{
		Autonomy:          AutonomyCheckpointed,
		TokenBudget:       2_000_000,
		MaxParallelAgents: 4,
		MaxHealAttempts:   5,
	}
}

// Normalize repairs out-of-range values instead of rejecting the request; these
// are tuning knobs, not correctness constraints.
func (s *ProjectSettings) Normalize() {
	if !s.Autonomy.Valid() {
		s.Autonomy = AutonomyCheckpointed
	}
	if s.TokenBudget <= 0 || s.TokenBudget > 100_000_000 {
		s.TokenBudget = 2_000_000
	}
	if s.MaxParallelAgents <= 0 || s.MaxParallelAgents > 32 {
		s.MaxParallelAgents = 4
	}
	if s.MaxHealAttempts < 0 || s.MaxHealAttempts > 20 {
		s.MaxHealAttempts = 5
	}
}

const (
	maxPromptLength = 20000
	maxNameLength   = 120
)

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify produces a filesystem- and URL-safe identifier. It is also the
// directory name of the generated workspace, so it must never contain path
// separators or traversal sequences.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '-'
	}, s)
	s = slugStrip.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	if s == "" {
		s = "project"
	}
	return s
}

// TitleFromPrompt derives a human-readable project name from a free-form brief
// so users are not forced to name things before they start.
func TitleFromPrompt(prompt string) string {
	p := strings.TrimSpace(prompt)
	for _, prefix := range []string{"build me a ", "build me an ", "build a ", "build an ", "build ", "create a ", "create an ", "create ", "make a ", "make an ", "make ", "i want a ", "i want an ", "i need a ", "i need an "} {
		if len(p) >= len(prefix) && strings.EqualFold(p[:len(prefix)], prefix) {
			p = p[len(prefix):]
			break
		}
	}
	if idx := strings.IndexAny(p, ".\n"); idx > 0 {
		p = p[:idx]
	}
	p = strings.TrimSpace(p)
	if p == "" {
		return "Untitled Project"
	}
	words := strings.Fields(p)
	if len(words) > 8 {
		words = words[:8]
	}
	for i, w := range words {
		r := []rune(w)
		if len(r) > 0 && unicode.IsLower(r[0]) {
			r[0] = unicode.ToUpper(r[0])
			words[i] = string(r)
		}
	}
	name := strings.Join(words, " ")
	if len([]rune(name)) > maxNameLength {
		name = string([]rune(name)[:maxNameLength])
	}
	return name
}

// NewProject constructs a validated project aggregate from a natural-language
// brief.
func NewProject(ownerID ID, name, prompt string, settings ProjectSettings, now time.Time) (*Project, error) {
	v := NewValidation()
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		v.Add("prompt", "a product brief is required")
	}
	if len([]rune(prompt)) > maxPromptLength {
		v.Add("prompt", "brief must be at most 20000 characters")
	}
	if ownerID.IsZero() {
		v.Add("owner_id", "owner is required")
	}
	if !v.OK() {
		return nil, v.Err()
	}

	name = strings.TrimSpace(name)
	if name == "" {
		name = TitleFromPrompt(prompt)
	}
	if len([]rune(name)) > maxNameLength {
		return nil, Invalid("name_too_long", "name must be at most 120 characters")
	}

	settings.Normalize()

	return &Project{
		ID:        NewID(),
		OwnerID:   ownerID,
		Name:      name,
		Slug:      Slugify(name),
		Prompt:    prompt,
		Category:  CategoryCustom,
		Status:    ProjectDraft,
		Stack:     Settings{},
		Settings:  settings,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}, nil
}

// Rename updates the display name and slug together so they never drift.
func (p *Project) Rename(name string, now time.Time) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return Invalid("name_required", "name is required")
	}
	if len([]rune(name)) > maxNameLength {
		return Invalid("name_too_long", "name must be at most 120 characters")
	}
	p.Name = name
	p.Slug = Slugify(name)
	p.UpdatedAt = now.UTC()
	return nil
}

// Archived reports whether the project is hidden from the default listing.
func (p *Project) Archived() bool { return p.Status == ProjectArchived }

// Clone returns a deep copy, used when ownership of the aggregate crosses a
// goroutine boundary. See Run.Clone for the rationale.
func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Stack = cloneSettings(p.Stack)
	clone.DeletedAt = cloneTime(p.DeletedAt)

	if p.Settings.PreferredLanguages != nil {
		clone.Settings.PreferredLanguages = append([]string(nil), p.Settings.PreferredLanguages...)
	}
	if p.Settings.ModelOverrides != nil {
		clone.Settings.ModelOverrides = make(map[string]string, len(p.Settings.ModelOverrides))
		for k, v := range p.Settings.ModelOverrides {
			clone.Settings.ModelOverrides[k] = v
		}
	}
	return &clone
}
