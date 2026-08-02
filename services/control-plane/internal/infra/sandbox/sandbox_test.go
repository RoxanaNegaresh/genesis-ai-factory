package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/infra/sandbox"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

func newExecutor(t *testing.T) (*sandbox.Executor, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox is only implemented on Linux")
	}
	return sandbox.New(sandbox.DefaultConfig(), nil), t.TempDir()
}

func run(t *testing.T, e *sandbox.Executor, workspace string, req port.ExecRequest) *port.ExecResult {
	t.Helper()
	if req.Timeout == 0 {
		req.Timeout = 30 * time.Second
	}
	result, err := e.Exec(context.Background(), workspace, req)
	if err != nil {
		t.Fatalf("exec %s: %v", req.Command, err)
	}
	return result
}

func TestCapabilitiesAreReportedHonestly(t *testing.T) {
	e, _ := newExecutor(t)
	caps := e.Capabilities()

	t.Logf("isolation: namespaces=%v network=%v fs=%v memory=%v degraded=%v",
		caps.Namespaces, caps.NetworkIsolated, caps.FilesystemConfined,
		caps.MemoryLimited, caps.Degraded)

	// The report must be internally consistent. Claiming network isolation
	// without a network namespace would be a lie a caller cannot detect.
	if caps.NetworkIsolated {
		found := false
		for _, ns := range caps.Namespaces {
			if ns == "net" {
				found = true
			}
		}
		if !found {
			t.Fatal("network isolation claimed without a network namespace")
		}
	}
	if len(caps.Namespaces) == 0 && caps.Complete() {
		t.Fatal("no isolation was achieved but nothing was reported as degraded")
	}
}

func TestExecRunsCommandsAndCapturesOutput(t *testing.T) {
	e, workspace := newExecutor(t)

	result := run(t, e, workspace, port.ExecRequest{
		Command: "sh", Args: []string{"-c", "echo out; echo err >&2; exit 0"},
	})
	if !result.OK() {
		t.Fatalf("command failed: exit=%d %s", result.ExitCode, result.Output())
	}
	if !strings.Contains(result.Stdout, "out") {
		t.Errorf("stdout not captured: %q", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "err") {
		t.Errorf("stderr not captured: %q", result.Stderr)
	}
	if result.Duration <= 0 {
		t.Error("duration was not measured")
	}
}

func TestExecPropagatesExitCode(t *testing.T) {
	e, workspace := newExecutor(t)

	result := run(t, e, workspace, port.ExecRequest{
		Command: "sh", Args: []string{"-c", "exit 42"},
	})
	if result.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %d", result.ExitCode)
	}
	if result.OK() {
		t.Fatal("a failing command must not report OK")
	}
}

// The single most important property: generated code must not reach the
// network. Without this the sandbox provides no meaningful protection at all.
func TestNetworkIsolationBlocksEgress(t *testing.T) {
	e, workspace := newExecutor(t)
	if !e.Capabilities().NetworkIsolated {
		t.Skip("this host cannot create network namespaces")
	}

	result := run(t, e, workspace, port.ExecRequest{
		Command: "sh",
		Args:    []string{"-c", "cat /proc/net/dev | tail -n +3 | wc -l"},
		Network: port.NetworkNone,
	})

	// Only loopback may exist inside the namespace.
	interfaces := strings.TrimSpace(result.Stdout)
	if interfaces != "1" {
		t.Fatalf("expected exactly one interface (loopback), got %q\n%s", interfaces, result.Output())
	}
	if !result.Isolation.NetworkIsolated {
		t.Fatal("the result did not report network isolation")
	}
}

func TestNetworkIsolationIsTheDefault(t *testing.T) {
	e, workspace := newExecutor(t)
	if !e.Capabilities().NetworkIsolated {
		t.Skip("this host cannot create network namespaces")
	}

	// A request that says nothing about networking must get the safe default,
	// not the permissive one.
	result := run(t, e, workspace, port.ExecRequest{
		Command: "sh", Args: []string{"-c", "cat /proc/net/dev | tail -n +3 | wc -l"},
	})
	if strings.TrimSpace(result.Stdout) != "1" {
		t.Fatalf("network was not isolated by default: %q", result.Stdout)
	}
}

func TestHostNetworkIsOptInAndReported(t *testing.T) {
	e, workspace := newExecutor(t)

	result := run(t, e, workspace, port.ExecRequest{
		Command: "sh", Args: []string{"-c", "cat /proc/net/dev | tail -n +3 | wc -l"},
		Network: port.NetworkHost,
	})

	// Opting in must be visible in the report, so an audit can find every place
	// isolation was relaxed.
	if result.Isolation.NetworkIsolated {
		t.Fatal("host networking was requested but the result claims isolation")
	}
	found := false
	for _, note := range result.Isolation.Degraded {
		if strings.Contains(note, "host networking") {
			found = true
		}
	}
	if !found {
		t.Fatalf("relaxed isolation was not reported: %v", result.Isolation.Degraded)
	}
}

func TestWorkspaceConfinementRejectsEscape(t *testing.T) {
	e, workspace := newExecutor(t)

	for _, dir := range []string{"../", "../../etc", "/etc", "sub/../../.."} {
		_, err := e.Exec(context.Background(), workspace, port.ExecRequest{
			Command: "sh", Args: []string{"-c", "pwd"}, Dir: dir,
		})
		if err == nil {
			t.Errorf("working directory %q escaped the workspace and was accepted", dir)
		}
	}
}

func TestWorkingDirectoryIsTheWorkspace(t *testing.T) {
	e, workspace := newExecutor(t)
	if err := os.MkdirAll(filepath.Join(workspace, "api"), 0o750); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	result := run(t, e, workspace, port.ExecRequest{
		Command: "pwd", Dir: "api",
	})
	if !strings.HasSuffix(strings.TrimSpace(result.Stdout), "/api") {
		t.Fatalf("wrong working directory: %q", result.Stdout)
	}
}

// The host environment holds JWT secrets, database URLs and API tokens.
// Generated code must never see them.
func TestHostSecretsAreNotInherited(t *testing.T) {
	e, workspace := newExecutor(t)

	t.Setenv("GENESIS_JWT_SECRET", "super-secret-value-that-must-not-leak")
	t.Setenv("DATABASE_URL", "postgres://user:password@host/db")

	result := run(t, e, workspace, port.ExecRequest{
		Command: "env",
		Env:     map[string]string{"SAFE_VAR": "visible"},
	})

	for _, secret := range []string{"super-secret-value-that-must-not-leak", "postgres://user:password"} {
		if strings.Contains(result.Stdout, secret) {
			t.Errorf("a host secret leaked into the sandbox: %s", secret)
		}
	}
	if !strings.Contains(result.Stdout, "SAFE_VAR=visible") {
		t.Error("explicitly supplied environment was not passed through")
	}
	// PATH must survive or no toolchain can run.
	if !strings.Contains(result.Stdout, "PATH=") {
		t.Error("PATH was not passed through; toolchains will not resolve")
	}
}

func TestTimeoutKillsRunawayProcess(t *testing.T) {
	e, workspace := newExecutor(t)

	started := time.Now()
	result := run(t, e, workspace, port.ExecRequest{
		Command: "sleep", Args: []string{"60"}, Timeout: 2 * time.Second,
	})
	elapsed := time.Since(started)

	if !result.TimedOut {
		t.Fatalf("a hung command was not reported as timed out (exit=%d)", result.ExitCode)
	}
	if result.OK() {
		t.Fatal("a timed-out command must not report OK")
	}
	// Allow generous slack for the SIGTERM grace period.
	if elapsed > 12*time.Second {
		t.Fatalf("the timeout took %s to take effect", elapsed)
	}
}

// A build tool spawns compilers; killing only the leader leaves orphans that
// hold the workspace open and consume CPU indefinitely.
func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	e, workspace := newExecutor(t)

	marker := filepath.Join(workspace, "child-alive")
	script := "sh -c 'while true; do touch " + marker + "; sleep 0.2; done' & sleep 60"

	result := run(t, e, workspace, port.ExecRequest{
		Command: "sh", Args: []string{"-c", script}, Timeout: 2 * time.Second,
	})
	if !result.TimedOut {
		t.Fatalf("expected a timeout, got exit=%d", result.ExitCode)
	}

	// If the child survived it keeps touching the marker.
	_ = os.Remove(marker)
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a child process survived the timeout; the process group was not killed")
	}
}

func TestOutputIsCapped(t *testing.T) {
	e, workspace := newExecutor(t)

	// A program printing in a loop must not exhaust the server's memory.
	result := run(t, e, workspace, port.ExecRequest{
		Command:        "sh",
		Args:           []string{"-c", "i=0; while [ $i -lt 20000 ]; do echo '0123456789012345678901234567890123456789'; i=$((i+1)); done"},
		MaxOutputBytes: 64 << 10,
		Timeout:        30 * time.Second,
	})

	if !result.OutputTruncated {
		t.Fatalf("large output was not truncated (%d bytes captured)", len(result.Stdout))
	}
	if len(result.Stdout) > 256<<10 {
		t.Fatalf("the cap was not enforced: %d bytes captured", len(result.Stdout))
	}
	if !strings.Contains(result.Stdout, "truncated") {
		t.Error("truncation was not announced in the output")
	}
}

func TestMissingCommandIsReportedClearly(t *testing.T) {
	e, workspace := newExecutor(t)

	_, err := e.Exec(context.Background(), workspace, port.ExecRequest{
		Command: "definitely-not-a-real-binary-xyz",
	})
	if err == nil {
		t.Fatal("a missing command was accepted")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-real-binary-xyz") {
		t.Fatalf("the error does not name the missing command: %v", err)
	}
}

func TestEmptyCommandIsRejected(t *testing.T) {
	e, workspace := newExecutor(t)
	if _, err := e.Exec(context.Background(), workspace, port.ExecRequest{Command: "  "}); err == nil {
		t.Fatal("an empty command was accepted")
	}
}

func TestMissingWorkspaceIsRejected(t *testing.T) {
	e, _ := newExecutor(t)
	_, err := e.Exec(context.Background(), "/nonexistent/path/xyz", port.ExecRequest{Command: "true"})
	if err == nil {
		t.Fatal("a missing workspace was accepted")
	}
}

// --- long-running processes ----------------------------------------------

func TestStartAndStopLongRunningProcess(t *testing.T) {
	e, workspace := newExecutor(t)

	proc, err := e.Start(context.Background(), workspace, port.ExecRequest{
		Command: "sh", Args: []string{"-c", "echo started; sleep 30"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(proc.Output(), "started") {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(proc.Output(), "started") {
		t.Fatalf("the process produced no output: %q", proc.Output())
	}
	if !proc.Running() {
		t.Fatal("the process should still be running")
	}

	if err := proc.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatalf("wait after stop: %v", err)
	}
	if proc.Running() {
		t.Fatal("the process is still running after stop")
	}
}

// Port discovery is what lets the factory probe a server it did not write.
func TestPortDiscoveryFromOutput(t *testing.T) {
	e, workspace := newExecutor(t)

	cases := map[string]int{
		"listening on 127.0.0.1:8080":             8080,
		"Server started on port 3000":             3000,
		"now serving at http://localhost:5173":    5173,
		"INFO server listening addr=0.0.0.0:9090": 9090,
	}

	for line, want := range cases {
		proc, err := e.Start(context.Background(), workspace, port.ExecRequest{
			Command: "sh", Args: []string{"-c", "echo '" + line + "'; sleep 2"},
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}

		deadline := time.Now().Add(3 * time.Second)
		var discovered int
		for time.Now().Before(deadline) {
			if discovered = proc.Port(); discovered != 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = proc.Stop()
		_, _ = proc.Wait()

		if discovered != want {
			t.Errorf("for %q expected port %d, discovered %d", line, want, discovered)
		}
	}
}

func TestPortDiscoveryIgnoresNoise(t *testing.T) {
	e, workspace := newExecutor(t)

	proc, err := e.Start(context.Background(), workspace, port.ExecRequest{
		Command: "sh",
		Args:    []string{"-c", "echo 'compiled 42 files in 3 seconds'; echo 'warning: 80 issues found'; sleep 1"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	discovered := proc.Port()
	_ = proc.Stop()
	_, _ = proc.Wait()

	if discovered != 0 {
		t.Fatalf("a port was invented from unrelated output: %d", discovered)
	}
}

// A toolchain that falls back to the platform's default temporary directory
// will use /tmp, which on a container host is usually a small tmpfs. A Go
// build writes hundreds of megabytes of intermediate objects; when tmpfs runs
// out, the compiler is killed mid-link and prints a SIGSEGV stack trace that
// reads like a compiler bug in the code being built. This test exists because
// that misdiagnosis cost real time: the benchmark reported 35% and blamed the
// generated projects, when the generated projects were fine.
func TestScratchDirectoryIsProvidedOnTheWorkspaceFilesystem(t *testing.T) {
	e, workspace := newExecutor(t)

	result := run(t, e, workspace, port.ExecRequest{Command: "env"})

	if !strings.Contains(result.Stdout, "TMPDIR=") {
		t.Fatal("no TMPDIR was supplied; the toolchain will fall back to /tmp")
	}

	// The directory must be inside the workspace, so it shares the
	// workspace's filesystem and is removed along with it.
	var tmpdir string
	for _, line := range strings.Split(result.Stdout, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "TMPDIR="); ok {
			tmpdir = value
			break
		}
	}
	if !strings.HasPrefix(tmpdir, workspace) {
		t.Errorf("TMPDIR %q is outside the workspace %q", tmpdir, workspace)
	}
	if _, err := os.Stat(tmpdir); err != nil {
		t.Errorf("TMPDIR was advertised but does not exist: %v", err)
	}
}

// A caller that sets TMPDIR deliberately knows something the sandbox does not,
// so the explicit value must win.
func TestExplicitTMPDIROverridesTheDefault(t *testing.T) {
	e, workspace := newExecutor(t)

	custom := t.TempDir()
	result := run(t, e, workspace, port.ExecRequest{
		Command: "sh",
		Args:    []string{"-c", "echo $TMPDIR"},
		Env:     map[string]string{"TMPDIR": custom},
	})

	if strings.TrimSpace(result.Stdout) != custom {
		t.Errorf("expected the caller's TMPDIR %q, got %q", custom, strings.TrimSpace(result.Stdout))
	}
}

// The control plane must compile for every platform the desktop app ships on.
//
// Linux namespace syscalls once lived in the shared source file, so the whole
// server failed to cross-compile for Windows — undiscovered because nobody had
// tried until an installer was needed. Platform-specific process control now
// sits behind build tags, and this test fails if it leaks back.
func TestPlatformSpecificCodeStaysBehindBuildTags(t *testing.T) {
	source, err := os.ReadFile("sandbox.go")
	if err != nil {
		t.Fatalf("read sandbox.go: %v", err)
	}
	text := string(source)

	// Symbols that do not exist on Windows. Each must live in platform_linux.go.
	for _, symbol := range []string{
		"syscall.CLONE_NEW",
		"syscall.SysProcIDMap",
		"syscall.Kill",
		"syscall.Getpgid",
		"Cloneflags",
		"Setpgid",
	} {
		if strings.Contains(text, symbol) {
			t.Errorf("%s appears in the shared source; it breaks the Windows build. "+
				"Move it to platform_linux.go and add a portable counterpart in "+
				"platform_other.go", symbol)
		}
	}
}

// Both platform files must define the same set of helpers, or one target
// compiles and the other does not.
func TestPlatformFilesAgreeOnTheirInterface(t *testing.T) {
	required := []string{"namespaceAttrs", "plainAttrs", "killGroup", "processAlive"}

	for _, file := range []string{"platform_linux.go", "platform_other.go"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, fn := range required {
			if !strings.Contains(string(source), "func "+fn+"(") {
				t.Errorf("%s does not define %s", file, fn)
			}
		}
	}
}
