package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// codexSubagentProviderName is the conventional external provider name used
// when a config entry doesn't otherwise imply one
// (docs/hufu-external-coding-agent-runtime-spec.md §12).
const codexSubagentProviderName = "codex"

// codexAppServerProviderType is the only agent.SubagentProviderConfig.Type
// this phase recognizes (§6.1's example config). A configured provider of
// any other type is never registered (see Coordinator.SubagentRegistry).
const codexAppServerProviderType = "codex-app-server"

const (
	codexDefaultStartupTimeout = 15 * time.Second
	codexDefaultInterruptGrace = 3 * time.Second
	codexDefaultShutdownGrace  = 3 * time.Second
	codexDefaultMaxTranscript  = 16 << 20
	// codexSandboxReadOnly/codexSandboxWorkspaceWrite are the only two
	// sandbox modes this provider ever requests (§10.5's mapping never
	// produces a third); danger-full-access (INV-08) is structurally
	// unreachable, not merely avoided by convention.
	codexSandboxReadOnly       = "read-only"
	codexSandboxWorkspaceWrite = "workspace-write"
)

// codexResultRepairPrompt/codexResultRepairInstruction implement §23's
// "schema-only result repair prompt": no new work is requested, only a
// structured account of the turn that already ran. The instruction spells
// out that the workspace is already frozen, so an app-server that
// nonetheless attempts a write gains nothing — canonicalization never
// re-observes the workspace after this point (see attemptResultRepair).
const (
	codexResultRepairPrompt      = "Report the outcome of your previous turn as a structured result. Do not perform any further work."
	codexResultRepairInstruction = "This is a read-only repair turn. The workspace state from your previous turn is already frozen and will not be re-observed, so any further file changes will not be recorded or credited. Do not modify any files. Respond only with a WorkerResultProposal matching the required schema, describing the final status of the work you already attempted."
)

// CodexProviderFailureClass classifies a RunAttempt failure so recovery
// policy can distinguish them (spec.md §33). This is a deliberately small
// subset of the full taxonomy §33 names — only the classes this phase's
// control flow actually produces; the rest (provider_sandbox_mismatch,
// provider_model_mismatch, provider_resume_failed, ...) already exist as
// distinguishable plain errors from codex_appserver_protocol.go and can gain
// their own typed class later without changing this type.
type CodexProviderFailureClass string

const (
	CodexFailureUnavailable        CodexProviderFailureClass = "provider_unavailable"
	CodexFailureProcessCrash       CodexProviderFailureClass = "provider_process_crash"
	CodexFailureCancelled          CodexProviderFailureClass = "provider_cancelled"
	CodexFailureTimeout            CodexProviderFailureClass = "provider_timeout"
	CodexFailureProtocolError      CodexProviderFailureClass = "provider_protocol_error"
	CodexFailureWorkspaceViolation CodexProviderFailureClass = "provider_workspace_violation"
)

// CodexProviderError wraps a RunAttempt failure with its classification.
type CodexProviderError struct {
	Class CodexProviderFailureClass
	Err   error
}

func (e *CodexProviderError) Error() string {
	if e.Err == nil {
		return string(e.Class)
	}
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

func (e *CodexProviderError) Unwrap() error { return e.Err }

func codexFail(class CodexProviderFailureClass, err error) error {
	return &CodexProviderError{Class: class, Err: err}
}

// CodexSubagentProvider is a "codex-app-server" SubagentProvider
// (docs/hufu-external-coding-agent-runtime-spec.md §12, PR-11). It executes
// exactly one already-authorized attempt through one app-server process
// (§13.1) and reports evidence; Hufu retains retry, recovery, verification,
// receipts, completion, and memory policy — this provider never decides any
// of those (mirrors HufuLocalSubagentProvider's own contract note).
//
// name is the team.yaml `subagent-providers` map key this instance was
// configured under (conventionally "codex"), not a hardcoded constant: the
// registry key comes from the provider's own Name(), so a differently-named
// config entry must produce a provider registered under that same name.
type CodexSubagentProvider struct {
	coordinator *Coordinator
	name        string
	config      agent.SubagentProviderConfig
	preflight   *codexPreflightCache
}

// NewCodexSubagentProvider builds a codex-app-server provider from its
// parsed team config (§6.1), registered under name. Construction performs no
// process/model call — only structural readiness for later per-attempt
// preflight (§25, §27).
func NewCodexSubagentProvider(c *Coordinator, name string, config agent.SubagentProviderConfig) *CodexSubagentProvider {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = codexSubagentProviderName
	}
	return &CodexSubagentProvider{coordinator: c, name: name, config: config, preflight: &codexPreflightCache{}}
}

// codexPreflightCache caches §25's provider-level preflight result across
// RunAttempt calls — "cacheable by provider binary/version but MUST be
// invalidated when executable identity changes". It is keyed by the
// resolved executable's absolute path plus its size and modification time,
// so a rebuilt or replaced binary at the same configured path automatically
// invalidates the cache without needing an explicit version string to
// track.
//
// This deliberately only covers the checks that need no subprocess: command
// executable exists, execution world is supported, workspace root is valid,
// and (when CODEX_HOME is inherited) it looks authenticated. §25 also lists
// "app-server initialize works", "required protocol methods available",
// "process-tree cleanup available", and §26's per-method capability checks
// (thread/start accepted, turn/interrupt accepted or advertised, ...) —
// these are NOT independently re-verified here via a second, throwaway
// process. The real app-server has no side-effect-free capability-
// introspection RPC (confirmed live against codex-cli 0.153.4, 2026-09-08 —
// see §26's "Corrected" note and §13.3), so a dedicated preflight probe
// could only ever repeat the exact same initialize handshake RunAttempt's
// own StartCodexAppServer call already performs — a call that already
// fails closed, before any workspace side effect, if the binary or
// protocol is actually broken. A second probe would only double the
// process-launch cost without adding a real safety property.
type codexPreflightCache struct {
	mu    sync.Mutex
	key   string
	err   error
	valid bool
}

// run executes (or reuses a cached result of) p's cheap preflight checks.
func (c *codexPreflightCache) run(p *CodexSubagentProvider) error {
	key, keyErr := codexPreflightCacheKey(p.config.Command)
	c.mu.Lock()
	if c.valid && keyErr == nil && key == c.key {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	err := codexRunCheapPreflightChecks(p)
	if keyErr == nil {
		c.mu.Lock()
		c.key, c.err, c.valid = key, err, true
		c.mu.Unlock()
	}
	return err
}

// codexPreflightCacheKey resolves argv[0] to an absolute path and combines
// it with the resolved file's size/mtime, so replacing the binary at the
// same configured path is detected without any explicit version string.
func codexPreflightCacheKey(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("no command configured")
	}
	resolved, err := exec.LookPath(argv[0])
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%d", resolved, info.Size(), info.ModTime().UnixNano()), nil
}

// codexRunCheapPreflightChecks implements the local, no-subprocess-needed
// half of §25's preflight list. See codexPreflightCache's doc comment for
// which checks are deliberately not included and why.
func codexRunCheapPreflightChecks(p *CodexSubagentProvider) error {
	if len(p.config.Command) == 0 {
		return fmt.Errorf("codex provider has no configured command")
	}
	if _, err := exec.LookPath(p.config.Command[0]); err != nil {
		return fmt.Errorf("executable %q not found: %w", p.config.Command[0], err)
	}
	if p.config.ExecutionWorld != "" && p.config.ExecutionWorld != localExecutionWorldName {
		return fmt.Errorf("unsupported execution world %q", p.config.ExecutionWorld)
	}
	workspace := ""
	if p.coordinator != nil {
		workspace = p.coordinator.projectDir
	}
	if _, err := resolveSnapshotRoot(workspace); err != nil {
		return fmt.Errorf("workspace root invalid: %w", err)
	}
	if codexHomeInherited(p.config.InheritEnv) {
		if err := codexHomeLooksAuthenticated(); err != nil {
			return err
		}
	}
	return nil
}

func codexHomeInherited(inheritEnv []string) bool {
	for _, name := range inheritEnv {
		if strings.EqualFold(strings.TrimSpace(name), "CODEX_HOME") {
			return true
		}
	}
	return false
}

// codexHomeLooksAuthenticated is a best-effort heuristic, not a guarantee:
// Hufu cannot safely make a real authenticated API call during preflight —
// that would be a genuine side effect and cost, not a preflight check — so
// this only confirms CODEX_HOME points at a directory containing the auth
// file the real CLI writes on login. auth.json's content is never read or
// validated; that is exactly the kind of secret material Hufu must never
// touch.
//
// **Fixed 2026-09-08** (found running the real §38 smoke suite against a
// genuine account for the first time): an unset CODEX_HOME env var used to
// be treated as an outright preflight failure. That is wrong — the real
// codex CLI itself defaults to `$HOME/.codex` when CODEX_HOME is not set,
// and a team.yaml listing "CODEX_HOME" in inherit-env is declaring "let the
// child see whatever CODEX_HOME resolves to", not "CODEX_HOME must be
// explicitly exported". Every live smoke scenario failed preflight with
// "CODEX_HOME is not set" until this fallback was added to match the real
// CLI's own default resolution.
func codexHomeLooksAuthenticated() error {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("CODEX_HOME is not set and the default ($HOME/.codex) could not be resolved: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("CODEX_HOME %q is not a directory: %w", home, err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		return fmt.Errorf("CODEX_HOME %q has no auth.json (codex login required): %w", home, err)
	}
	return nil
}

func (p *CodexSubagentProvider) Name() string { return p.name }

func (p *CodexSubagentProvider) Capabilities() SubagentCapabilities {
	return SubagentCapabilities{
		SupportsHufuTools:   false,
		SupportsTypedResult: false, // canonical TaskResult is Hufu-only, §8.1
		SupportsActivities:  true,
		SupportsResumeToken: true,
	}
}

func (p *CodexSubagentProvider) durations() (startup, interruptGrace, shutdownGrace time.Duration) {
	startup = parseDurationOr(p.config.StartupTimeout, codexDefaultStartupTimeout)
	interruptGrace = parseDurationOr(p.config.InterruptGrace, codexDefaultInterruptGrace)
	shutdownGrace = parseDurationOr(p.config.ShutdownGrace, codexDefaultShutdownGrace)
	return
}

func parseDurationOr(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// transcript is a small, bounded, append-only record of this attempt's
// protocol milestones (§17.1: "JSON-RPC traffic and relevant provider
// activities SHOULD be persisted as an opaque, bounded artifact"). It
// intentionally records milestones rather than every raw frame — the
// scope this phase actually needs evidence for — and is written to a file
// under the workspace so a TranscriptRef survives the attempt.
type codexTranscript struct {
	lines    []string
	maxBytes int64
	bytes    int64
}

func newCodexTranscript(maxBytes int64) *codexTranscript {
	if maxBytes <= 0 {
		maxBytes = codexDefaultMaxTranscript
	}
	return &codexTranscript{maxBytes: maxBytes}
}

func (t *codexTranscript) record(format string, args ...any) {
	line := fmt.Sprintf("%s %s", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
	if t.bytes+int64(len(line)) > t.maxBytes {
		if len(t.lines) == 0 || t.lines[len(t.lines)-1] != "...[truncated]" {
			t.lines = append(t.lines, "...[truncated]")
		}
		return
	}
	t.lines = append(t.lines, line)
	t.bytes += int64(len(line))
}

func (t *codexTranscript) persist(workspace, taskID string, attempt int) (string, error) {
	dir := filepath.Join(workspace, logsDir, "codex-transcripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("codex transcript: create directory: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-attempt-%d-%d.log", taskID, attempt, time.Now().UnixNano()))
	content := strings.Join(t.lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("codex transcript: write: %w", err)
	}
	return path, nil
}

// RunAttempt implements §13.2/§14/§16's production flow for one attempt:
// prepare the execution world, start one app-server process, initialize,
// start-or-resume the thread (persisting the session binding before any
// turn depends on it), run exactly one turn, canonicalize the untrusted
// proposal against the actually-observed workspace delta, and always tear
// the process down — on every exit path, success or failure.
func (p *CodexSubagentProvider) RunAttempt(ctx context.Context, request AttemptRequest) (result AttemptResult, resultErr error) {
	if p == nil || p.coordinator == nil {
		return AttemptResult{}, fmt.Errorf("codex attempt requires a coordinator")
	}
	// §25: preflight failure MUST occur before the task enters provider
	// execution — this runs (or reuses a cached result) before any process,
	// workspace, or protocol activity below.
	preflight := p.preflight
	if preflight == nil {
		preflight = &codexPreflightCache{}
	}
	if err := preflight.run(p); err != nil {
		return AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("codex preflight: %w", err))
	}
	// §29: Hufu's ModelTopology (local extra-model fanout) is a separate
	// concept from Codex's own internal multi-agent behavior, and v1 has no
	// implementation routing one to the other. Silently running a single
	// Codex occurrence for a multi-leaf ModelTopology would misrepresent
	// Codex as having covered every leaf when it only ever saw one prompt;
	// fail closed instead of reinterpreting local fanout as something Codex
	// never agreed to do.
	if len(request.Task.ModelTopology) > 1 {
		return AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("codex provider does not support extra-model fanout (ModelTopology has %d entries); route this task through hufu-local or reduce it to a single model", len(request.Task.ModelTopology)))
	}

	transcript := newCodexTranscript(p.config.MaxTranscriptBytes)
	workspace := p.coordinator.projectDir
	transcript.record("attempt start task=%s attempt=%d model=%s", request.TaskID, request.Attempt, request.ModelID)

	finish := func(res AttemptResult, err error) (AttemptResult, error) {
		if ref, perr := transcript.persist(workspace, request.TaskID, request.Attempt); perr == nil {
			res.TranscriptRef = ref
		}
		return res, err
	}

	startupTimeout, interruptGrace, shutdownGrace := p.durations()
	world := NewLocalExecutionWorld()
	prepared, err := world.Prepare(ctx, ExecutionWorldSpec{
		RunID: request.RunID, TaskID: request.TaskID, Attempt: request.Attempt,
		Root: workspace, CWD: workspace, SideEffect: request.Task.SideEffect,
		EnvironmentAllowlist: p.config.InheritEnv,
	})
	if err != nil {
		transcript.record("execution world prepare failed: %v", err)
		return finish(AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("prepare execution world: %w", err)))
	}
	defer func() { _ = world.Release(context.Background(), prepared) }()

	sandbox := codexSandboxReadOnly
	if len(prepared.WritableRoots) > 0 {
		sandbox = codexSandboxWorkspaceWrite
	}

	startCtx := ctx
	var startCancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		startCtx, startCancel = context.WithTimeout(ctx, startupTimeout)
		defer startCancel()
	}
	proc, err := StartCodexAppServer(startCtx, NewProcessSupervisor(), CodexProcessConfig{
		Argv: p.config.Command, Dir: workspace, Env: prepared.Environment,
		MaxFrameBytes: int(p.config.MaxEventBytes), StartupTimeout: startupTimeout,
	})
	if err != nil {
		transcript.record("start app-server failed: %v", err)
		return finish(AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("start codex app-server: %w", err)))
	}
	sup := NewProcessSupervisor()
	processStopped := make(chan struct{})
	var stopOnce codexStopOnce
	stopProcess := func() {
		stopOnce.do(func() {
			p.stopCodexProcess(sup, proc, interruptGrace, shutdownGrace, transcript)
			close(processStopped)
		})
	}
	defer stopProcess()

	if request.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}

	existingThreadID := ""
	if request.ProviderBinding != nil {
		existingThreadID = request.ProviderBinding.SessionID
	}
	threadCfg := CodexThreadConfig{
		CWD: prepared.CWD, Model: request.ModelID, Sandbox: sandbox,
		DeveloperInstructions: "Work only inside the provided workspace. Produce the final response matching the supplied JSON schema. Do not claim verification or receipt authority.",
	}
	effective, err := codexStartOrResumeThread(ctx, proc.Client, existingThreadID, threadCfg, func(state CodexEffectiveThreadState) error {
		transcript.record("session bound thread_id=%s", state.ThreadID)
		return p.coordinator.persistProviderSessionBinding(context.Background(), request.TaskID, request.Attempt, ProviderBinding{
			Provider: p.name, Protocol: p.config.Protocol, SessionID: state.ThreadID,
			ExecutionWorldID: prepared.ID, CWD: state.CWD, EffectiveModel: state.Model, SandboxMode: state.Sandbox,
			ResumeSupported: true,
		})
	})
	if err != nil {
		transcript.record("thread start/resume failed: %v", err)
		return finish(AttemptResult{ProviderSessionID: existingThreadID}, p.classifyThreadError(ctx, err))
	}
	result.ProviderSessionID = effective.ThreadID

	// turnIDCh carries turn/start's synchronous ack (the real turn id,
	// required by turn/interrupt — the protocol has no "interrupt whatever
	// is active" shorthand) from the turn goroutine to the cancellation
	// branch below, which runs concurrently with it.
	turnIDCh := make(chan string, 1)
	onTurnStarted := func(id string) {
		select {
		case turnIDCh <- id:
		default:
		}
	}
	turnDone := make(chan struct{})
	var turnResult CodexTurnResult
	var turnErr error
	go func() {
		defer close(turnDone)
		turnResult, turnErr = codexRunTurn(ctx, proc.Client, effective.ThreadID, request.Prompt, onTurnStarted)
	}()

	select {
	case <-turnDone:
	case <-ctx.Done():
		transcript.record("attempt context done while turn in flight: %v", ctx.Err())
		class := CodexFailureCancelled
		if request.Timeout > 0 && ctx.Err() == context.DeadlineExceeded {
			class = CodexFailureTimeout
		}
		// Bounded by interruptGrace, not context.Background(): turn/interrupt
		// is a best-effort courtesy to the app-server, and an app-server that
		// never acks it (or a fake-server fixture with no scripted response)
		// must never block the cancellation path itself — stopProcess's own
		// interrupt-grace/terminate/kill escalation is what actually
		// guarantees forward progress. The same budget also covers the rare
		// race where cancellation arrives before turn/start's own ack: if no
		// turn id shows up in time, skip the (now impossible, since the real
		// protocol requires turnId) interrupt call and fall straight through
		// to process-tree escalation.
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), interruptGrace)
		var turnID string
		select {
		case turnID = <-turnIDCh:
		case <-interruptCtx.Done():
		}
		if turnID != "" {
			_ = codexTurnInterrupt(interruptCtx, proc.Client, effective.ThreadID, turnID)
		}
		interruptCancel()
		stopProcess()
		<-turnDone // codexRunTurn observes the same ctx and returns once the process/context closes
		delta := p.snapshotDeltaBestEffort(context.Background(), world, prepared)
		return finish(AttemptResult{ProviderSessionID: effective.ThreadID, WorkspaceDelta: delta}, codexFail(class, ctx.Err()))
	}

	if turnErr != nil {
		transcript.record("turn failed: %v", turnErr)
		var protoErr *CodexProtocolIncompleteError
		if !errors.As(turnErr, &protoErr) {
			delta := p.snapshotDeltaBestEffort(context.Background(), world, prepared)
			return finish(AttemptResult{ProviderSessionID: effective.ThreadID, WorkspaceDelta: delta}, p.classifyTurnError(turnErr))
		}

		// §23: an invalid/missing proposal MUST NOT immediately rerun the
		// full coding task. Freeze whatever the workspace already shows
		// before attempting exactly one read-only, schema-only repair — a
		// repair proposal can never retroactively change what Hufu already
		// observed here.
		stopProcess()
		frozen, snapErr := world.Snapshot(context.Background(), prepared)
		if snapErr != nil {
			transcript.record("repair: freeze snapshot failed: %v", snapErr)
			return finish(AttemptResult{ProviderSessionID: effective.ThreadID}, codexFail(CodexFailureWorkspaceViolation, fmt.Errorf("freeze workspace snapshot: %w", snapErr)))
		}
		frozenDelta, diffErr := NewWorkspaceSnapshotter().Diff(context.Background(), prepared.Baseline, frozen)
		if diffErr != nil {
			transcript.record("repair: freeze diff failed: %v", diffErr)
			return finish(AttemptResult{ProviderSessionID: effective.ThreadID}, codexFail(CodexFailureWorkspaceViolation, fmt.Errorf("freeze workspace diff: %w", diffErr)))
		}
		if err := ValidateExecutionWorldDelta(prepared, frozenDelta); err != nil {
			transcript.record("repair: workspace violation before repair: %v", err)
			return finish(AttemptResult{ProviderSessionID: effective.ThreadID, WorkspaceDelta: frozenDelta}, codexFail(CodexFailureWorkspaceViolation, err))
		}

		canonical, repairTurnID, repairErr := p.attemptResultRepair(ctx, sup, request, effective.ThreadID, prepared, frozenDelta, transcript, startupTimeout, interruptGrace, shutdownGrace)
		if repairErr != nil {
			transcript.record("repair failed: %v", repairErr)
			return finish(AttemptResult{ProviderSessionID: effective.ThreadID, WorkspaceDelta: frozenDelta, ProviderTurnID: repairTurnID}, repairErr)
		}
		return finish(AttemptResult{
			ProviderSessionID: effective.ThreadID, WorkspaceDelta: frozenDelta, ProviderTurnID: repairTurnID,
			CanonicalResult: canonical, Output: canonical.Summary,
		}, nil)
	}
	transcript.record("turn completed turn_id=%s status=%s", turnResult.TurnID, proposalStatusForLog(turnResult.Proposal))
	result.ProviderTurnID = turnResult.TurnID

	stopProcess()

	final, snapErr := world.Snapshot(context.Background(), prepared)
	if snapErr != nil {
		transcript.record("final snapshot failed: %v", snapErr)
		return finish(result, codexFail(CodexFailureWorkspaceViolation, fmt.Errorf("final workspace snapshot: %w", snapErr)))
	}
	wdelta, diffErr := NewWorkspaceSnapshotter().Diff(context.Background(), prepared.Baseline, final)
	if diffErr != nil {
		transcript.record("workspace diff failed: %v", diffErr)
		return finish(result, codexFail(CodexFailureWorkspaceViolation, fmt.Errorf("workspace diff: %w", diffErr)))
	}
	result.WorkspaceDelta = wdelta
	if err := ValidateExecutionWorldDelta(prepared, wdelta); err != nil {
		transcript.record("workspace violation: %v", err)
		return finish(result, codexFail(CodexFailureWorkspaceViolation, err))
	}

	canonical, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(), request, AttemptResult{ResultProposal: turnResult.Proposal}, wdelta, prepared.Root)
	if err != nil {
		transcript.record("canonicalize failed: %v", err)
		return finish(result, fmt.Errorf("canonicalize codex result: %w", err))
	}
	result.ResultProposal = turnResult.Proposal
	result.CanonicalResult = canonical
	result.Output = canonical.Summary
	return finish(result, nil)
}

// attemptResultRepair implements §23's repair flow: start a fresh app-server
// process (the original one is already stopped by the caller before this
// runs), resume the same thread while forcing a read-only sandbox, and send
// exactly one schema-only turn asking only for a structured account of the
// work already done. It never re-observes the workspace itself —
// canonicalization always uses frozenDelta, the delta the caller captured
// before calling this — so a repair turn cannot retroactively change
// observed workspace history even if the app-server disobeys the read-only
// instruction. Returns the canonical result, the repair turn's ID
// (diagnostic only, may be empty on failure), and an error.
func (p *CodexSubagentProvider) attemptResultRepair(
	ctx context.Context, sup ProcessSupervisor, request AttemptRequest,
	threadID string, prepared *PreparedExecutionWorld, frozenDelta WorkspaceDelta,
	transcript *codexTranscript, startupTimeout, interruptGrace, shutdownGrace time.Duration,
) (canonical *TaskResult, turnID string, resultErr error) {
	transcript.record("attempting result-only repair thread_id=%s", threadID)

	startCtx := ctx
	var startCancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		startCtx, startCancel = context.WithTimeout(ctx, startupTimeout)
		defer startCancel()
	}
	repairProc, err := StartCodexAppServer(startCtx, sup, CodexProcessConfig{
		Argv: p.config.Command, Dir: p.coordinator.projectDir, Env: prepared.Environment,
		MaxFrameBytes: int(p.config.MaxEventBytes), StartupTimeout: startupTimeout,
	})
	if err != nil {
		transcript.record("repair: start app-server failed: %v", err)
		return nil, "", codexFail(CodexFailureUnavailable, fmt.Errorf("start codex app-server for repair: %w", err))
	}
	defer p.stopCodexProcess(sup, repairProc, interruptGrace, shutdownGrace, transcript)

	// Sandbox is forced to read-only here regardless of the original task's
	// side-effect-derived sandbox: the repair turn MUST NOT have
	// workspace-write permission (§23). DeveloperInstructions is thread-
	// scoped in the real protocol, so the read-only repair instruction is
	// bound to this resumed thread, not passed per-turn.
	threadCfg := CodexThreadConfig{
		CWD: prepared.CWD, Model: request.ModelID, Sandbox: codexSandboxReadOnly,
		DeveloperInstructions: codexResultRepairInstruction,
	}
	effective, err := codexStartOrResumeThread(ctx, repairProc.Client, threadID, threadCfg, nil)
	if err != nil {
		transcript.record("repair: thread/resume failed: %v", err)
		return nil, "", codexFail(CodexFailureProtocolError, fmt.Errorf("codex repair resume: %w", err))
	}

	turnResult, turnErr := codexRunTurn(ctx, repairProc.Client, effective.ThreadID, codexResultRepairPrompt, nil)
	if turnErr != nil {
		transcript.record("repair: turn failed: %v", turnErr)
		return nil, turnResult.TurnID, p.classifyTurnError(turnErr)
	}
	transcript.record("repair: turn completed turn_id=%s status=%s", turnResult.TurnID, proposalStatusForLog(turnResult.Proposal))

	canonical, err = NewExternalResultCanonicalizer().Canonicalize(context.Background(), request, AttemptResult{ResultProposal: turnResult.Proposal}, frozenDelta, prepared.Root)
	if err != nil {
		transcript.record("repair: canonicalize failed: %v", err)
		return nil, turnResult.TurnID, fmt.Errorf("canonicalize codex repair result: %w", err)
	}
	return canonical, turnResult.TurnID, nil
}

func proposalStatusForLog(p *WorkerResultProposal) string {
	if p == nil {
		return "<none>"
	}
	return p.Status
}

// snapshotDeltaBestEffort captures whatever workspace delta is observable
// after a cancellation/crash, without letting a snapshot failure mask the
// original error (§16.3: "Hufu SHALL attempt to preserve ... post-cancel
// workspace snapshot").
func (p *CodexSubagentProvider) snapshotDeltaBestEffort(ctx context.Context, world ExecutionWorld, prepared *PreparedExecutionWorld) WorkspaceDelta {
	final, err := world.Snapshot(ctx, prepared)
	if err != nil {
		return WorkspaceDelta{}
	}
	delta, err := NewWorkspaceSnapshotter().Diff(ctx, prepared.Baseline, final)
	if err != nil {
		return WorkspaceDelta{}
	}
	return delta
}

// stopCodexProcess implements §16.1's required cancellation flow: (protocol
// turn/interrupt already happened, if applicable, before this is called);
// wait interrupt-grace; terminate the process tree; wait shutdown-grace;
// force-kill — reaching every descendant at each escalation, never just the
// direct child.
//
// The client is closed first, before waiting for the process to exit. Its
// stdin is bridged to the child through an io.Pipe, and os/exec's Wait
// cannot return until the goroutine copying into that pipe sees EOF — which
// only happens once the pipe's write end (owned by the client) closes.
// Waiting for process exit before closing the client deadlocks: the process
// exiting does not, by itself, unblock a Go-level io.Pipe read.
func (p *CodexSubagentProvider) stopCodexProcess(sup ProcessSupervisor, proc *CodexAppServerProcess, interruptGrace, shutdownGrace time.Duration, transcript *codexTranscript) {
	_ = proc.Client.Close()
	done := make(chan struct{})
	go func() {
		_ = proc.Wait() // memoized: safe alongside StartCodexAppServer's own crash-monitor Wait
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(interruptGrace):
	}
	transcript.record("process still active after interrupt grace, terminating")
	_ = sup.TerminateTree(context.Background(), proc.Handle())
	select {
	case <-done:
		return
	case <-time.After(shutdownGrace):
	}
	transcript.record("process still active after shutdown grace, force-killing")
	_ = sup.KillTree(context.Background(), proc.Handle())
	<-done
}

func (p *CodexSubagentProvider) classifyThreadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return codexFail(CodexFailureCancelled, err)
	}
	return codexFail(CodexFailureProtocolError, err)
}

func (p *CodexSubagentProvider) classifyTurnError(err error) error {
	var protoErr *CodexProtocolIncompleteError
	if errors.As(err, &protoErr) {
		return err
	}
	return codexFail(CodexFailureProcessCrash, err)
}

// codexStopOnce avoids double-stopping the same process from both a defer
// and an explicit mid-function call.
type codexStopOnce struct{ done bool }

func (o *codexStopOnce) do(f func()) {
	if o.done {
		return
	}
	o.done = true
	f()
}

var _ SubagentProvider = (*CodexSubagentProvider)(nil)
