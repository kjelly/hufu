package inspect

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func followUpContextFixture(t *testing.T, corrupt bool) InspectQuery {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "follow-up", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	appendEvent := func(event team.RunEvent) team.RunEvent {
		t.Helper()
		persisted, err := store.AppendPersisted(event)
		if err != nil {
			t.Fatal(err)
		}
		return persisted
	}
	source := appendEvent(team.RunEvent{Type: string(team.EventRunFinished), RunID: "previous", Actor: "coordinator", Payload: jsonBytes(t, team.RunFinishedEventPayload{
		Outcome: team.RunOutcomeCompleted, Response: "Complete earlier report\nhttps://example.test/evidence\nLAST_FINDING",
	})})
	appendEvent(team.RunEvent{Type: string(team.EventRunStarted), Actor: "coordinator", Payload: []byte(`{}`)})
	appendEvent(team.RunEvent{Type: string(team.EventWorkflowCycleStarted), Actor: "coordinator", Payload: []byte(`{"task_id_floor":0,"request":"investigate further"}`)})
	item, err := team.PreviousResponseContextItem(source)
	if err != nil {
		t.Fatal(err)
	}
	request := team.ContextRequest{
		SchemaVersion: team.ContextRequestSchemaVersion, RunID: "follow-up", TaskID: "1", Attempt: 1,
		AgentName: "worker", AgentRole: "worker", Goal: "investigate further", Phase: team.PhasePrepare, Trigger: team.ContextTriggerTaskDispatch,
	}
	request.AssignRequestID()
	manifest, err := team.BuildContextInjectionManifest(request, team.CompiledContext{IncludedItems: []team.ContextItem{item}}, nil, "worker", time.Now(), agent.DefaultMemoryLearningPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if corrupt {
		manifest.Items[0].ContentHash = "incorrect-injected-bytes"
	}
	appendEvent(team.RunEvent{Type: string(team.EventTaskCreated), Actor: "coordinator", TaskID: "1", Payload: jsonBytes(t, map[string]any{
		"id": "1", "agent": "worker", "status": team.TaskPending,
	})})
	appendEvent(team.RunEvent{Type: string(team.EventTaskCompleted), Actor: "worker", TaskID: "1", Payload: jsonBytes(t, map[string]any{
		"id": "1", "agent": "worker", "status": team.TaskDone, "context_manifests": []team.ContextInjectionManifest{manifest},
	})})
	return InspectQuery{Workspace: workspace, RunID: "follow-up", TaskID: "1", ProjectID: "project", AgentID: "worker"}
}

func TestInspectFollowUpReportResolvesExactEventWithoutMemoryDatabase(t *testing.T) {
	query := followUpContextFixture(t, false)
	for _, show := range []bool{false, true} {
		envelope, err := InspectContext(t.Context(), query, ContextOptions{ShowContent: show})
		if err != nil {
			t.Fatal(err)
		}
		data := envelope.Data.(ContextData)
		if len(data.Items) != 1 || !data.Items[0].Available || data.Items[0].SourceType != "event_store" || data.Manifests[0].ContentHash == "" {
			t.Fatalf("report source attribution unavailable: %+v", data)
		}
		if strings.Contains(data.Items[0].Content, "LAST_FINDING") != show {
			t.Fatalf("show-content=%t did not control full report disclosure", show)
		}
	}
	_, err := inspectContext(t.Context(), query, ContextOptions{ShowContent: true}, func(string) (string, error) {
		return "", errors.New("redactor unavailable")
	})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("report redaction did not fail closed: %v", err)
	}
}

func TestInspectFollowUpReportRejectsMismatchedInjectedBytes(t *testing.T) {
	_, err := InspectContext(t.Context(), followUpContextFixture(t, true), ContextOptions{ShowContent: true})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered report binding accepted: %v", err)
	}
}

func TestInspectFollowUpReportCannotResolveOutsideBranch(t *testing.T) {
	ids := map[string]struct{}{"previous_response:sibling-event": {}}
	_, err := loadPreviousResponseItems(InspectQuery{}, ContextOptions{ShowContent: true}, Lineage{}, []ContextManifestItemData{
		{ContextItemID: "previous_response:sibling-event", Kind: "previous_response", Source: "event_store:sibling-event", ContentHash: "digest"},
	}, ids, func(s string) (string, error) { return s, nil })
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("out-of-branch report was exposed: %v", err)
	}
}
