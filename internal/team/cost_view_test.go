package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/cost"
)

func TestProjectCostViewRejectsUnmatchedSettlement(t *testing.T) {
	settlement := cost.SettlementEvent{
		SchemaVersion: cost.EventSchemaVersion,
		InvocationIdentity: cost.InvocationIdentity{
			ProviderInvocationID: "pinv-unmatched", RunID: "run-cost-view", TaskID: "task-1", OccurrenceAttempt: 1,
			Agent: "worker", Role: "worker", Purpose: cost.PurposeWorker,
		},
		ExecutionTarget: "openai/gpt", PriceSnapshotID: "price-missing", FinalMicros: new(int64(1)),
		EstimateSource: cost.EstimateUsage, BillingMode: cost.BillingMetered,
		Outcome: cost.OutcomeSuccess, SettledAt: "2026-09-29T00:00:00Z",
	}
	payload, err := cost.EncodeEvent(cost.Event{Kind: cost.EventSettled, Settlement: &settlement})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ProjectCostView([]RunEvent{{
		RunID: settlement.RunID, TaskID: settlement.TaskID, Attempt: settlement.OccurrenceAttempt,
		Type: string(EventCostSettled), Payload: payload,
	}}, settlement.RunID, "")
	if err == nil {
		t.Fatal("unmatched settlement projected successfully")
	}
}
