//go:build !linux

package sandbox

import (
	"os/exec"
	"syscall"
)

// Process control on platforms without Linux namespaces.
//
// Windows and macOS get no namespace isolation. That is stated plainly in the
// isolation report rather than being papered over: a sandbox that silently
// provides less confinement than it claims is worse than one that admits the
// gap, because callers make security decisions on the report.
//
// What still applies everywhere: the host environment is never inherited, so
// no credential is visible to generated code; the working directory is
// confined to the workspace; output is capped; and the process is killed at
// its deadline.

// namespaceAttrs returns nil: there are no namespaces to request.
func namespaceAttrs(_ bool) *syscall.SysProcAttr { return nil }

// plainAttrs returns nil, which asks for default process creation.
func plainAttrs() *syscall.SysProcAttr { return nil }

// killGroup terminates the process.
//
// On Windows a child does not inherit a killable process group by default, so
// this kills the leader. A build tool that spawns compilers can therefore
// leave orphans on Windows; the timeout still fires and the parent still exits.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// processAlive reports whether a process is still running.
//
// Signal 0 is not meaningful on Windows, so liveness is inferred from whether
// the process has been reaped: ProcessState is nil until Wait returns.
func processAlive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return cmd.ProcessState == nil
}
