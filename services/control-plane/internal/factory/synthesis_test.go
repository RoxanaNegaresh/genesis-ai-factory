package factory_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/factory"
)

// A realistic synthesis response for a domain no built-in blueprint covers.
const vetClinicBlueprint = `{
  "name": "Veterinary Clinic Management",
  "description": "Manage animal patients, their owners, appointments, treatments and invoices for a small veterinary practice.",
  "epics": ["Patient records", "Appointments", "Treatments", "Billing"],
  "roles": ["admin", "veterinarian", "receptionist"],
  "entities": [
    {
      "name": "Owner", "plural": "owners", "description": "A person responsible for one or more animals",
      "fields": [
        {"name": "full_name", "type": "text", "required": true},
        {"name": "email", "type": "text", "required": true},
        {"name": "phone", "type": "text", "required": false}
      ]
    },
    {
      "name": "Animal", "plural": "animals", "description": "An animal patient of the clinic",
      "fields": [
        {"name": "name", "type": "text", "required": true},
        {"name": "species", "type": "enum", "required": true, "enum": ["dog", "cat", "rabbit", "bird"]},
        {"name": "date_of_birth", "type": "timestamp", "required": false},
        {"name": "owner_id", "type": "ref", "required": true, "ref": "Owner"},
        {"name": "weight_kg", "type": "decimal", "required": false}
      ]
    },
    {
      "name": "Appointment", "plural": "appointments", "description": "A scheduled visit",
      "fields": [
        {"name": "scheduled_at", "type": "timestamp", "required": true},
        {"name": "animal_id", "type": "ref", "required": true, "ref": "Animal"},
        {"name": "status", "type": "enum", "required": true, "enum": ["booked", "attended", "cancelled"]},
        {"name": "reason", "type": "text", "required": false}
      ]
    },
    {
      "name": "Treatment", "plural": "treatments", "description": "A procedure or medication administered",
      "fields": [
        {"name": "description", "type": "text", "required": true},
        {"name": "appointment_id", "type": "ref", "required": true, "ref": "Appointment"},
        {"name": "cost", "type": "decimal", "required": true}
      ]
    },
    {
      "name": "User", "plural": "users", "description": "A clinic staff account",
      "fields": [
        {"name": "email", "type": "text", "required": true},
        {"name": "display_name", "type": "text", "required": true},
        {"name": "role", "type": "enum", "required": true, "enum": ["admin", "veterinarian", "receptionist"]},
        {"name": "password_hash", "type": "text", "required": true}
      ]
    }
  ],
  "screens": [
    {"name": "Today", "route": "/", "purpose": "Appointments scheduled for today", "primary_data": "Appointment"},
    {"name": "Animals", "route": "/animals", "purpose": "Search the patient register", "primary_data": "Animal"},
    {"name": "Animal Detail", "route": "/animals/:id", "purpose": "Full clinical history for one animal", "primary_data": "Animal"},
    {"name": "Owners", "route": "/owners", "purpose": "Contact directory of animal owners", "primary_data": "Owner"}
  ]
}`

func synthesize(t *testing.T, response string, prompt string) (factory.Blueprint, bool) {
	t.Helper()
	root := t.TempDir()
	client := &fakeLLM{responses: []string{response}}
	reasoning := factory.NewReasoningForTest(client, nil, 500000)
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleArchitect, nil, nil)
	return factory.SynthesizeBlueprint(context.Background(), tb, reasoning, prompt)
}

func TestSynthesisProducesUsableBlueprint(t *testing.T) {
	blueprint, ok := synthesize(t, vetClinicBlueprint, "Build a management system for a veterinary clinic")
	if !ok {
		t.Fatal("synthesis rejected a well-formed blueprint")
	}

	if blueprint.Name != "Veterinary Clinic Management" {
		t.Fatalf("name not carried through: %q", blueprint.Name)
	}
	if len(blueprint.Entities) != 5 {
		t.Fatalf("expected 5 entities, got %d", len(blueprint.Entities))
	}
	if len(blueprint.Screens) != 4 {
		t.Fatalf("expected 4 screens, got %d", len(blueprint.Screens))
	}

	// The domain must actually be modelled, not genericised.
	names := map[string]bool{}
	for _, e := range blueprint.Entities {
		names[e.Name] = true
	}
	for _, want := range []string{"Owner", "Animal", "Appointment", "Treatment", "User"} {
		if !names[want] {
			t.Errorf("entity %s is missing from the synthesised blueprint", want)
		}
	}

	// Audit fields are the generator's responsibility and must be present.
	for _, e := range blueprint.Entities {
		fields := map[string]bool{}
		for _, f := range e.Fields {
			fields[f.Name] = true
		}
		for _, required := range []string{"id", "created_at", "updated_at"} {
			if !fields[required] {
				t.Errorf("%s is missing %s", e.Name, required)
			}
		}
	}

	if err := factory.ValidateBlueprint(blueprint); err != nil {
		t.Fatalf("synthesised blueprint fails its own validation: %v", err)
	}
}

// Structural mistakes models actually make must be repaired, not fatal.
func TestSynthesisRepairsStructuralMistakes(t *testing.T) {
	messy := `{
      "name": "Fleet Maintenance",
      "description": "Track vehicles, service schedules and mechanics for a haulage company fleet.",
      "epics": ["Vehicles", "Servicing", "Reporting"],
      "roles": ["admin", "mechanic"],
      "entities": [
        {
          "name": "vehicle fleet", "plural": "vehicle fleet", "description": "A truck in the fleet",
          "fields": [
            {"name": "Registration Number", "type": "text", "required": true},
            {"name": "id", "type": "text", "required": true},
            {"name": "created_at", "type": "timestamp", "required": true},
            {"name": "depot_id", "type": "ref", "required": true, "ref": "DoesNotExist"},
            {"name": "fuel", "type": "enum", "required": true, "enum": ["diesel"]}
          ]
        },
        {
          "name": "Service", "plural": "services", "description": "A maintenance event on a vehicle",
          "fields": [
            {"name": "performed_at", "type": "timestamp", "required": true},
            {"name": "cost", "type": "decimal", "required": true}
          ]
        },
        {
          "name": "Mechanic", "plural": "mechanics", "description": "A person who services vehicles",
          "fields": [{"name": "full_name", "type": "text", "required": true}]
        },
        {
          "name": "Depot", "plural": "depots", "description": "A site where vehicles are based",
          "fields": [{"name": "site_name", "type": "text", "required": true}]
        }
      ],
      "screens": [
        {"name": "Fleet", "route": "vehicles", "purpose": "All vehicles and their status", "primary_data": "vehicle fleet"},
        {"name": "Ghost", "route": "/ghost", "purpose": "References a missing entity", "primary_data": "Nowhere"},
        {"name": "Services", "route": "/services", "purpose": "Maintenance history", "primary_data": "Service"}
      ]
    }`

	blueprint, ok := synthesize(t, messy, "Build a fleet maintenance system")
	if !ok {
		t.Fatal("synthesis rejected a blueprint that should have been repaired")
	}

	byName := map[string]factory.Entity{}
	for _, e := range blueprint.Entities {
		byName[e.Name] = e
	}

	// "vehicle fleet" must become a valid Go identifier.
	vehicle, found := byName["VehicleFleet"]
	if !found {
		t.Fatalf("entity name was not sanitised into an identifier; got %v", keysOf(byName))
	}
	// A plural identical to the singular would collide with nothing useful.
	if vehicle.Plural == "" || vehicle.Plural == "vehicle fleet" {
		t.Fatalf("plural was not normalised: %q", vehicle.Plural)
	}

	// Duplicated audit fields must not appear twice in the struct.
	counts := map[string]int{}
	for _, f := range vehicle.Fields {
		counts[f.Name]++
	}
	for _, audit := range []string{"id", "created_at", "updated_at"} {
		if counts[audit] != 1 {
			t.Errorf("%s appears %d times; it must appear exactly once", audit, counts[audit])
		}
	}
	if counts["registration_number"] != 1 {
		t.Errorf("field name was not snake-cased: %v", counts)
	}

	// A dangling reference must degrade to text, never emit a broken FK.
	for _, f := range vehicle.Fields {
		if f.Name == "depot_id" {
			if f.Type == "ref" {
				t.Error("a reference to a nonexistent entity survived normalisation")
			}
		}
		// A single-value enum is not an enum.
		if f.Name == "fuel" && f.Type == "enum" {
			t.Error("a one-value enum should have degraded to text")
		}
	}

	// A route without a leading slash is unreachable.
	for _, s := range blueprint.Screens {
		if !strings.HasPrefix(s.Route, "/") {
			t.Errorf("route %q was not normalised", s.Route)
		}
		if s.Name == "Ghost" && s.PrimaryData != "" {
			t.Error("a screen referencing a missing entity kept its dangling reference")
		}
	}

	// A User entity is mandatory for the generated auth layer.
	if _, hasUser := byName["User"]; !hasUser {
		t.Error("the missing User entity was not added")
	}

	if err := factory.ValidateBlueprint(blueprint); err != nil {
		t.Fatalf("repaired blueprint still fails validation: %v", err)
	}
}

func TestSynthesisRejectsUnusableOutput(t *testing.T) {
	// Too few entities to build anything: the generic template is better.
	thin := `{
      "name": "Thing Tracker",
      "description": "A system that tracks things of various kinds for people who have them.",
      "epics": ["One", "Two", "Three"],
      "roles": ["admin", "user"],
      "entities": [
        {"name": "Thing", "plural": "things", "description": "A thing that is tracked",
         "fields": [{"name": "label", "type": "text", "required": true}]},
        {"name": "User", "plural": "users", "description": "An account",
         "fields": [{"name": "email", "type": "text", "required": true}]}
      ],
      "screens": [
        {"name": "Things", "route": "/things", "purpose": "List every thing", "primary_data": "Thing"},
        {"name": "Detail", "route": "/things/:id", "purpose": "One thing", "primary_data": "Thing"}
      ]
    }`

	if _, ok := synthesize(t, thin, "Build a thing tracker"); ok {
		t.Fatal("a blueprint with too few entities was accepted")
	}
}

func TestSynthesisIsUnavailableWithoutAModel(t *testing.T) {
	root := t.TempDir()
	tb := factory.NewWorkspaceToolbelt(root, domain.RoleArchitect, nil, nil)
	if _, ok := factory.SynthesizeBlueprint(context.Background(), tb, nil, "anything"); ok {
		t.Fatal("synthesis must not claim success without a model")
	}
}

func TestValidateBlueprintCatchesStructuralFaults(t *testing.T) {
	base := factory.BlueprintFor(domain.CategoryCRM)
	if err := factory.ValidateBlueprint(base); err != nil {
		t.Fatalf("a built-in blueprint must validate: %v", err)
	}

	// Every built-in blueprint must satisfy the same gate synthesis must pass,
	// or the gate is testing something the pipeline does not actually require.
	for _, bp := range factory.Blueprints() {
		if err := factory.ValidateBlueprint(bp); err != nil {
			t.Errorf("built-in blueprint %s fails validation: %v", bp.Key, err)
		}
	}

	broken := base
	broken.Entities = append([]factory.Entity{}, base.Entities...)
	broken.Entities[0].Fields = append([]factory.Field{}, base.Entities[0].Fields...)
	broken.Entities[0].Fields[0].Name = ""
	if err := factory.ValidateBlueprint(broken); err == nil {
		t.Error("a field with no name was accepted")
	}
}

// The end-to-end guarantee: a synthesised blueprint must generate a project
// that compiles, exactly like a built-in one.
func TestSynthesizedBlueprintGeneratesCompilingProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping toolchain test in short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}

	blueprint, ok := synthesize(t, vetClinicBlueprint, "Build a veterinary clinic system")
	if !ok {
		t.Fatal("synthesis failed")
	}

	root := t.TempDir()
	project := &domain.Project{
		ID: domain.NewID(), OwnerID: domain.NewID(),
		Name: "Vet Clinic", Slug: "vet-clinic",
		Prompt: "Build a veterinary clinic system", WorkspacePath: root,
		Settings: domain.DefaultProjectSettings(),
	}
	run, _ := domain.NewRun(project.ID, project.OwnerID, domain.RunBuild, nil, 0, time.Now())

	bb := factory.NewBlackboard(project, run)
	bb.Blueprint = blueprint
	bb.Classification = factory.Classification{Category: domain.CategoryCustom}
	bb.Put(domain.NewArtifact(project.ID, run.ID, nil, domain.ArtifactDBSchema,
		"DATA_MODEL.md", "text/markdown", "# Schema", time.Now()))

	tb := factory.NewWorkspaceToolbelt(root, domain.RoleBackend, nil, nil)
	backend, _ := factory.NewRegistry().Get(domain.RoleBackend)
	if _, err := backend.Execute(context.Background(), bb, tb); err != nil {
		t.Fatalf("backend agent failed on a synthesised blueprint: %v", err)
	}

	apiDir := filepath.Join(root, "api")
	run2 := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, goBin, args...)
		cmd.Dir = apiDir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run2("mod", "tidy"); err != nil {
		t.Skipf("cannot resolve dependencies (offline?): %v\n%s", err, out)
	}
	if out, err := run2("build", "./..."); err != nil {
		t.Fatalf("synthesised blueprint produced code that does not compile:\n%s", out)
	}
	if out, err := run2("test", "./internal/domain/"); err != nil {
		t.Fatalf("generated tests fail for a synthesised blueprint:\n%s", out)
	}

	// The generated code must reflect the actual domain.
	animal, err := os.ReadFile(filepath.Join(apiDir, "internal", "domain", "animal.go"))
	if err != nil {
		t.Fatalf("the Animal entity was not generated: %v", err)
	}
	if !strings.Contains(string(animal), "type Animal struct") {
		t.Fatal("Animal struct missing from generated code")
	}
	if !strings.Contains(string(animal), "AnimalSpecies") {
		t.Fatal("the species enum was not generated")
	}
}

func keysOf(m map[string]factory.Entity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Regression from a real model run: several entities were returned with the
// literal plural "name", which collapsed them onto one table and caused the
// whole synthesised blueprint to be discarded.
func TestSynthesisResolvesTableNameCollisions(t *testing.T) {
	colliding := `{
      "name": "Veterinary Clinic",
      "description": "Track animals, their owners and clinic appointments for a small practice.",
      "epics": ["Patients", "Appointments", "Billing"],
      "roles": ["admin", "vet"],
      "entities": [
        {"name": "Animal", "plural": "name", "description": "An animal patient of the clinic",
         "fields": [{"name": "call_name", "type": "text", "required": true}]},
        {"name": "Owner", "plural": "name", "description": "The person responsible for an animal",
         "fields": [{"name": "full_name", "type": "text", "required": true}]},
        {"name": "Appointment", "plural": "name", "description": "A scheduled clinic visit",
         "fields": [{"name": "scheduled_at", "type": "timestamp", "required": true}]},
        {"name": "User", "plural": "users", "description": "A staff account",
         "fields": [{"name": "email", "type": "text", "required": true}]}
      ],
      "screens": [
        {"name": "Animals", "route": "/animals", "purpose": "The patient register", "primary_data": "Animal"},
        {"name": "Owners", "route": "/owners", "purpose": "Owner contact details", "primary_data": "Owner"},
        {"name": "Diary", "route": "/diary", "purpose": "Upcoming appointments", "primary_data": "Appointment"}
      ]
    }`

	blueprint, ok := synthesize(t, colliding, "Build a veterinary clinic system")
	if !ok {
		t.Fatal("colliding table names should be repaired, not rejected")
	}

	tables := map[string]string{}
	for _, e := range blueprint.Entities {
		if previous, clash := tables[e.Plural]; clash {
			t.Fatalf("%s and %s still share the table name %q", previous, e.Name, e.Plural)
		}
		tables[e.Plural] = e.Name
	}

	// The repair must derive sensible names, not arbitrary ones.
	byName := map[string]factory.Entity{}
	for _, e := range blueprint.Entities {
		byName[e.Name] = e
	}
	if animal := byName["Animal"]; !strings.HasPrefix(animal.Plural, "animal") {
		t.Errorf("Animal got an unrelated table name %q", animal.Plural)
	}

	if err := factory.ValidateBlueprint(blueprint); err != nil {
		t.Fatalf("repaired blueprint fails validation: %v", err)
	}
}
