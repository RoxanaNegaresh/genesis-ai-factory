package factory_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The persistence layer is the difference between a project that compiles and
// a product that stores data. These tests assert on the generated source
// rather than on the generator's intent: the only evidence that counts is what
// lands on disk.

func TestGeneratedProjectHasAPersistenceLayer(t *testing.T) {
	workspace := generateProject(t, "Build a CRM for a sales team")
	dir := filepath.Join(workspace, "api", "internal", "infra", "postgres")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("no persistence package was generated: %v", err)
	}

	var repos int
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "_repository.go") {
			repos++
		}
	}
	if repos < 3 {
		t.Fatalf("only %d repository implementations were generated", repos)
	}

	// Every port must have a matching implementation. A port without one is
	// exactly the gap this layer exists to close, and counting both sides is
	// the only way to notice when a new entity type slips through.
	portDir := filepath.Join(workspace, "api", "internal", "port")
	portEntries, err := os.ReadDir(portDir)
	if err != nil {
		t.Fatalf("read ports: %v", err)
	}
	var ports int
	for _, entry := range portEntries {
		if strings.HasSuffix(entry.Name(), "_repository.go") {
			ports++
		}
	}
	// auth_repository.go implements two ports (user and session) declared in
	// port/auth.go rather than in a *_repository.go file, so it is counted
	// separately instead of being expected to pair up one-to-one.
	if ports != repos-1 {
		t.Errorf("%d repository ports but %d implementations", ports, repos)
	}
}

// Generated SQL must never interpolate a caller-supplied value into the
// statement text. This walks the real files and fails on the shape of the
// mistake rather than trusting that it was avoided.
func TestGeneratedRepositoriesParameteriseEverySQLValue(t *testing.T) {
	workspace := generateProject(t, "Build a CRM for a sales team")
	dir := filepath.Join(workspace, "api", "internal", "infra", "postgres")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read persistence package: %v", err)
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_repository.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		text := string(source)

		// The only formatting into a statement is placeholder numbering,
		// which renders an int. A %s or %v would mean a value was pasted in.
		for _, forbidden := range []string{
			`fmt.Sprintf("SELECT`, `fmt.Sprintf("INSERT`,
			`fmt.Sprintf("UPDATE`, `fmt.Sprintf("DELETE`,
		} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s builds SQL with %s", entry.Name(), forbidden)
			}
		}

		// A query built by concatenating a variable is the same defect in a
		// different shape.
		if strings.Contains(text, `" + f.Query`) || strings.Contains(text, `"+f.Query`) {
			t.Errorf("%s concatenates the search term into SQL", entry.Name())
		}

		// Reads must exclude soft-deleted rows, or archiving does nothing.
		if !strings.Contains(text, "deleted_at IS NULL") {
			t.Errorf("%s does not filter soft-deleted rows", entry.Name())
		}

		// Pagination must be keyset, not OFFSET.
		if strings.Contains(text, "OFFSET") {
			t.Errorf("%s paginates with OFFSET instead of a keyset", entry.Name())
		}
	}
}

// The generated persistence package must parse. A template that emits invalid
// Go is a defect the compiler would catch only after `go mod tidy` succeeds,
// which is slow and happens far from the change that caused it.
func TestGeneratedPersistenceParses(t *testing.T) {
	workspace := generateProject(t, "Build a marketplace for handmade goods")
	dir := filepath.Join(workspace, "api", "internal", "infra", "postgres")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read persistence package: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the persistence package is empty")
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if _, err := parser.ParseFile(fset, path, nil, parser.AllErrors); err != nil {
			t.Errorf("%s does not parse: %v", entry.Name(), err)
		}
	}
}

// registerRoutes must construct and mount every handler. An unmounted handler
// is unreachable, which from the caller's side is indistinguishable from a
// handler that was never written.
func TestGeneratedRouterMountsEveryHandler(t *testing.T) {
	workspace := generateProject(t, "Build a CRM for a sales team")

	mainPath := filepath.Join(workspace, "api", "cmd", "server", "main.go")
	source, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)

	start := strings.Index(text, "func registerRoutes(")
	if start < 0 {
		t.Fatal("registerRoutes was not generated")
	}
	body := text[start:]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}

	handlerDir := filepath.Join(workspace, "api", "internal", "adapter", "http")
	entries, err := os.ReadDir(handlerDir)
	if err != nil {
		t.Fatalf("read handlers: %v", err)
	}

	var handlers int
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, "_handler.go") {
			continue
		}
		handlers++
	}
	if handlers == 0 {
		t.Fatal("no handlers were generated")
	}

	// Resource handlers mount on the guarded group; the auth handler mounts
	// publicly on the root group because a caller cannot present a token
	// before obtaining one. Both spellings count as mounted.
	mounted := strings.Count(body, ".Register(guarded)") + strings.Count(body, ".Register(r)")
	if mounted != handlers {
		t.Errorf("%d handlers were generated but %d are mounted:\n%s",
			handlers, mounted, body)
	}

	// Every resource route must sit behind authentication. A resource handler
	// mounted on the root group would be public, which is the failure this
	// whole layer exists to prevent.
	if strings.Contains(body, "RequireAuth") {
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, ".Register(r)") {
				continue
			}
			if !strings.Contains(line, "NewAuthHandler") {
				t.Errorf("a resource handler is mounted outside the guarded group: %s",
					strings.TrimSpace(line))
			}
		}
	}

	// Mounting a handler is pointless if it has no repository behind it.
	if !strings.Contains(body, "postgres.New") {
		t.Error("handlers are mounted without a repository implementation")
	}
}

// Multi-word resources are where naming conventions break. SellerProfile must
// become the table "seller_profiles", the route "/seller-profiles" and the
// error-code stem "seller_profile" — three different spellings, each correct
// for its own consumer. Getting one of them from the wrong helper is silent:
// the code compiles and only fails when a client calls the URL it was told to.
func TestMultiWordResourcesUseTheRightSpellingEverywhere(t *testing.T) {
	workspace := generateProject(t, "Build a marketplace for handmade goods")

	handler := filepath.Join(workspace, "api", "internal", "adapter", "http",
		"seller_profile_handler.go")
	source, err := os.ReadFile(handler)
	if err != nil {
		t.Skipf("this blueprint has no SellerProfile entity: %v", err)
	}
	if !strings.Contains(string(source), `r.Group("/seller-profiles")`) {
		t.Errorf("the route is not kebab-case; a snake_case URL is not conventional REST")
	}

	repo := filepath.Join(workspace, "api", "internal", "infra", "postgres",
		"seller_profile_repository.go")
	repoSource, err := os.ReadFile(repo)
	if err != nil {
		t.Fatalf("read repository: %v", err)
	}
	repoText := string(repoSource)

	// The table keeps underscores: PostgreSQL identifiers are not hyphenated.
	if !strings.Contains(repoText, "seller_profiles") {
		t.Error("the repository does not address the seller_profiles table")
	}
	if strings.Contains(repoText, "seller-profiles") {
		t.Error("the repository uses a hyphenated SQL identifier")
	}

	// An error code with a space in it cannot be matched on by a client.
	for _, line := range strings.Split(repoText, "\n") {
		if !strings.Contains(line, "domain.NotFound(") &&
			!strings.Contains(line, "wrapReadError(") &&
			!strings.Contains(line, "wrapWriteError(") {
			continue
		}
		if strings.Contains(line, `"seller profile"`) {
			t.Errorf("an error code contains a space: %s", strings.TrimSpace(line))
		}
	}
}

// The OpenAPI document is the contract clients generate from. If it disagrees
// with the router, every generated client is wrong in a way that only shows up
// at runtime.
func TestOpenAPIPathsMatchTheRouter(t *testing.T) {
	workspace := generateProject(t, "Build a marketplace for handmade goods")

	spec, err := os.ReadFile(filepath.Join(workspace, "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	specText := string(spec)

	handlerDir := filepath.Join(workspace, "api", "internal", "adapter", "http")
	entries, err := os.ReadDir(handlerDir)
	if err != nil {
		t.Fatalf("read handlers: %v", err)
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_handler.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(handlerDir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		// Extract the literal the handler actually mounts.
		text := string(source)
		marker := `r.Group("/`
		start := strings.Index(text, marker)
		if start < 0 {
			continue
		}
		rest := text[start+len(marker):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			continue
		}
		route := rest[:end]

		// The auth handler mounts a /auth prefix and documents its concrete
		// endpoints (/auth/login, /auth/register, ...) rather than the bare
		// group, which is not a real path.
		if route == "auth" {
			for _, endpoint := range []string{"/auth/login", "/auth/register", "/auth/refresh", "/auth/logout"} {
				if !strings.Contains(specText, "  "+endpoint+":") {
					t.Errorf("the router mounts %s but openapi.yaml does not document it", endpoint)
				}
			}
			continue
		}
		if !strings.Contains(specText, "  /"+route+":") {
			t.Errorf("the router mounts /%s but openapi.yaml does not document it", route)
		}
	}
}
