package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// The self-healing loop.
//
// This is the capability the previous five versions were building toward, and
// it only became possible once each piece existed: the runner reports precisely
// what failed, the patch engine applies a minimal fix atomically, and git makes
// every attempt reversible.
//
// Four principles govern it, each chosen because its opposite is a known way
// for automated repair to make things worse:
//
//	minimal diff       — patch the failure, never regenerate the file. A rewrite
//	                     discards working code to fix one line.
//	monotonic progress — an attempt that increases the failure count is reverted.
//	                     Without this, a model "fixing" one error while creating
//	                     two walks the project steadily downhill.
//	bounded            — a fixed attempt budget. A model that cannot fix an error
//	                     in three tries will not fix it in thirty, and an
//	                     unbounded loop is how a defect becomes an unbounded spend.
//	learned            — every successful fix is written to memory keyed by the
//	                     error signature, so the same failure is cheaper next time.

// Diagnosis classifies a verification failure.
type Diagnosis struct {
	// Stage that failed.
	Stage VerificationStage
	// Category of the failure, used to select a repair strategy and as the
	// memory key.
	Category string
	// Signature is a normalised fingerprint of the error, stable across runs
	// and projects so a lesson learned once applies again.
	Signature string
	// Files the error points at, most specific first.
	Files []string
	// Message is the salient error text.
	Message string
	// Details are the individual compiler or test errors.
	Details []string
	// Repairable reports whether healing should be attempted at all.
	Repairable bool
}

// HealAttempt records one repair cycle.
type HealAttempt struct {
	Number   int           `json:"number"`
	Category string        `json:"category"`
	Patched  []string      `json:"patched,omitempty"`
	Improved bool          `json:"improved"`
	Reverted bool          `json:"reverted,omitempty"`
	Duration time.Duration `json:"duration_ms"`
	Error    string        `json:"error,omitempty"`
}

// HealReport summarises a healing session.
type HealReport struct {
	Attempts []HealAttempt `json:"attempts"`
	Healed   bool          `json:"healed"`
	// Initial and Final describe the verification state before and after.
	Initial string `json:"initial"`
	Final   string `json:"final"`
}

// Summary renders a one-line outcome.
func (r HealReport) Summary() string {
	switch {
	case r.Healed:
		return fmt.Sprintf("repaired after %d attempt(s): %s", len(r.Attempts), r.Final)
	case len(r.Attempts) == 0:
		return "nothing to repair"
	default:
		return fmt.Sprintf("could not repair after %d attempt(s): %s", len(r.Attempts), r.Final)
	}
}

// goCompileError matches the standard Go compiler diagnostic format.
var goCompileError = regexp.MustCompile(`^(?:\s*)([\w./\-]+\.go):(\d+):(?:(\d+):)?\s+(.+)$`)

// goTestFailure matches a failing test.
var goTestFailure = regexp.MustCompile(`^\s*--- FAIL: (\w+)`)

// Diagnose classifies a verification report into an actionable problem.
//
// Classification drives everything downstream: which files to show the model,
// what to ask it for, and whether to attempt repair at all. Getting this wrong
// wastes an expensive inference call on a problem no patch can fix.
func Diagnose(report *VerificationReport) *Diagnosis {
	if report == nil || report.Verified() {
		return nil
	}

	for _, stage := range report.Stages {
		if stage.OK || stage.Skipped {
			continue
		}

		switch stage.Stage {
		case StageInstall:
			// Dependency resolution failures are environmental — a proxy
			// outage, a missing module. A model cannot fix them by editing
			// source, and pretending otherwise burns budget.
			return &Diagnosis{
				Stage: stage.Stage, Category: "dependencies", Repairable: false,
				Message:   "dependency resolution failed",
				Signature: "deps:" + normaliseSignature(stage.Detail),
			}

		case StageBuild:
			return diagnoseCompilation(stage)

		case StageTest:
			return diagnoseTests(stage)

		case StageServe:
			return &Diagnosis{
				Stage: stage.Stage, Category: "startup", Repairable: true,
				Message:   "the service failed to start: " + stage.Detail,
				Details:   meaningfulLines(stage.Output, 5),
				Signature: "startup:" + normaliseSignature(stage.Detail),
			}

		case StageProbe:
			return &Diagnosis{
				Stage: stage.Stage, Category: "health", Repairable: true,
				Message:   "the service started but did not answer a health probe",
				Details:   meaningfulLines(stage.Output, 5),
				Signature: "health:" + normaliseSignature(stage.Detail),
			}
		}
	}
	return nil
}

func diagnoseCompilation(stage StageResult) *Diagnosis {
	diagnosis := &Diagnosis{
		Stage: stage.Stage, Category: "compile", Repairable: true,
		Message: "the project does not compile",
	}

	seen := map[string]bool{}
	for _, line := range strings.Split(stage.Output, "\n") {
		match := goCompileError.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		file, lineNo, text := match[1], match[2], match[4]

		if !seen[file] {
			seen[file] = true
			diagnosis.Files = append(diagnosis.Files, file)
		}
		diagnosis.Details = append(diagnosis.Details, fmt.Sprintf("%s:%s: %s", file, lineNo, text))
	}

	if len(diagnosis.Details) == 0 {
		// The compiler said something we do not recognise; keep the raw tail so
		// the model still has the actual error rather than a guess.
		diagnosis.Details = meaningfulLines(stage.Output, 5)
	}
	// The signature ignores line numbers and identifiers so the same class of
	// error matches across projects.
	diagnosis.Signature = "compile:" + normaliseSignature(strings.Join(diagnosis.Details, " "))
	return diagnosis
}

func diagnoseTests(stage StageResult) *Diagnosis {
	diagnosis := &Diagnosis{
		Stage: stage.Stage, Category: "test", Repairable: true,
		Message: "the generated tests fail",
	}

	for _, line := range strings.Split(stage.Output, "\n") {
		if match := goTestFailure.FindStringSubmatch(line); match != nil {
			diagnosis.Details = append(diagnosis.Details, "failing test: "+match[1])
			continue
		}
		// Test failures report the file through the same compiler-style prefix.
		if match := goCompileError.FindStringSubmatch(line); match != nil {
			file := match[1]
			if !containsString(diagnosis.Files, file) {
				diagnosis.Files = append(diagnosis.Files, file)
			}
			diagnosis.Details = append(diagnosis.Details, strings.TrimSpace(line))
		}
	}
	if len(diagnosis.Details) == 0 {
		diagnosis.Details = meaningfulLines(stage.Output, 6)
	}
	diagnosis.Signature = "test:" + normaliseSignature(strings.Join(diagnosis.Details, " "))
	return diagnosis
}

// signatureNoise strips the parts of an error that vary between occurrences of
// the same underlying problem.
var (
	numberRun  = regexp.MustCompile(`\d+`)
	quotedText = regexp.MustCompile(`"[^"]*"`)
	pathRun    = regexp.MustCompile(`[\w./\-]+\.go`)
	spaceRun   = regexp.MustCompile(`\s+`)
)

// normaliseSignature produces a stable fingerprint for an error.
//
// Line numbers, quoted identifiers and file paths all change between projects
// while the underlying mistake is identical. Stripping them is what lets a
// lesson learned on one build apply to the next.
func normaliseSignature(text string) string {
	text = strings.ToLower(text)
	text = pathRun.ReplaceAllString(text, "F")
	text = quotedText.ReplaceAllString(text, "Q")
	text = numberRun.ReplaceAllString(text, "N")
	text = spaceRun.ReplaceAllString(text, " ")
	text = strings.TrimSpace(text)
	if len(text) > 160 {
		text = text[:160]
	}
	return text
}

// healSchema constrains a repair proposal.
//
// The model returns anchored edits, not whole files. That is the minimal-diff
// principle expressed in the schema itself: there is no field in which to
// return a rewritten file.
var healSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["diagnosis", "edits"],
  "properties": {
    "diagnosis": {"type": "string", "minLength": 15, "maxLength": 400},
    "edits": {
      "type": "array", "minItems": 1, "maxItems": 5,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["path", "find", "replace"],
        "properties": {
          "path": {"type": "string", "minLength": 3, "maxLength": 200},
          "find": {"type": "string", "minLength": 3, "maxLength": 2000},
          "replace": {"type": "string", "maxLength": 2000},
          "why": {"type": "string", "maxLength": 200}
        }
      }
    }
  }
}`)

// Healer repairs a project that fails verification.
type Healer struct {
	runner    *Runner
	reasoning *Reasoning
	// MaxAttempts bounds the loop.
	MaxAttempts int
}

// NewHealer constructs a healer.
func NewHealer(runner *Runner, reasoning *Reasoning, maxAttempts int) *Healer {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return &Healer{runner: runner, reasoning: reasoning, MaxAttempts: maxAttempts}
}

// Available reports whether healing can be attempted.
func (h *Healer) Available() bool {
	return h != nil && h.runner != nil && h.runner.Available() && h.reasoning.Enabled()
}

// Heal repairs a failing project, returning what happened.
func (h *Healer) Heal(
	ctx context.Context,
	tb *WorkspaceToolbelt,
	bb *Blackboard,
	initial *VerificationReport,
) *HealReport {
	report := &HealReport{Initial: initial.Summary(), Final: initial.Summary()}

	diagnosis := Diagnose(initial)
	if diagnosis == nil {
		return report
	}
	if !diagnosis.Repairable {
		tb.Emit(ctx, domain.LevelWarn,
			"The failure is not repairable by editing code: "+diagnosis.Message, nil)
		return report
	}
	if !h.Available() {
		tb.Emit(ctx, domain.LevelInfo,
			"Verification failed and no reasoning model is available to repair it: "+diagnosis.Message, nil)
		return report
	}

	current := initial
	previousFailures := failureCount(initial)

	for attempt := 1; attempt <= h.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			break
		}

		record := HealAttempt{Number: attempt, Category: diagnosis.Category}
		started := time.Now()

		tb.Emit(ctx, domain.LevelInfo,
			fmt.Sprintf("Repair attempt %d of %d: %s", attempt, h.MaxAttempts, diagnosis.Message),
			map[string]any{"attempt": attempt, "category": diagnosis.Category})

		// Snapshot before changing anything so a worse outcome can be undone.
		before := h.captureFiles(ctx, tb, diagnosis.Files)

		patch, err := h.proposePatch(ctx, tb, bb, diagnosis)
		if err != nil || patch == nil || len(patch.Edits) == 0 {
			record.Error = "the model proposed no usable repair"
			if err != nil {
				record.Error = err.Error()
			}
			record.Duration = time.Since(started)
			report.Attempts = append(report.Attempts, record)
			break
		}

		result, err := tb.ApplyPatch(ctx, *patch)
		if err != nil || !result.OK() {
			reasons := make([]string, 0, len(result.Failures))
			for _, failure := range result.Failures {
				reasons = append(reasons, failure.Error())
			}
			record.Error = "the repair did not apply: " + strings.Join(reasons, "; ")
			record.Duration = time.Since(started)
			report.Attempts = append(report.Attempts, record)

			// A patch that would not apply teaches the next attempt what the
			// file actually contains.
			diagnosis.Details = append(diagnosis.Details, "previous repair failed: "+record.Error)
			continue
		}
		record.Patched = append(result.Applied, result.Created...)

		// Re-verify. This is the only evidence that matters.
		next, err := h.runner.Verify(ctx, tb, bb.Project.WorkspacePath, GoToolchain())
		if err != nil {
			record.Error = "re-verification failed: " + err.Error()
			record.Duration = time.Since(started)
			report.Attempts = append(report.Attempts, record)
			break
		}

		failures := failureCount(next)
		record.Improved = failures < previousFailures

		if next.Verified() {
			record.Duration = time.Since(started)
			report.Attempts = append(report.Attempts, record)
			report.Healed = true
			report.Final = next.Summary()

			tb.Emit(ctx, domain.LevelInfo,
				fmt.Sprintf("Repaired after %d attempt(s): %s", attempt, next.Summary()),
				map[string]any{"attempts": attempt, "patched": record.Patched})

			// Record the lesson so the same error is cheaper next time.
			h.remember(ctx, bb, diagnosis, patch)
			return report
		}

		if !record.Improved {
			// Monotonic progress: an attempt that did not reduce the failure
			// count is reverted rather than compounded.
			record.Reverted = true
			h.restoreFiles(ctx, tb, before)
			tb.Emit(ctx, domain.LevelWarn,
				fmt.Sprintf("Repair attempt %d made no progress and was reverted", attempt), nil)
		} else {
			current = next
			previousFailures = failures
			// The next attempt works from the new, closer state.
			if updated := Diagnose(next); updated != nil {
				diagnosis = updated
			}
		}

		record.Duration = time.Since(started)
		report.Attempts = append(report.Attempts, record)
	}

	report.Final = current.Summary()
	tb.Emit(ctx, domain.LevelWarn,
		fmt.Sprintf("Could not repair after %d attempt(s). %s", len(report.Attempts), report.Final),
		map[string]any{"attempts": len(report.Attempts), "category": diagnosis.Category})
	return report
}

// proposePatch asks the model for a minimal repair.
func (h *Healer) proposePatch(
	ctx context.Context,
	tb Toolbelt,
	bb *Blackboard,
	diagnosis *Diagnosis,
) (*Patch, error) {
	// Show the model the files the error points at, not the whole project. A
	// focused context produces a focused fix and fits a small context window.
	var sources strings.Builder
	for _, file := range diagnosis.Files {
		content, err := tb.ReadFile(ctx, "api/"+file)
		if err != nil {
			content, err = tb.ReadFile(ctx, file)
			if err != nil {
				continue
			}
		}
		if len(content) > 6000 {
			content = content[:6000] + "\n… (truncated)"
		}
		fmt.Fprintf(&sources, "### %s\n```go\n%s\n```\n\n", file, content)
	}

	lessons := memoryContext(h.reasoning.recall(ctx, bb.Project.ID, diagnosis.Signature, 3))

	prompt := NewPrompt(h.reasoning.PromptBudget(ctx)).
		Add("The failure", diagnosis.Message+"\n\n"+strings.Join(diagnosis.Details, "\n"), 0).
		Add("Source files", sources.String(), 1).
		Add("Previous lessons", lessons, 3).
		Add("Your task", `Fix this failure with the smallest possible change.

- Return anchored edits: the exact text to find, and what to replace it with.
- The "find" text must appear EXACTLY ONCE in the file, character for character,
  including whitespace. Copy it from the source above; do not retype it.
- Change only what is needed to fix the error. Do not reformat, rename or
  refactor anything else.
- Do not rewrite whole files or whole functions unless the function is the error.`, 0)

	document := h.reasoning.think(ctx, tb, domain.RoleQA, "a repair",
		houseStyle+"\n\nYou are a senior Go engineer fixing a specific compilation or test failure. You make the smallest change that works.",
		prompt.String(), "repair", healSchema, port.ClassCode, 0.1)
	if document == nil {
		return nil, fmt.Errorf("the model produced no repair")
	}

	patch := &Patch{Title: "fix: " + stringField(document, "diagnosis"), Author: "Sentry"}
	for _, raw := range objectSlice(document, "edits") {
		path := strings.TrimSpace(stringField(raw, "path"))
		find := stringField(raw, "find")
		if path == "" || find == "" {
			continue
		}
		// The model reports paths relative to the module; the workspace root is
		// one level up.
		if !strings.HasPrefix(path, "api/") && !strings.HasPrefix(path, "web/") {
			path = "api/" + strings.TrimPrefix(path, "./")
		}
		patch.Edits = append(patch.Edits, FileEdit{
			Path: path, Kind: EditModify, Reason: stringField(raw, "why"),
			Hunks: []Hunk{{Find: find, Replace: stringField(raw, "replace"), Context: stringField(raw, "why")}},
		})
	}
	if len(patch.Edits) == 0 {
		return nil, fmt.Errorf("the repair contained no usable edits")
	}
	return patch, nil
}

// remember stores a successful fix keyed by the error signature.
func (h *Healer) remember(ctx context.Context, bb *Blackboard, diagnosis *Diagnosis, patch *Patch) {
	if h.reasoning == nil || h.reasoning.memory == nil {
		return
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "Error: %s\n", diagnosis.Message)
	for _, edit := range patch.Edits {
		if edit.Reason != "" {
			fmt.Fprintf(&summary, "Fix in %s: %s\n", edit.Path, edit.Reason)
		}
	}
	_, _ = h.reasoning.memory.Remember(ctx, port.Memory{
		Scope: port.ScopeGlobal, Kind: port.KindLesson,
		Title:      "Repair for " + diagnosis.Category + " failure",
		Content:    summary.String(),
		Metadata:   map[string]any{"signature": diagnosis.Signature},
		Importance: 0.8,
	})
}

// captureFiles snapshots content so a failed attempt can be undone.
func (h *Healer) captureFiles(ctx context.Context, tb *WorkspaceToolbelt, files []string) map[string]string {
	snapshot := map[string]string{}
	for _, file := range files {
		for _, candidate := range []string{"api/" + file, file} {
			if content, err := tb.ReadFile(ctx, candidate); err == nil {
				snapshot[candidate] = content
				break
			}
		}
	}
	return snapshot
}

func (h *Healer) restoreFiles(ctx context.Context, tb *WorkspaceToolbelt, snapshot map[string]string) {
	for path, content := range snapshot {
		_ = tb.WriteFile(ctx, path, content)
	}
}

// failureCount measures how far a report is from success, so progress can be
// compared between attempts.
func failureCount(report *VerificationReport) int {
	if report == nil {
		return 99
	}
	count := 0
	for _, stage := range report.Stages {
		if !stage.OK && !stage.Skipped {
			count++
			// Weight by how much output the failure produced, which correlates
			// with the number of distinct errors.
			count += strings.Count(stage.Output, "\n") / 20
		}
	}
	// A report that never reached later stages is worse than one that did.
	if !report.Compiles {
		count += 8
	}
	if !report.TestsPass {
		count += 4
	}
	if !report.Starts {
		count += 2
	}
	if !report.Responds {
		count++
	}
	return count
}

// meaningfulLines extracts the most informative lines from command output.
func meaningfulLines(output string, limit int) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "go: downloading") {
			continue
		}
		lines = append(lines, trimmed)
	}
	// The tail holds the actual error; compilers print context first.
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
}

func containsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
