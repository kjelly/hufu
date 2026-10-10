package team

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"gopkg.in/yaml.v3"
)

func completedEvidenceSources(c *Coordinator, count int) []string {
	var ids []string
	for range count {
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reader"}})[0]
		item.Status = TaskDone
		item.TypedResult = &TaskResult{TaskID: item.ID, Agent: item.Agent, Status: TaskResultStatusSuccess, Summary: "accepted evidence"}
		ids = append(ids, item.ID)
	}
	return ids
}

func TestEvidenceSourceLimitIsBoundedAndKeepsDefault(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	ids := completedEvidenceSources(c, 12)
	for _, tc := range []struct {
		limit int
		want  string
	}{
		{0, "at most 8"}, {8, "at most 8"}, {11, "at most 11"}, {12, ""},
		{64, ""}, {-1, "between 0 and 64"}, {65, "between 0 and 64"},
	} {
		t.Run(fmt.Sprint(tc.limit), func(t *testing.T) {
			task := TaskDef{Agent: "writer", EvidenceFrom: ids, Execution: ExecutionContract{RequiresEvidence: true, MaxEvidenceSources: tc.limit}}
			bound, err := c.bindEvidenceSources([]TaskDef{task})
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v, want %q", err, tc.want)
				}
				return
			}
			if err != nil || !slices.Equal(bound[0].EvidenceFrom, ids) {
				t.Fatalf("full evidence handoff was lost: bound=%#v err=%v", bound, err)
			}
		})
	}
	// Raising capacity never authorizes a missing or unfinished source.
	unfinished := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reader"}})[0]
	for _, id := range []string{"999", unfinished.ID} {
		if _, err := c.bindEvidenceSources([]TaskDef{{Agent: "writer", EvidenceFrom: []string{id}, Execution: ExecutionContract{MaxEvidenceSources: 64}}}); err == nil {
			t.Fatalf("capacity override authorized unavailable source %s", id)
		}
	}
}

func TestEvidenceSourceLimitIsConfigurationOwnedAndDurable(t *testing.T) {
	var declared TaskDef
	if err := yaml.Unmarshal([]byte("id: merge\nagent: writer\nexecution:\n  requires-evidence: true\n  max-evidence-sources: 12\n"), &declared); err != nil {
		t.Fatal(err)
	}
	session := &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{BindTaskGoalContracts: true}}, ContractTasks: []TaskDef{declared}}
	bound, effective, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "writer", ContractID: "merge", Execution: ExecutionContract{MaxEvidenceSources: 64}}})
	if err != nil || bound[0].Execution.MaxEvidenceSources != 12 {
		t.Fatalf("static capacity was overridden: bound=%#v err=%v", bound, err)
	}
	raw, err := json.Marshal(bound[0])
	if err != nil {
		t.Fatal(err)
	}
	var restored TaskDef
	if err := json.Unmarshal(raw, &restored); err != nil || restored.Execution.MaxEvidenceSources != 12 {
		t.Fatalf("checkpoint lost capacity: restored=%#v err=%v", restored, err)
	}
	session.ContractTasks[0].Execution.MaxEvidenceSources = 13
	_, changed, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "writer", ContractID: "merge"}})
	if err != nil || changed[0].Hash == effective[0].Hash {
		t.Fatalf("capacity change did not change contract identity: err=%v", err)
	}
	for _, key := range []string{"max_evidence_sources", "max-evidence-sources", "MAX_EVIDENCE_SOURCES"} {
		if err := rejectModelExecutionRuntimeOwnedFields(json.RawMessage(fmt.Sprintf(`{"%s":64}`, key))); err == nil {
			t.Fatalf("model could set configuration-owned %s", key)
		}
	}
	for _, limit := range []int{-1, 65} {
		if result := ValidateExecutionContractFull(TaskDef{Execution: ExecutionContract{MaxEvidenceSources: limit}}, agent.VerifierLintError); result.Valid {
			t.Fatalf("invalid capacity %d passed preflight", limit)
		}
	}
}
