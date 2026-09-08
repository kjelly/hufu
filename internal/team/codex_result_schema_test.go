package team

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// Phase 4 tests (spec.md §36 PR-10): the Codex output schema and the final
// thread/read extraction, against a real fake app-server subprocess.

// TestCodexOutputSchemaMatchesWorkerResultProposal proves the generated
// schema requires status/summary, enumerates the exact allowed status
// values, forbids unknown fields, and never exposes a trusted Hufu field
// (§14.5).
func TestCodexOutputSchemaMatchesWorkerResultProposal(t *testing.T) {
	schema := codexWorkerResultProposalSchema()
	if schema["additionalProperties"] != false {
		t.Fatalf("schema additionalProperties = %v, want false", schema["additionalProperties"])
	}
	required, _ := schema["required"].([]string)
	if !slices.Contains(required, "status") || !slices.Contains(required, "summary") {
		t.Fatalf("schema required = %v, want status and summary", required)
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatal("schema has no properties")
	}
	statusSchema, _ := props["status"].(map[string]any)
	enumVals, _ := statusSchema["enum"].([]string)
	wantStatuses := []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked}
	if !slices.Equal(enumVals, wantStatuses) {
		t.Fatalf("status enum = %v, want %v", enumVals, wantStatuses)
	}
	for _, forbidden := range []string{"task_id", "run_id", "attempt", "agent", "provider", "sha256", "receipt_ids", "verification", "evidence"} {
		if _, ok := props[forbidden]; ok {
			t.Fatalf("schema exposes trusted field %q, want it absent", forbidden)
		}
	}
}

// runCodexTurnWithFinalOutput scripts a fake server that acknowledges
// turn/start, immediately signals turn/completed, then returns finalOutput
// from thread/read.
func runCodexTurnWithFinalOutput(t *testing.T, finalOutput string, extraNotifications ...fakeCodexNotification) (CodexTurnResult, error) {
	t.Helper()
	notifications := append(append([]fakeCodexNotification(nil), extraNotifications...),
		fakeCodexNotification{Method: "turn/completed", Params: rawJSON(t, map[string]any{"turn_id": "turn-1"})})
	server := startFakeCodexServer(t, []fakeCodexStep{
		{Result: rawJSON(t, map[string]any{"turn_id": "turn-1"}), Notifications: notifications},
		{Result: rawJSON(t, map[string]any{"final_output": finalOutput})},
	})
	defer server.Client.Close()
	return codexRunTurn(context.Background(), server.Client, "thread-1", "do the work", "")
}

// TestCodexMissingProposalIsProtocolIncomplete proves a turn that completes
// without any decodable proposal becomes a typed CodexProtocolIncompleteError
// — never a silent success and never a crash.
func TestCodexMissingProposalIsProtocolIncomplete(t *testing.T) {
	result, err := runCodexTurnWithFinalOutput(t, "")
	var protoErr *CodexProtocolIncompleteError
	if !errors.As(err, &protoErr) {
		t.Fatalf("err = %v, want *CodexProtocolIncompleteError", err)
	}
	if result.Proposal != nil {
		t.Fatalf("result.Proposal = %#v, want nil", result.Proposal)
	}
}

// TestCodexInvalidProposalDoesNotBecomeTaskResult proves a structurally
// invalid proposal (here: a forged trusted field, which the strict schema
// has no slot for) is treated exactly like a missing one — it never reaches
// the point of being usable as a TaskResult.
func TestCodexInvalidProposalDoesNotBecomeTaskResult(t *testing.T) {
	forged := `{"status":"success","summary":"done","task_id":"forged-task"}`
	result, err := runCodexTurnWithFinalOutput(t, forged)
	var protoErr *CodexProtocolIncompleteError
	if !errors.As(err, &protoErr) {
		t.Fatalf("err = %v, want *CodexProtocolIncompleteError", err)
	}
	if result.Proposal != nil {
		t.Fatalf("result.Proposal = %#v, want nil", result.Proposal)
	}
	if result.RawFinalOutput != forged {
		t.Fatalf("RawFinalOutput = %q, want the raw forged output preserved for diagnostics", result.RawFinalOutput)
	}
}

// TestCodexTerminalReadWinsOverDroppedActivityNotification proves the
// driver never depends on non-terminal streaming notifications for
// correctness: even interspersed with unrelated/malformed activity
// notifications, only turn/completed matters, and the final proposal always
// comes from the authoritative thread/read (§13.4).
func TestCodexTerminalReadWinsOverDroppedActivityNotification(t *testing.T) {
	noise := []fakeCodexNotification{
		{Method: "item/started", Params: rawJSON(t, map[string]any{"unexpected": "shape"})},
		{Method: "item/commandExecution/outputDelta", Params: rawJSON(t, "not even an object")},
		{Method: "turn/plan/updated", Params: nil},
	}
	result, err := runCodexTurnWithFinalOutput(t, validProposalJSON(""), noise...)
	if err != nil {
		t.Fatal(err)
	}
	if result.Proposal == nil || result.Proposal.Status != TaskResultStatusSuccess {
		t.Fatalf("result.Proposal = %#v, want a successful decoded proposal despite the dropped/noisy activity notifications", result.Proposal)
	}
}
