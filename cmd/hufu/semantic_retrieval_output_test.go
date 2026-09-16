package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func TestSemanticRetrievalIdentityAppearsInJSONAndReportProjections(t *testing.T) {
	identity := &team.SemanticRetrievalIdentity{
		Mode: contextstore.RetrievalActive, ModelID: "tiny-zh", ModelRevision: "r1",
		ModelHash: strings.Repeat("a", 64), GenerationID: "generation-1", SourceRevision: 7,
		RetrievalPolicyVersion: team.SemanticRetrievalPolicyVersion,
		FallbackReason:         contextstore.SemanticFallbackNone,
	}
	item := &team.TodoItem{
		ID: "task-1", Agent: "worker", Desc: "semantic task", Status: team.TaskDone,
		ExecutionReceipt: &team.ExecutionReceipt{RunID: "run", TaskID: "task-1", Attempt: 1, Semantic: identity},
	}
	projection := jsonRunTask{
		ID: item.ID, Agent: item.Agent, Desc: item.Desc, Status: string(item.Status),
		SemanticRetrieval: team.SemanticRetrievalIdentityForTask(item),
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"semantic_retrieval"`, `"generation_id":"generation-1"`, `"retrieval_policy_version":"hybrid-exact-fts-semantic-rrf60-mmr075-v1"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("JSON projection missing %q: %s", want, encoded)
		}
	}

	report := buildReportMD(&reportData{StartedAt: time.Now(), Todos: []*team.TodoItem{item}}, "demo", "done")
	for _, want := range []string{"## Semantic Retrieval Receipts", "`generation-1`", "`tiny-zh`", "`hybrid-exact-fts-semantic-rrf60-mmr075-v1`"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestLegacyJSONTaskOmitsSemanticRetrieval(t *testing.T) {
	encoded, err := json.Marshal(jsonRunTask{ID: "task-1", Agent: "worker", Status: string(team.TaskDone)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "semantic_retrieval") {
		t.Fatalf("legacy task projection changed: %s", encoded)
	}
}
