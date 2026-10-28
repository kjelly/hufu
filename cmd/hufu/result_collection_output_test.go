package main

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestResultCollectionOutputPreservesBlockedStateAndOmitsContent(t *testing.T) {
	item := &team.TodoItem{ID: "task-1", Status: team.TaskBlocked, Output: "private work report", ExecutionReceipt: &team.ExecutionReceipt{
		RepairProvenance: &team.RepairProvenance{OperatorGrants: []team.ResultRepairGrant{{Recovery: &team.ResultCollectionRecoveryAssessment{Disposition: team.RecoveryRepairClaims, OriginalReason: team.RepairFailureSource, Fingerprint: "assessment-fingerprint"}}}},
		ResultCollection: &team.ResultCollectionCheckpoint{State: team.ResultCollectionBlocked, TurnsUsed: 2, TurnLimit: 2, FailureReason: team.RepairFailureInvalidSchema, Work: team.ResultCollectionRef{ArtifactID: "private-artifact"}},
	}}
	projection := jsonRunTask{ID: item.ID, Status: string(item.Status), ResultCollection: resultCollectionSummaries(item)}
	data, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	var report strings.Builder
	writeResultCollectionReport(&report, []*team.TodoItem{item})
	for _, output := range []string{string(data), report.String()} {
		if !strings.Contains(output, "blocked") || !strings.Contains(output, "invalid_schema") || !strings.Contains(output, "repair_claims") || !strings.Contains(output, "source_invalid") || strings.Contains(output, "private") {
			t.Fatalf("unsafe/incomplete output: %s", output)
		}
	}
	if item.Status != team.TaskBlocked || item.TypedResult != nil {
		t.Fatal("projection fabricated completion")
	}
	legacy, err := json.Marshal(jsonRunTask{ID: "legacy"})
	if err != nil || strings.Contains(string(legacy), "result_collection") {
		t.Fatalf("legacy output gained fake collection status: %s err=%v", legacy, err)
	}
}
