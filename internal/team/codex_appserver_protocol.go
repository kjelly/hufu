package team

import (
	"context"
	"encoding/json"
	"fmt"
)

// This file builds Codex's specific thread/turn vocabulary on top of the
// generic CodexRPCClient (docs/hufu-external-coding-agent-runtime-spec.md
// §12, §13.3, §14). Still no real SubagentProvider wiring (PR-11, Phase 5) —
// these functions are exercised directly against a fake app-server fixture.

const (
	codexMethodInitialize   = "initialize"
	codexMethodThreadStart  = "thread/start"
	codexMethodThreadResume = "thread/resume"
	codexMethodTurnStart    = "turn/start"
	codexMethodThreadRead   = "thread/read"

	// codexNotificationTurnCompleted is the one notification the driver
	// requires for correctness; every other notification is advisory
	// telemetry only (§13.3).
	codexNotificationTurnCompleted = "turn/completed"
)

const hufuCodexClientName = "hufu"

// codexInitializeResult's exact field names are a documented assumption
// pending verification against the real codex app-server (§38 opt-in smoke
// tests); nothing downstream depends on its contents beyond the call
// succeeding before any thread operation (§13.2 step 8).
type codexInitializeResult struct {
	ProtocolVersion string `json:"protocol_version,omitempty"`
	ServerVersion   string `json:"server_version,omitempty"`
}

func codexInitialize(ctx context.Context, client *CodexRPCClient) (codexInitializeResult, error) {
	var result codexInitializeResult
	params := struct {
		ClientName string `json:"client_name"`
	}{ClientName: hufuCodexClientName}
	if err := client.Call(ctx, codexMethodInitialize, params, &result); err != nil {
		return codexInitializeResult{}, fmt.Errorf("codex initialize: %w", err)
	}
	return result, nil
}

// CodexThreadConfig is what Hufu requires of a thread — the assertion side
// of §14.1's "Hufu MUST verify the returned effective: cwd; sandbox; model".
type CodexThreadConfig struct {
	CWD                   string
	Model                 string
	Sandbox               string // "read-only" | "workspace-write" | "danger-full-access"
	RuntimeWorkspaceRoots []string
}

// CodexEffectiveThreadState is what the app-server actually reports for a
// (newly started or resumed) thread.
type CodexEffectiveThreadState struct {
	ThreadID string
	CWD      string
	Model    string
	Sandbox  string
}

type codexThreadStartParams struct {
	Ephemeral             bool     `json:"ephemeral"`
	CWD                   string   `json:"cwd"`
	RuntimeWorkspaceRoots []string `json:"runtime_workspace_roots,omitempty"`
	ApprovalPolicy        string   `json:"approval_policy"`
	Sandbox               string   `json:"sandbox"`
	Model                 string   `json:"model,omitempty"`
}

type codexThreadStartResult struct {
	ThreadID string `json:"thread_id"`
	CWD      string `json:"cwd"`
	Model    string `json:"model"`
	Sandbox  string `json:"sandbox"`
}

type codexThreadResumeParams struct {
	ThreadID string `json:"thread_id"`
}

type codexThreadResumeResult struct {
	ThreadID string `json:"thread_id"`
	CWD      string `json:"cwd"`
	Model    string `json:"model"`
	Sandbox  string `json:"sandbox"`
}

// codexSandboxRank orders sandbox modes from least to most permissive so a
// widening mismatch can be detected generically (§14.1: "A mismatch that
// widens permissions MUST fail before the coding turn").
var codexSandboxRank = map[string]int{
	"read-only":          0,
	"workspace-write":    1,
	"danger-full-access": 2,
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
	return nil
}

// codexStartOrResumeThread implements §14.1/§14.2: start a fresh thread when
// no durable session exists, or resume by thread_id when one does. Either
// way, the effective cwd/model/sandbox is verified before onSessionBound is
// invoked — and onSessionBound (the durable persistence callback) always
// runs before the caller is allowed to proceed to turn/start, so a crash
// between the two can never leave an in-flight turn with no durable session
// record (§7.4, "Session binding MUST be persisted before starting a later
// retry/resume that depends on it").
func codexStartOrResumeThread(ctx context.Context, client *CodexRPCClient, existingThreadID string, cfg CodexThreadConfig, onSessionBound func(CodexEffectiveThreadState) error) (CodexEffectiveThreadState, error) {
	var effective CodexEffectiveThreadState
	if existingThreadID != "" {
		var result codexThreadResumeResult
		if err := client.Call(ctx, codexMethodThreadResume, codexThreadResumeParams{ThreadID: existingThreadID}, &result); err != nil {
			return CodexEffectiveThreadState{}, fmt.Errorf("codex thread/resume: %w", err)
		}
		effective = CodexEffectiveThreadState{ThreadID: existingThreadID, CWD: result.CWD, Model: result.Model, Sandbox: result.Sandbox}
	} else {
		var result codexThreadStartResult
		params := codexThreadStartParams{
			Ephemeral: false, CWD: cfg.CWD, RuntimeWorkspaceRoots: cfg.RuntimeWorkspaceRoots,
			ApprovalPolicy: "never", Sandbox: cfg.Sandbox, Model: cfg.Model,
		}
		if err := client.Call(ctx, codexMethodThreadStart, params, &result); err != nil {
			return CodexEffectiveThreadState{}, fmt.Errorf("codex thread/start: %w", err)
		}
		effective = CodexEffectiveThreadState(result)
	}

	if err := validateCodexEffectiveThreadState(cfg, effective); err != nil {
		return CodexEffectiveThreadState{}, err
	}
	if onSessionBound != nil {
		if err := onSessionBound(effective); err != nil {
			return CodexEffectiveThreadState{}, fmt.Errorf("persist codex session binding: %w", err)
		}
	}
	return effective, nil
}

type codexTurnStartParams struct {
	ThreadID             string          `json:"thread_id"`
	Input                string          `json:"input"`
	DeveloperInstruction string          `json:"developer_instruction,omitempty"`
	OutputSchema         json.RawMessage `json:"output_schema"`
}

type codexTurnStartResult struct {
	TurnID string `json:"turn_id"`
}

type codexThreadReadParams struct {
	ThreadID string `json:"thread_id"`
}

type codexThreadReadResult struct {
	FinalOutput string `json:"final_output"`
}

// CodexProtocolIncompleteError classifies a turn that completed without ever
// producing a valid WorkerResultProposal — missing or structurally invalid.
// It is a distinct type (not a plain fmt.Errorf) so callers can recognize
// "the workspace may already have changed but there is no trusted result"
// and route to result-only repair (§23) rather than a generic execution
// failure.
type CodexProtocolIncompleteError struct {
	Reason         error
	RawFinalOutput string
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
}

// codexRunTurn implements §14.4/§14.5 and §13.4: send the already-compiled
// Hufu prompt with a strict output schema, wait for the terminal
// turn/completed notification (never assuming any non-terminal notification
// arrived), then perform the authoritative thread/read and strictly decode
// its final output as a WorkerResultProposal. A missing or invalid proposal
// becomes a CodexProtocolIncompleteError, never a fabricated success.
func codexRunTurn(ctx context.Context, client *CodexRPCClient, threadID, prompt, developerInstruction string) (CodexTurnResult, error) {
	schema, err := json.Marshal(codexWorkerResultProposalSchema())
	if err != nil {
		return CodexTurnResult{}, fmt.Errorf("codex turn/start: marshal output schema: %w", err)
	}
	var startResult codexTurnStartResult
	params := codexTurnStartParams{ThreadID: threadID, Input: prompt, DeveloperInstruction: developerInstruction, OutputSchema: schema}
	if err := client.Call(ctx, codexMethodTurnStart, params, &startResult); err != nil {
		return CodexTurnResult{}, fmt.Errorf("codex turn/start: %w", err)
	}

	if err := waitForCodexTurnCompleted(ctx, client, startResult.TurnID); err != nil {
		return CodexTurnResult{TurnID: startResult.TurnID}, err
	}

	var readResult codexThreadReadResult
	if err := client.Call(ctx, codexMethodThreadRead, codexThreadReadParams{ThreadID: threadID}, &readResult); err != nil {
		return CodexTurnResult{TurnID: startResult.TurnID}, fmt.Errorf("codex thread/read: %w", err)
	}

	proposal, decodeErr := DecodeWorkerResultProposal([]byte(readResult.FinalOutput))
	if decodeErr != nil {
		return CodexTurnResult{TurnID: startResult.TurnID, RawFinalOutput: readResult.FinalOutput},
			&CodexProtocolIncompleteError{Reason: decodeErr, RawFinalOutput: readResult.FinalOutput}
	}
	return CodexTurnResult{TurnID: startResult.TurnID, Proposal: proposal, RawFinalOutput: readResult.FinalOutput}, nil
}

// waitForCodexTurnCompleted blocks on the notification stream until the
// terminal turn/completed notification arrives, discarding every other
// notification along the way (§13.3: "MUST NOT require every non-terminal
// notification for correctness"). Since v1 runs exactly one app-server
// process per attempt (§13.1) with exactly one active turn, a
// turn/completed notification that omits turn_id is unambiguous; one that
// includes it is still checked for an exact match as a defensive measure.
func waitForCodexTurnCompleted(ctx context.Context, client *CodexRPCClient, turnID string) error {
	for {
		select {
		case notif := <-client.Notifications():
			if notif.Method != codexNotificationTurnCompleted {
				continue
			}
			var params struct {
				TurnID string `json:"turn_id"`
			}
			_ = json.Unmarshal(notif.Params, &params)
			if params.TurnID == "" || turnID == "" || params.TurnID == turnID {
				return nil
			}
		case <-client.Done():
			err := client.Err()
			if err == nil {
				err = fmt.Errorf("codex app-server disconnected")
			}
			return fmt.Errorf("codex app-server disconnected while waiting for turn/completed: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
