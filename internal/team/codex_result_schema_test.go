package team

import (
	"context"
	"errors"
	"slices"
	"strings"
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
	if !slices.Contains(required, "status") || !slices.Contains(required, "summary") || !slices.Contains(required, "files_read") {
		t.Fatalf("schema required = %v, want status, summary, and files_read", required)
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatal("schema has no properties")
	}
	filesReadSchema, _ := props["files_read"].(map[string]any)
	if filesReadSchema == nil {
		t.Fatal("schema has no files_read property")
	}
	filesReadType, _ := filesReadSchema["type"].([]string)
	if !slices.Equal(filesReadType, []string{"array", "null"}) {
		t.Fatalf("files_read type = %v, want nullable array", filesReadType)
	}
	if filesReadSchema["maxItems"] != workerResultProposalMaxFilesRead {
		t.Fatalf("files_read maxItems = %v, want %d", filesReadSchema["maxItems"], workerResultProposalMaxFilesRead)
	}
	filesReadItems, _ := filesReadSchema["items"].(map[string]any)
	if filesReadItems == nil || filesReadItems["type"] != "string" {
		t.Fatalf("files_read items = %v, want non-empty strings", filesReadSchema["items"])
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
	// The real OpenAI structured-output validator behind codex-cli 0.153.4
	// rejects any object schema where a declared property is missing from
	// "required" (confirmed live, not a guess — see codex_result_schema.go's
	// doc comment). Guard that constraint here so a future field addition
	// can't silently regress it.
	for key := range props {
		if !slices.Contains(required, key) {
			t.Fatalf("property %q is not in required %v, want every declared property required (real API strict-mode constraint)", key, required)
		}
	}
}

// TestCodexFilesReadProposalDecodes proves the new schema field is also
// accepted by the strict Hufu-side proposal decoder. The Codex app-server
// schema and the decoder are two separate boundaries; both must agree before
// a grounded review task can satisfy its /files_read verifier.
func TestCodexFilesReadProposalDecodes(t *testing.T) {
	proposal, err := DecodeWorkerResultProposal([]byte(`{"status":"success","summary":"review complete","files_read":["internal/team/coordinator.go"]}`))
	if err != nil {
		t.Fatalf("DecodeWorkerResultProposal: %v", err)
	}
	if len(proposal.FilesRead) != 1 || proposal.FilesRead[0] != "internal/team/coordinator.go" {
		t.Fatalf("FilesRead = %#v, want the Codex-reported path", proposal.FilesRead)
	}
}

// runCodexTurnWithFinalOutput scripts a fake server that acknowledges
// turn/start, then signals turn/completed with a Turn whose items embed
// finalOutput as the terminal agentMessage — mirroring the real protocol,
// where the completion notification itself carries the schema-constrained
// final answer (no separate thread/read call).
func runCodexTurnWithFinalOutput(t *testing.T, finalOutput string, extraNotifications ...fakeCodexNotification) (CodexTurnResult, error) {
	return runCodexTurnWithOptions(t, finalOutput, codexTurnOptions{}, extraNotifications...)
}

func runCodexTurnWithOptions(t *testing.T, finalOutput string, options codexTurnOptions, extraNotifications ...fakeCodexNotification) (CodexTurnResult, error) {
	t.Helper()
	turn := map[string]any{
		"id":     "turn-1",
		"status": "completed",
		"items": []map[string]any{
			{"type": "agentMessage", "phase": "final_answer", "text": finalOutput},
		},
	}
	notifications := append(append([]fakeCodexNotification(nil), extraNotifications...),
		fakeCodexNotification{Method: "turn/completed", Params: rawJSON(t, map[string]any{"threadId": "thread-1", "turn": turn})})
	server := startFakeCodexServer(t, []fakeCodexStep{
		{Result: rawJSON(t, map[string]any{"turn": map[string]any{"id": "turn-1"}}), Notifications: notifications},
	})
	defer server.Client.Close()
	return codexRunTurn(t.Context(), server.Client, "thread-1", "do the work", options)
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
// comes from that notification's own embedded turn items (§13.4).
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

func TestCodexActivitySummaryIsSafeAndUseful(t *testing.T) {
	activity, ok := codexActivityForNotification(CodexNotification{
		Method: "item/started",
		Params: rawJSON(t, map[string]any{
			"item": map[string]any{"type": "commandExecution", "command": "secret command"},
		}),
	}, false)
	if !ok {
		t.Fatal("expected item/started to produce an activity summary")
	}
	if activity.Message != "Codex item started (commandExecution)" {
		t.Fatalf("activity message = %q, want a safe commandExecution summary", activity.Message)
	}
	if strings.Contains(activity.Message, "secret command") {
		t.Fatalf("activity message exposed command contents: %q", activity.Message)
	}
}

func TestCodexActivityDetailIncludesUsefulBoundedContentAndRedactsSecrets(t *testing.T) {
	activity, ok := codexActivityForNotification(CodexNotification{
		Method: "item/completed",
		Params: rawJSON(t, map[string]any{
			"item": map[string]any{
				"type":             "commandExecution",
				"command":          "go test ./...",
				"status":           "completed",
				"exitCode":         0,
				"aggregatedOutput": "all tests passed API_KEY=super-secret",
			},
		}),
	}, true)
	if !ok {
		t.Fatal("expected detailed item activity")
	}
	for _, want := range []string{"command=", "go test ./...", "status=completed", "exit_code=0", "all tests passed", "[REDACTED]"} {
		if !strings.Contains(activity.Message, want) {
			t.Fatalf("activity message = %q, want it to contain %q", activity.Message, want)
		}
	}
	if strings.Contains(activity.Message, "super-secret") {
		t.Fatalf("activity message exposed secret: %q", activity.Message)
	}
}

func TestCodexDetailedActivitySurfacesDeltasOnlyWhenRequested(t *testing.T) {
	notification := CodexNotification{
		Method: "item/commandExecution/outputDelta",
		Params: rawJSON(t, map[string]any{"delta": "go test: PASS"}),
	}
	if activity, ok := codexActivityForNotification(notification, false); ok || activity.Message != "" {
		t.Fatalf("non-detailed activity = %#v, %t; want omitted", activity, ok)
	}
	activity, ok := codexActivityForNotification(notification, true)
	if !ok || !strings.Contains(activity.Message, "go test: PASS") {
		t.Fatalf("detailed activity = %#v, %t; want command output", activity, ok)
	}
}

func TestCodexDetailedTurnReportsFinalResponse(t *testing.T) {
	var activities []codexTurnActivity
	result, err := runCodexTurnWithOptions(t, validProposalJSON(`"details":"review complete"`), codexTurnOptions{
		DetailedOutput: true,
		OnActivity: func(activity codexTurnActivity) {
			activities = append(activities, activity)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Proposal == nil {
		t.Fatal("expected decoded proposal")
	}
	if len(activities) == 0 || !strings.Contains(activities[len(activities)-1].Message, "review complete") {
		t.Fatalf("activities = %#v, want final response detail", activities)
	}
}

func TestCodexTurnRejectsUnsupportedReasoningEffortBeforeRPC(t *testing.T) {
	server := startFakeCodexServer(t, nil)
	defer server.Client.Close()

	_, err := codexRunTurn(context.Background(), server.Client, "thread-1", "do the work", codexTurnOptions{ReasoningEffort: "maximum"})
	if err == nil || !strings.Contains(err.Error(), "unsupported Codex reasoning effort") {
		t.Fatalf("err = %v, want unsupported-effort validation error", err)
	}
	if calls := server.CallLog(t); len(calls) != 0 {
		t.Fatalf("RPC calls = %v, want none for invalid effort", calls)
	}
}
