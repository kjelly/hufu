package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

// codexSubagentProviderName is the conventional external provider name used
// when a config entry doesn't otherwise imply one
// (docs/architecture/execution-runtime.md).
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
	// Failure evidence is diagnostic only: it must remain bounded and redacted
	// before it enters task output, retry context, or durable failure records.
	codexFailureEvidenceMaxRunes = 12000
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
	codexResultRepairPrompt          = "Report the outcome of your previous turn as a structured result. Do not perform any further work."
	codexResultEncodingInstruction   = "For this Codex app-server response, `files_read` MUST be a JSON array of non-empty strings (workspace-relative paths or authorized opaque artifact IDs), never objects. The Hufu-local `submit_result` object form does not apply to this response; follow the supplied strict WorkerResultProposal schema."
	codexWorkerDeveloperInstructions = "Work only inside the provided workspace. Produce the final response matching the supplied JSON schema. Do not claim verification or receipt authority. " + codexResultEncodingInstruction
	codexResultRepairInstruction     = "This is a read-only repair turn. The workspace state from your previous turn is already frozen and will not be re-observed, so any further file changes will not be recorded or credited. Do not modify any files. Respond only with a WorkerResultProposal matching the required schema, describing the final status of the work you already attempted. " + codexResultEncodingInstruction
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
// (docs/architecture/execution-runtime.md). It executes
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

func codexPlatformUserHomeDir() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current account: %w", err)
	}
	home := strings.TrimSpace(current.HomeDir)
	if home == "" {
		return "", fmt.Errorf("current account has no home directory")
	}
	return home, nil
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
	config = enforceCodexBackendConcurrency(config)
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
func (c *codexPreflightCache) run(p *CodexSubagentProvider, environment []string) error {
	key, keyErr := codexPreflightCacheKey(p.config.Command, environment)
	c.mu.Lock()
	if c.valid && keyErr == nil && key == c.key {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	err := codexRunCheapPreflightChecks(p, environment)
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
func codexPreflightCacheKey(argv, environment []string) (string, error) {
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
	environmentDigest := sha256.Sum256([]byte(strings.Join(environment, "\x00")))
	return fmt.Sprintf("%s:%d:%d:%s", resolved, info.Size(), info.ModTime().UnixNano(), hex.EncodeToString(environmentDigest[:])), nil
}

// codexRunCheapPreflightChecks implements the local, no-subprocess-needed
// half of §25's preflight list. See codexPreflightCache's doc comment for
// which checks are deliberately not included and why.
func codexRunCheapPreflightChecks(p *CodexSubagentProvider, environment []string) error {
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
		var err error
		workspace, _, err = p.coordinator.codexExecutionWorld(p.name, nil)
		if err != nil {
			return fmt.Errorf("resolve frozen Codex execution world: %w", err)
		}
	}
	if _, err := resolveSnapshotRoot(workspace); err != nil {
		return fmt.Errorf("workspace root invalid: %w", err)
	}
	if codexHomeInherited(p.config.InheritEnv) {
		if err := codexHomeLooksAuthenticated(environment); err != nil {
			return err
		}
	}
	return nil
}

func codexHomeInherited(inheritEnv []string) bool {
	for _, name := range inheritEnv {
		switch strings.TrimSpace(name) {
		case "CODEX_HOME", "HOME":
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
func codexHomeLooksAuthenticated(environment []string) error {
	home, err := codexHomePathFromEnvironment(environment)
	if err != nil {
		return fmt.Errorf("CODEX_HOME is not set and the default ($HOME/.codex) could not be resolved: %w", err)
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

// codexHomePathFromEnvironment resolves the state directory that the child
// app-server can actually see. The environment passed to an external
// provider is allowlisted, so this must not use os.Environ or silently assume
// that the Hufu process's full environment is inherited. A CODEX_HOME value
// wins; otherwise an allowlisted HOME, or the platform user-home fallback
// used by Codex when neither variable is inherited, determines ~/.codex.
func codexHomePathFromEnvironment(environment []string) (string, error) {
	return codexHomePathFromEnvironmentWithHomeResolver(environment, codexPlatformUserHomeDir)
}

func codexHomePathFromEnvironmentWithHomeResolver(environment []string, resolveHome func() (string, error)) (string, error) {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	home := values["CODEX_HOME"]
	if home == "" {
		home = values["HOME"]
	}
	if home == "" {
		if resolveHome == nil {
			return "", fmt.Errorf("resolve Codex platform user home: no resolver configured")
		}
		var err error
		home, err = resolveHome()
		if err != nil {
			return "", fmt.Errorf("resolve Codex platform user home: %w", err)
		}
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve Codex platform user home: resolved home is empty")
	}
	if values["CODEX_HOME"] == "" {
		home = filepath.Join(home, ".codex")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve Codex state directory %q: %w", home, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return filepath.Clean(abs), nil
	}
	return filepath.Clean(resolved), nil
}

// acquireCodexHomeStateLock serializes app-server lifetimes for one resolved
// CODEX_HOME across both coordinators in this process and independent Hufu
// processes. The kernel file lock is released automatically if a process
// crashes, unlike a lock-directory protocol that could strand a future run.
func acquireCodexHomeStateLock(ctx context.Context, home string) (func(), error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return func() {}, nil
	}
	return acquireCodexStateFileLock(ctx, filepath.Join(home, ".hufu-app-server.lock"))
}

func (p *CodexSubagentProvider) acquireSharedCodexStateLock(ctx context.Context) (func(), error) {
	return p.acquireSharedCodexStateLockWithEnvironment(ctx, p.childEnvironment(), nil)
}

func (p *CodexSubagentProvider) acquireSharedCodexStateLockWithHomeResolver(ctx context.Context, resolveHome func() (string, error)) (func(), error) {
	return p.acquireSharedCodexStateLockWithEnvironment(ctx, p.childEnvironment(), resolveHome)
}

func (p *CodexSubagentProvider) childEnvironment() []string {
	if p == nil {
		return nil
	}
	if p.coordinator != nil {
		return p.coordinator.executionEnvironmentForBackend(p.name, p.config.InheritEnv)
	}
	return buildAllowlistedEnvironment(p.config.InheritEnv)
}

func (p *CodexSubagentProvider) acquireSharedCodexStateLockWithEnvironment(ctx context.Context, environment []string, resolveHome func() (string, error)) (func(), error) {
	if p == nil {
		return nil, fmt.Errorf("codex provider is unavailable")
	}
	var (
		home string
		err  error
	)
	if resolveHome == nil {
		home, err = codexHomePathFromEnvironment(environment)
	} else {
		home, err = codexHomePathFromEnvironmentWithHomeResolver(environment, resolveHome)
	}
	if err != nil {
		return nil, err
	}
	return acquireCodexHomeStateLock(ctx, home)
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
// scope this phase actually needs evidence for — and is sealed directly into
// Hufu's CAS so its reference is opaque, immutable, and task-scoped.
type codexTranscript struct {
	mu       sync.Mutex
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
	t.mu.Lock()
	defer t.mu.Unlock()
	line := fmt.Sprintf("%s %s", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
	line = utils.RedactSecrets(line)
	if t.bytes+int64(len(line)) > t.maxBytes {
		if len(t.lines) == 0 || t.lines[len(t.lines)-1] != "...[truncated]" {
			t.lines = append(t.lines, "...[truncated]")
		}
		return
	}
	t.lines = append(t.lines, line)
	t.bytes += int64(len(line))
}

func (t *codexTranscript) content() string {
	t.mu.Lock()
	content := strings.Join(t.lines, "\n") + "\n"
	t.mu.Unlock()
	return boundCodexTranscriptContent(content, t.maxBytes)
}

func (t *codexTranscript) persist(workspace, provider, runID, taskID string, attempt int) (string, error) {
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(provider) == "" ||
		strings.TrimSpace(runID) == "" || strings.TrimSpace(taskID) == "" || attempt <= 0 {
		return "", fmt.Errorf("codex transcript: complete run, provider, task, and attempt identity is required")
	}
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		return "", fmt.Errorf("codex transcript: open artifact store: %w", err)
	}
	content := t.content()
	id := codexTranscriptArtifactID(provider, runID, taskID, attempt, content)
	if err := putCodexTranscript(context.Background(), store, id, provider, runID, taskID, attempt, []byte(content), time.Time{}); err != nil {
		return "", err
	}
	return id, nil
}

func putCodexTranscript(ctx context.Context, store *FileArtifactStore, id, provider, runID, taskID string, attempt int, content []byte, completedAt time.Time) error {
	putResult, err := store.Put(ctx, PutArtifactRequest{
		ID:          id,
		Kind:        codexProviderTranscriptKind,
		Role:        codexProviderTranscriptRole,
		Path:        "provider-transcripts/" + id + ".log",
		Description: "Bounded redacted Codex provider transcript",
		MediaType:   "text/plain",
		Content:     content,
		RunID:       runID,
		TaskID:      taskID,
		Attempt:     attempt,
		Provider:    provider,
		CreatedAt:   completedAt,
	})
	if err != nil {
		return fmt.Errorf("codex transcript: persist artifact: %w", err)
	}
	if _, err := verifyCodexTranscriptArtifact(ctx, store, putResult.ID, provider, runID, taskID, attempt); err != nil {
		return fmt.Errorf("codex transcript: verify artifact: %w", err)
	}
	return nil
}

const (
	codexProviderTranscriptKind = "provider_transcript"
	codexProviderTranscriptRole = "diagnostic"
)

func codexTranscriptArtifactID(provider, runID, taskID string, attempt int, content string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", provider, runID, taskID, attempt, content)))
	return "provider-transcript-" + hex.EncodeToString(digest[:])
}

// verifyCodexTranscriptArtifact is the read-after-write boundary for provider
// diagnostics. The receipt stores only the returned opaque ID, so the CAS
// metadata must prove the exact provider/run/task/attempt occurrence before
// the ID can cross that boundary.
func verifyCodexTranscriptArtifact(ctx context.Context, store *FileArtifactStore, id, provider, runID, taskID string, attempt int) (ArtifactRef, error) {
	ref, err := codexTranscriptArtifactRef(ctx, store, id, provider, runID, taskID, attempt)
	if err != nil {
		return ArtifactRef{}, err
	}
	if err := store.Verify(ctx, ref); err != nil {
		return ArtifactRef{}, err
	}
	return ref, nil
}

func codexTranscriptArtifactRef(ctx context.Context, store *FileArtifactStore, id, provider, runID, taskID string, attempt int) (ArtifactRef, error) {
	if store == nil || !validArtifactID(id) {
		return ArtifactRef{}, fmt.Errorf("provider transcript reference %q is invalid", id)
	}
	ref, err := store.Get(ctx, id)
	if err != nil {
		return ArtifactRef{}, err
	}
	if ref.Kind != codexProviderTranscriptKind || ref.Role != codexProviderTranscriptRole || ref.Type != codexProviderTranscriptKind ||
		ref.Provider != provider || ref.RunID != runID || ref.TaskID != taskID || ref.Attempt != attempt ||
		ref.SHA256 == "" || ref.Bytes < 0 || ref.ByteSize != ref.Bytes {
		return ArtifactRef{}, fmt.Errorf("provider transcript %q has mismatched immutable scope metadata", id)
	}
	return ref, nil
}

func boundCodexTranscriptContent(content string, maxBytes int64) string {
	content = utils.RedactSecrets(content)
	if maxBytes <= 0 || int64(len(content)) <= maxBytes {
		return content
	}
	limit := int(maxBytes)
	const marker = "...[truncated]"
	if limit <= len(marker) {
		return marker[:limit]
	}
	cut := limit - len(marker)
	for cut > 0 && cut < len(content) && (content[cut]&0xc0) == 0x80 {
		cut--
	}
	return content[:cut] + marker
}

// finishCodexAttempt seals the provider transcript before returning. A mixed
// fanout success keeps it in the recovery stage until the verified receipt is
// ready to publish. Ordinary attempts publish the transcript at this boundary.
func (p *CodexSubagentProvider) finishCodexAttempt(
	transcript *codexTranscript, artifactWorkspace string, request AttemptRequest,
	res AttemptResult, err error,
) (AttemptResult, error) {
	if rejection, ok := errors.AsType[*localResultAdmissionError](err); ok {
		err = &externalResultAdmissionError{reason: rejection.reason, cause: err}
	}
	if request.sealFanoutResult && err == nil && res.CanonicalResult != nil && taskResultStatusIsSuccessful(res.CanonicalResult.Status) {
		ref, stageErr := stageCodexFanoutTranscript(artifactWorkspace, p.name, request, transcript.content())
		if stageErr != nil {
			if res.repairProvenance != nil {
				res.CanonicalResult, res.invariantClaims, res.Output = nil, nil, ""
				return res, &externalResultRepairError{cause: stageErr}
			}
			return AttemptResult{}, stageErr
		}
		res.TranscriptRef = ref
		return res, nil
	}
	ref, persistErr := transcript.persist(artifactWorkspace, p.name, request.RunID, request.TaskID, request.Attempt)
	if persistErr == nil {
		res.TranscriptRef = ref
		return res, err
	}
	hadCanonicalResult := res.CanonicalResult != nil
	res.TranscriptRef = ""
	res.CanonicalResult = nil
	res.ResultProposal = nil
	res.invariantClaims = nil
	if hadCanonicalResult {
		res.Output = ""
	}
	transcriptErr := codexFail(CodexFailureUnavailable, fmt.Errorf("persist provider transcript: %w", persistErr))
	if err != nil {
		err = errors.Join(err, transcriptErr)
	} else {
		err = transcriptErr
	}
	if res.repairProvenance != nil {
		err = &externalResultRepairError{cause: err}
	}
	return res, err
}

// RunAttempt implements §13.2/§14/§16's production flow for one attempt:
// prepare the execution world, start one app-server process, initialize,
// start-or-resume the thread (persisting the session binding before any
// turn depends on it), run exactly one turn, canonicalize the untrusted
// proposal against the actually-observed workspace delta, and always tear
// the process down — on every exit path, success or failure.
func (p *CodexSubagentProvider) newAttemptTranscript(sealFanoutResult bool) *codexTranscript {
	limit := p.config.MaxTranscriptBytes
	if sealFanoutResult && limit > codexDefaultMaxTranscript {
		limit = codexDefaultMaxTranscript
	}
	return newCodexTranscript(limit)
}

func prepareCodexExecutionWorld(ctx context.Context, world *LocalExecutionWorld, spec ExecutionWorldSpec, session *TeamSession) (*PreparedExecutionWorld, error) {
	if session != nil {
		spec.ControlWorkspace = session.Workspace
		version := session.WorkspaceVersion
		spec.RootIdentity = version.SubjectIdentity
		if spec.RootIdentity != nil && version.SubjectRoot != spec.Root {
			return nil, errWorkspaceSnapshotRootChanged
		}
	}
	return world.Prepare(ctx, spec)
}

func (p *CodexSubagentProvider) RunAttempt(ctx context.Context, request AttemptRequest) (result AttemptResult, resultErr error) {
	if err := p.validateAttemptEntry(ctx, request); err != nil {
		return AttemptResult{}, err
	}
	normalizedReasoningEffort, err := normalizeCodexReasoningEffort(request.ReasoningEffort)
	if err != nil {
		return AttemptResult{}, codexFail(CodexFailureProtocolError, err)
	}
	request.ReasoningEffort = normalizedReasoningEffort
	childEnvironment := p.childEnvironment()
	// §25: preflight failure MUST occur before the task enters provider
	// execution — this runs (or reuses a cached result) before any process,
	// workspace, or protocol activity below.
	preflight := p.preflight
	if preflight == nil {
		preflight = &codexPreflightCache{}
	}
	if err := preflight.run(p, childEnvironment); err != nil {
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

	// Choose the session to resume before any process or workspace side
	// effect: an empty projection does not prove no session was bound.
	existingThreadID, err := p.coordinator.resumableBackendSessionID(ctx, request)
	if err != nil {
		return AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("resolve durable Codex session: %w", err))
	}

	transcript := p.newAttemptTranscript(request.sealFanoutResult)
	workspace, networkDisabled, worldPolicyErr := p.coordinator.codexExecutionWorld(p.name, request.Agent)
	if worldPolicyErr != nil {
		return AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("resolve frozen Codex execution world: %w", worldPolicyErr))
	}
	artifactWorkspace := p.coordinator.artifactStoreRootPath()
	if artifactWorkspace == "" {
		artifactWorkspace = workspace
	}
	transcript.record("attempt start task=%s attempt=%d model=%s", request.TaskID, request.Attempt, request.ModelID)

	// The local execution world owns workspace preparation, leases, and
	// post-turn effect verification. It intentionally does not claim network
	// control: the Codex app-server is the provider control plane and must be
	// able to bootstrap and reach its model. For a no-net task, the task-plane
	// network boundary is Codex's native sandbox. codexStartOrResumeThread
	// verifies its effective networkAccess=false before any turn/start and
	// fails closed when that proof is missing or permits network access.
	world := NewLocalExecutionWorld()
	stateLock, lockErr := p.acquireSharedCodexStateLockWithEnvironment(ctx, childEnvironment, nil)
	if lockErr != nil {
		transcript.record("Codex shared state lock failed: %v", lockErr)
		return p.finishCodexAttempt(transcript, artifactWorkspace, request, AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("acquire Codex shared state lock: %w", lockErr)))
	}
	defer stateLock()
	imageInput, err := p.prepareImageAttempt(ctx, request, childEnvironment)
	if err != nil {
		return AttemptResult{}, err
	}
	defer imageInput.Close()

	startupTimeout, interruptGrace, shutdownGrace := p.durations()
	session := p.coordinator.session
	prepared, err := prepareCodexExecutionWorld(ctx, world, ExecutionWorldSpec{
		RunID: request.RunID, TaskID: request.TaskID, Attempt: request.Attempt,
		Root: workspace, CWD: workspace, SideEffect: request.Task.SideEffect,
		NetworkAllowed: !networkDisabled, EnvironmentAllowlist: p.config.InheritEnv,
		Environment: childEnvironment,
	}, session)
	if err != nil {
		// A failed managed prepare has not authorized any write through the
		// subject path, including a transcript when artifacts default there.
		if session != nil && session.WorkspaceVersion.SubjectIdentity != nil {
			return AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("prepare execution world: %w", err))
		}
		transcript.record("execution world prepare failed: %v", err)
		return p.finishCodexAttempt(transcript, artifactWorkspace, request, AttemptResult{}, codexFail(CodexFailureUnavailable, fmt.Errorf("prepare execution world: %w", err)))
	}
	defer func() { _ = world.Release(context.Background(), prepared) }()
	providerDir, err := prepared.providerDir()
	if err != nil {
		return p.finishCodexAttempt(transcript, artifactWorkspace, request, AttemptResult{}, codexFail(CodexFailureUnavailable, err))
	}

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
	if imageInput != nil {
		if err := p.coordinator.validateCodexImageLease(ctx, request); err != nil {
			return AttemptResult{}, err
		}
	}
	sup := NewProcessSupervisor()
	proc, err := p.startCodexAttemptProcess(startCtx, sup, request, CodexProcessConfig{
		Argv: p.config.Command, Dir: prepared.processDir(), Env: prepared.Environment,
		MaxFrameBytes: int(p.config.MaxEventBytes), StartupTimeout: startupTimeout,
	})
	if err != nil {
		transcript.record("start app-server failed: %v", err)
		res := AttemptResult{}
		failure := p.classifyThreadStartup(err)
		if init, ok := errors.AsType[*codexInitializationFailure](err); ok && init.settled {
			res, failure = p.settleFailedCodexWorkspace(context.WithoutCancel(ctx), request, world, prepared, res, failure, init.secrets)
		}
		return p.finishCodexAttempt(transcript, artifactWorkspace, request, res, failure)
	}
	durable := context.WithoutCancel(ctx)
	stopProcess := sync.OnceValue(func() error {
		err := p.stopCodexProcess(proc, interruptGrace, shutdownGrace, transcript)
		if err != nil {
			p.coordinator.markWorkspaceProviderUnsettled(request)
		}
		return err
	})
	proc.Client.finishProcess = stopProcess
	defer func() { _ = stopProcess() }()
	finishFailure := func(res AttemptResult, failure error, secrets *utils.SecretRedactionScope) (AttemptResult, error) {
		if secrets == nil {
			secrets = proc.Client.newDiagnostics(nil).secrets
		}
		if err := stopProcess(); err != nil {
			failure = err
		} else {
			res, failure = p.settleFailedCodexWorkspace(durable, request, world, prepared, res, failure, secrets)
		}
		recordCodexFailureEvidence(transcript, "provider turn output", res.Output)
		return p.finishCodexAttempt(transcript, artifactWorkspace, request, res, failure)
	}

	if request.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}

	threadCfg := CodexThreadConfig{
		CWD: providerDir, Model: request.ModelID, Sandbox: sandbox,
		NetworkDisabled:       networkDisabled,
		DeveloperInstructions: codexWorkerDeveloperInstructions,
	}
	effective, err := codexStartOrResumeThread(ctx, proc.Client, existingThreadID, threadCfg, func(state CodexEffectiveThreadState) error {
		transcript.record("session bound thread_id=%s", state.ThreadID)
		return p.coordinator.persistProviderSessionBinding(context.Background(), request.TaskID, request.Attempt, ProviderBinding{
			Provider: p.name, Protocol: p.config.Protocol, SessionID: state.ThreadID,
			ExecutionWorldID: prepared.ID, CWD: prepared.CWD, EffectiveModel: state.Model, SandboxMode: state.Sandbox,
			ResumeSupported: true,
		})
	})
	if err != nil {
		transcript.record("thread start/resume failed: %v", err)
		if !proc.Client.providerIdentitySafe(existingThreadID) {
			existingThreadID = ""
		}
		return finishFailure(AttemptResult{ProviderSessionID: existingThreadID}, p.classifyThreadError(ctx, err), nil)
	}
	result.ProviderSessionID = effective.ThreadID

	reportCodexActivity := func(activity codexTurnActivity) {
		transcript.record("turn activity method=%s message=%s elapsed=%s heartbeat=%t", activity.Method, activity.Message, activity.Elapsed.Round(time.Millisecond), activity.Heartbeat)
		if p.coordinator == nil {
			return
		}
		agentName := ""
		if request.Agent != nil {
			agentName = request.Agent.Name
		}
		p.coordinator.report(StatusEvent{
			Type:    "codex_activity",
			Agent:   agentName,
			Message: activity.Message,
			Model:   request.ModelID,
			TodoID:  request.TaskID,
			Data: map[string]any{
				"codex_method": activity.Method,
				"elapsed_ms":   activity.Elapsed.Milliseconds(),
				"heartbeat":    activity.Heartbeat,
			},
		})
	}
	onTurnStarted := func(string) error {
		message := "Codex turn accepted"
		if request.ReasoningEffort != "" {
			message += " (reasoning effort: " + request.ReasoningEffort + ")"
		}
		reportCodexActivity(codexTurnActivity{Method: codexMethodTurnStart, Message: message})
		return nil
	}
	if err := p.admitImageTurn(ctx, request, imageInput); err != nil {
		return finishFailure(result, err, nil)
	}
	var nativeInput []codexUserInput
	if imageInput != nil {
		nativeInput = imageInput.projection.Inputs(request.Prompt)
	}
	turnResult, turnErr := runManagedCodexTurn(ctx, proc.Client, effective.ThreadID, request.Prompt, codexTurnOptions{
		CWD:               providerDir,
		Input:             nativeInput,
		ResultProjection:  projectionForContract(request.resultContractSchema),
		RuntimeCompletion: hasRuntimeCompletion(request.resultContractSchema),
		ReasoningEffort:   request.ReasoningEffort,
		DetailedOutput:    p.coordinator.verbose,
		OnTurnStarted:     onTurnStarted,
		OnActivity:        reportCodexActivity,
	}, interruptGrace, transcript)

	// Use the diagnostic sealed by the turn before recording any errors.
	result.Output = turnResult.diagnosticOutput()
	result.ProviderTurnID = turnResult.TurnID
	if failure := codexAttemptContextFailure(ctx, request.Timeout, proc.Client.completedBeforeCancellation()); failure != nil {
		return finishFailure(result, failure, turnResult.diagnosticSecrets)
	}
	if turnErr != nil {
		transcript.record("turn failed: %v", turnErr)
		if _, incomplete := errors.AsType[*CodexProtocolIncompleteError](turnErr); !incomplete {
			return finishFailure(result, p.classifyTurnError(turnErr), turnResult.diagnosticSecrets)
		}
	}

	transcript.record("turn completed turn_id=%s status=%s", turnResult.TurnID, proposalStatusForLog(turnResult.Proposal))
	if err := stopProcess(); err != nil {
		return p.finishCodexAttempt(transcript, artifactWorkspace, request, result, err)
	}
	settleCtx := ctx
	if proc.Client.completedBeforeCancellation() {
		settleCtx = durable
	}
	result, err = p.finalizeCodexTurn(settleCtx, &request, world, prepared, turnResult, turnErr, result, transcript)
	if localResultRejectionReason(err) == RepairFailureInvalidSchema {
		result, err = p.repairCodexResult(ctx, sup, request, world, prepared, result, err, transcript, artifactWorkspace, reportCodexActivity, turnResult.diagnosticSecrets)
	}
	return p.finishCodexAttempt(transcript, artifactWorkspace, request, result, err)
}

func runManagedCodexTurn(ctx context.Context, client *CodexRPCClient, threadID, prompt string, options codexTurnOptions, interruptGrace time.Duration, transcript *codexTranscript) (result CodexTurnResult, turnErr error) {
	turnDone := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case <-turnDone:
		case <-ctx.Done():
		}
		if ctx.Err() != nil && !client.completedBeforeCancellation() {
			transcript.record("attempt context done while turn in flight: %v", ctx.Err())
			interruptCodexTurn(client, threadID, interruptGrace)
			_ = client.finishProcess()
		}
	}()
	defer func() {
		close(turnDone)
		<-monitorDone
		if result.TurnID == "" {
			result.TurnID = client.acknowledgedTurnID()
		}
	}()
	return codexRunTurn(ctx, client, threadID, prompt, options)
}

// A canceled response waiter leaves its sent start pending through this
// bounded wait for a wire-authenticated ACK.
func interruptCodexTurn(client *CodexRPCClient, threadID string, grace time.Duration) {
	if client.observedTurnTerminal() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if turnID := client.awaitAcknowledgedTurn(ctx); turnID != "" && client.observedTurnTerminal() == nil {
		_ = codexTurnInterrupt(ctx, client, threadID, turnID)
	}
}

func (p *CodexSubagentProvider) repairCodexResult(ctx context.Context, sup ProcessSupervisor, request AttemptRequest, world ExecutionWorld, prepared *PreparedExecutionWorld, result AttemptResult, rejection error, transcript *codexTranscript, workspace string, onActivity func(codexTurnActivity), secrets *utils.SecretRedactionScope) (AttemptResult, error) {
	recordCodexFailureEvidence(transcript, "original provider turn output", result.Output)
	ref, err := transcript.persist(workspace, p.name, request.RunID, request.TaskID, request.Attempt)
	if err != nil {
		return result, rejectLocalResultPersistence(err)
	}
	durable := context.WithoutCancel(ctx)
	key, proof, err := p.coordinator.reserveExternalResultCorrection(durable, request, result, ref, prepared, rejection)
	if err != nil {
		return result, err
	}
	result.repairProvenance = proof
	startup, interrupt, shutdown := p.durations()
	acknowledgementHandled := false
	canonical, claims, repairOutput, repairTurnID, err := p.attemptResultRepair(ctx, sup, request, world, result.ProviderSessionID, prepared, result.WorkspaceDelta, rejection, transcript, onActivity, func(turnID string) error {
		acknowledgementHandled = true
		if err := p.coordinator.recordExternalCorrectionTurn(durable, key, proof, turnID); err != nil {
			return err
		}
		onActivity(codexTurnActivity{Method: codexMethodTurnStart, Message: "Codex result correction accepted"})
		return nil
	}, startup, interrupt, shutdown, secrets)
	if !acknowledgementHandled && repairTurnID != "" {
		if recordErr := p.coordinator.recordExternalCorrectionTurn(durable, key, proof, repairTurnID); recordErr != nil {
			err = errors.Join(err, recordErr)
		}
	}
	if proof.ProviderCorrectionTurnID != "" {
		result.ProviderTurnID = proof.ProviderCorrectionTurnID
	}
	if err == nil {
		if proof.ProviderCorrectionTurnID != repairTurnID {
			err = rejectResultSourceRead(errTaskResultStale)
		} else {
			var response *externalResultCorrectionResponse
			response, err = p.coordinator.checkpointExternalCorrectionResponse(durable, key, repairTurnID, canonical)
			if err == nil {
				canonical = response.Result
				proof.PreparedCandidateSHA256 = response.ResultSHA256
			}
		}
	}
	if err != nil {
		transcript.record("repair failed: %v", err)
		result.Output = combineCodexFailureEvidence(result.Output, repairOutput)
		recordCodexFailureEvidence(transcript, "repair provider turn output", repairOutput)
		return result, &externalResultRepairError{cause: err}
	}
	onActivity(codexTurnActivity{Method: codexNotificationTurnCompleted, Message: "Codex result correction recorded"})
	proof.Success, proof.SubmittedResult, proof.Error = true, cloneTaskResult(canonical), ""
	result.CanonicalResult, result.invariantClaims, result.Output = canonical, claims, canonical.Summary
	return result, nil
}

func (p *CodexSubagentProvider) finalizeCodexTurn(
	ctx context.Context, request *AttemptRequest, world ExecutionWorld, prepared *PreparedExecutionWorld,
	turnResult CodexTurnResult, turnErr error, result AttemptResult, transcript *codexTranscript,
) (AttemptResult, error) {
	final, wdelta, err := p.observeCodexWorkspaceEffects(context.WithoutCancel(ctx), *request, world, prepared, prepared.Baseline, turnResult.diagnosticSecrets)
	result.WorkspaceDelta = wdelta
	if err != nil {
		transcript.record("workspace settlement failed: %v", err)
		recordCodexFailureEvidence(transcript, "provider turn output", result.Output)
		return result, err
	}
	request.resultWorkspaceBaseline, request.resultWorkspaceSnapshot = &prepared.Baseline, &final

	if turnErr != nil {
		return result, classifyRejectedCodexResult(ctx, *request, turnResult, wdelta, prepared.Root, turnErr)
	}
	canonical, claims, err := turnResult.canonicalize(ctx, *request, wdelta, prepared.Root)
	if err != nil {
		transcript.record("canonicalize failed: %v", err)
		recordCodexFailureEvidence(transcript, "rejected provider turn output", result.Output)
		return result, fmt.Errorf("canonicalize codex result: %w", err)
	}
	result.CanonicalResult = canonical
	result.invariantClaims = claims
	result.Output = canonical.Summary
	return result, nil
}

func classifyRejectedCodexResult(ctx context.Context, request AttemptRequest, result CodexTurnResult, delta WorkspaceDelta, root string, err error) error {
	if _, incomplete := errors.AsType[*CodexProtocolIncompleteError](err); !incomplete {
		return err
	}
	if sourceErr := authenticateRejectedExternalResult(ctx, request, result.RawFinalOutput, delta, root); sourceErr != nil {
		return redactCodexError(sourceErr, result.diagnosticSecrets)
	}
	if result.diagnosticsUnsafe {
		return rejectIncompleteCodexDiagnostics()
	}
	return rejectLocalResult(RepairFailureInvalidSchema, err)
}

// attemptResultRepair implements §23's repair flow: start a fresh app-server
// process (the original one is already stopped by the caller before this
// runs), resume the same thread while forcing a read-only sandbox, and send
// exactly one schema-only turn asking only for a structured account of the
// work already done. The correction's workspace is checked independently
// after stopping it, and canonicalization uses the frozen original delta.
// Returns the canonical result, bounded/redacted repair output,
// the repair turn's ID (diagnostic only, may be empty on failure), and an
// error. The untrusted decoded proposal remains local to this function and
// is never returned on a failed attempt.
func (p *CodexSubagentProvider) attemptResultRepair(
	ctx context.Context, sup ProcessSupervisor, request AttemptRequest, world ExecutionWorld,
	threadID string, prepared *PreparedExecutionWorld, frozenDelta WorkspaceDelta, rejection error,
	transcript *codexTranscript, onActivity func(codexTurnActivity), onTurnStarted func(string) error,
	startupTimeout, interruptGrace, shutdownGrace time.Duration, secrets *utils.SecretRedactionScope,
) (canonical *TaskResult, claims *[]InvariantAssessmentClaim, output, turnID string, resultErr error) {
	providerDir, err := prepared.providerDir()
	if err != nil {
		return nil, nil, "", "", classifyCodexRepairFailure(ctx, codexFail(CodexFailureUnavailable, err))
	}
	transcript.record("attempting result-only repair thread_id=%s", threadID)

	var stopRepair func() error
	var startupFailure error
	verifyReadonly := sync.OnceValue(func() error {
		if isUnsettledCodexInitialization(startupFailure) {
			return p.classifyThreadStartup(startupFailure)
		}
		if stopRepair != nil {
			if err := stopRepair(); err != nil {
				p.coordinator.markWorkspaceProviderUnsettled(request)
				return err
			}
		}
		return p.validateCodexRepairWorkspace(context.WithoutCancel(ctx), request, world, prepared, secrets)
	})
	defer func() {
		if err := verifyReadonly(); err != nil {
			canonical, claims, resultErr = nil, nil, err
		}
	}()
	repairProc, err := p.startCodexAttemptProcess(ctx, sup, request, CodexProcessConfig{
		Argv: p.config.Command, Dir: prepared.processDir(), Env: prepared.Environment,
		MaxFrameBytes: int(p.config.MaxEventBytes), StartupTimeout: startupTimeout,
	})
	if err != nil {
		startupFailure = redactCodexError(err, secrets)
		if init, ok := errors.AsType[*codexInitializationFailure](err); ok {
			secrets = init.secrets
		}
		transcript.record("repair: start app-server failed: %v", startupFailure)
		return nil, nil, "", "", classifyCodexRepairFailure(ctx, p.classifyThreadStartup(startupFailure))
	}
	secrets = repairProc.Client.newDiagnostics(secrets).secrets
	stopRepair = sync.OnceValue(func() error { return p.stopCodexProcess(repairProc, interruptGrace, shutdownGrace, transcript) })
	repairProc.Client.finishProcess = stopRepair

	// Sandbox is forced to read-only here regardless of the original task's
	// side-effect-derived sandbox: the repair turn MUST NOT have
	// workspace-write permission (§23). DeveloperInstructions is thread-
	// scoped in the real protocol, so the read-only repair instruction is
	// bound to this resumed thread, not passed per-turn.
	threadCfg := CodexThreadConfig{
		CWD: providerDir, Model: request.ModelID, Sandbox: codexSandboxReadOnly,
		NetworkDisabled:       !prepared.NetworkAllowed,
		DeveloperInstructions: codexResultRepairInstruction,
	}
	effective, err := codexStartOrResumeThread(ctx, repairProc.Client, threadID, threadCfg, nil)
	if err != nil {
		err = redactCodexError(err, secrets)
		transcript.record("repair: thread/resume failed: %v", err)
		// The repair runs after a turn changed the workspace, so it keeps the
		// protocol disposition that preserves the partial result, even when
		// the cause is a session resume mismatch.
		return nil, nil, "", "", classifyCodexRepairFailure(ctx, codexFail(CodexFailureProtocolError, withFailureClassOverride(fmt.Errorf("codex repair resume: %w", err), FailureProtocol)))
	}

	repairPrompt := codexResultRepairPrompt + request.invariantRepairInstructions
	if localResultRejectionReason(rejection) == RepairFailureInvalidSchema {
		diagnostic := utils.TruncateRunes(utils.RedactSecrets(rejection.Error()), codexFailureEvidenceMaxRunes)
		repairPrompt += "\n\nCorrect the following result fields only:\n" + diagnostic
	}
	turnResult, turnErr := runManagedCodexTurn(ctx, repairProc.Client, effective.ThreadID, repairPrompt, codexTurnOptions{
		CWD:               providerDir,
		ResultProjection:  projectionForContract(request.resultContractSchema),
		RuntimeCompletion: hasRuntimeCompletion(request.resultContractSchema),
		ReasoningEffort:   request.ReasoningEffort,
		DetailedOutput:    p.coordinator.verbose,
		OnTurnStarted:     onTurnStarted,
		OnActivity:        onActivity,
		diagnosticSecrets: secrets,
	}, interruptGrace, transcript)
	output = turnResult.diagnosticOutput()
	if turnResult.diagnosticSecrets != nil {
		secrets = turnResult.diagnosticSecrets
	}
	if err := verifyReadonly(); err != nil {
		return nil, nil, output, turnResult.TurnID, err
	}
	settleCtx := ctx
	if repairProc.Client.completedBeforeCancellation() {
		settleCtx = context.WithoutCancel(ctx)
	}
	if err := classifyCodexRepairFailure(settleCtx, nil); err != nil {
		return nil, nil, output, turnResult.TurnID, err
	}
	if turnErr != nil {
		transcript.record("repair: turn failed: %v", turnErr)
		classified := classifyRejectedCodexResult(settleCtx, request, turnResult, frozenDelta, prepared.Root, p.classifyTurnError(turnErr))
		return nil, nil, output, turnResult.TurnID, classifyCodexRepairFailure(settleCtx, classified)
	}
	transcript.record("repair: turn completed turn_id=%s status=%s", turnResult.TurnID, proposalStatusForLog(turnResult.Proposal))

	canonical, claims, err = turnResult.canonicalize(settleCtx, request, frozenDelta, prepared.Root)
	if err != nil {
		transcript.record("repair: canonicalize failed: %v", err)
		return nil, nil, output, turnResult.TurnID, fmt.Errorf("canonicalize codex repair result: %w", err)
	}
	return canonical, claims, "", turnResult.TurnID, nil
}

func (p *CodexSubagentProvider) validateCodexRepairWorkspace(ctx context.Context, request AttemptRequest, world ExecutionWorld, prepared *PreparedExecutionWorld, secrets *utils.SecretRedactionScope) error {
	baseline := request.resultWorkspaceSnapshot
	if baseline == nil {
		err := errors.New("codex repair has no verified original workspace snapshot")
		p.coordinator.markWorkspaceProviderEffectsUnverified(request, err, secrets)
		return codexFail(CodexFailureWorkspaceViolation, err)
	}
	readonly := *prepared
	readonly.WritableRoots = nil
	_, _, err := p.observeCodexWorkspaceEffects(ctx, request, world, &readonly, *baseline, secrets)
	return err
}

func classifyCodexRepairFailure(ctx context.Context, err error) error {
	cause := ctx.Err()
	if cause == nil {
		cause = err
	}
	switch {
	case errors.Is(cause, context.Canceled):
		return codexFail(CodexFailureCancelled, cause)
	case errors.Is(cause, context.DeadlineExceeded):
		return codexFail(CodexFailureTimeout, withFailureClassOverride(cause, FailureTimeout))
	default:
		return err
	}
}

// boundedCodexFailureEvidence preserves only diagnostic provider output. It
// is intentionally separate from canonical result summaries: a response that
// fails Hufu validation must never be promoted to trusted task output.
func boundedCodexFailureEvidence(raw string) string {
	return utils.TruncateRunes(redactCodexProviderText(raw, nil), codexFailureEvidenceMaxRunes)
}

func redactCodexProviderText(raw string, secrets *utils.SecretRedactionScope) string {
	if secrets == nil {
		diagnostics := newCodexTurnDiagnostics(nil)
		diagnostics.learn(raw)
		if diagnostics.unsafe {
			return "[invalid provider JSON]"
		}
		secrets = diagnostics.secrets
	}
	body, structured := codexDiagnosticJSON(raw)
	if !structured {
		return secrets.RedactText(raw)
	}
	if redacted, err := redactCodexFailureJSON([]byte(body), secrets); err == nil {
		return orderedCodexFailureEvidence(body, redacted)
	}
	return "[invalid provider JSON]"
}

// Learn from every readable frame before emitting any part of the response.
// A frame discarded by the diagnostic formatter can define a secret that is
// repeated in the retained envelope or a protocol decoding error.
func learnCodexDiagnosticSecrets(raw string, secrets *utils.SecretRedactionScope) error {
	learnFrame := func(frame string) error {
		return inspectReadablePayloadViews(jsontext.Value(frame), func(view []byte) error {
			return secrets.DiscoverJSON(view)
		})
	}
	_, err := inspectProviderResultJSONFrames(raw, resultCollectionResponseMaxBytes, func(frame string) error {
		if err := learnFrame(frame); err != nil {
			return err
		}
		var nestedErr error
		err := inspectProviderResultEnvelopeObjects(frame, 0, func(fields []submissionSourceField) {
			for _, field := range fields {
				if nestedErr != nil || !sourceMemberMatches(field.name, "structured_payload_json", true) || field.raw.Kind() != '"' {
					continue
				}
				var encoded string
				if nestedErr = json.Unmarshal(field.raw, &encoded); nestedErr == nil {
					_, nestedErr = inspectProviderResultJSONFrames(encoded, resultPayloadMaxBytes, learnFrame)
				}
			}
		})
		if err != nil {
			return err
		}
		return nestedErr
	})
	return err
}

// Complete readable values can be safely processed as JSON even when the
// provider stopped mid-response. Unreadable tails never enter a text sink.
func codexDiagnosticJSON(raw string) (string, bool) {
	body := strings.TrimSpace(collectionSourceInspectionJSON(raw))
	start := strings.IndexAny(body, "{[")
	if start < 0 {
		return "", false
	}
	body = body[start:]
	// Provider result diagnostics carry one object. An array wrapper can
	// conceal a wire-encoded payload that text redaction cannot inspect.
	if body[0] != '{' {
		return "", true
	}
	if len(body) > resultCollectionResponseMaxBytes {
		body = body[:resultCollectionResponseMaxBytes]
	}
	if jsontext.Value(body).IsValid() {
		return body, true
	}
	prefix, consumed := readableSourceContainerPrefix(body)
	if strings.ContainsAny(body[consumed:], "{[") || !jsontext.Value(prefix).IsValid() {
		return "", true
	}
	return prefix, true
}

// The wire DTO also carries structured JSON inside a string. Apply the same
// JSON redaction there so escaped credential keys are handled before the
// outer envelope is reduced to diagnostic text.
func redactCodexFailureJSON(body []byte, secrets *utils.SecretRedactionScope) ([]byte, error) {
	if jsontext.Value(body).Kind() == '{' {
		var members map[string]jsontext.Value
		if err := json.Unmarshal(body, &members); err != nil {
			return nil, err
		}
		changed := false
		for name, raw := range members {
			if !sourceMemberMatches(name, "structured_payload_json", true) || raw.Kind() != '"' {
				continue
			}
			var encoded string
			if err := json.Unmarshal(raw, &encoded); err != nil {
				continue
			}
			inner, structured := codexDiagnosticJSON(encoded)
			if !structured {
				continue
			}
			redacted, err := secrets.RedactJSONCompact([]byte(inner))
			if err != nil {
				redacted = []byte("[invalid structured payload JSON]")
			}
			members[name], err = json.Marshal(string(redacted))
			if err != nil {
				return nil, err
			}
			changed = true
		}
		if changed {
			var err error
			body, err = json.Marshal(members)
			if err != nil {
				return nil, err
			}
		}
	}
	return secrets.RedactJSONCompact(body)
}

// Keep the provider's top-level field order when applying the diagnostic
// bound, so long details cannot displace earlier path or status evidence.
func orderedCodexFailureEvidence(original string, redacted []byte) string {
	var members map[string]jsontext.Value
	if err := json.Unmarshal(redacted, &members); err != nil || members == nil {
		return string(redacted)
	}
	var ordered strings.Builder
	encoder := jsontext.NewEncoder(&ordered)
	if err := encoder.WriteToken(jsontext.BeginObject); err != nil {
		return string(redacted)
	}
	for _, field := range readableSourceMembers(original) {
		value, present := members[field.name]
		if !present {
			continue
		}
		if err := encoder.WriteToken(jsontext.String(field.name)); err != nil {
			return string(redacted)
		}
		if err := encoder.WriteValue(value); err != nil {
			return string(redacted)
		}
		delete(members, field.name)
	}
	if err := encoder.WriteToken(jsontext.EndObject); err != nil || len(members) != 0 {
		return string(redacted)
	}
	return strings.TrimSpace(ordered.String())
}

// combineCodexFailureEvidence keeps both sides of a protocol repair failure
// visible without allowing either untrusted provider response to dominate the
// bounded task diagnostic. Each side is redacted/bounded before being labeled,
// and the combined result remains within the same overall budget.
func combineCodexFailureEvidence(original, repair string) string {
	type evidencePart struct {
		label string
		text  string
	}
	parts := make([]evidencePart, 0, 2)
	if strings.TrimSpace(original) != "" {
		parts = append(parts, evidencePart{label: "original provider turn output", text: original})
	}
	if strings.TrimSpace(repair) != "" {
		parts = append(parts, evidencePart{label: "repair provider turn output", text: repair})
	}
	if len(parts) == 0 {
		return ""
	}
	partBudget := (codexFailureEvidenceMaxRunes - 64) / len(parts)
	var b strings.Builder
	for i, part := range parts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s:\n%s", part.label, utils.TruncateRunes(part.text, partBudget))
	}
	return utils.TruncateRunes(b.String(), codexFailureEvidenceMaxRunes)
}

func recordCodexFailureEvidence(transcript *codexTranscript, label, evidence string) {
	if transcript == nil || strings.TrimSpace(evidence) == "" {
		return
	}
	transcript.record("%s: %s", label, evidence)
}

func proposalStatusForLog(p *WorkerResultProposal) string {
	if p == nil {
		return "<none>"
	}
	return p.Status
}

// Only settled provider effects are eligible for workspace evidence.
func (p *CodexSubagentProvider) settleFailedCodexWorkspace(ctx context.Context, request AttemptRequest, world ExecutionWorld, prepared *PreparedExecutionWorld, result AttemptResult, failure error, secrets *utils.SecretRedactionScope) (AttemptResult, error) {
	_, delta, err := p.observeCodexWorkspaceEffects(ctx, request, world, prepared, prepared.Baseline, secrets)
	result.WorkspaceDelta = delta
	if err != nil {
		return result, err
	}
	return result, failure
}

func (p *CodexSubagentProvider) observeCodexWorkspaceEffects(ctx context.Context, request AttemptRequest, world ExecutionWorld, prepared *PreparedExecutionWorld, baseline WorkspaceSnapshot, secrets *utils.SecretRedactionScope) (WorkspaceSnapshot, WorkspaceDelta, error) {
	final, err := world.Snapshot(ctx, prepared)
	if err != nil {
		err = fmt.Errorf("final workspace snapshot: %w", err)
		p.coordinator.markWorkspaceProviderEffectsUnverified(request, err, secrets)
		return WorkspaceSnapshot{}, WorkspaceDelta{}, codexFail(CodexFailureWorkspaceViolation, redactCodexError(err, secrets))
	}
	delta, err := NewWorkspaceSnapshotter().Diff(ctx, baseline, final)
	if err != nil {
		err = fmt.Errorf("workspace diff: %w", err)
		p.coordinator.markWorkspaceProviderEffectsUnverified(request, err, secrets)
		return WorkspaceSnapshot{}, WorkspaceDelta{}, codexFail(CodexFailureWorkspaceViolation, redactCodexError(err, secrets))
	}
	if err := ValidateExecutionWorldDelta(prepared, delta); err != nil {
		p.coordinator.markWorkspaceUnauthorizedMutation(request, delta, err, secrets)
		return final, delta, codexFail(CodexFailureWorkspaceViolation, redactCodexError(err, secrets))
	}
	return final, delta, nil
}

func (p *CodexSubagentProvider) startCodexAttemptProcess(ctx context.Context, sup ProcessSupervisor, request AttemptRequest, config CodexProcessConfig) (*CodexAppServerProcess, error) {
	proc, err := StartCodexAppServer(ctx, sup, config)
	if isUnsettledCodexInitialization(err) {
		p.coordinator.markWorkspaceProviderUnsettled(request)
	}
	return proc, err
}

func isUnsettledCodexInitialization(err error) bool {
	startup, ok := errors.AsType[*codexInitializationFailure](err)
	return ok && !startup.settled
}

func (p *CodexSubagentProvider) classifyThreadStartup(err error) error {
	if isUnsettledCodexInitialization(err) {
		return codexFail(CodexFailureWorkspaceViolation, withFailureClassOverride(err, FailureEnvironment))
	}
	return codexFail(CodexFailureUnavailable, withFailureClassOverride(fmt.Errorf("start codex app-server: %w", err), FailureEnvironment))
}

// Close stdin for a graceful exit before escalating to the managed group.
// A direct leader exit does not establish that the workspace is settled.
func (p *CodexSubagentProvider) stopCodexProcess(proc *CodexAppServerProcess, interruptGrace, shutdownGrace time.Duration, transcript *codexTranscript) error {
	defer proc.stderr.finish()
	_ = proc.Client.Close()
	if proc.waitForTreeExit(interruptGrace) {
		return nil
	}
	transcript.record("process group still active after interrupt grace, terminating")
	_ = proc.sup.TerminateTree(context.Background(), proc.Handle())
	if proc.waitForTreeExit(shutdownGrace) {
		return nil
	}
	transcript.record("process group still active after shutdown grace, force-killing")
	if err := proc.forceStop(defaultCodexProcessReapTimeout); err != nil {
		transcript.record("could not confirm process group exit")
		return codexFail(CodexFailureWorkspaceViolation, withFailureClassOverride(
			errors.New("could not confirm that the Codex process group stopped"), FailureEnvironment))
	}
	return nil
}

func codexAttemptContextFailure(ctx context.Context, timeout time.Duration, completed bool) error {
	cause := ctx.Err()
	if cause == nil || completed {
		return nil
	}
	class := CodexFailureCancelled
	if timeout > 0 && errors.Is(cause, context.DeadlineExceeded) {
		class = CodexFailureTimeout
	}
	return codexFail(class, cause)
}

func (p *CodexSubagentProvider) classifyThreadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return codexFail(CodexFailureCancelled, err)
	}
	return codexFail(CodexFailureProtocolError, err)
}

func (p *CodexSubagentProvider) classifyTurnError(err error) error {
	if _, classified := errors.AsType[*CodexProviderError](err); classified {
		return err
	}
	if _, rejected := errors.AsType[*localResultAdmissionError](err); rejected {
		return err
	}
	var protoErr *CodexProtocolIncompleteError
	if errors.As(err, &protoErr) {
		return err
	}
	return codexFail(CodexFailureProcessCrash, err)
}

var _ SubagentProvider = (*CodexSubagentProvider)(nil)

func (*CodexSubagentProvider) SupportsNativeImageInput() bool { return true }
