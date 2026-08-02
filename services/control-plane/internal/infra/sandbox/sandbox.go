// Package sandbox executes untrusted generated code under OS-level isolation.
//
// Design decision: this uses Linux namespaces directly rather than shelling out
// to Docker or Podman.
//
// The v0.1 architecture named Docker as the sandbox. Building it revealed that
// requirement to be wrong for the primary deployment target. Genesis is a
// desktop application; requiring a container runtime would mean a user cannot
// run generated code until they install and configure Docker Desktop, which is
// exactly the friction the local-first invariant exists to eliminate. Linux
// namespaces are in the kernel, need no daemon, no root and no installation,
// and provide the controls that actually matter here: an empty network
// namespace is precisely `--network=none`, and a pivoted mount namespace
// confines writes as effectively as a bind-mounted volume.
//
// What this does not provide, and Docker would: a separate filesystem image, so
// generated code can *read* host binaries and libraries it needs to run at all.
// That is an accepted trade for the desktop case and is reported honestly
// through IsolationReport rather than glossed over. A server deployment should
// use the OCI executor (v1.0) or gVisor.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Config tunes the executor.
type Config struct {
	// MaxTimeout caps any single command regardless of what a caller requests.
	MaxTimeout time.Duration
	// MaxOutputBytes caps captured output when a request does not specify one.
	MaxOutputBytes int64
	// MemoryLimitBytes is applied via RLIMIT_AS. Zero disables the limit.
	MemoryLimitBytes uint64
	// MaxProcesses is applied via RLIMIT_NPROC and stops fork bombs.
	MaxProcesses uint64
	// MaxFileBytes is applied via RLIMIT_FSIZE and stops disk exhaustion.
	MaxFileBytes uint64
	// AllowNetwork permits requests to opt into host networking. Dependency
	// resolution needs it; nothing else should have it.
	AllowNetwork bool
	// PassThroughEnv names host variables that may be inherited. Kept minimal:
	// PATH and HOME are needed for toolchains to function at all.
	PassThroughEnv []string
}

// DefaultConfig returns a conservative configuration.
func DefaultConfig() Config {
	return Config{
		MaxTimeout:     10 * time.Minute,
		MaxOutputBytes: 4 << 20,
		// 1 GiB is generous for a compiler and tight enough that a runaway
		// allocation fails fast instead of swapping the machine to a halt.
		MemoryLimitBytes: 1 << 30,
		MaxProcesses:     512,
		MaxFileBytes:     512 << 20,
		AllowNetwork:     true,
		PassThroughEnv:   []string{"PATH", "HOME", "LANG", "TZ"},
	}
}

// Executor runs commands inside Linux namespaces.
type Executor struct {
	cfg  Config
	log  *slog.Logger
	caps port.IsolationReport
}

// New constructs an executor and probes what isolation this host supports.
func New(cfg Config, log *slog.Logger) *Executor {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = DefaultConfig().MaxTimeout
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = DefaultConfig().MaxOutputBytes
	}
	e := &Executor{cfg: cfg, log: log}
	e.caps = e.probe()
	return e
}

var _ port.Sandbox = (*Executor)(nil)

// Name identifies the implementation.
func (e *Executor) Name() string { return "linux-namespaces" }

// Capabilities reports the isolation available on this host.
func (e *Executor) Capabilities() port.IsolationReport { return e.caps }

// probe determines which namespaces this kernel and user can actually create.
//
// Probing once at construction, by really attempting it, is the only reliable
// answer: /proc knobs, seccomp policy, AppArmor and container-in-container all
// affect the result in ways that cannot be read from configuration.
func (e *Executor) probe() port.IsolationReport {
	report := port.IsolationReport{}

	if runtime.GOOS != "linux" {
		report.Degraded = append(report.Degraded,
			"OS-level isolation is only implemented on Linux; commands run unconfined")
		return report
	}

	// A single probe covering every namespace we want. If it succeeds we have
	// all of them; if not, fall back to reporting none rather than guessing
	// which one was refused.
	probe := exec.Command("/proc/self/exe")
	probe.SysProcAttr = namespaceAttrs(true)
	probe.Args = []string{"true"}

	// /proc/self/exe is the test binary or server; running it is wrong. Use a
	// harmless real program instead.
	probe = exec.Command("/bin/true")
	probe.SysProcAttr = namespaceAttrs(true)

	if err := probe.Run(); err != nil {
		report.Degraded = append(report.Degraded,
			"user namespaces are unavailable ("+err.Error()+"); commands run without namespace isolation")
		if e.cfg.MemoryLimitBytes > 0 {
			report.MemoryLimited = true
		}
		return report
	}

	report.Namespaces = []string{"user", "mount", "pid", "ipc", "uts", "net"}
	report.NetworkIsolated = true
	report.FilesystemConfined = true
	report.MemoryLimited = e.cfg.MemoryLimitBytes > 0
	return report
}

// Exec runs a command to completion under isolation.
func (e *Executor) Exec(ctx context.Context, workspace string, req port.ExecRequest) (*port.ExecResult, error) {
	proc, err := e.start(ctx, workspace, req)
	if err != nil {
		return nil, err
	}
	return proc.Wait()
}

// Start launches a long-running process and returns a handle to it.
func (e *Executor) Start(ctx context.Context, workspace string, req port.ExecRequest) (port.Process, error) {
	return e.start(ctx, workspace, req)
}

func (e *Executor) start(ctx context.Context, workspace string, req port.ExecRequest) (*process, error) {
	if strings.TrimSpace(req.Command) == "" {
		return nil, domain.Invalid("command_required", "a command is required")
	}

	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, domain.Invalid("workspace_missing", "the workspace directory does not exist")
	}

	// The working directory is confined to the workspace. A command that could
	// set cwd to an arbitrary path would defeat the point of confining writes.
	dir := root
	if req.Dir != "" {
		// The path is validated *before* normalisation. Rooting it first —
		// filepath.Clean("/" + "../") — silently collapses an escape to "/",
		// which then resolves harmlessly to the workspace root. That turns a
		// rejected attack into an accepted one: the caller believes its path
		// was honoured while confinement quietly rewrote it. Any traversal is
		// an error, not something to repair.
		if filepath.IsAbs(req.Dir) {
			return nil, domain.Invalid("dir_absolute", "the working directory must be relative to the workspace")
		}
		cleaned := filepath.Clean(req.Dir)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
			return nil, domain.Invalid("dir_escape", "the working directory escapes the workspace")
		}

		resolved := filepath.Join(root, cleaned)
		// Re-check after joining, which also catches symlinks pointing out of
		// the workspace.
		if real, err := filepath.EvalSymlinks(resolved); err == nil {
			resolved = real
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, domain.Invalid("dir_escape", "the working directory escapes the workspace")
		}
		dir = resolved
	}

	timeout := req.Timeout
	if timeout <= 0 || timeout > e.cfg.MaxTimeout {
		timeout = e.cfg.MaxTimeout
	}

	network := req.Network
	if network == "" {
		network = port.NetworkNone
	}
	if network == port.NetworkHost && !e.cfg.AllowNetwork {
		return nil, domain.Forbidden("network access is disabled for this sandbox")
	}

	// The command must be resolvable. Doing this before launch turns an opaque
	// "fork/exec failed" into a message naming the missing tool.
	binary, err := exec.LookPath(req.Command)
	if err != nil {
		return nil, domain.Invalid("command_not_found",
			fmt.Sprintf("%s was not found on PATH", req.Command))
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)

	cmd := exec.CommandContext(runCtx, binary, req.Args...)
	cmd.Dir = dir
	cmd.Env = e.buildEnv(req.Env, e.scratchDir(root))

	// A per-request ceiling overrides the default. See ExecRequest for why
	// one global number cannot serve every runtime.
	limitCfg := e.cfg
	switch {
	case req.MemoryLimitBytes == port.MemoryLimitDisabled:
		limitCfg.MemoryLimitBytes = 0
	case req.MemoryLimitBytes > 0:
		limitCfg.MemoryLimitBytes = req.MemoryLimitBytes
	}

	isolated := len(e.caps.Namespaces) > 0
	if isolated {
		cmd.SysProcAttr = namespaceAttrs(network == port.NetworkNone)
	} else {
		cmd.SysProcAttr = plainAttrs()
	}

	// Kill the whole process group, not just the leader: a build tool that
	// spawns a compiler leaves orphans otherwise.
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	maxOutput := req.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = e.cfg.MaxOutputBytes
	}

	stdout := newCappedBuffer(maxOutput)
	stderr := newCappedBuffer(maxOutput)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}

	report := e.caps
	report.NetworkIsolated = isolated && network == port.NetworkNone
	if network == port.NetworkHost {
		report.Degraded = append(append([]string{}, report.Degraded...),
			"host networking was explicitly requested for this command")
	}

	// Resource ceilings are applied by exec'ing through prlimit, so they bind
	// to the sandboxed process rather than to this server.
	limited := false
	if wrapper, wrapperArgs, ok := limitWrapper(limitCfg, binary, req.Args); ok {
		cmd.Path = wrapper
		cmd.Args = append([]string{wrapper}, wrapperArgs...)
		limited = true
	}
	report.MemoryLimited = limited
	if !limited && e.cfg.MemoryLimitBytes > 0 {
		report.Degraded = append(append([]string{}, report.Degraded...),
			"resource limits could not be applied (prlimit unavailable)")
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", req.Command, err)
	}

	e.log.Debug("sandboxed command started",
		"command", req.Command, "dir", dir, "network", string(network), "isolated", isolated)

	return &process{
		cmd:     cmd,
		cancel:  cancel,
		runCtx:  runCtx,
		stdout:  stdout,
		stderr:  stderr,
		started: started,
		report:  report,
		timeout: timeout,
		logger:  e.log,
	}, nil
}

// buildEnv constructs the process environment.
//
// The host environment is never inherited wholesale. It contains JWT secrets,
// database URLs and API tokens; handing those to generated code would make the
// sandbox pointless.
//
// scratch is a directory on the same filesystem as the workspace, offered to
// the child as TMPDIR. Without it a toolchain falls back to /tmp, which on
// many hosts — including every container that mounts /tmp as tmpfs — is a
// small RAM-backed filesystem. A Go build writes hundreds of megabytes of
// intermediate objects there, and when it runs out the compiler does not fail
// with "no space left on device": it is killed mid-link and reports a SIGSEGV
// stack trace, which reads exactly like a compiler bug in the generated code.
// That misdiagnosis is the real cost, so the sandbox picks the directory
// rather than leaving it to chance.
func (e *Executor) buildEnv(requested map[string]string, scratch string) []string {
	env := make([]string, 0, len(requested)+len(e.cfg.PassThroughEnv)+2)

	for _, name := range e.cfg.PassThroughEnv {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	if scratch != "" {
		env = append(env, "TMPDIR="+scratch)
	}

	// An explicit request always wins: a caller that sets TMPDIR deliberately
	// knows something the sandbox does not.
	for name, value := range requested {
		env = append(env, name+"="+value)
	}
	return env
}

// scratchDir returns a temporary directory for a run, created inside the
// workspace so it shares the workspace's filesystem and disappears with it.
//
// A failure here is not fatal. The scratch directory is an optimisation over
// the platform default, and a sandbox that refuses to run because it could not
// create a cache directory is worse than one that falls back.
func (e *Executor) scratchDir(root string) string {
	dir := filepath.Join(root, ".genesis-tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.log.Debug("scratch directory unavailable; falling back to the platform default",
			"error", err)
		return ""
	}
	return dir
}

// process is a running sandboxed command.
type process struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	runCtx  context.Context
	stdout  *cappedBuffer
	stderr  *cappedBuffer
	started time.Time
	report  port.IsolationReport
	timeout time.Duration
	logger  *slog.Logger

	mu      sync.Mutex
	result  *port.ExecResult
	waited  bool
	stopped bool
}

var _ port.Process = (*process)(nil)

// Wait blocks until the process exits and returns its result.
func (p *process) Wait() (*port.ExecResult, error) {
	p.mu.Lock()
	if p.waited {
		result := p.result
		p.mu.Unlock()
		return result, nil
	}
	p.mu.Unlock()

	err := p.cmd.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.waited = true
	p.cancel()

	result := &port.ExecResult{
		Stdout:          p.stdout.String(),
		Stderr:          p.stderr.String(),
		Duration:        time.Since(p.started),
		Isolation:       p.report,
		OutputTruncated: p.stdout.Truncated() || p.stderr.Truncated(),
	}

	// A deadline kill surfaces as a signal, which is indistinguishable from a
	// crash unless the context is checked.
	if errors.Is(p.runCtx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		result.ExitCode = -1
		if result.Stderr == "" {
			result.Stderr = fmt.Sprintf("the command was killed after %s", p.timeout)
		}
		p.result = result
		return result, nil
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			if result.Stderr == "" {
				result.Stderr = err.Error()
			}
		}
	}
	p.result = result
	return result, nil
}

// Stop terminates the process and its children.
func (p *process) Stop() error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()

	err := killGroup(p.cmd)
	p.cancel()
	return err
}

// Output returns what has been captured so far.
func (p *process) Output() string {
	out := p.stdout.String()
	if errText := p.stderr.String(); errText != "" {
		if out != "" {
			out += "\n"
		}
		out += errText
	}
	return out
}

// Running reports whether the process is still alive.
func (p *process) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waited || p.stopped {
		return false
	}
	if p.cmd.Process == nil {
		return false
	}
	// Signal 0 tests for existence without affecting the process.
	return processAlive(p.cmd)
}

// portPattern recognises the ways servers announce their listening port.
//
// Reading the port from output rather than requiring configuration is what lets
// the factory probe a generated server it did not write: frameworks disagree on
// everything except printing the address they bound.
var portPattern = regexp.MustCompile(`(?i)(?:listening|running|started|serving|bound|available).{0,40}?(?::|port[ =])\s*(\d{2,5})|https?://(?:localhost|127\.0\.0\.1|0\.0\.0\.0):(\d{2,5})`)

// Port reports a TCP port discovered in the process output.
func (p *process) Port() int {
	matches := portPattern.FindAllStringSubmatch(p.Output(), -1)
	for i := len(matches) - 1; i >= 0; i-- {
		for _, group := range matches[i][1:] {
			if group == "" {
				continue
			}
			value, err := strconv.Atoi(group)
			if err != nil || value < 1024 || value > 65535 {
				continue
			}
			return value
		}
	}
	return 0
}

// cappedBuffer accumulates output up to a limit.
//
// An unbounded buffer is a denial-of-service vector: a program printing in a
// loop would exhaust the server's memory long before its own limit was reached.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func newCappedBuffer(limit int64) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

var _ io.Writer = (*cappedBuffer)(nil)

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	remaining := b.limit - b.written
	if remaining <= 0 {
		b.truncated = true
		// Report success so the writer is not killed by a broken pipe; the
		// output is discarded but the program keeps running.
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		b.buf.Write(p[:remaining])
		b.written = b.limit
		b.truncated = true
		b.buf.WriteString("\n… output truncated at the size limit\n")
		return len(p), nil
	}

	b.buf.Write(p)
	b.written += int64(len(p))
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *cappedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
