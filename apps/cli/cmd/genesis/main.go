// Command genesis is the terminal client for the Genesis AI Factory.
//
// Argument parsing is implemented directly rather than with a framework: the
// verb set is small and stable, and a zero-dependency CLI cross-compiles to
// Windows and Linux as a single static binary with nothing to audit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/genesis-ai-factory/cli/internal/client"
	"github.com/genesis-ai-factory/cli/internal/commands"
	"github.com/genesis-ai-factory/cli/internal/ui"
)

var (
	version = "1.2.0"
	commit  = "dev"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			ui.Error(os.Stderr, "%s", apiErr.Error())
			if apiErr.RequestID != "" {
				fmt.Fprintf(os.Stderr, "  %s %s\n", ui.Gray("request:"), apiErr.RequestID)
			}
		} else {
			ui.Error(os.Stderr, "%s", err.Error())
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}

	verb := args[0]
	rest := args[1:]

	switch verb {
	case "-h", "--help", "help":
		usage()
		return nil
	case "-v", "--version", "version":
		fmt.Printf("genesis %s (%s)\n", version, commit)
		return nil
	}

	cli := &commands.Context{
		Client: client.FromEnvironment(),
		Out:    os.Stdout,
		Err:    os.Stderr,
	}
	ctx := context.Background()

	switch verb {
	case "create", "new", "build":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		name := fs.String("name", "", "explicit project name (derived from the brief otherwise)")
		noWatch := fs.Bool("no-watch", false, "return immediately instead of streaming the build")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		prompt := strings.Join(fs.Args(), " ")
		return commands.Create(ctx, cli, prompt, *name, !*noWatch)

	case "watch", "logs":
		if len(rest) == 0 {
			return errors.New("usage: genesis watch <run-id>")
		}
		return commands.Watch(ctx, cli, rest[0])

	case "status":
		if len(rest) == 0 {
			return errors.New("usage: genesis status <run-id>")
		}
		return commands.Status(ctx, cli, rest[0])

	case "projects", "ls":
		return commands.Projects(ctx, cli)

	case "agents":
		runID := ""
		if len(rest) > 0 {
			runID = rest[0]
		}
		return commands.Agents(ctx, cli, runID)

	case "artifacts", "docs":
		fs := flag.NewFlagSet("artifacts", flag.ContinueOnError)
		name := fs.String("name", "", "print one artifact by name or kind")
		// Go's flag package stops parsing at the first positional argument, so
		// `artifacts <id> --name X` would silently ignore the flag. Reordering
		// is what users actually type, and silently dropping their flag is a
		// worse failure than an error.
		if err := fs.Parse(reorderFlagsFirst(rest)); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return errors.New("usage: genesis artifacts <run-id> [--name PRD.md]")
		}
		return commands.Artifacts(ctx, cli, fs.Arg(0), *name)

	case "analyze", "analyse":
		path := "."
		if len(rest) > 0 {
			path = rest[0]
		}
		return commands.Analyze(ctx, cli, path)

	case "blueprints", "templates":
		return commands.Blueprints(ctx, cli)

	case "models", "model":
		return commands.Models(ctx, cli)

	case "cancel", "stop":
		if len(rest) == 0 {
			return errors.New("usage: genesis cancel <run-id>")
		}
		return commands.Cancel(ctx, cli, rest[0])

	case "login":
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		email := fs.String("email", "", "account email")
		password := fs.String("password", "", "account password")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *email == "" || *password == "" {
			return errors.New("usage: genesis login --email you@example.com --password '…'")
		}
		return commands.Login(ctx, cli, *email, *password)

	case "register", "signup":
		fs := flag.NewFlagSet("register", flag.ContinueOnError)
		email := fs.String("email", "", "account email")
		password := fs.String("password", "", "account password (at least 10 characters)")
		name := fs.String("name", "", "display name")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *email == "" || *password == "" {
			return errors.New("usage: genesis register --email you@example.com --password '…'")
		}
		return commands.Register(ctx, cli, *email, *password, *name)

	case "doctor", "check":
		return commands.Doctor(ctx, cli)

	default:
		return fmt.Errorf("unknown command %q — run 'genesis help' to see what is available", verb)
	}
}

// reorderFlagsFirst moves flag arguments ahead of positional ones so the
// standard flag package sees them.
func reorderFlagsFirst(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		// A flag written as "--name value" consumes the next argument, unless
		// it already carries one as "--name=value".
		if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func usage() {
	out := os.Stdout
	ui.Banner(out)

	fmt.Fprintln(out, ui.Bold("  USAGE"))
	fmt.Fprintln(out, "    genesis <command> [arguments]")
	fmt.Fprintln(out)

	fmt.Fprintln(out, ui.Bold("  BUILD"))
	line(out, `create "Build a CRM system"`, "Describe a product and build it")
	line(out, "create ... --no-watch", "Start a build without streaming it")
	line(out, "watch <run-id>", "Stream a build's event log")
	line(out, "status <run-id>", "Show phase-by-phase progress")
	line(out, "cancel <run-id>", "Stop a running build")
	fmt.Fprintln(out)

	fmt.Fprintln(out, ui.Bold("  INSPECT"))
	line(out, "projects", "List your projects")
	line(out, "agents [run-id]", "Show the agent roster or a live board")
	line(out, "artifacts <run-id>", "List generated documents")
	line(out, "artifacts <run-id> --name PRD.md", "Print one document")
	line(out, "blueprints", "List built-in product templates")
	line(out, "models", "Show whether model reasoning is active")
	line(out, "analyze [path]", "Analyse an existing codebase")
	fmt.Fprintln(out)

	fmt.Fprintln(out, ui.Bold("  ACCOUNT"))
	line(out, "login --email … --password …", "Sign in and store a session")
	line(out, "register --email … --password …", "Create an account")
	line(out, "doctor", "Check the environment and connection")
	fmt.Fprintln(out)

	fmt.Fprintln(out, ui.Bold("  ENVIRONMENT"))
	line(out, "GENESIS_API", "Control plane URL (default http://127.0.0.1:8787)")
	line(out, "GENESIS_TOKEN", "Access token (overrides the stored session)")
	line(out, "GENESIS_DATA_DIR", "Where the session file lives")
	line(out, "NO_COLOR", "Disable coloured output")
	fmt.Fprintln(out)

	fmt.Fprintln(out, ui.Bold("  EXAMPLE"))
	fmt.Fprintf(out, "    %s\n", ui.Gray(`genesis create "Build a Jira competitor with kanban boards"`))
	fmt.Fprintln(out)
}

func line(out *os.File, command, description string) {
	pad := 34 - len(command)
	if pad < 1 {
		pad = 1
	}
	fmt.Fprintf(out, "    %s%s%s\n", command, strings.Repeat(" ", pad), ui.Gray(description))
}
