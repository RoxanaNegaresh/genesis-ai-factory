package domain

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewIDIsSortableAndUnique(t *testing.T) {
	const n = 5000
	seen := make(map[ID]struct{}, n)
	ids := make([]ID, 0, n)
	for i := 0; i < n; i++ {
		id := NewID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated: %s", id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		t.Fatal("ids are not lexicographically time-sortable")
	}
}

func TestIDVersionAndVariant(t *testing.T) {
	id := NewID()
	if id[14] != '7' {
		t.Fatalf("expected UUID version 7, got %q in %s", id[14], id)
	}
	switch id[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("expected RFC4122 variant, got %q in %s", id[19], id)
	}
}

func TestIDTimeRoundTrip(t *testing.T) {
	before := time.Now().UTC().Add(-2 * time.Second)
	id := NewID()
	got, ok := id.Time()
	if !ok {
		t.Fatal("could not extract time from id")
	}
	if got.Before(before) || got.After(time.Now().UTC().Add(2*time.Second)) {
		t.Fatalf("embedded timestamp %s out of range", got)
	}
}

func TestParseID(t *testing.T) {
	valid := NewID()
	if _, err := ParseID(valid.String()); err != nil {
		t.Fatalf("valid id rejected: %v", err)
	}
	if _, err := ParseID(strings.ToUpper(valid.String())); err != nil {
		t.Fatalf("uppercase id should be normalised: %v", err)
	}
	for _, bad := range []string{"", "abc", "../../etc/passwd", strings.Repeat("g", 36),
		"0189d1a0-1234-7abc-8def-00000000000"} {
		if _, err := ParseID(bad); err == nil {
			t.Fatalf("expected rejection of %q", bad)
		}
	}
}

func TestSlugifyIsPathSafe(t *testing.T) {
	cases := map[string]string{
		"Build a Jira Competitor": "build-a-jira-competitor",
		"../../etc/passwd":        "etc-passwd",
		"  CRM  System!!  ":       "crm-system",
		"???":                     "project",
		"a/b\\c":                  "a-b-c",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"../../x", "a/b", "..", "./."} {
		got := Slugify(in)
		if strings.Contains(got, "/") || strings.Contains(got, "\\") || strings.Contains(got, "..") {
			t.Errorf("Slugify(%q) produced unsafe path component %q", in, got)
		}
	}
}

func TestTitleFromPrompt(t *testing.T) {
	cases := map[string]string{
		"Build a Jira competitor":                      "Jira Competitor",
		"create an ERP system for manufacturing":       "ERP System For Manufacturing",
		"Build an Airbnb-like marketplace. With chat.": "Airbnb-like Marketplace",
		"": "Untitled Project",
	}
	for in, want := range cases {
		if got := TitleFromPrompt(in); got != want {
			t.Errorf("TitleFromPrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewProjectValidation(t *testing.T) {
	now := time.Now()
	owner := NewID()

	if _, err := NewProject(owner, "", "   ", DefaultProjectSettings(), now); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected validation error for empty prompt, got %v", err)
	}
	if _, err := NewProject(Nil, "x", "build a crm", DefaultProjectSettings(), now); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected validation error for missing owner, got %v", err)
	}
	p, err := NewProject(owner, "", "Build a CRM system", DefaultProjectSettings(), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Name != "CRM System" || p.Slug != "crm-system" {
		t.Fatalf("derived name/slug wrong: %q / %q", p.Name, p.Slug)
	}
	if p.Status != ProjectDraft || p.Category != CategoryCustom {
		t.Fatalf("unexpected initial state: %v %v", p.Status, p.Category)
	}
}

func TestProjectSettingsNormalize(t *testing.T) {
	s := ProjectSettings{Autonomy: "nonsense", TokenBudget: -5, MaxParallelAgents: 999, MaxHealAttempts: -1}
	s.Normalize()
	if s.Autonomy != AutonomyCheckpointed || s.TokenBudget != 2_000_000 || s.MaxParallelAgents != 4 || s.MaxHealAttempts != 5 {
		t.Fatalf("normalize did not clamp values: %+v", s)
	}
}

func TestRunLifecycle(t *testing.T) {
	now := time.Now()
	r, err := NewRun(NewID(), NewID(), RunBuild, Settings{"prompt": "x"}, 1000, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != RunPending {
		t.Fatalf("expected pending, got %s", r.Status)
	}
	if err := r.Start(now); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if err := r.Start(now); err == nil {
		t.Fatal("double start should conflict")
	}
	if err := r.Succeed(Settings{"files": 12}, now.Add(time.Minute)); err != nil {
		t.Fatalf("succeed failed: %v", err)
	}
	if !r.Status.Terminal() {
		t.Fatal("succeeded run should be terminal")
	}
	if err := r.Fail(RunError{Code: "x"}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict after terminal, got %v", err)
	}
	if d := r.Duration(now.Add(time.Minute)); d != time.Minute {
		t.Fatalf("expected 1m duration, got %s", d)
	}
}

func TestRunCancelIsIdempotent(t *testing.T) {
	now := time.Now()
	r, _ := NewRun(NewID(), NewID(), RunBuild, nil, 0, now)
	_ = r.Start(now)
	if err := r.RequestCancel(now); err != nil {
		t.Fatalf("cancel request failed: %v", err)
	}
	if err := r.RequestCancel(now); err != nil {
		t.Fatalf("second cancel request should be a no-op, got %v", err)
	}
	if !r.CancelRequested() {
		t.Fatal("cancel not recorded")
	}
	if err := r.Cancel(now); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if r.Status != RunCanceled {
		t.Fatalf("expected canceled, got %s", r.Status)
	}
}

func TestNewRunPhasesCoversLoop(t *testing.T) {
	ph := NewRunPhases(NewID(), time.Now())
	if len(ph) != len(PhaseOrder) {
		t.Fatalf("expected %d phases, got %d", len(PhaseOrder), len(ph))
	}
	for i, p := range ph {
		if p.Ordinal != i || p.Name != PhaseOrder[i] || p.Status != PhasePending {
			t.Fatalf("phase %d malformed: %+v", i, p)
		}
	}
}

func TestTaskDependencyGate(t *testing.T) {
	now := time.Now()
	a := NewTask(NewID(), NewID(), RoleCEO, "vision", "", 10, now)
	b := NewTask(a.RunID, a.PhaseID, RolePM, "prd", "", 9, now).DependsOnTasks(a.ID)

	state := map[ID]TaskStatus{a.ID: TaskRunning}
	if b.CanRun(state) {
		t.Fatal("task ran before its dependency succeeded")
	}
	state[a.ID] = TaskSucceeded
	if !b.CanRun(state) {
		t.Fatal("task should be runnable once dependency succeeded")
	}
}

func TestTaskRetryThenFail(t *testing.T) {
	now := time.Now()
	task := NewTask(NewID(), NewID(), RoleBackend, "api", "", 5, now)
	task.MaxAttempts = 2

	if err := task.Claim(now); err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	task.Fail("compile error", now)
	if task.Status != TaskPending {
		t.Fatalf("expected retryable pending, got %s", task.Status)
	}
	if err := task.Claim(now); err != nil {
		t.Fatalf("second claim failed: %v", err)
	}
	task.Fail("compile error", now)
	if task.Status != TaskFailed {
		t.Fatalf("expected terminal failure, got %s", task.Status)
	}
	if err := task.Claim(now); err == nil {
		t.Fatal("claiming an exhausted task must fail")
	}
}

func TestAgentRosterIntegrity(t *testing.T) {
	roster := AgentRoster()
	if len(roster) != 11 {
		t.Fatalf("expected 11 agents, got %d", len(roster))
	}
	seen := map[AgentRole]bool{}
	for _, a := range roster {
		if seen[a.Role] {
			t.Fatalf("duplicate role %s", a.Role)
		}
		seen[a.Role] = true
		if a.Name == "" || a.Mission == "" || a.ModelClass == "" {
			t.Fatalf("incomplete profile for %s", a.Role)
		}
		if !a.Phase.Valid() {
			t.Fatalf("agent %s has invalid phase %s", a.Role, a.Phase)
		}
		if len(a.Produces) == 0 {
			t.Fatalf("agent %s produces nothing", a.Role)
		}
	}
	// Every phase that has agents must be reachable in the canonical order.
	for _, p := range []Phase{PhaseAnalyze, PhaseDesign, PhaseBuild, PhaseVerify, PhaseShip} {
		if len(AgentsForPhase(p)) == 0 {
			t.Fatalf("phase %s has no agents", p)
		}
	}
}

func TestValidTopic(t *testing.T) {
	id := NewID()
	if !ValidTopic(RunTopic(id)) || !ValidTopic(ProjectTopic(id)) || !ValidTopic(SystemTopic) {
		t.Fatal("valid topics rejected")
	}
	for _, bad := range []string{"", "run:", "run:not-a-uuid", "evil:*", "project:../x"} {
		if ValidTopic(bad) {
			t.Fatalf("topic %q should be rejected", bad)
		}
	}
}

func TestArtifactHashingAndStorageSelection(t *testing.T) {
	now := time.Now()
	a := NewArtifact(NewID(), NewID(), nil, ArtifactPRD, "prd.md", "text/markdown", "hello", now)
	b := NewArtifact(a.ProjectID, a.RunID, nil, ArtifactPRD, "prd.md", "text/markdown", "hello", now)
	if a.SHA256 != b.SHA256 {
		t.Fatal("identical bodies must hash identically for dedup")
	}
	if a.Storage != StorageInline {
		t.Fatalf("small artifact should be inline, got %s", a.Storage)
	}
	big := NewArtifact(a.ProjectID, a.RunID, nil, ArtifactCodeBackend, "big.go", "text/plain",
		strings.Repeat("x", InlineLimit+1), now)
	if big.Storage != StorageFile {
		t.Fatalf("large artifact should spill to file storage, got %s", big.Storage)
	}
}

func TestValidationErrorAggregates(t *testing.T) {
	v := NewValidation()
	if v.Err() != nil {
		t.Fatal("empty validation should produce no error")
	}
	v.Add("email", "required").Add("password", "too short")
	err := v.Err()
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected validation sentinel, got %v", err)
	}
	var de *Error
	if !errors.As(err, &de) || len(de.Fields) != 2 {
		t.Fatalf("expected two field errors, got %+v", err)
	}
}

func TestRoleHierarchy(t *testing.T) {
	if !RoleOwner.AtLeast(RoleAdmin) || !RoleAdmin.AtLeast(RoleMember) || !RoleMember.AtLeast(RoleViewer) {
		t.Fatal("role ranking broken")
	}
	if RoleViewer.AtLeast(RoleMember) {
		t.Fatal("viewer must not outrank member")
	}
}

func TestPasswordAndEmailPolicy(t *testing.T) {
	if err := ValidatePassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := ValidatePassword("correct-horse-battery"); err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
	if err := ValidateEmail("not-an-email"); err == nil {
		t.Fatal("invalid email accepted")
	}
	if err := ValidateEmail("dev@genesis.local"); err != nil {
		t.Fatalf("valid email rejected: %v", err)
	}
	if NormalizeEmail("  DeV@Genesis.Local ") != "dev@genesis.local" {
		t.Fatal("email normalisation failed")
	}
}
