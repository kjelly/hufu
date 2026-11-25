package team

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

// This file builds Codex's specific thread/turn vocabulary on top of the
// generic CodexRPCClient (docs/architecture/execution-runtime.md).
//
// Field shapes here are verified against the real, installed codex CLI
// (codex-cli 0.153.4, `codex app-server`, JSON Schema dumped via
// `codex app-server generate-json-schema` and a live stdio round trip — not
// a guess): camelCase throughout, `initialize` takes a `clientInfo` object
// rather than a flat client name, `turn/start.input` is a `UserInput[]`
// array rather than a plain string, thread ids live at `result.thread.id`
// (not a flat `thread_id`), and a `turn/completed` notification's own
// `turn.items` already carries the schema-constrained final assistant
// message — `thread/read` is not needed to obtain it, and the app-server
// itself deprecates full-history `thread/read` for this purpose in favor of
// reading the notification/turn payload directly.
const (
	codexMethodInitialize    = "initialize"
	codexMethodThreadStart   = "thread/start"
	codexMethodThreadResume  = "thread/resume"
	codexMethodTurnStart     = "turn/start"
	codexMethodTurnInterrupt = "turn/interrupt"

	// codexNotificationTurnCompleted is the one notification the driver
	// requires for correctness. Other notifications are advisory telemetry;
	// the driver must tolerate them, but selected high-signal events are
	// surfaced to Hufu's status reporter so a quiet coding turn remains
	// observable.
	codexNotificationTurnCompleted = "turn/completed"
	codexTurnHeartbeatInterval     = 10 * time.Second
)

const (
	hufuCodexClientName    = "hufu"
	hufuCodexClientVersion = "0.1.0"
)

type codexClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// codexInitializeResult's fields are read from the real app-server's
// response but are not load-bearing for Hufu's driver — only call success
// matters before any thread operation (§13.2 step 8).
type codexInitializeResult struct {
	UserAgent string `json:"userAgent,omitempty"`
	CodexHome string `json:"codexHome,omitempty"`
}

func codexInitialize(ctx context.Context, client *CodexRPCClient, onError func(jsonRPCResponse) error) (codexInitializeResult, error) {
	var result codexInitializeResult
	params := struct {
		ClientInfo codexClientInfo `json:"clientInfo"`
	}{ClientInfo: codexClientInfo{Name: hufuCodexClientName, Version: hufuCodexClientVersion}}
	if err := client.call(ctx, codexMethodInitialize, params, &result, onError); err != nil {
		return codexInitializeResult{}, fmt.Errorf("codex initialize: %w", err)
	}
	return result, nil
}

// CodexThreadConfig is what Hufu requires of a thread — the assertion side
// of §14.1's "Hufu MUST verify the returned effective: cwd; sandbox; model".
type CodexThreadConfig struct {
	CWD     string
	Model   string
	Sandbox string // "read-only" | "workspace-write" | "danger-full-access"
	// NetworkDisabled requires the effective sandbox response to explicitly
	// report networkAccess=false before Hufu permits turn/start. It is kept
	// separate from the default-allowed case so older protocol fixtures and
	// providers that do not request no-net do not gain an implicit denial.
	NetworkDisabled bool
	// DeveloperInstructions is thread-scoped in the real protocol (there is
	// no per-turn developer-instruction field) — it applies to every turn
	// run on this thread for as long as the thread lives.
	DeveloperInstructions string
}

// CodexEffectiveThreadState is what the app-server actually reports for a
// (newly started or resumed) thread.
type CodexEffectiveThreadState struct {
	ThreadID           string
	CWD                string
	Model              string
	Sandbox            string
	NetworkAccess      bool
	NetworkAccessKnown bool
}

type codexThreadRef struct {
	ID string `json:"id"`
}

type codexThreadStartParams struct {
	CWD                   string `json:"cwd"`
	ApprovalPolicy        string `json:"approvalPolicy"`
	Sandbox               string `json:"sandbox"`
	Model                 string `json:"model,omitempty"`
	DeveloperInstructions string `json:"developerInstructions,omitempty"`
}

// codexThreadStartResult/codexThreadResumeResult decode only the fields
// Hufu's driver actually needs from the real, much larger response (Thread,
// instructionSources, approvalsReviewer, reasoningEffort, ... are all
// silently ignored — this is a plain json.Unmarshal, not a strict decode,
// since this is a trusted transport-internal shape, not the untrusted
// external-result boundary). Sandbox is decoded as raw JSON because the
// response's sandbox is a policy object (e.g. {"type":"readOnly",...}), not
// the plain string the request side accepts — see codexSandboxPolicyMode.
type codexThreadStartResult struct {
	Thread  codexThreadRef  `json:"thread"`
	CWD     string          `json:"cwd"`
	Model   string          `json:"model"`
	Sandbox json.RawMessage `json:"sandbox"`
}

type codexThreadResumeParams struct {
	ThreadID string `json:"threadId"`
	CWD      string `json:"cwd,omitempty"`
	// Sandbox lets a resume request a narrower sandbox than the thread's
	// original (§23: a result-only repair resume MUST force read-only,
	// regardless of what sandbox the original task's turn ran under).
	// Omitted for an ordinary retry/restart resume where the caller expects
	// the thread's existing sandbox back unchanged.
	Sandbox               string `json:"sandbox,omitempty"`
	DeveloperInstructions string `json:"developerInstructions,omitempty"`
}

type codexThreadResumeResult struct {
	Thread  codexThreadRef  `json:"thread"`
	CWD     string          `json:"cwd"`
	Model   string          `json:"model"`
	Sandbox json.RawMessage `json:"sandbox"`
}

// codexSandboxRank orders sandbox modes from least to most permissive so a
// widening mismatch can be detected generically (§14.1: "A mismatch that
// widens permissions MUST fail before the coding turn").
var codexSandboxRank = map[string]int{
	"read-only":          0,
	"workspace-write":    1,
	"danger-full-access": 2,
}

// codexSandboxPolicyMode maps the real app-server's response-side sandbox
// policy object (a discriminated union: {"type":"readOnly"|"workspaceWrite"|
// "dangerFullAccess", ...}) back to Hufu's plain request-side mode string.
// The request side already accepts (and the real server already honors) the
// plain string directly — this mapping exists only because the response
// echoes back the richer object form, confirmed via a live thread/start.
func codexSandboxPolicyMode(raw json.RawMessage) string {
	var policy struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		return ""
	}
	switch policy.Type {
	case "readOnly":
		return codexSandboxReadOnly
	case "workspaceWrite":
		return codexSandboxWorkspaceWrite
	case "dangerFullAccess":
		return "danger-full-access"
	default:
		return ""
	}
}

func codexSandboxPolicyNetworkAccess(raw json.RawMessage) (allowed, known bool) {
	var policy struct {
		NetworkAccess *bool `json:"networkAccess"`
	}
	if err := json.Unmarshal(raw, &policy); err != nil || policy.NetworkAccess == nil {
		return false, false
	}
	return *policy.NetworkAccess, true
}

func validateCodexEffectiveThreadState(cfg CodexThreadConfig, effective CodexEffectiveThreadState) error {
	if effective.CWD != cfg.CWD {
		return fmt.Errorf("codex effective cwd %q does not match requested %q", effective.CWD, cfg.CWD)
	}
	if cfg.Model != "" && effective.Model != cfg.Model {
		return fmt.Errorf("codex effective model %q does not match bound model %q; provider model fallback is disabled in v1", effective.Model, cfg.Model)
	}
	wantRank, wantOK := codexSandboxRank[cfg.Sandbox]
	gotRank, gotOK := codexSandboxRank[effective.Sandbox]
	if !wantOK {
		return fmt.Errorf("codex requested sandbox %q is not a recognized mode", cfg.Sandbox)
	}
	if !gotOK {
		return fmt.Errorf("codex effective sandbox %q is not a recognized mode", effective.Sandbox)
	}
	if gotRank > wantRank {
		return fmt.Errorf("codex effective sandbox %q widens requested %q", effective.Sandbox, cfg.Sandbox)
	}
	if cfg.NetworkDisabled {
		if !effective.NetworkAccessKnown {
			return fmt.Errorf("codex effective sandbox omitted networkAccess while no-net is required")
		}
		if effective.NetworkAccess {
			return fmt.Errorf("codex effective sandbox permits network access while no-net is required")
		}
	}
	return nil
}

// CodexSessionResumeMismatchError is a thread/resume that answered with a
// thread other than the durable one. No binding or turn follows it.
type CodexSessionResumeMismatchError struct {
	Requested string
	Returned  string
}

func (e *CodexSessionResumeMismatchError) Error() string {
	return fmt.Sprintf("codex thread/resume returned thread %q, want the durable session %q", e.Returned, e.Requested)
}

// FailureClassOverride blocks the task for reconciliation: one mismatched
// resume does not prove the durable session is unrecoverable.
func (e *CodexSessionResumeMismatchError) FailureClassOverride() TaskFailureClass {
	return FailureSessionResumeMismatch
}

// codexStartOrResumeThread implements §14.1/§14.2: start a fresh thread when
// no durable session exists, or resume by thread id when one does. Either
// way, the effective cwd/model/sandbox is verified before onSessionBound is
// invoked — and onSessionBound (the durable persistence callback) always
// runs before the caller is allowed to proceed to turn/start, so a crash
// between the two can never leave an in-flight turn with no durable session
// record (§7.4, "Session binding MUST be persisted before starting a later
// retry/resume that depends on it").
func codexStartOrResumeThread(ctx context.Context, client *CodexRPCClient, existingThreadID string, cfg CodexThreadConfig, onSessionBound func(CodexEffectiveThreadState) error) (CodexEffectiveThreadState, error) {
	var effective CodexEffectiveThreadState
	if existingThreadID != "" {
		if !client.providerIdentitySafe(existingThreadID) {
			return CodexEffectiveThreadState{}, errCodexPrivateProviderIdentity
		}
		var result codexThreadResumeResult
		params := codexThreadResumeParams{ThreadID: existingThreadID, CWD: cfg.CWD, Sandbox: cfg.Sandbox, DeveloperInstructions: cfg.DeveloperInstructions}
		if err := client.Call(ctx, codexMethodThreadResume, params, &result); err != nil {
			return CodexEffectiveThreadState{}, fmt.Errorf("codex thread/resume: %w", err)
		}
		// Resuming must continue the durable session. Binding a different
		// thread here would silently give the task a second session.
		if !client.providerIdentitySafe(result.Thread.ID) {
			return CodexEffectiveThreadState{}, errCodexPrivateProviderIdentity
		}
		if result.Thread.ID != existingThreadID {
			return CodexEffectiveThreadState{}, client.redactStderrError(&CodexSessionResumeMismatchError{Requested: existingThreadID, Returned: result.Thread.ID})
		}
		networkAccess, networkKnown := codexSandboxPolicyNetworkAccess(result.Sandbox)
		effective = CodexEffectiveThreadState{ThreadID: result.Thread.ID, CWD: result.CWD, Model: result.Model, Sandbox: codexSandboxPolicyMode(result.Sandbox), NetworkAccess: networkAccess, NetworkAccessKnown: networkKnown}
	} else {
		var result codexThreadStartResult
		params := codexThreadStartParams{
			CWD: cfg.CWD, ApprovalPolicy: "never", Sandbox: cfg.Sandbox, Model: cfg.Model,
			DeveloperInstructions: cfg.DeveloperInstructions,
		}
		if err := client.Call(ctx, codexMethodThreadStart, params, &result); err != nil {
			return CodexEffectiveThreadState{}, fmt.Errorf("codex thread/start: %w", err)
		}
		networkAccess, networkKnown := codexSandboxPolicyNetworkAccess(result.Sandbox)
		effective = CodexEffectiveThreadState{ThreadID: result.Thread.ID, CWD: result.CWD, Model: result.Model, Sandbox: codexSandboxPolicyMode(result.Sandbox), NetworkAccess: networkAccess, NetworkAccessKnown: networkKnown}
	}

	if !client.providerIdentitySafe(effective.ThreadID) {
		return CodexEffectiveThreadState{}, errCodexPrivateProviderIdentity
	}
	if err := validateCodexEffectiveThreadState(cfg, effective); err != nil {
		return CodexEffectiveThreadState{}, client.redactStderrError(err)
	}
	if onSessionBound != nil {
		if err := onSessionBound(effective); err != nil {
			return CodexEffectiveThreadState{}, fmt.Errorf("persist codex session binding: %w", err)
		}
	}
	return effective, nil
}

type codexTurnStartParams struct {
	ThreadID     string           `json:"threadId"`
	CWD          string           `json:"cwd,omitempty"`
	Input        []codexUserInput `json:"input"`
	OutputSchema json.RawMessage  `json:"outputSchema"`
	Effort       string           `json:"effort,omitempty"`
}

type codexTurnOptions struct {
	CWD               string
	Input             []codexUserInput
	ResultProjection  *resultClaimsProjection
	RuntimeCompletion bool
	ReasoningEffort   string
	DetailedOutput    bool
	OnTurnStarted     func(turnID string) error
	OnActivity        func(codexTurnActivity)
	diagnosticSecrets *utils.SecretRedactionScope
}

// codexTurnActivity is intentionally a safe summary rather than a raw JSON-
// RPC payload. App-server notifications may contain command arguments,
// prompts, or other sensitive workspace/provider data.
type codexTurnActivity struct {
	Method    string
	Message   string
	Elapsed   time.Duration
	Heartbeat bool
}

func normalizeCodexReasoningEffort(raw string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(raw))
	if effort == "" {
		return "", nil
	}
	if !agent.ValidReasoningEfforts[effort] {
		return "", fmt.Errorf("unsupported Codex reasoning effort %q (want high, medium, low, or none)", raw)
	}
	return effort, nil
}

type codexTurnRef struct {
	ID string `json:"id"`
}

type codexTurnStartResult struct {
	ThreadID json.RawMessage `json:"threadId,omitempty"`
	Turn     codexTurnRef    `json:"turn"`
}

func (r *codexTurnStartResult) UnmarshalJSON(data []byte) error {
	type wire codexTurnStartResult
	if err := jsonv2.Unmarshal(data, (*wire)(r)); err != nil {
		return fmt.Errorf("invalid or ambiguous turn acknowledgement")
	}
	return nil
}

type codexTurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// codexTurnInterrupt sends turn/interrupt, the first step of §16.1's
// cancellation flow: a graceful, in-protocol request to stop the current
// turn before Hufu escalates to signaling the process tree. Both threadId
// and turnId are required by the real protocol; callers with no known
// in-flight turn id (a race with turn/start's own ack) should skip calling
// this and rely on process-tree escalation instead.
func codexTurnInterrupt(ctx context.Context, client *CodexRPCClient, threadID, turnID string) error {
	if err := client.Call(ctx, codexMethodTurnInterrupt, codexTurnInterruptParams{ThreadID: threadID, TurnID: turnID}, nil); err != nil {
		return fmt.Errorf("codex turn/interrupt: %w", err)
	}
	return nil
}

type codexTurnError struct {
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// codexTurn mirrors the real protocol's Turn object closely enough for
// Hufu's needs (id/items/status/error) — confirmed live via
// turn/completed's own notification payload, which already carries a full
// Turn including items, not just an id.
type codexTurn struct {
	ID     json.RawMessage   `json:"id"`
	Items  []json.RawMessage `json:"items"`
	Status string            `json:"status"`
	Error  *codexTurnError   `json:"error,omitempty"`

	diagnosticsUnsafe bool
	diagnosticSecrets *utils.SecretRedactionScope
}

type codexTurnCompletedParams struct {
	ThreadID json.RawMessage `json:"threadId"`
	Turn     codexTurn       `json:"turn"`
}

func codexOptionalIDMatches(raw json.RawMessage, expected string) bool {
	if len(raw) == 0 {
		return true
	}
	var id *string
	return json.Unmarshal(raw, &id) == nil && id != nil && *id == expected
}

// codexFinalAgentMessage returns the last item marked as the turn's final
// answer — the one whose text is the schema-constrained JSON Hufu asked for
// via outputSchema. Scanning from the end handles a turn with more than one
// agentMessage item (intermediate progress messages before the final one).
func codexFinalAgentMessage(items []json.RawMessage) (string, bool) {
	for i := len(items) - 1; i >= 0; i-- {
		var item struct {
			Type  string `json:"type"`
			Phase string `json:"phase"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(items[i], &item); err == nil && item.Type == "agentMessage" && item.Phase == "final_answer" {
			return item.Text, true
		}
	}
	return "", false
}

// CodexProtocolIncompleteError classifies a turn that completed without ever
// producing a valid WorkerResultProposal — missing or structurally invalid.
// It is a distinct type (not a plain fmt.Errorf) so callers can recognize
// "the workspace may already have changed but there is no trusted result"
// and route to result-only repair (§23) rather than a generic execution
// failure.
type CodexProtocolIncompleteError struct {
	Reason error
}

func (e *CodexProtocolIncompleteError) Error() string {
	return fmt.Sprintf("codex turn completed without a valid WorkerResultProposal: %v", e.Reason)
}

func (e *CodexProtocolIncompleteError) Unwrap() error { return e.Reason }

// CodexTurnResult is the outcome of one coding turn.
type CodexTurnResult struct {
	TurnID         string
	Proposal       *WorkerResultProposal
	RawFinalOutput string

	diagnosticsUnsafe bool
	redactedOutput    string
	diagnosticSecrets *utils.SecretRedactionScope
}

// codexRunTurn implements §14.4/§14.5 and §13.4: send the already-compiled
// Hufu prompt with a strict output schema, wait for the terminal
// turn/completed notification (never assuming any non-terminal notification
// arrived), then extract and strictly decode its embedded final answer as a
// WorkerResultProposal. A missing/invalid proposal, or a turn the app-server
// itself reports as failed, becomes a CodexProtocolIncompleteError, never a
// fabricated success.
//
// options.OnTurnStarted, if non-nil, runs with the authenticated turn ID
// before the worker consumes the notifications for that turn.
func codexRunTurn(ctx context.Context, client *CodexRPCClient, threadID, prompt string, options codexTurnOptions) (CodexTurnResult, error) {
	effort, err := normalizeCodexReasoningEffort(options.ReasoningEffort)
	if err != nil {
		return CodexTurnResult{}, fmt.Errorf("codex turn/start: %w", err)
	}
	schema, err := json.Marshal(codexWorkerResultSchema(options.ResultProjection, options.RuntimeCompletion))
	if err != nil {
		return CodexTurnResult{}, fmt.Errorf("codex turn/start: marshal output schema: %w", err)
	}
	var startResult codexTurnStartResult
	params := codexTurnStartParams{
		ThreadID:     threadID,
		CWD:          options.CWD,
		Input:        []codexUserInput{{Type: "text", Text: prompt}},
		OutputSchema: schema,
		Effort:       effort,
	}
	if len(options.Input) > 0 {
		params.Input = options.Input
	}
	diagnostics := client.newDiagnostics(options.diagnosticSecrets)
	if err := client.call(ctx, codexMethodTurnStart, params, &startResult, func(reply jsonRPCResponse) error {
		return client.redactTurnStartError(reply, diagnostics, threadID)
	}); err != nil {
		return CodexTurnResult{diagnosticSecrets: diagnostics.secrets}, fmt.Errorf("codex turn/start: %w", client.redactStderrError(err))
	}
	turnID := startResult.Turn.ID
	if !client.providerIdentitySafe(turnID) {
		return CodexTurnResult{diagnosticSecrets: diagnostics.secrets}, codexFail(CodexFailureProtocolError, errCodexPrivateProviderIdentity)
	}
	if strings.TrimSpace(turnID) == "" {
		return CodexTurnResult{}, fmt.Errorf("codex turn/start: response did not identify the active turn")
	}
	if !codexOptionalIDMatches(startResult.ThreadID, threadID) {
		return CodexTurnResult{}, fmt.Errorf("codex turn/start: response identifies a different thread")
	}
	if options.OnTurnStarted != nil {
		if err := options.OnTurnStarted(turnID); err != nil {
			return CodexTurnResult{TurnID: turnID}, err
		}
	}

	if target := client.notificationTarget.Load(); target != nil && target.turnID == turnID && target.admissionErr != nil {
		return CodexTurnResult{TurnID: turnID}, codexFail(CodexFailureProtocolError, target.admissionErr)
	}

	completed, err := waitForCodexTurnCompleted(ctx, client, threadID, turnID, options.OnActivity, options.DetailedOutput, diagnostics)
	if err != nil {
		return CodexTurnResult{TurnID: turnID, diagnosticSecrets: diagnostics.secrets}, err
	}
	result := CodexTurnResult{TurnID: turnID, diagnosticsUnsafe: completed.diagnosticsUnsafe, diagnosticSecrets: completed.diagnosticSecrets}
	if completed.Status != "completed" {
		status := failedCodexTurnStatus(completed.Status)
		msg := status
		if completed.Error != nil && completed.Error.Message != "" {
			msg = completed.Error.Message
		}
		return result, fmt.Errorf("codex turn ended with status %q: %s", status, msg)
	}

	finalText, ok := codexFinalAgentMessage(completed.Items)
	if !ok {
		return result,
			&CodexProtocolIncompleteError{Reason: fmt.Errorf("turn/completed carried no final agent message")}
	}

	result.RawFinalOutput = finalText
	if completed.diagnosticsUnsafe {
		result.redactedOutput = "[invalid provider JSON]"
	} else {
		result.redactedOutput = utils.TruncateRunes(redactCodexProviderText(finalText, completed.diagnosticSecrets), codexFailureEvidenceMaxRunes)
	}
	proposal, decodeErr := decodeWorkerResultProposal([]byte(finalText), options.RuntimeCompletion)
	status := "invalid"
	if decodeErr == nil {
		result.Proposal = proposal
		status = proposal.Status
	}
	if options.DetailedOutput && !completed.diagnosticsUnsafe {
		reportCodexFinalResponse(options.OnActivity, result.redactedOutput, status)
	}
	if decodeErr != nil {
		reason := "decode worker result proposal: malformed response"
		if !completed.diagnosticsUnsafe {
			reason = completed.diagnosticSecrets.RedactText(decodeErr.Error())
		}
		return result, &CodexProtocolIncompleteError{Reason: errors.New(reason)}
	}
	return result, nil
}

// waitForCodexTurnCompleted blocks on the notification stream until the
// terminal turn/completed notification arrives. Non-terminal notifications
// remain advisory (§13.3: "MUST NOT require every non-terminal notification
// for correctness"), but selected safe summaries and periodic heartbeats are
// forwarded to onActivity. Since v1 runs exactly one app-server process per
// attempt (§13.1) with exactly one active turn, a turn/completed notification
// can omit thread and turn IDs. Any supplied ID must match the active turn.
func waitForCodexTurnCompleted(ctx context.Context, client *CodexRPCClient, threadID, turnID string, onActivity func(codexTurnActivity), detailed bool, diagnostics *codexTurnDiagnostics) (codexTurn, error) {
	start := time.Now()
	var heartbeat <-chan time.Time
	if onActivity != nil {
		heartbeat = time.Tick(codexTurnHeartbeatInterval)
	}
	done := client.Done()
	canceled := ctx.Done()
	for {
		if done == nil && len(client.notifications) == 0 {
			return codexTurn{}, codexTurnDisconnected(client)
		}
		select {
		case notif := <-client.notifications:
			client.releaseNotification(notif)
			if notif.Method == codexNotificationTurnCompleted {
				var params codexTurnCompletedParams
				if err := jsonv2.Unmarshal(notif.Params, &params); err != nil {
					continue
				}
				if !codexOptionalIDMatches(params.ThreadID, threadID) || !codexOptionalIDMatches(params.Turn.ID, turnID) {
					continue
				}
				if notif.advisoryDropped {
					diagnostics.discard()
				}
				client.finishDiagnosticProcess(diagnostics)
				diagnostics.learnNotification(notif)
				diagnostics.complete(&params.Turn, onActivity, time.Since(start), detailed)
				return params.Turn, nil
			}
			if address, valid := notif.readAddress(); !valid || !address.matches(threadID, turnID) {
				continue
			}
			if diagnostics.deferActivity(notif, detailed && onActivity != nil) {
				continue
			}
			if activity, ok := codexActivityForNotification(notif, false, nil); ok && onActivity != nil {
				activity.Elapsed = time.Since(start)
				onActivity(activity)
			}
		case <-heartbeat:
			elapsed := time.Since(start)
			onActivity(codexTurnActivity{
				Method:    "heartbeat",
				Message:   fmt.Sprintf("Codex is still working (%s elapsed)", elapsed.Round(time.Second)),
				Elapsed:   elapsed,
				Heartbeat: true,
			})
		case <-done:
			// The wire reader publishes all preceding notifications before
			// closing, even when a later frame is malformed.
			done = nil
		case <-canceled:
			if client.completedBeforeCancellation() {
				canceled = nil
				continue
			}
			return codexTurn{}, ctx.Err()
		}
	}
}

func codexTurnDisconnected(client *CodexRPCClient) error {
	return fmt.Errorf("codex app-server disconnected while waiting for turn/completed: %w", client.Err())
}

func codexActivityForNotification(notif CodexNotification, detailed bool, secrets *utils.SecretRedactionScope) (codexTurnActivity, bool) {
	message := ""
	switch notif.Method {
	case "turn/started":
		message = "Codex turn started"
	case "thread/status/changed":
		message = "Codex status changed"
	case "turn/plan/updated":
		message = "Codex plan updated"
	case "item/started":
		message = "Codex item started (" + codexNotificationItemType(notif.Params) + ")"
	case "item/completed":
		message = "Codex item completed (" + codexNotificationItemType(notif.Params) + ")"
	case "error", "turn/failed":
		message = "Codex reported an app-server error"
	default:
		return codexTurnActivity{}, false
	}
	if detailed && (notif.Method == "item/started" || notif.Method == "item/completed") {
		if detail := codexNotificationItemDetail(notif.Params, secrets); detail != "" {
			message += ": " + detail
		}
	}
	if strings.HasSuffix(message, ": ") {
		return codexTurnActivity{}, false
	}
	return codexTurnActivity{Method: notif.Method, Message: message}, true
}

func codexNotificationItemType(raw json.RawMessage) string {
	var payload struct {
		Item struct {
			Type string `json:"type"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "item"
	}
	switch payload.Item.Type {
	case "agentMessage", "commandExecution", "fileChange", "mcpToolCall", "reasoning", "webSearch":
		return payload.Item.Type
	default:
		return "item"
	}
}

const codexActivityDetailMaxRunes = 1600

type codexNotificationItem struct {
	Type             string          `json:"type"`
	Phase            string          `json:"phase,omitempty"`
	Status           string          `json:"status,omitempty"`
	Command          json.RawMessage `json:"command,omitempty"`
	ExitCode         *int            `json:"exitCode,omitempty"`
	LegacyExitCode   *int            `json:"exit_code,omitempty"`
	AggregatedOutput string          `json:"aggregatedOutput,omitempty"`
	FormattedOutput  string          `json:"formattedOutput,omitempty"`
	Stdout           string          `json:"stdout,omitempty"`
	Stderr           string          `json:"stderr,omitempty"`
	Text             string          `json:"text,omitempty"`
	SummaryText      string          `json:"summaryText,omitempty"`
}

func readCodexNotificationItem(raw json.RawMessage) (codexNotificationItem, bool) {
	var envelope struct {
		Item *codexNotificationItem `json:"item"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Item == nil {
		return codexNotificationItem{}, false
	}
	return *envelope.Item, true
}

func codexNotificationItemDetail(raw json.RawMessage, secrets *utils.SecretRedactionScope) string {
	item, ok := readCodexNotificationItem(raw)
	if !ok {
		return ""
	}
	if item.Type == "agentMessage" {
		if output := codexActivityText(redactCodexProviderText(item.Text, secrets)); output != "" {
			return "output=" + output
		}
		return ""
	}
	parts := make([]string, 0, 5)
	if command := codexActivityText(secrets.RedactText(codexRawValue(item.Command))); command != "" {
		parts = append(parts, "command="+command)
	}
	if item.Phase != "" {
		parts = append(parts, "phase="+codexActivityText(secrets.RedactText(item.Phase)))
	}
	if item.Status != "" {
		parts = append(parts, "status="+codexActivityText(secrets.RedactText(item.Status)))
	}
	exitCode := item.ExitCode
	if exitCode == nil {
		exitCode = item.LegacyExitCode
	}
	if exitCode != nil {
		parts = append(parts, fmt.Sprintf("exit_code=%d", *exitCode))
	}
	output := firstNonEmpty(item.AggregatedOutput, item.FormattedOutput, item.Stdout, item.Stderr, item.Text, item.SummaryText)
	if output != "" {
		parts = append(parts, "output="+codexActivityText(secrets.RedactText(output)))
	}
	return strings.Join(parts, " ")
}

func codexRawValue(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	return string(raw)
}

func codexActivityText(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\r", "")
	return codexQuoteActivityText(utils.TruncateRunes(value, codexActivityDetailMaxRunes))
}

func codexQuoteActivityText(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\n\"") {
		return strconv.Quote(value)
	}
	return value
}

func reportCodexFinalResponse(onActivity func(codexTurnActivity), finalText, status string) {
	if onActivity == nil {
		return
	}
	message := "Codex final response"
	if status != "" {
		message += " (status=" + status + ")"
	}
	if detail := codexActivityText(finalText); detail != "" {
		message += ": " + detail
	}
	onActivity(codexTurnActivity{Method: codexNotificationTurnCompleted, Message: message})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
