//go:build linux

package sandbox

import (
	"fmt"
	"os/exec"
	"strconv"
)

// Resource limits.
//
// These are rlimits rather than cgroups. Cgroup v2 would be stricter — a real
// memory ceiling with OOM kill instead of allocation failure — but delegating a
// cgroup subtree requires root or systemd cooperation, and a desktop
// application can assume neither. This host, for example, exposes cgroup v2
// controllers but refuses to delegate a writable subtree. rlimits need no
// privilege and stop the failures that actually occur: fork bombs, runaway
// allocation, and filling the disk.
//
// Go's exec package removed SysProcAttr.Rlimits, and setting limits in the
// parent would constrain the server itself. The limits are therefore applied by
// exec'ing through prlimit(1), which sets them on itself and then execs the
// target, so they are inherited exactly once by the sandboxed process.

// limitWrapper returns the command and arguments needed to apply rlimits,
// wrapping the original invocation. It returns ok=false when limits cannot be
// applied, so the caller can report degraded isolation rather than assume it.
func limitWrapper(cfg Config, binary string, args []string) (string, []string, bool) {
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		return binary, args, false
	}

	wrapped := make([]string, 0, len(args)+6)

	if cfg.MemoryLimitBytes > 0 {
		// RLIMIT_AS caps the virtual address space. The Go runtime reserves a
		// large virtual arena, so this is deliberately generous: the purpose is
		// to stop unbounded allocation, not to account memory precisely.
		wrapped = append(wrapped, "--as="+strconv.FormatUint(cfg.MemoryLimitBytes, 10))
	}
	if cfg.MaxFileBytes > 0 {
		wrapped = append(wrapped, "--fsize="+strconv.FormatUint(cfg.MaxFileBytes, 10))
	}
	if cfg.MaxProcesses > 0 {
		wrapped = append(wrapped, "--nproc="+strconv.FormatUint(cfg.MaxProcesses, 10))
	}
	if len(wrapped) == 0 {
		return binary, args, false
	}

	wrapped = append(wrapped, "--", binary)
	wrapped = append(wrapped, args...)
	return prlimit, wrapped, true
}

// describeLimits renders the active ceilings for the isolation report.
func describeLimits(cfg Config) string {
	return fmt.Sprintf("memory=%dMiB files=%dMiB processes=%d",
		cfg.MemoryLimitBytes>>20, cfg.MaxFileBytes>>20, cfg.MaxProcesses)
}
