// Package arch enforces the Clean Architecture dependency rule as an
// executable test.
//
// Architecture documented in a wiki decays within weeks. Architecture asserted
// by a test that fails the build cannot decay: a violating import does not
// merge. This file is the enforcement mechanism referenced by
// docs/01-ARCHITECTURE.md.
package arch_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/genesis-ai-factory/control-plane"

// layerRules maps a layer to the layers it is permitted to import. Anything not
// listed is forbidden. The rule is one-directional: dependencies point inward.
var layerRules = map[string][]string{
	// The innermost layer must be self-contained: it may import nothing from
	// this module at all. That is what makes the business rules portable and
	// testable without any infrastructure.
	"internal/domain": {},

	// Ports declare the interfaces the inner layers own.
	"internal/port": {"internal/domain"},

	// Application services orchestrate the domain through ports. They must not
	// know about HTTP, SQL or any concrete technology.
	"internal/usecase": {"internal/domain", "internal/port", "internal/infra/crypto"},

	// The factory is application-level orchestration.
	"internal/factory": {"internal/domain", "internal/port", "internal/infra/vcs"},

	// Adapters translate between the outside world and use cases.
	"internal/adapter/http": {"internal/domain", "internal/port", "internal/usecase", "internal/factory", "internal/adapter/ws"},
	"internal/adapter/ws":   {"internal/domain", "internal/port"},

	// Infrastructure implements ports.
	"internal/infra/sqlstore": {"internal/domain", "internal/port"},
	"internal/infra/bus":      {"internal/domain", "internal/port"},
	"internal/infra/crypto":   {"internal/domain"},
	"internal/infra/sandbox":  {"internal/domain", "internal/port"},
	"internal/infra/vcs":      {"internal/domain", "internal/port"},
	"internal/infra/migrate":  {},

	// Configuration is a leaf.
	"internal/config": {},
}

func TestDependencyRule(t *testing.T) {
	root := repoRoot(t)

	for layer, allowed := range layerRules {
		layer, allowed := layer, allowed
		t.Run(layer, func(t *testing.T) {
			dir := filepath.Join(root, layer)
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				t.Skipf("layer %s does not exist yet", layer)
			}

			permitted := map[string]bool{}
			for _, a := range allowed {
				permitted[a] = true
			}

			for _, file := range goFiles(t, dir) {
				// Test files may reach further: a conformance test legitimately
				// wires infrastructure to verify it against the port contract.
				if strings.HasSuffix(file, "_test.go") {
					continue
				}
				for _, imp := range imports(t, file) {
					if !strings.HasPrefix(imp, modulePath+"/") {
						continue // standard library or third party
					}
					target := strings.TrimPrefix(imp, modulePath+"/")
					if target == layer || strings.HasPrefix(target, layer+"/") {
						continue // same layer
					}
					if !permitted[target] {
						rel, _ := filepath.Rel(root, file)
						t.Errorf(
							"illegal dependency: %s imports %q\n"+
								"  layer %q may only import: %v\n"+
								"  dependencies must point inward (adapter/infra → usecase → port → domain)",
							rel, target, layer, allowed)
					}
				}
			}
		})
	}
}

// TestDomainIsPure asserts that the innermost layer imports no infrastructure
// packages even from third parties. A domain that imports a database driver or
// a web framework is not a domain, it is a data-access layer with ambitions.
func TestDomainIsPure(t *testing.T) {
	root := repoRoot(t)
	forbidden := []string{
		"database/sql", "net/http", "github.com/gofiber", "github.com/jackc",
		"modernc.org/sqlite", "github.com/redis", "golang.org/x/crypto",
	}

	for _, file := range goFiles(t, filepath.Join(root, "internal", "domain")) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, imp := range imports(t, file) {
			for _, bad := range forbidden {
				if imp == bad || strings.HasPrefix(imp, bad+"/") {
					rel, _ := filepath.Rel(root, file)
					t.Errorf("domain purity violation: %s imports %q", rel, imp)
				}
			}
		}
	}
}

// TestNoGlobalMutableState catches package-level variables that are not
// constants or errors. Shared mutable globals make tests order-dependent and
// concurrency unsafe, and they hide dependencies from the composition root.
func TestNoGlobalMutableState(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{
		// Sentinel error values and immutable registries are intentional.
		"ErrNotFound": true, "ErrConflict": true, "ErrValidation": true,
		"ErrUnauthorized": true, "ErrForbidden": true, "ErrUnavailable": true,
		"ErrBudget": true, "ErrCanceled": true, "ErrInvalidID": true,
		"ErrInvalidHash": true, "ErrIncompatibleVersion": true,
		"PhaseOrder": true, "agentRoster": true, "registry": true,
		"migrationFS": true, "slugStrip": true, "secretPatterns": true,
		"placeholderHints": true, "layerRules": true,
		// Immutable lookup tables and embedded schema documents. Go has no
		// const map or const []byte, so these must be vars; they are never
		// written after initialisation.
		"stopwords": true, "visionSchema": true, "prdSchema": true,
		"archSchema": true, "classifySchema": true, "logicSchema": true,
		"rulesSchema": true, "blueprintSchema": true, "forbiddenInBody": true,
		"weights": true, "placeholderHintsList": true,
		// Compiled regular expressions: immutable after package init, and
		// compiling them per call would be pure waste.
		"portPattern": true, "noiseLine": true, "skipDirs": true,
		"goCompileError": true, "goTestFailure": true, "numberRun": true,
		"quotedText": true, "pathRun": true, "spaceRun": true, "healSchema": true,
		// Documented, mutex-guarded ID sequence state.
		"idMu": true, "lastMillis": true, "lastSeq": true,
		// Link-time build metadata.
		"version": true, "commit": true, "buildDate": true,
	}

	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		for _, file := range goFilesRecursive(t, base) {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			for _, decl := range parsed.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						if name.Name == "_" || allowed[name.Name] {
							continue
						}
						rel, _ := filepath.Rel(root, file)
						t.Errorf("package-level mutable variable %q in %s: "+
							"inject dependencies instead of using globals", name.Name, rel)
					}
				}
			}
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Walk up to the directory containing go.mod.
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate module root")
	return ""
}

func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func goFilesRecursive(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func imports(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := make([]string, 0, len(parsed.Imports))
	for _, imp := range parsed.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		out = append(out, path)
	}
	return out
}
