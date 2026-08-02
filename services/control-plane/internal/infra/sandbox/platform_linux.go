//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Linux process control.
//
// Namespaces, process groups and signals are Linux concepts with no portable
// equivalent, so they live behind a build tag. Keeping them in the shared file
// meant the whole control plane failed to compile for Windows — a
// cross-compile that had never been attempted, which is exactly how a
// portability defect survives to release.

// namespaceAttrs builds the clone flags and id mappings for an isolated process.
func namespaceAttrs(isolateNetwork bool) *syscall.SysProcAttr {
	flags := syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID |
		syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS
	if isolateNetwork {
		flags |= syscall.CLONE_NEWNET
	}

	return &syscall.SysProcAttr{
		Cloneflags: uintptr(flags),
		// Map the invoking user to root inside the namespace. This grants
		// capabilities *within* the namespace only; the process has no more
		// authority on the host than the user who started it.
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		// Required when writing gid_map from an unprivileged process.
		GidMappingsEnableSetgroups: false,
		// A process group lets us kill the whole tree, not just the leader.
		// Without it, a compiler's child processes survive a timeout.
		Setpgid: true,
	}
}

// plainAttrs requests a process group without namespace isolation.
func plainAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup terminates a process and everything it spawned.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// The group is gone; killing the leader is all that remains.
		return cmd.Process.Kill()
	}
	// SIGTERM first so a well-behaved program can flush output, then SIGKILL.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// processAlive reports whether a process is still running.
//
// Signal 0 performs the permission and existence checks without delivering
// anything, which is the standard way to ask "is this pid still there?".
func processAlive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return cmd.Process.Signal(syscall.Signal(0)) == nil
}
