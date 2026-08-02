package factory

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
)

// The code factory.
//
// v0.1 generated structure from templates. v0.2 added model reasoning to the
// design documents. v0.3 closes the loop: models now author the *bodies* of
// business-logic functions, inside a skeleton the templates guarantee.
//
// The division of labour is deliberate and is the central bet of this design:
//
//	structure  → templates   (must be identical across files, or the repository
//	                          becomes unnavigable and unmaintainable)
//	semantics  → models      (must be specific to this product, which a template
//	                          cannot know)
//
// The mechanism that makes this safe is that generated bodies are never trusted
// as text. Every body is parsed as Go before it is written, checked against a
// small set of rules, and discarded in favour of a compiling default if it
// fails. A model cannot break the build here, only decline to improve it.

// FunctionSpec describes a body the model is asked to write.
type FunctionSpec struct {
	// Name is the method or function identifier.
	Name string
	// Receiver is the method receiver declaration, empty for a free function.
	Receiver string
	// Signature is everything after the name: parameters and results.
	Signature string
	// Doc is the comment placed above the function.
	Doc string
	// Purpose is the natural-language instruction given to the model.
	Purpose string
	// Fallback is a compiling implementation used when generation fails or is
	// rejected. It is never empty: the build must always succeed.
	Fallback string
	// Imports the body is permitted to rely on, already present in the file.
	Imports []string
}

// Render produces the complete function declaration for a body.
func (f FunctionSpec) Render(body string) string {
	var sb strings.Builder
	if f.Doc != "" {
		for _, line := range strings.Split(f.Doc, "\n") {
			sb.WriteString("// ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("func ")
	if f.Receiver != "" {
		sb.WriteString("(")
		sb.WriteString(f.Receiver)
		sb.WriteString(") ")
	}
	sb.WriteString(f.Name)
	sb.WriteString(f.Signature)
	sb.WriteString(" {\n")

	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			sb.WriteString("\n")
			continue
		}
		sb.WriteString("\t")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("}\n")
	return sb.String()
}

// BodyRejection explains why a generated body was not used.
type BodyRejection struct {
	Function string
	Reason   string
}

func (r BodyRejection) Error() string {
	return fmt.Sprintf("%s: %s", r.Function, r.Reason)
}

// maxBodyLines bounds a generated function. A model that emits four hundred
// lines for "validate this struct" has misunderstood the task, and reviewing
// that by hand costs more than writing it.
const maxBodyLines = 120

// forbiddenInBody are constructs a generated business-logic body must not use.
//
// This is not a security sandbox — that is the executor's job in v0.4. It is a
// correctness and reviewability guard: these constructs indicate the model has
// wandered outside the function it was asked to write.
var forbiddenInBody = []struct {
	token  string
	reason string
}{
	{"package ", "a body must not declare a package"},
	{"import ", "a body must not add imports; use only what the file already imports"},
	{"func main(", "a body must not define an entry point"},
	{"os.Exit(", "a body must not terminate the process"},
	{"panic(", "a body must return an error rather than panicking"},
	{"go func(", "a body must not spawn goroutines"},
	{"exec.Command", "a body must not execute commands"},
	{"unsafe.", "a body must not use the unsafe package"},
}

// ValidateBody parses a generated function body and rejects it if it is
// malformed, oversized, empty, or uses forbidden constructs.
//
// Parsing is the point. A body that does not compile would break the entire
// generated project, and "the AI wrote code that does not build" is precisely
// the failure that makes such tools untrustworthy. Rejecting into a working
// fallback converts a hard failure into a soft one.
func ValidateBody(spec FunctionSpec, body string) error {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return BodyRejection{spec.Name, "the body is empty"}
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) > maxBodyLines {
		return BodyRejection{spec.Name,
			fmt.Sprintf("the body is %d lines, over the %d line limit", len(lines), maxBodyLines)}
	}

	for _, forbidden := range forbiddenInBody {
		if strings.Contains(trimmed, forbidden.token) {
			return BodyRejection{spec.Name, forbidden.reason}
		}
	}

	// Assemble a minimal compilable file around the body and parse it. This
	// catches unbalanced braces, stray prose and syntax errors that a
	// string-matching check never would.
	var probe strings.Builder
	probe.WriteString("package p\n")
	for _, imp := range spec.Imports {
		fmt.Fprintf(&probe, "import %q\n", imp)
	}
	probe.WriteString(spec.Render(trimmed))

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "generated.go", probe.String(), parser.AllErrors)
	if err != nil {
		return BodyRejection{spec.Name, "the body is not valid Go: " + firstParseError(err)}
	}

	// The body must actually be the function we asked for, not a redefinition
	// of something else the model decided to write instead.
	var found bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name != spec.Name {
			return BodyRejection{spec.Name, "the body defines an unexpected function " + fn.Name.Name}
		}
		found = true
	}
	if !found {
		return BodyRejection{spec.Name, "no function declaration was produced"}
	}
	return nil
}

// firstParseError trims a multi-error parse failure to its first, most
// actionable line.
func firstParseError(err error) string {
	message := err.Error()
	if idx := strings.Index(message, "\n"); idx > 0 {
		message = message[:idx]
	}
	// Strip the synthetic filename and column noise, which is meaningless to a
	// reader who never sees the probe file.
	if idx := strings.Index(message, "generated.go:"); idx >= 0 {
		rest := message[idx+len("generated.go:"):]
		if colon := strings.Index(rest, ": "); colon >= 0 {
			return rest[colon+2:]
		}
	}
	return message
}

// GoFile assembles a formatted Go source file.
//
// Formatting is applied with go/format rather than by careful string building:
// gofmt output is canonical, so two runs producing semantically identical code
// produce byte-identical files, which keeps artifacts content-addressable and
// diffs meaningful.
type GoFile struct {
	Package string
	Doc     string
	Imports []string
	Decls   []string
}

// String renders and formats the file, falling back to the unformatted source
// if it cannot be parsed so that a defect is visible rather than swallowed.
func (g GoFile) String() string {
	var sb strings.Builder

	if g.Doc != "" {
		for _, line := range strings.Split(g.Doc, "\n") {
			sb.WriteString("// ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	fmt.Fprintf(&sb, "package %s\n\n", g.Package)

	if len(g.Imports) > 0 {
		if len(g.Imports) == 1 {
			fmt.Fprintf(&sb, "import %q\n\n", g.Imports[0])
		} else {
			sb.WriteString("import (\n")
			for _, imp := range g.Imports {
				fmt.Fprintf(&sb, "\t%q\n", imp)
			}
			sb.WriteString(")\n\n")
		}
	}

	for i, decl := range g.Decls {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(strings.TrimRight(decl, "\n"))
		sb.WriteString("\n")
	}

	source := sb.String()
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return source
	}
	return string(formatted)
}
