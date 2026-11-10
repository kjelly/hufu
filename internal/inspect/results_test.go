package inspect

import (
	"bytes"
	"crypto/sha256"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

func appendHistoricalFixture(t *testing.T, store *team.EventStore, event team.RunEvent, payload any) team.RunEvent {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event.Payload = encoded
	if event.Actor == "" {
		event.Actor = "worker"
	}
	if event.Timestamp == "" {
		event.Timestamp = "2026-10-13T10:00:00Z"
	}
	durable, err := store.AppendPersisted(event)
	if err != nil {
		t.Fatal(err)
	}
	return durable
}

func newHistoricalFixture(t *testing.T) (string, *team.EventStore) {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-first", "session-first")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetInvocationID("inv-history")
	appendHistoricalFixture(t, store, team.RunEvent{Type: "run_started"}, map[string]any{"team": "review"})
	return workspace, store
}

func appendHistoricalTask(t *testing.T, store *team.EventStore, run, task, content string) team.RunEvent {
	t.Helper()
	appendHistoricalFixture(t, store, team.RunEvent{RunID: run, Type: "task_created", TaskID: task}, map[string]any{
		"id": task, "status": "pending", "agent": "helper", "desc": content,
	})
	return appendHistoricalFixture(t, store, team.RunEvent{RunID: run, Type: "task_completed", TaskID: task}, map[string]any{
		"id": task, "status": "done", "agent": "helper", "output": content, "attempt": 1,
		"typed_result": map[string]any{"summary": content, "details": "完整結果：" + content, "task_id": task, "agent": "helper", "attempt": 1, "status": "success"},
	})
}

func TestHistoricalResultsPreserveFollowUpCycleRequest(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	appendHistoricalFixture(t, store, team.RunEvent{Type: string(team.EventUserMessageAdded)}, team.SessionMessageEventPayload{Role: "user", Content: "original investigation"})
	appendHistoricalTask(t, store, "run-first", "1", "previous report")
	appendHistoricalFixture(t, store, team.RunEvent{Type: string(team.EventWorkflowStateChanged)}, team.WorkflowStateChangedPayload{
		State: team.PhaseDone, PhaseResults: map[team.Phase]team.PhaseResult{team.PhaseVerify: {Status: team.PhaseStatusSuccess}},
	})
	appendHistoricalFixture(t, store, team.RunEvent{Type: string(team.EventRunStarted), RunID: "run-followup"}, map[string]any{"team": "review"})
	request := appendHistoricalFixture(t, store, team.RunEvent{Type: string(team.EventWorkflowCycleStarted), RunID: "run-followup"}, team.WorkflowCycleStartedPayload{TaskIDFloor: 1, Request: "investigate that report further"})
	appendHistoricalTask(t, store, "run-followup", "2", "follow-up finding")
	snapshot, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Team: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Results) != 2 {
		t.Fatalf("historical cycles lost results: %#v", snapshot.Results)
	}
	for _, row := range snapshot.Results {
		if row.TaskID == "2" && (row.OriginalPrompt != "investigate that report further" || row.OriginalPromptEventID != request.ID) {
			t.Fatalf("follow-up request not linked: %#v", row)
		}
		if row.TaskID == "1" && row.OriginalPrompt != "original investigation" {
			t.Fatalf("previous request overwritten: %#v", row)
		}
	}
}

func TestHistoricalResultsPreserveCanonicalTypedPayloadIdentity(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	const collision = "historical-inspect-runtime-collision-86b3da1f"
	outputs, outputHash, err := team.CanonicalizeRuntimeOutputs(map[string]any{"receipt": map[string]any{
		"diagnostic": collision, "count": jsonv1.Number("9007199254740993"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	appendHistoricalFixture(t, store, team.RunEvent{Type: "task_created", TaskID: "1"}, map[string]any{"id": "1", "agent": "helper", "status": "pending"})
	data, err := jsonv1.Marshal(map[string]any{"id": "1", "agent": "helper", "status": "done", "retries": 1,
		"typed_result": &team.TaskResult{TaskID: "1", Agent: "helper", Attempt: 1, Status: team.TaskResultStatusSuccess,
			Summary: collision, Source: "runtime", RuntimeOutputs: outputs, RuntimeOutputsHash: outputHash}})
	if err != nil {
		t.Fatal(err)
	}
	durable, err := store.AppendPersisted(team.RunEvent{Type: "task_completed", TaskID: "1", Actor: "helper", Attempt: 2, Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	var terminal map[string]jsontext.Value
	if err := json.Unmarshal(durable.Payload, &terminal); err != nil {
		t.Fatal(err)
	}
	payloadHash := sha256.Sum256(terminal["typed_result"])
	utils.RedactSecrets("api_token: " + collision)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(runtimestore.Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Team: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Results) != 1 {
		t.Fatalf("lost typed result: %+v", snapshot.Results)
	}
	row := snapshot.Results[0]
	if row.PayloadSHA256 != fmt.Sprintf("%x", payloadHash) || row.SourceHash != durable.Hash || row.Attempt != 2 || row.TypedResult.Attempt != 1 {
		t.Fatalf("canonical source identity was replaced by its redacted view: %+v", row)
	}
	if strings.Contains(row.Summary, collision) || !bytes.Contains(row.TypedResultJSON, []byte(collision)) {
		t.Fatal("prose redaction and canonical runtime output preservation were conflated")
	}
	receipt := row.TypedResult.RuntimeOutputs["receipt"].(map[string]any)
	if receipt["count"] != jsonv1.Number("9007199254740993") || receipt["diagnostic"] != collision || row.TypedResult.RuntimeOutputsHash != outputHash {
		t.Fatal("historical reader changed receipt-bound values or rounded a number")
	}
	after, err := os.ReadFile(runtimestore.Path(workspace))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("historical projection rewrote the canonical database")
	}
}

func TestHistoricalResultsSeparateRepeatedTaskIDsAndTeams(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	first := appendHistoricalTask(t, store, "run-first", "1", "逾時問題：延長 deadline")
	appendHistoricalFixture(t, store, team.RunEvent{RunID: "run-second", Type: "run_started"}, map[string]any{"team": "review"})
	second := appendHistoricalTask(t, store, "run-second", "1", "第二次結果")
	appendHistoricalFixture(t, store, team.RunEvent{RunID: "run-other", Type: "run_started"}, map[string]any{"team": "other"})
	appendHistoricalTask(t, store, "run-other", "1", "OTHER_TEAM_SENTINEL")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Team: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Results) != 2 {
		t.Fatalf("results = %#v", snapshot.Results)
	}
	for _, row := range snapshot.Results {
		if row.Team != "review" || strings.Contains(row.Output, "OTHER_TEAM") || row.TypedResult == nil {
			t.Fatalf("scope/content mismatch: %#v", row)
		}
		if row.ResultID != team.HistoricalResultID(first) && row.ResultID != team.HistoricalResultID(second) {
			t.Fatalf("wrong source ID: %s", row.ResultID)
		}
	}
	if snapshot.Results[0].ResultID == snapshot.Results[1].ResultID {
		t.Fatal("reused todo ID collided across runs")
	}
	if _, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Team: "unknown"}); !errors.Is(err, ErrScopeConflict) {
		t.Fatalf("unknown team error = %v", err)
	}
}

func TestHistoricalResultsSurviveNewSessionAndJournalCompactionProjection(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	durable := appendHistoricalTask(t, store, "run-first", "1", "保存的完整分析")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := team.CleanRunDirs(workspace); err != nil {
		t.Fatal(err)
	}
	// A cache compaction or --no-journal must not be the historical authority.
	runtime, err := runtimestore.Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Exec(t.Context(), "DELETE FROM task_journal"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := tree.CreateRootBranch("fresh")
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = fresh.ID
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	active, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Team: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active.Results) != 0 {
		t.Fatal("fresh branch silently widened to history")
	}
	all, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Team: "review", AllBranches: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Results) != 1 || all.Results[0].ResultID != team.HistoricalResultID(durable) || all.Results[0].TypedResult.Details != "完整結果：保存的完整分析" {
		t.Fatalf("canonical history lost: %#v", all.Results)
	}
}

func TestHistoricalResultsKeepInvalidatedAttemptAndLatestResult(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	old := appendHistoricalTask(t, store, "run-first", "1", "old output")
	appendHistoricalFixture(t, store, team.RunEvent{Type: "task_created", TaskID: "1"}, map[string]any{
		"id": "1", "status": "pending", "agent": "helper", "desc": "old output", "reset_for_retry": true, "retries": 1,
	})
	appendHistoricalFixture(t, store, team.RunEvent{Type: "task_completed", TaskID: "1", Attempt: 2}, map[string]any{
		"id": "1", "status": "done", "agent": "helper", "retries": 1, "output": "new output",
		"typed_result": map[string]any{"summary": "new result", "details": "new details", "attempt": 2},
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Team: "review"})
	if err != nil {
		t.Fatal(err)
	}
	oldIndex := slices.IndexFunc(snapshot.Results, func(row HistoricalResult) bool { return row.ResultID == team.HistoricalResultID(old) })
	if len(snapshot.Results) != 2 || oldIndex < 0 {
		t.Fatalf("lost old attempt: %#v", snapshot.Results)
	}
	if snapshot.Results[oldIndex].Validity != "invalidated" || snapshot.Results[oldIndex].Output != "old output" {
		t.Fatalf("old result mutated: %#v", snapshot.Results[oldIndex])
	}
	for _, row := range snapshot.Results {
		if row.Attempt == 2 && (row.TypedResult == nil || row.TypedResult.Details != "new details") {
			t.Fatalf("wrong winning result: %#v", row)
		}
	}
}

func TestHistoricalResultBranchInheritanceUsesStableID(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	request := appendHistoricalFixture(t, store, team.RunEvent{Type: "user_message_added"}, team.SessionMessageEventPayload{Role: "user", Content: "parent request"})
	durable := appendHistoricalTask(t, store, "run-first", "1", "parent result")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := team.OpenEventStoreReadOnly(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	branch, err := tree.CreateBranch("child", durable.ID, reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Branch: branch.ID, Team: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Results) != 1 || snapshot.Results[0].ResultID != team.HistoricalResultID(durable) {
		t.Fatalf("inherited identity changed: %#v", snapshot.Results)
	}
	all, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, AllBranches: true, Team: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Results) != 1 || len(all.Results[0].Branches) != 2 {
		t.Fatalf("inherited result duplicated: %#v", all.Results)
	}
	if all.Results[0].OriginalPrompt != "parent request" || all.Results[0].OriginalPromptEventID != request.ID {
		t.Fatalf("inherited result lost its request: %#v", all.Results)
	}
}

func TestHistoricalResultsAreReadOnlyAndFailClosedForCorruption(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	appendHistoricalTask(t, store, "run-first", "1", "api_key=private-key")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "runtime.sqlite")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := marshalHistoricalJSON(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-key") {
		t.Fatal("historical projection leaked secret")
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatalf("query wrote runtime database: %v", err)
	}
	runtime, err := runtimestore.Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Exec(t.Context(), "UPDATE events SET payload = '{}' WHERE type = 'task_completed'"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace}); !errors.Is(err, ErrIntegrity) || len(got.Results) != 0 {
		t.Fatalf("corrupted history was exposed: %#v, %v", got, err)
	}
}

func TestHistoricalResultsBindOriginalRequestAcrossRunsAndResume(t *testing.T) {
	workspace, store := newHistoricalFixture(t)
	request := appendHistoricalFixture(t, store, team.RunEvent{Type: "user_message_added"}, team.SessionMessageEventPayload{Role: "user", Content: "原始調查 api_key=private-request-secret"})
	appendHistoricalFixture(t, store, team.RunEvent{Type: "user_message_added"}, team.SessionMessageEventPayload{Role: "user", Content: "later continuation"})
	appendHistoricalFixture(t, store, team.RunEvent{Type: "task_created", TaskID: "1"}, map[string]any{"id": "1", "status": "pending", "agent": "helper", "desc": "delegated verification"})
	appendHistoricalFixture(t, store, team.RunEvent{RunID: "run-resume", Type: "run_started"}, map[string]any{"team": "review"})
	appendHistoricalFixture(t, store, team.RunEvent{RunID: "run-resume", Type: "task_created", TaskID: "1"}, map[string]any{"id": "1", "status": "pending", "reset_for_resume": true})
	appendHistoricalFixture(t, store, team.RunEvent{RunID: "run-resume", Type: "task_completed", TaskID: "1"}, map[string]any{"id": "1", "status": "done", "output": "resumed result"})
	resumeRequest := appendHistoricalFixture(t, store, team.RunEvent{RunID: "run-resume", Type: "user_message_added"}, team.SessionMessageEventPayload{Role: "user", Content: "new run request"})
	appendHistoricalFixture(t, store, team.RunEvent{RunID: "run-resume", Type: "run_finished"}, map[string]any{"outcome": "completed", "response": "final response"})
	appendHistoricalFixture(t, store, team.RunEvent{RunID: "run-missing", Type: "run_started"}, map[string]any{"team": "review"})
	appendHistoricalTask(t, store, "run-missing", "1", "unrelated result without prompt")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadHistoricalResults(t.Context(), HistoricalResultScope{Workspace: workspace, Team: "review", AllBranches: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Results) != 3 {
		t.Fatalf("results = %#v", snapshot.Results)
	}
	for _, row := range snapshot.Results {
		switch {
		case row.Kind == "run_response":
			if row.OriginalPrompt != "new run request" || row.OriginalPromptEventID != resumeRequest.ID {
				t.Fatalf("final response used another run's prompt: %#v", row)
			}
		case row.RunID == "run-resume":
			if row.OriginRunID != "run-first" || row.OriginalPromptEventID != request.ID || !strings.Contains(row.OriginalPrompt, "原始調查") || strings.Contains(row.OriginalPrompt, "private-request-secret") || strings.Contains(row.OriginalPrompt, "continuation") {
				t.Fatalf("resumed task lost original redacted request: %#v", row)
			}
		default:
			if row.OriginalPrompt != "" || row.OriginalPromptEventID != "" {
				t.Fatalf("missing request borrowed another run's prompt: %#v", row)
			}
		}
	}
}
