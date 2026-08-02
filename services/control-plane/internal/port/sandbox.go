package port

import (
	"context"
	"time"
)

// NetworkPolicy controls what a sandboxed process may reach.
type NetworkPolicy string

const (
	// NetworkNone isolates the process in an empty network namespace with only
	// loopback. This is the default for everything the factory runs, because
	// generated code has no legitimate reason to reach the internet and every
	// reason not to be able to.
	NetworkNone NetworkPolicy = "none"
	// NetworkHost shares the host network. Required for dependency resolution
	// (go mod download, npm install) and nothing else.
	NetworkHost NetworkPolicy = "host"
)

// ExecRequest describes a command to run under isolation.
type ExecRequest struct {
	// Command and Args are executed directly; there is no shell, so no shell
	// injection surface.
	Command string
	Args    []string
	// Dir is relative to the workspace root and is confined to it.
	Dir string
	// Env is the complete environment. The host environment is never inherited:
	// it holds tokens, database URLs and credentials that generated code must
	// not see.
	Env map[string]string
	// Timeout bounds wall-clock execution.
	Timeout time.Duration
	// Network selects the egress policy.
	Network NetworkPolicy
	// MaxOutputBytes caps captured stdout+stderr so a runaway process cannot
	// exhaust memory through logging alone.
	MaxOutputBytes int64
	// Stdin is optional input.
	Stdin string
	// MemoryLimitBytes overrides the sandbox's default address-space ceiling
	// for this one command. Zero means use the default;
	// MemoryLimitDisabled means apply no ceiling at all.
	//
	// It exists because RLIMIT_AS caps *virtual* address space, not resident
	// memory, and runtimes differ enormously in how much they reserve. V8
	// maps a multi-gigabyte arena before executing a line of JavaScript, so a
	// ceiling that is generous for a Go compiler kills node outright — and it
	// dies inside V8 with a stack trace that says nothing about rlimits.
	// Rather than raise the global limit to whatever the hungriest runtime
	// needs, each toolchain declares its own.
	MemoryLimitBytes uint64
}

// MemoryLimitDisabled requests that no address-space ceiling be applied.
//
// It is distinct from zero, which means "use the default". A runtime that
// cannot function under any RLIMIT_AS — anything instantiating WebAssembly,
// for instance — must say so explicitly rather than being handed a number so
// large it never binds, which would look like a limit while being none.
const MemoryLimitDisabled uint64 = 1

// ExecResult reports the outcome of a sandboxed command.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	// TimedOut reports that the process was killed at the deadline rather than
	// exiting on its own.
	TimedOut bool
	// OutputTruncated reports that output exceeded MaxOutputBytes.
	OutputTruncated bool
	// Isolation records what was actually applied, which may be weaker than
	// requested on a host lacking the necessary primitives. Reporting this
	// honestly matters: a caller must be able to tell whether "it ran safely"
	// is a guarantee or an aspiration.
	Isolation IsolationReport
}

// OK reports whether the command succeeded.
func (r ExecResult) OK() bool { return r.ExitCode == 0 && !r.TimedOut }

// Output returns stdout and stderr combined, which is what a human reading a
// build failure actually wants.
func (r ExecResult) Output() string {
	if r.Stderr == "" {
		return r.Stdout
	}
	if r.Stdout == "" {
		return r.Stderr
	}
	return r.Stdout + "\n" + r.Stderr
}

// IsolationReport describes the isolation actually achieved.
type IsolationReport struct {
	// Namespaces applied, for example "user", "mount", "pid", "net".
	Namespaces []string `json:"namespaces"`
	// NetworkIsolated reports that the process could not reach the network.
	NetworkIsolated bool `json:"network_isolated"`
	// FilesystemConfined reports that writes were restricted to the workspace.
	FilesystemConfined bool `json:"filesystem_confined"`
	// MemoryLimited reports that a memory ceiling was enforced.
	MemoryLimited bool `json:"memory_limited"`
	// Degraded lists isolation that was requested but could not be applied.
	Degraded []string `json:"degraded,omitempty"`
}

// Complete reports whether every requested control was applied.
func (r IsolationReport) Complete() bool { return len(r.Degraded) == 0 }

// Sandbox executes untrusted commands under isolation.
//
// The interface is intentionally small. Everything richer — build pipelines,
// test parsing, health probing — is composed on top in the application layer,
// so the isolation mechanism can be replaced (namespaces here, gVisor or a
// remote runner elsewhere) without touching anything that uses it.
type Sandbox interface {
	// Exec runs one command to completion.
	Exec(ctx context.Context, workspace string, req ExecRequest) (*ExecResult, error)
	// Start launches a long-running process and returns a handle. Used for
	// servers that must stay up while they are probed.
	Start(ctx context.Context, workspace string, req ExecRequest) (Process, error)
	// Capabilities reports what isolation this sandbox can provide on this
	// host, so callers can warn rather than silently running unprotected.
	Capabilities() IsolationReport
	// Name identifies the implementation.
	Name() string
}

// Process is a running sandboxed program.
type Process interface {
	// Wait blocks until the process exits.
	Wait() (*ExecResult, error)
	// Stop terminates the process and its children.
	Stop() error
	// Output returns what has been captured so far.
	Output() string
	// Port reports a TCP port the process is listening on, discovered by
	// watching its output, or 0 if none has been seen.
	Port() int
	// Running reports whether the process is still alive.
	Running() bool
}
