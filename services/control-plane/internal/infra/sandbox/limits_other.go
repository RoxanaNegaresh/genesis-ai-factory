//go:build !linux

package sandbox

// Resource limits are only implemented on Linux.
//
// On other platforms the executor reports degraded isolation rather than
// silently running unconstrained, so a caller can decide whether that is
// acceptable instead of discovering it from an incident.

func limitWrapper(cfg Config, binary string, args []string) (string, []string, bool) {
	return binary, args, false
}

func describeLimits(cfg Config) string { return "not supported on this platform" }
