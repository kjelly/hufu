package inspect

import (
	json "encoding/json/v2"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectContextReplaysHistoricalQueryWithoutRetrieval(t *testing.T) {
	plan := team.BuildContextQueryPlan(team.ContextRequest{Goal: "private historical query"}, team.ContextQueryHints{})
	selection, err := team.SelectContextQuerySources(t.Context(), nil, plan, []team.ContextQuerySource{{Kind: team.QuerySharedPersistent, Request: contextstore.SearchRequest{Scope: contextstore.Scope{ProjectID: "project-1", TeamID: "team-1"}, Limit: 20}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture := buildRunFixture(t, selection)
	// No context.sqlite exists: inspection must project the durable selection
	// without opening a writable store or trying to recreate its query.
	envelope, err := InspectContext(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, TaskID: fixture.taskID, ProjectID: "project-1", TeamID: "team-1"}, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(ContextData)
	if len(data.Manifests) != 1 || len(data.MemoryManifests) != 1 {
		t.Fatal("historical manifests missing")
	}
	context, memory := data.Manifests[0], data.MemoryManifests[0]
	if context.Query == nil || memory.Query == nil || context.QueryStatus != "query_plan_required" || memory.QueryStatus != "query_plan_required" || memory.QueryHash != selection.CompositeHash || memory.ModelExecutionID != "execution-1" {
		t.Fatal("replay lost query or model identity")
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private historical query") || strings.Contains(string(encoded), "fixture private content") {
		t.Fatal("content-free historical inspection exposed plaintext")
	}
}
