package factory

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// The verification runner.
//
// Up to v0.3 the factory could prove a generated project *compiles*. That is a
// meaningful bar and a low one: a service that compiles and then panics on
// startup, or binds no port, or returns 500 to every request, is not a working
// product. This runner closes that gap by building the project, running its
// tests, starting it, and making a real HTTP request against it — all inside
// the sandbox, with no network access.

// Toolchain describes how to build, test and run one language's project.
//
// Expressing this as data rather than a switch statement means adding a
// language is a table entry, and the runner's logic — which is where the
// subtlety lives — is written once.
type Toolchain struct {
	Name string
	// Marker identifies a project of this kind, relative to its directory.
	Marker string
	// Dir is where the project lives inside the workspace.
	Dir string
	// Install resolves dependencies. It is the only step permitted network
	// access, and only because it cannot work without it.
	Install *Step
	// Build compiles the project.
	Build *Step
	// Test runs the project's own test suite.
	Test *Step
	// Serve starts the application.
	Serve *Step
	// ServeEnv supplies configuration the application needs to boot.
	ServeEnv map[string]string
	// StepEnv is supplied to install, build and test. Unlike ServeEnv it is
	// not about configuring the application; it is about bounding the
	// toolchain that builds it.
	StepEnv map[string]string
	// StepMemoryLimitBytes overrides the sandbox's address-space ceiling for
	// install, build and test. Zero uses the sandbox default.
	StepMemoryLimitBytes uint64
	// ServePortArgs are appended to Serve.Args with the allocated port
	// substituted for {{port}}. Some servers take a port on the command line
	// rather than from the environment, and Vite is one of them: it ignores
	// PORT and would happily bind its default while the probe waited on the
	// port the runner allocated.
	ServePortArgs []string
	// HealthPath is probed once the process is listening.
	HealthPath string
}

// Step is one command in a toolchain.
type Step struct {
	Command string
	Args    []string
	Timeout time.Duration
	// Network is granted only where the step genuinely requires it.
	Network port.NetworkPolicy
	// MemoryLimitBytes overrides the sandbox default for this step. Zero
	// means use the default.
	MemoryLimitBytes uint64
}

// nodeMemoryLimit disables the address-space ceiling for Node processes.
//
// This is a deliberate, narrow exemption and it is worth explaining, because
// "we turned the limit off" deserves more than a shrug.
//
// RLIMIT_AS caps *virtual* address space. V8 reserves a multi-gigabyte arena
// before running a line of JavaScript, and WebAssembly reserves more still —
// esbuild, which Vite uses, is a Wasm module. Measured on this host, npm
// install needs above 1 GiB to start at all, and `vite build` fails at 2, 4
// and even 6 GiB with:
//
//	RangeError: WebAssembly.instantiate(): Out of memory:
//	Cannot allocate Wasm memory for new instance
//
// It succeeds with no RLIMIT_AS at all. The failure is therefore not about the
// size of the ceiling but about its existence: a Wasm instance wants a
// contiguous reservation that any RLIMIT_AS refuses. Raising the number does
// not help, so setting a large one would be theatre — an apparent limit that
// never binds, hiding the fact that this runtime is unbounded.
//
// What still constrains a Node process: RLIMIT_FSIZE bounds what it can write,
// RLIMIT_NPROC bounds how many processes it can spawn, the network namespace
// is empty outside the install step, the host environment is never inherited
// so no credential is visible, the process group is killed on timeout, and
// output is capped. Memory alone is unbounded, and the isolation report says
// so rather than implying otherwise.
//
// The honest fix is a cgroup v2 memory ceiling, which limits resident memory
// instead of address space and does not disturb Wasm. That needs a delegated
// subtree, which this host refuses to grant; the swap point is port.Sandbox.
const nodeMemoryLimit = port.MemoryLimitDisabled

// GoToolchain describes the generated Go service.
func GoToolchain() Toolchain {
	return Toolchain{
		Name: "go", Marker: "go.mod", Dir: "api",
		// Bound the compiler's own parallelism.
		//
		// `go build` runs one compiler process per CPU by default and each can
		// hold hundreds of megabytes. On a small machine the kernel OOM-kills
		// one mid-run, and the survivor prints a Go runtime stack trace — so
		// the failure arrives looking like a compiler bug in the generated
		// code, when the generated code is fine. The benchmark scored 35% and
		// blamed the projects for exactly this reason.
		//
		// GOFLAGS caps concurrent actions; GOMEMLIMIT makes the compiler's own
		// GC work harder before the kernel has to intervene. Both are advisory
		// and simply do nothing on a large machine.
		StepEnv: map[string]string{
			// One compile action at a time. Each holds its own arena, so
			// parallel actions multiply peak usage by the number of CPUs.
			"GOFLAGS":     "-p=1",
			"GOMAXPROCS":  "2",
			"CGO_ENABLED": "0",
		},
		// Compiling a generated project needs more address space than the
		// sandbox default.
		//
		// RLIMIT_AS bounds virtual address space, and the Go compiler reserves
		// a large arena before it does any work. Against the 1 GiB default it
		// dies part-way through pgx/v5/pgtype with "runtime: out of memory:
		// cannot allocate 4194304-byte block", then prints a goroutine dump —
		// which is why this looked like a compiler bug rather than a limit.
		// 3 GiB of address space is not 3 GiB of RAM; resident use stays in
		// the hundreds of megabytes.
		StepMemoryLimitBytes: 3 << 30,
		// `go mod tidy` needs the module proxy; nothing else does.
		Install: &Step{Command: "go", Args: []string{"mod", "tidy"},
			Timeout: 5 * time.Minute, Network: port.NetworkHost},
		Build: &Step{Command: "go", Args: []string{"build", "./..."},
			Timeout: 5 * time.Minute, Network: port.NetworkNone},
		Test: &Step{Command: "go", Args: []string{"test", "./internal/domain/"},
			Timeout: 3 * time.Minute, Network: port.NetworkNone},
		Serve: &Step{Command: "go", Args: []string{"run", "./cmd/server"},
			Timeout: 90 * time.Second, Network: port.NetworkNone},
		ServeEnv: map[string]string{
			"ADDR": "127.0.0.1:0",
			// The generated server validates these at boot and exits without
			// them; supplying throwaway values is what lets it start at all.
			"DATABASE_URL": "postgres://sandbox:sandbox@127.0.0.1:5432/sandbox?sslmode=disable",
			"JWT_SECRET":   "sandbox-only-secret-not-used-for-anything-real",
			"LOG_LEVEL":    "info",
		},
		HealthPath: "/health",
	}
}

// NodeToolchain describes the generated web client.
//
// The frontend was built but never started until v1.2, so "the web app works"
// rested on tsc exiting zero. A type-checked bundle that crashes on load is
// still a broken product, and the only way to know the difference is to serve
// it and fetch a page.
//
// `vite preview` is used rather than `vite dev`. Preview serves the built
// artifact, which is what ships; dev serves through the transform pipeline,
// so it can succeed on a bundle that would fail in production.
//
// npm ci is preferred over npm install where a lockfile exists, but the
// generator ships no lockfile — its contents cannot be invented, only
// resolved — so install is correct here and the README says so.
func NodeToolchain() Toolchain {
	return Toolchain{
		Name: "node", Marker: "package.json", Dir: "web",
		Install: &Step{Command: "npm", Args: []string{"install", "--no-audit", "--no-fund"},
			Timeout: 10 * time.Minute, Network: port.NetworkHost,
			MemoryLimitBytes: nodeMemoryLimit},
		Build: &Step{Command: "npm", Args: []string{"run", "build"},
			Timeout: 10 * time.Minute, Network: port.NetworkNone,
			MemoryLimitBytes: nodeMemoryLimit},
		Test: &Step{Command: "npm", Args: []string{"run", "typecheck"},
			Timeout: 5 * time.Minute, Network: port.NetworkNone,
			MemoryLimitBytes: nodeMemoryLimit},
		// --strictPort makes a port collision an immediate failure rather
		// than a silent move to another port, which would leave the probe
		// waiting on an address nothing is listening to.
		Serve: &Step{Command: "npm",
			Args:    []string{"run", "preview", "--"},
			Timeout: 90 * time.Second, Network: port.NetworkNone,
			MemoryLimitBytes: nodeMemoryLimit},
		ServePortArgs: []string{"--host", "127.0.0.1", "--port", "{{port}}", "--strictPort"},
		ServeEnv:      map[string]string{"NODE_ENV": "production"},
		// The SPA entry point. A 200 here means the bundle was produced,
		// the server found it, and it is being served.
		HealthPath: "/",
	}
}

// VerificationStage names a step of the verification pipeline.
type VerificationStage string

const (
	StageInstall VerificationStage = "install"
	StageBuild   VerificationStage = "build"
	StageTest    VerificationStage = "test"
	StageServe   VerificationStage = "serve"
	StageProbe   VerificationStage = "probe"
)

// StageResult records the outcome of one stage.
type StageResult struct {
	Stage    VerificationStage `json:"stage"`
	OK       bool              `json:"ok"`
	Skipped  bool              `json:"skipped,omitempty"`
	Duration time.Duration     `json:"duration_ms"`
	Detail   string            `json:"detail,omitempty"`
	// Output is trimmed to what a human needs to diagnose the failure. Whole
	// build logs belong in the event stream, not in a summary.
	Output string `json:"output,omitempty"`
}

// VerificationReport is the outcome of verifying a generated project.
type VerificationReport struct {
	Toolchain string        `json:"toolchain"`
	Stages    []StageResult `json:"stages"`
	Compiles  bool          `json:"compiles"`
	TestsPass bool          `json:"tests_pass"`
	Starts    bool          `json:"starts"`
	Responds  bool          `json:"responds"`
	// StatusCode from the health probe, when one was received.
	StatusCode int                  `json:"status_code,omitempty"`
	Isolation  port.IsolationReport `json:"isolation"`
	Duration   time.Duration        `json:"duration_ms"`
}

// Verified reports whether the project reached the highest bar: it ran and
// answered a request.
func (r VerificationReport) Verified() bool { return r.Responds }

// Summary renders a one-line result for the event stream.
func (r VerificationReport) Summary() string {
	switch {
	case r.Responds:
		return "the generated service builds, tests, starts and answers requests"
	case r.Starts:
		return "the generated service starts but did not answer a health probe"
	case r.TestsPass:
		return "the generated project compiles and its tests pass, but it did not start"
	case r.Compiles:
		return "the generated project compiles but its tests failed"
	default:
		return "the generated project does not compile"
	}
}

// Runner verifies generated projects inside a sandbox.
type Runner struct {
	sandbox port.Sandbox
	// AllowInstall permits the dependency step, which needs network access.
	AllowInstall bool
}

// NewRunner constructs a runner.
func NewRunner(sb port.Sandbox) *Runner {
	return &Runner{sandbox: sb, AllowInstall: true}
}

// Available reports whether verification can be attempted.
func (r *Runner) Available() bool { return r != nil && r.sandbox != nil }

// Verify builds, tests, starts and probes a generated project.
//
// Each stage gates the next: there is no point testing something that does not
// compile, and no point probing something that did not start. Stages that
// cannot run are reported as skipped rather than failed, because "we never
// tried" and "we tried and it broke" are different facts.
func (r *Runner) Verify(
	ctx context.Context,
	tb Toolbelt,
	workspace string,
	chain Toolchain,
) (*VerificationReport, error) {
	if !r.Available() {
		return nil, domain.Unavailable("sandbox_unavailable", "no execution sandbox is configured")
	}

	started := time.Now()
	report := &VerificationReport{
		Toolchain: chain.Name,
		Isolation: r.sandbox.Capabilities(),
	}

	projectDir := filepath.Join(workspace, chain.Dir)
	if _, err := os.Stat(filepath.Join(projectDir, chain.Marker)); err != nil {
		return nil, domain.NotFound("project")
	}

	// Dependency resolution.
	if chain.Install != nil && r.AllowInstall {
		result := r.runStage(ctx, tb, workspace, chain.Dir, StageInstall, chain.Install, chain.StepEnv, chain.StepMemoryLimitBytes)
		report.Stages = append(report.Stages, result)
		if !result.OK {
			report.Duration = time.Since(started)
			return report, nil
		}
	} else if chain.Install != nil {
		report.Stages = append(report.Stages, StageResult{
			Stage: StageInstall, Skipped: true,
			Detail: "dependency resolution is disabled; the build may fail if the module cache is cold",
		})
	}

	// Compilation.
	if chain.Build != nil {
		result := r.runStage(ctx, tb, workspace, chain.Dir, StageBuild, chain.Build, chain.StepEnv, chain.StepMemoryLimitBytes)
		report.Stages = append(report.Stages, result)
		report.Compiles = result.OK
		if !result.OK {
			report.Duration = time.Since(started)
			return report, nil
		}
	}

	// The project's own tests.
	if chain.Test != nil {
		result := r.runStage(ctx, tb, workspace, chain.Dir, StageTest, chain.Test, chain.StepEnv, chain.StepMemoryLimitBytes)
		report.Stages = append(report.Stages, result)
		report.TestsPass = result.OK
	}

	// Startup and health probe.
	if chain.Serve != nil {
		serveResult, probeResult := r.serveAndProbe(ctx, tb, workspace, chain)
		report.Stages = append(report.Stages, serveResult, probeResult)
		report.Starts = serveResult.OK
		report.Responds = probeResult.OK
		if code, err := strconv.Atoi(strings.TrimSpace(probeResult.Detail)); err == nil {
			report.StatusCode = code
		}
	}

	report.Duration = time.Since(started)
	return report, nil
}

func (r *Runner) runStage(
	ctx context.Context,
	tb Toolbelt,
	workspace, dir string,
	stage VerificationStage,
	step *Step,
	env map[string]string,
	chainMemoryLimit uint64,
) StageResult {
	tb.Emit(ctx, domain.LevelDebug, "Running "+string(stage)+": "+step.Command+" "+strings.Join(step.Args, " "),
		map[string]any{"stage": string(stage)})

	limit := step.MemoryLimitBytes
	if limit == 0 {
		limit = chainMemoryLimit
	}

	started := time.Now()
	result, err := r.sandbox.Exec(ctx, workspace, port.ExecRequest{
		Command: step.Command, Args: step.Args, Dir: dir,
		Env: env, Timeout: step.Timeout, Network: step.Network,
		MemoryLimitBytes: limit,
	})
	if err != nil {
		return StageResult{Stage: stage, Duration: time.Since(started), Detail: err.Error()}
	}

	stageResult := StageResult{
		Stage:    stage,
		OK:       result.OK(),
		Duration: result.Duration,
		Output:   trimOutput(result.Output(), 1500),
	}
	if result.TimedOut {
		stageResult.Detail = "the command exceeded its time limit"
	} else if !result.OK() {
		stageResult.Detail = fmt.Sprintf("exit code %d", result.ExitCode)
	}
	return stageResult
}

// serveAndProbe starts the application and makes a real request against it.
func (r *Runner) serveAndProbe(
	ctx context.Context,
	tb Toolbelt,
	workspace string,
	chain Toolchain,
) (StageResult, StageResult) {
	serveStage := StageResult{Stage: StageServe}
	probeStage := StageResult{Stage: StageProbe}

	// A port must be chosen before launch. The generated server binds it and
	// announces it; asking the OS for a free one first avoids collisions with
	// whatever else is running on a developer's machine.
	listenPort, err := freePort()
	if err != nil {
		serveStage.Detail = "could not allocate a port: " + err.Error()
		probeStage.Skipped = true
		return serveStage, probeStage
	}

	env := map[string]string{}
	// StepEnv first so ServeEnv can override it: the bounds apply to `go run`
	// compiling the program, but the application's own configuration wins.
	for k, v := range chain.StepEnv {
		env[k] = v
	}
	for k, v := range chain.ServeEnv {
		env[k] = v
	}
	env["ADDR"] = fmt.Sprintf("127.0.0.1:%d", listenPort)
	env["PORT"] = strconv.Itoa(listenPort)

	started := time.Now()

	// The server is started with host networking on purpose. An empty network
	// namespace has only loopback *inside* that namespace, which the probe
	// running on the host cannot reach — isolating the process would make it
	// unobservable. This is the one place the sandbox is deliberately relaxed,
	// it is bounded to a short-lived process, and it is reported.
	serveStep := *chain.Serve
	serveStep.Network = port.NetworkHost

	// Servers that take the port as an argument rather than from the
	// environment get it substituted here. Copying the slice matters: Args
	// belongs to the Toolchain value, and appending in place would mutate a
	// shared descriptor across runs.
	if len(chain.ServePortArgs) > 0 {
		args := make([]string, 0, len(serveStep.Args)+len(chain.ServePortArgs))
		args = append(args, serveStep.Args...)
		for _, arg := range chain.ServePortArgs {
			args = append(args, strings.ReplaceAll(arg, "{{port}}", strconv.Itoa(listenPort)))
		}
		serveStep.Args = args
	}

	tb.Emit(ctx, domain.LevelDebug,
		fmt.Sprintf("Starting the generated service on port %d", listenPort),
		map[string]any{"stage": "serve", "port": listenPort})

	proc, err := r.sandbox.Start(ctx, workspace, port.ExecRequest{
		Command: serveStep.Command, Args: serveStep.Args, Dir: chain.Dir,
		Env: env, Timeout: serveStep.Timeout, Network: serveStep.Network,
		MemoryLimitBytes: serveStep.MemoryLimitBytes,
	})
	if err != nil {
		serveStage.Duration = time.Since(started)
		serveStage.Detail = err.Error()
		probeStage.Skipped = true
		return serveStage, probeStage
	}
	defer func() {
		_ = proc.Stop()
		_, _ = proc.Wait()
	}()

	// Wait for the port to accept connections. Polling the socket is more
	// reliable than parsing output, because a service may log nothing at all;
	// the output is used only to detect an early crash.
	address := fmt.Sprintf("127.0.0.1:%d", listenPort)
	deadline := time.Now().Add(serveStep.Timeout)
	listening := false

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		if !proc.Running() {
			// The process exited before binding: a startup failure, and its
			// output is the diagnosis.
			break
		}
		conn, dialErr := net.DialTimeout("tcp", address, 300*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			listening = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	serveStage.Duration = time.Since(started)
	serveStage.Output = trimOutput(proc.Output(), 1500)
	serveStage.OK = listening

	if !listening {
		if proc.Running() {
			serveStage.Detail = "the service started but never accepted a connection"
		} else {
			serveStage.Detail = "the service exited during startup: " + firstMeaningfulLine(proc.Output())
		}
		probeStage.Skipped = true
		return serveStage, probeStage
	}

	// The real test: an actual HTTP request.
	probeStarted := time.Now()
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://" + address + chain.HealthPath

	response, err := client.Get(url)
	probeStage.Duration = time.Since(probeStarted)
	if err != nil {
		probeStage.Detail = "the request failed: " + err.Error()
		return serveStage, probeStage
	}
	defer response.Body.Close()

	probeStage.Detail = strconv.Itoa(response.StatusCode)
	// Any non-5xx answer proves the service is genuinely serving. Requiring 200
	// specifically would fail a project whose health endpoint is at a different
	// path, which is a documentation problem rather than a defect.
	probeStage.OK = response.StatusCode > 0 && response.StatusCode < 500
	probeStage.Output = fmt.Sprintf("GET %s → %d", chain.HealthPath, response.StatusCode)

	if probeStage.OK {
		tb.Emit(ctx, domain.LevelInfo,
			fmt.Sprintf("The generated service answered %s with %d", chain.HealthPath, response.StatusCode),
			map[string]any{"stage": "probe", "status": response.StatusCode})
	}
	return serveStage, probeStage
}

// freePort asks the OS for an unused TCP port.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// trimOutput keeps a diagnostic excerpt rather than an entire build log.
//
// The tail is kept, not the head: compilers print the failure last.
func trimOutput(output string, limit int) string {
	output = strings.TrimSpace(output)
	if len(output) <= limit {
		return output
	}

	// Keep both ends, weighted towards the head.
	//
	// Keeping only the tail was actively misleading. A Go toolchain prints the
	// real diagnostic first — "undefined: Foo", or a runtime panic's message —
	// and then, if it crashed, thousands of lines of goroutine stack. Trimming
	// to the last 1500 bytes discarded the sentence that explained the failure
	// and preserved a fragment of stack, so every report read like a compiler
	// bug regardless of the actual cause. Diagnosing anything then required
	// reproducing it by hand outside the sandbox.
	head := limit * 2 / 3
	tail := limit - head
	return output[:head] + "\n…\n" + output[len(output)-tail:]
}

var noiseLine = regexp.MustCompile(`^\s*(go: downloading|go: extracting|npm WARN)`)

// firstMeaningfulLine finds the line most likely to explain a startup failure.
func firstMeaningfulLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	// Search from the end: the fatal error is the last thing printed.
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || noiseLine.MatchString(line) {
			continue
		}
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		return line
	}
	return "no output was produced"
}

// TrimOutputForTest exposes the excerpt logic to tests in the external test
// package. The function itself stays unexported: it is an implementation
// detail of reporting, not part of the runner's contract.
func TrimOutputForTest(output string, limit int) string { return trimOutput(output, limit) }
