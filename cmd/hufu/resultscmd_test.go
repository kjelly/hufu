package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/resultquery"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func resultsTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func resultsTestFixture(t *testing.T) (string, []string) {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-review", "session-review")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	store.SetInvocationID("inv-results")
	appendEvent := func(event team.RunEvent, payload any) team.RunEvent {
		t.Helper()
		event.Payload = resultsTestJSON(t, payload)
		if event.Actor == "" {
			event.Actor = "helper"
		}
		event.Timestamp = "2026-10-13T10:00:00Z"
		durable, err := store.AppendPersisted(event)
		if err != nil {
			t.Fatal(err)
		}
		return durable
	}
	appendEvent(team.RunEvent{Type: "run_started"}, map[string]any{"team": "review"})
	ids := []string{}
	for i := range 3 {
		id := fmt.Sprint(i + 1)
		appendEvent(team.RunEvent{Type: "task_created", TaskID: id}, map[string]any{"id": id, "status": "pending", "agent": "helper", "desc": "timeout 分析 " + id})
		durable := appendEvent(team.RunEvent{Type: "task_completed", TaskID: id}, map[string]any{
			"id": id, "status": "done", "agent": "helper", "output": "完整任務結果 " + id,
			"typed_result": map[string]any{"summary": "調整 deadline", "details": "保存的分析與解法 " + id, "status": "success", "task_id": id, "attempt": 1},
		})
		ids = append(ids, team.HistoricalResultID(durable))
	}
	appendEvent(team.RunEvent{Type: "run_finished"}, map[string]any{"outcome": "completed", "response": "coordinator final response"})
	appendEvent(team.RunEvent{Type: "run_started", RunID: "run-other"}, map[string]any{"team": "other"})
	appendEvent(team.RunEvent{Type: "task_created", RunID: "run-other", TaskID: "1"}, map[string]any{"id": "1", "status": "pending", "agent": "other-agent", "desc": "OTHER_TEAM_SENTINEL"})
	appendEvent(team.RunEvent{Type: "task_completed", RunID: "run-other", TaskID: "1"}, map[string]any{"id": "1", "status": "done", "agent": "other-agent", "output": "OTHER_TEAM_SENTINEL"})
	return workspace, ids
}

func resultsTestDeps(t *testing.T) resultsDeps {
	t.Helper()
	return resultsDeps{workspace: defaultWorkspaceCommandDeps(), load: inspectpkg.LoadHistoricalResults, now: func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) },
		generate: func(context.Context, *operatorpkg.OperatorSnapshot, string, string, string, sidecar.Profile) (explainAIResult, error) {
			t.Fatal("offline command contacted a model")
			return explainAIResult{}, nil
		}}
}

func executeResultsTest(t *testing.T, deps resultsDeps, args ...string) (resultsEnvelope, string, error) {
	t.Helper()
	command := newResultsCommandWithDeps(deps)
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs(args)
	err := command.Execute()
	var envelope resultsEnvelope
	if strings.HasPrefix(stdout.String(), "{") {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &envelope); decodeErr != nil {
			t.Fatalf("invalid JSON output: %v\n%s", decodeErr, stdout.String())
		}
	}
	return envelope, stdout.String(), err
}

func TestResultsOfflineCommandsRequireNoRunIDAndRespectTeam(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	for _, args := range [][]string{
		{"list", "--team", "review"},
		{"--team", "review", "search", "timeout"},
		{"latest", "--team", "review"},
		{"show", ids[0], "--team", "review"},
		{"final", "--team", "review"},
		{"final", ids[0], "--team", "review"},
		{"invocation", "inv-results", "--team", "review"},
	} {
		_, output, err := executeResultsTest(t, resultsTestDeps(t), append(args, "--workspace", workspace)...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(output, "OTHER_TEAM_SENTINEL") || args[0] != "final" && !strings.Contains(output, "res-") {
			t.Fatalf("wrong scope/result output: %s", output)
		}
		if args[0] == "final" && output != "coordinator final response\n" {
			t.Fatalf("final did not reprint the saved response: %s", output)
		}
		if args[0] == "show" && !strings.Contains(output, "保存的分析與解法") {
			t.Fatal("show omitted saved details")
		}
	}
	if _, _, err := executeResultsTest(t, resultsTestDeps(t), "show", ids[0], "--workspace", workspace, "--team", "other"); err == nil {
		t.Fatal("show widened a conflicting team selector")
	}
}

func TestResultsFinalReadsSavedResponseAndShowExplainsWorkerResult(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	for _, args := range [][]string{{"final"}, {"final", ids[0]}, {"final", ids[0][:16]}} {
		value, _, err := executeResultsTest(t, deps, append(args, "--workspace", workspace, "--team", "review", "--output", "json")...)
		if err != nil {
			t.Fatal(err)
		}
		if len(value.Results) != 1 || value.Results[0].Kind != "run_response" || value.Results[0].Output != "coordinator final response" {
			t.Fatalf("final returned a worker result: %#v", value)
		}
	}
	_, output, err := executeResultsTest(t, deps, "show", ids[0], "--workspace", workspace, "--team", "review")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"individual agent task result", "Task: 1", "run: run-review", "hufu results final " + ids[0], "--branch", "保存的分析與解法"} {
		if !strings.Contains(output, required) {
			t.Fatalf("show omitted %q: %s", required, output)
		}
	}
	if _, _, err := executeResultsTest(t, deps, "final", ids[0], "--workspace", workspace, "--team", "other"); err == nil {
		t.Fatal("final crossed team scope")
	}
	for _, flags := range [][]string{{"--agent", "helper"}, {"--status", "done"}, {"--cursor", "invalid"}} {
		if _, _, err := executeResultsTest(t, deps, append([]string{"final", "--workspace", workspace}, flags...)...); err == nil {
			t.Fatalf("final silently ignored worker/pagination filter: %v", flags)
		}
	}
}

func TestResultsInvocationIncludesTaskResultsAndRunResponse(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	value, _, err := executeResultsTest(t, resultsTestDeps(t), "invocation", "inv-results", "--workspace", workspace, "--team", "review", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if value.Matched != 4 || len(value.Results) != 4 {
		t.Fatalf("invocation dropped results: %#v", value)
	}
	runResponses := 0
	for _, row := range value.Results {
		if row.Kind == "run_response" {
			runResponses++
			if row.Output != "coordinator final response" {
				t.Fatal("run response changed")
			}
		}
	}
	if runResponses != 1 {
		t.Fatal("run response unavailable")
	}
}

func TestResultsCursorPinsFiltersAndEventSnapshot(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	first, _, err := executeResultsTest(t, deps, "list", "--workspace", workspace, "--team", "review", "--limit", "1", "--output", "json")
	if err != nil || first.NextCursor == "" {
		t.Fatalf("missing cursor: %#v %v", first, err)
	}
	second, _, err := executeResultsTest(t, deps, "list", "--workspace", workspace, "--team", "review", "--limit", "1", "--output", "json", "--cursor", first.NextCursor)
	if err != nil || len(second.Results) != 1 || first.Results[0].ResultID == second.Results[0].ResultID {
		t.Fatalf("bad continuation: %#v %v", second, err)
	}
	if _, _, err := executeResultsTest(t, deps, "search", "different", "--workspace", workspace, "--team", "review", "--cursor", first.NextCursor); err == nil {
		t.Fatal("cursor used with changed filters")
	}
	store, err := team.NewEventStore(workspace, "run-review", "session-review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{Type: "assistant_message_added", Actor: "coordinator", Payload: resultsTestJSON(t, map[string]any{"role": "assistant", "content": "new event"})}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeResultsTest(t, deps, "list", "--workspace", workspace, "--team", "review", "--cursor", first.NextCursor); err == nil {
		t.Fatal("stale snapshot cursor was accepted")
	}
}

func TestResultsQueryUsesBoundedModelAndSourceCitations(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	calls := 0
	deps.generate = func(ctx context.Context, snapshot *operatorpkg.OperatorSnapshot, model, purpose, prompt string, profile sidecar.Profile) (explainAIResult, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("model query lacks a total deadline")
		}
		if snapshot.Scope.TeamName != "review" || model != "local/test" || strings.Contains(prompt, "OTHER_TEAM_SENTINEL") {
			t.Fatal("wrong model/team evidence scope")
		}
		var response any
		switch purpose {
		case "historical_query_resolver":
			response = resultquery.Plan{SchemaVersion: 1, Resolution: "resolved", Intent: "summarize", Selectors: resultquery.Selectors{Team: "review"}, TimeRange: resultquery.TimeRange{Timezone: "UTC"}, Statuses: []string{"done"}, StatusBasis: "at_terminal", SearchTerms: []string{"timeout"}, Order: "relevance", Limit: 20}
		case "historical_query_selection":
			response = map[string]any{"result_ids": []string{ids[0]}}
		default:
			response = map[string]any{"blocks": []resultquery.AnswerBlock{{Kind: "fact", Text: "使用保存的 deadline 解法。", Sources: []string{ids[0]}}}}
		}
		return explainAIResult{model: model, analysis: string(resultsTestJSON(t, response))}, nil
	}
	envelope, _, err := executeResultsTest(t, deps, "query", "昨天的 timeout 解法？", "--workspace", workspace, "--team", "review", "--model", "local/test", "--timezone", "UTC", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || envelope.Query == nil || len(envelope.Query.Blocks) != 1 || envelope.Results[0].ResultID != ids[0] {
		t.Fatalf("incomplete query output: %#v calls=%d", envelope, calls)
	}
}

func TestResultsPreviewAndProviderFailureRemainReadOnly(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	preview, _, err := executeResultsTest(t, deps, "query", "昨天的結果", "--workspace", workspace, "--team", "review", "--preview", "--output", "json")
	if err != nil || preview.Operation != "preview" || preview.Query != nil {
		t.Fatalf("preview resolved a model query: %#v %v", preview, err)
	}
	deps.generate = func(context.Context, *operatorpkg.OperatorSnapshot, string, string, string, sidecar.Profile) (explainAIResult, error) {
		return explainAIResult{}, errors.New("provider unavailable")
	}
	response, _, err := executeResultsTest(t, deps, "query", "過去的結果", "--workspace", workspace, "--team", "review", "--output", "json")
	exit, ok := errors.AsType[*inspectExitError](err)
	if !ok || exit.code != 3 || response.Operation != "failed" {
		t.Fatalf("analysis failure concealed: %#v %v", response, err)
	}
}

func TestResultsDiscoversCurrentProjectWithoutWorkspaceOrRunIDs(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	subject := t.TempDir()
	if err := os.Mkdir(filepath.Join(subject, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry, err := workspacepkg.OpenReadWrite(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.CreateWorkspace(t.Context(), project.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	deps := resultsTestDeps(t)
	deps.workspace.stateRoot = func() (string, error) { return stateRoot, nil }
	deps.workspace.getwd = func() (string, error) { return subject, nil }
	loaded := 0
	deps.load = func(ctx context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
		loaded++
		if scope.Workspace != workspace.ControlRoot || scope.Team != "review" {
			t.Fatalf("wrong registry binding: %#v", scope)
		}
		return inspectpkg.HistoricalSnapshot{Results: []inspectpkg.HistoricalResult{}, Coverage: inspectpkg.HistoricalCoverage{Scope: scope}}, nil
	}
	if _, _, err := executeResultsTest(t, deps, "list", "--team", "review"); err != nil {
		t.Fatal(err)
	}
	if loaded != 1 {
		t.Fatal("did not resolve registered team workspace")
	}
}

func TestResultIdentityTerminalAndJSONLPresentation(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	id := "res-" + strings.Repeat("a", 64)
	event := team.StatusEvent{Type: "task_result_ready", TeamName: "review", TodoID: "7", Data: map[string]any{"result_id": id, "run_id": "run-1", "source_event_id": "evt-1", "branch_id": "main", "attempt": 1}}
	writer := &testStatusWriter{}
	dispatchStatusEvent(writer, &reporterState{}, event)
	if !strings.Contains(writer.b.String(), id) || !strings.Contains(writer.b.String(), "--team 'review'") {
		t.Fatalf("missing live reference: %s", writer.b.String())
	}
	opts.quietMode = true
	quiet := &testStatusWriter{}
	dispatchStatusEvent(quiet, &reporterState{}, event)
	if quiet.b.String() != "" {
		t.Fatal("result identity ignored quiet mode")
	}
	encoded := jsonStatusEvent{Type: event.Type, Result: resultStatusIdentity(event)}
	data := resultsTestJSON(t, encoded)
	if !bytes.Contains(data, []byte(`"result_id":"`+id)) || !bytes.Contains(data, []byte(`"schema_version":1`)) {
		t.Fatalf("missing JSONL identity: %s", data)
	}
}
