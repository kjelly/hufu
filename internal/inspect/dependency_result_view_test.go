package inspect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectContextProjectsSelectedDependencyProvenance(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "view-run", "view-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	policy := team.DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}
	encoded, err := json.Marshal(policy, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	attribution := &team.DependencyResultViewAttribution{Policy: policy, PolicySHA256: hex.EncodeToString(digest[:]), Sources: []team.DependencyResultViewSource{{
		BranchID: "main", RunID: "source-run", TaskID: "source", Attempt: 1, OccurrenceRevision: 4,
		SourceEventID: "source-event", SourceEventSHA256: strings.Repeat("b", 64), FullViewSHA256: strings.Repeat("c", 64),
	}}}
	request := team.ContextRequest{SchemaVersion: team.ContextRequestSchemaVersion, RunID: "view-run", TaskID: "consumer", Attempt: 1, AgentName: "worker", AgentRole: "worker", Goal: "review", Phase: team.PhasePrepare, Trigger: team.ContextTriggerTaskDispatch, Purpose: "task_execution"}
	request.AssignRequestID()
	manifest, err := team.BuildContextInjectionManifest(request, team.CompiledContext{IncludedItems: []team.ContextItem{{
		ID: "dependency_results", Kind: "dependency_result", Content: "private_summary_sentinel", Required: true, DependencyResultView: attribution,
	}}}, nil, "worker", time.Now(), agent.DefaultMemoryLearningPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(team.RunEvent{Type: string(team.EventTaskCreated), TaskID: "consumer", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{
		"id": "consumer", "agent": "worker", "status": team.TaskPending, "context_manifests": []team.ContextInjectionManifest{manifest},
	})}); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectContext(t.Context(), InspectQuery{Workspace: workspace, RunID: "view-run", TaskID: "consumer", ProjectID: "project", AgentID: "worker"}, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(ContextData)
	if len(data.Manifests) != 1 || data.Manifests[0].DependencyResultView == nil || data.Manifests[0].ModelCalled == nil || *data.Manifests[0].ModelCalled {
		t.Fatalf("selected request preparation was misrepresented: %+v", data.Manifests)
	}
	encoded, err = json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"selected", "summary", "source-event", "source-run", attribution.PolicySHA256, `"model_called":false`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("inspection omitted %s", want)
		}
	}
	if strings.Contains(string(encoded), "private_summary_sentinel") {
		t.Fatal("content-free inspection leaked the selected source content")
	}
	data.Manifests[0].DependencyResultView.Policy.Fields[0] = "artifacts"
	if attribution.Policy.Fields[0] != "summary" {
		t.Fatal("inspector attribution aliases canonical source metadata")
	}
}
