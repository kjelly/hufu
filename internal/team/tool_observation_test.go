package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

func observationFixture(t *testing.T) (*Coordinator, context.Context, *TodoItem) {
	t.Helper()
	c := newDirectTypedCoordinator(t, "view,recall_observation,source__read_document", nil, nil)
	store, err := NewEventStore(c.session.Workspace, "run-first", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c.eventStore, c.executionRunID = store, "run-first"
	c.SetEventJournal(eventStoreJournal{store: store})
	c.mcpManager = readOnlyRecoveryManager(t)
	c.coreTools = append(c.coreTools, &recallObservationTool{coordinator: c})
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "read source"}})[0]
	ctx := observationContext(t, item.ID)
	return c, ctx, item
}

func observationContext(t *testing.T, id string) context.Context {
	t.Helper()
	ctx := gatewayTestContext(t, nil, "view", recallObservationToolName, "source__read_document", dynamicToolGatewayName)
	ctx = context.WithValue(ctx, todoIDKey{}, id)
	return context.WithValue(ctx, executionAttemptKey{}, 1)
}

func observationEvents(t *testing.T, c *Coordinator, kind EventType) []RunEvent {
	t.Helper()
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(events, func(e RunEvent) bool { return e.Type != string(kind) })
}

func recallResponse(t *testing.T, c *Coordinator, ctx context.Context, id, input string) (fantasy.ToolResponse, map[string]any) {
	t.Helper()
	gate := &policyGatedTool{coordinator: c, inner: &recallObservationTool{coordinator: c}}
	transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript)
	if transcript != nil {
		if err := transcript.RecordToolCall(id, recallObservationToolName, input); err != nil {
			t.Fatal(err)
		}
	}
	response, err := gate.Run(ctx, fantasy.ToolCall{ID: id, Name: recallObservationToolName, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if transcript != nil {
		if err := transcript.RecordToolResult(id, recallObservationToolName, response.Content, response.IsError); err != nil {
			t.Fatal(err)
		}
	}
	var value map[string]any
	if !response.IsError {
		if err := json.Unmarshal([]byte(response.Content), &value); err != nil {
			t.Fatal(err)
		}
	}
	return response, value
}

func captureObservation(t *testing.T, c *Coordinator, ctx context.Context, name, input, text string) RunEvent {
	t.Helper()
	c.captureToolObservation(ctx, fantasy.ToolCall{ID: "source-call"}, name, input, text)
	events := observationEvents(t, c, EventToolObservationRecorded)
	if len(events) != 1 {
		t.Fatalf("observations = %d", len(events))
	}
	return events[0]
}

func nextObservationTask(t *testing.T, c *Coordinator) (*TodoItem, context.Context) {
	t.Helper()
	c.executionRunID = "run-follow-up"
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "follow up"}})[0]
	return item, observationContext(t, item.ID)
}

func TestObservationRecallUsesImmutablePriorOutputWithoutSourceReplay(t *testing.T) {
	c, ctx, _ := observationFixture(t)
	text := "complete source content 最後一段"
	event := captureObservation(t, c, ctx, "view", `{"file_path":"document.txt"}`, text)
	item, nextCtx := nextObservationTask(t, c)
	ctx = nextCtx
	response, found := recallResponse(t, c, ctx, "search", `{"action":"SEARCH","query":"document.txt"}`)
	if response.IsError || len(found["observations"].([]any)) != 1 {
		t.Fatalf("search: %s", response.Content)
	}
	response, read := recallResponse(t, c, ctx, "read", fmt.Sprintf(`{"action":"read","observation_id":%q}`, event.ID))
	if response.IsError || read["content"] != text || read["historical"] != true || read["next_offset"] != float64(-1) {
		t.Fatalf("read: %s", response.Content)
	}
	if len(observationEvents(t, c, EventToolObservationRecorded)) != 1 || len(observationEvents(t, c, EventToolObservationRead)) != 1 {
		t.Fatal("recall fabricated a fresh observation or lost its read attribution")
	}
	// Reopen the durable store and restore only the current workflow's task.
	// The producing task is no longer in the active projection or task cache.
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(c.session.Workspace, c.executionRunID, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c.eventStore = store
	c.SetEventJournal(eventStoreJournal{store: store})
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().restoreWorkflowCycle([]*TodoItem{cloneTodoItem(item)}, 1)
	response, _ = recallResponse(t, c, ctx, "read-after-resume", fmt.Sprintf(`{"action":"read","observation_id":%q}`, event.ID))
	if response.IsError {
		t.Fatalf("resume lost observation: %s", response.Content)
	}
}

func TestObservationRecallEnforcesScopeAndCurrentPermissions(t *testing.T) {
	for _, mode := range []string{"other_worker", "other_team", "team_denied", "source_removed", "source_denied", "source_not_exposed", "frozen_source_removed", "no_history", "bound", "current_task", "sibling", "missing_artifact", "changed_bytes", "read_event_failure"} {
		t.Run(mode, func(t *testing.T) {
			c, ctx, _ := observationFixture(t)
			event := captureObservation(t, c, ctx, "view", `{"file_path":"source.txt"}`, "original source text")
			originalCtx := ctx
			item, ctx := nextObservationTask(t, c)
			switch mode {
			case "other_team":
				c.session.Config.Name = "different-team"
			case "other_worker":
				c.session.Agents["worker"].Name = "different"
				item.Agent = "different"
			case "team_denied":
				c.session.Config.ToolsDenied = []string{recallObservationToolName}
			case "source_removed":
				c.session.Agents["worker"].Tools = recallObservationToolName
			case "source_denied":
				ctx = context.WithValue(ctx, tools.AgentToolsSessionPermissionsKey, map[string]bool{"view": false})
			case "source_not_exposed":
				ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{recallObservationToolName})
			case "frozen_source_removed":
				item.DynamicToolAuthorization = newEmptyDynamicToolAuthorizationSnapshot()
			case "no_history":
				c.executionProfile.Name = "test"
				c.executionProfile.DisableHistoricalMemory = true
			case "bound":
				item.WorksetBinding = &WorksetBinding{}
			case "current_task":
				c.executionRunID = "run-first"
				ctx = originalCtx
			case "sibling":
				tree := NewSessionTree()
				tree.Branches["other"] = &SessionBranch{ID: "other"}
				tree.ActiveBranch = "other"
				if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
					t.Fatal(err)
				}
			case "missing_artifact", "changed_bytes":
				var p ToolObservationPayload
				if err := json.Unmarshal(event.Payload, &p); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(c.session.Workspace, "logs", "artifacts", "data", p.Artifact.ID)
				if mode == "missing_artifact" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "read_event_failure":
				c.SetEventJournal(&observationReadFailJournal{EventJournal: c.EventJournal()})
			}
			response, _ := recallResponse(t, c, ctx, "denied-read", fmt.Sprintf(`{"action":"read","observation_id":%q}`, event.ID))
			if !response.IsError || strings.Contains(response.Content, "original source text") {
				t.Fatalf("scope/integrity denial leaked content: %s", response.Content)
			}
		})
	}
}

type observationReadFailJournal struct{ EventJournal }

func (j *observationReadFailJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if event.Type == string(EventToolObservationRead) {
		return RunEvent{}, fmt.Errorf("injected read attribution failure")
	}
	return j.EventJournal.Append(ctx, event)
}

func TestObservationRecallReadsUTF8RangesAndBoundsInputs(t *testing.T) {
	c, ctx, _ := observationFixture(t)
	event := captureObservation(t, c, ctx, "view", `{}`, "甲乙丙丁")
	_, ctx = nextObservationTask(t, c)
	var reconstructed strings.Builder
	for offset := 0; offset >= 0; {
		response, value := recallResponse(t, c, ctx, fmt.Sprint("read-", offset), fmt.Sprintf(`{"action":"read","observation_id":%q,"offset":%d,"limit":4}`, event.ID, offset))
		if response.IsError {
			t.Fatal(response.Content)
		}
		reconstructed.WriteString(value["content"].(string))
		offset = int(value["next_offset"].(float64))
	}
	if reconstructed.String() != "甲乙丙丁" {
		t.Fatal(reconstructed.String())
	}
	for _, input := range []string{
		`{"action":"read","observation_id":"x","offset":-1}`, `{"action":"read","observation_id":"x","limit":999999}`,
		`{"action":"search","limit":21}`, `{"action":"read"}`, `{"action":"search","action":"read"}`, `{"action":"search","query":"x","unexpected":true}`,
		fmt.Sprintf(`{"action":"read","observation_id":%q,"offset":1}`, event.ID),
		fmt.Sprintf(`{"action":"read","observation_id":%q,"limit":1}`, event.ID),
	} {
		response, _ := recallResponse(t, c, ctx, "bad", input)
		if !response.IsError {
			t.Fatalf("accepted invalid input: %s", input)
		}
	}
}

func TestObservationCaptureIsExplicitBoundedAndObservationOnly(t *testing.T) {
	c, ctx, _ := observationFixture(t)
	for _, name := range []string{"write", "submit_result", recallObservationToolName, "sink__write_note", "unknown"} {
		c.captureToolObservation(ctx, fantasy.ToolCall{ID: name}, name, `{}`, "not reusable")
	}
	c.captureToolObservation(ctx, fantasy.ToolCall{ID: "huge"}, "view", `{}`, strings.Repeat("x", maxObservationBytes+1))
	if len(observationEvents(t, c, EventToolObservationRecorded)) != 0 {
		t.Fatal("captured non-observation or oversized output")
	}
	for i := range maxObservationsPerAttempt + 1 {
		c.captureToolObservation(ctx, fantasy.ToolCall{ID: fmt.Sprint(i)}, "view", `{}`, "observed")
	}
	if len(observationEvents(t, c, EventToolObservationRecorded)) != maxObservationsPerAttempt {
		t.Fatal("capture cap not enforced")
	}
	for _, grant := range []string{"", "all", "view"} {
		c.session.Agents["worker"].Tools = grant
		if slices.Contains(agentToolNames(c.selectWorkerTools(c.session.Agents["worker"])), recallObservationToolName) {
			t.Fatalf("implicit recall grant: %q", grant)
		}
		if slices.Contains(c.staticToolGrantNames(c.session.Agents["worker"], TaskDef{}), recallObservationToolName) {
			t.Fatalf("implicit frozen recall grant: %q", grant)
		}
	}
}

func TestObservationCaptureRedactsBeforeStorageAndPreservesDigest(t *testing.T) {
	c, ctx, _ := observationFixture(t)
	const secret = "-----BEGIN RSA PRIVATE KEY-----\nMIIEobservationsecret\n-----END RSA PRIVATE KEY-----"
	event := captureObservation(t, c, ctx, "view", `{}`, "public source\n"+secret)
	p, body, err := c.loadToolObservation(t.Context(), event)
	if err != nil || strings.Contains(body.Output, "MIIEobservationsecret") || body.Output != utils.RedactSecrets("public source\n"+secret) {
		t.Fatalf("unredacted or changed observation: %+v %v", body, err)
	}
	_, ctx = nextObservationTask(t, c)
	response, _ := recallResponse(t, c, ctx, "redacted-read", fmt.Sprintf(`{"action":"read","observation_id":%q}`, event.ID))
	if response.IsError || strings.Contains(response.Content, "MIIEobservationsecret") {
		t.Fatalf("unsafe delivery: %s", response.Content)
	}
	current, _, err := c.loadToolObservation(t.Context(), event)
	if err != nil || current.Artifact.SHA256 != p.Artifact.SHA256 {
		t.Fatal("read mutated original observation")
	}
}

func TestObservationModelLeavesShareLimitsAndCanonicalBranch(t *testing.T) {
	c, ctx, _ := observationFixture(t)
	tree := NewSessionTree()
	tree.Branches["research"] = &SessionBranch{ID: "research"}
	tree.ActiveBranch = "research"
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	c.eventStore.SetBranchID("research")
	c.captureToolObservation(ctx, fantasy.ToolCall{ID: "same-id"}, "view", `{}`, "parent observation")
	leaf := cloneCoordinator(c, cloneSession(c.session, t.TempDir()))
	leaf.modelFanout = &modelFanoutBinding{parent: c}
	leaf.modelExecutionID = "model-execution-leaf"
	leaf.captureToolObservation(ctx, fantasy.ToolCall{ID: "same-id"}, "view", `{}`, "leaf observation")
	if got := len(observationEvents(t, c, EventToolObservationRecorded)); got != 2 {
		t.Fatalf("leaf call identity collided: %d", got)
	}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			publisher := c
			if i%2 == 1 {
				publisher = leaf
			}
			publisher.captureToolObservation(ctx, fantasy.ToolCall{ID: fmt.Sprint("concurrent-", i)}, "view", `{}`, "observed")
		})
	}
	wg.Wait()
	if got := len(observationEvents(t, c, EventToolObservationRecorded)); got != maxObservationsPerAttempt {
		t.Fatalf("model leaves bypassed shared cap: %d", got)
	}
	event := observationEvents(t, c, EventToolObservationRecorded)[0]
	_, ctx = nextObservationTask(t, c)
	leaf.executionRunID = c.executionRunID
	var recall *recallObservationTool
	for _, tool := range leaf.selectWorkerTools(leaf.session.Agents["worker"]) {
		if candidate, ok := tool.(*recallObservationTool); ok {
			recall = candidate
		}
	}
	if recall == nil || recall.coordinator != leaf {
		t.Fatal("leaf retained parent-bound recall handler")
	}
	response, err := (&policyGatedTool{coordinator: leaf, inner: recall}).Run(ctx, fantasy.ToolCall{ID: "leaf-read", Name: recallObservationToolName, Input: fmt.Sprintf(`{"action":"read","observation_id":%q}`, event.ID)})
	if err != nil || response.IsError || !strings.Contains(response.Content, "parent observation") || !strings.Contains(response.Content, "model-execution-leaf") {
		t.Fatalf("leaf lost canonical branch or read identity: %s %v", response.Content, err)
	}
}

func TestObservationReadRejectsConflictingIdempotency(t *testing.T) {
	c, ctx, _ := observationFixture(t)
	event := captureObservation(t, c, ctx, "view", `{}`, "first second")
	_, ctx = nextObservationTask(t, c)
	first, _ := recallResponse(t, c, ctx, "same-read", fmt.Sprintf(`{"action":"read","observation_id":%q,"limit":5}`, event.ID))
	if first.IsError {
		t.Fatal(first.Content)
	}
	conflict, _ := recallResponse(t, c, ctx, "same-read", fmt.Sprintf(`{"action":"read","observation_id":%q,"offset":6}`, event.ID))
	if !conflict.IsError || strings.Contains(conflict.Content, "second") {
		t.Fatalf("returned unrecorded read content: %s", conflict.Content)
	}
}
