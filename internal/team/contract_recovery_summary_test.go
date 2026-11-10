package team

import (
	"encoding/json/v2"
	"reflect"
	"testing"
)

func TestContractRecoveryPreviewExplainsDecisions(t *testing.T) {
	c, _, source := contractRecoveryFixture(t, func(c *Coordinator) { c.acceptanceSpec = &AcceptanceSpec{RequireNoUnresolvedTasks: true} })
	plan, err := c.AssessContractRecovery(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.ReopenedAcceptance, []string{"no_unresolved_tasks"}) {
		t.Fatalf("acceptance not reopened: %v", plan.ReopenedAcceptance)
	}
	want := []string{"result_revalidated", "original_failure", "upstream_changed"}
	for i, reason := range want {
		if plan.Tasks[i].Reason != reason || plan.Tasks[i].SourceStatus != source.Session.Tasks[i].Status {
			t.Fatalf("task %d explanation: %+v", i, plan.Tasks[i])
		}
	}
	if !reflect.DeepEqual(plan.Tasks[2].AffectedBy, []string{"1", "2"}) {
		t.Fatalf("upstream causes: %v", plan.Tasks[2].AffectedBy)
	}
	if len(plan.ContractChanges) != 3 {
		t.Fatalf("changes: %+v", plan.ContractChanges)
	}
	for _, change := range plan.ContractChanges {
		if change.Original.SchemaSHA256 == change.Accepted.SchemaSHA256 {
			t.Fatal("unchanged contract listed")
		}
	}
	changed := *plan
	changed.Tasks = append([]ContractRecoveryTask(nil), plan.Tasks...)
	changed.Tasks[0].Reason = "accepted_result"
	hash, err := recoveryPlanHash(changed)
	if err != nil || hash == plan.SHA256 {
		t.Fatal("explanation not bound to plan hash", err)
	}
	if _, err := c.ApplyContractRecovery(t.Context(), source, "tampered", hash); err == nil {
		t.Fatal("modified preview applied")
	}
	// Missing optional descriptions retain the old canonical JSON shape.
	for i := range changed.Tasks {
		changed.Tasks[i].Reason = ""
		changed.Tasks[i].SourceStatus = ""
		changed.Tasks[i].AffectedBy = nil
	}
	changed.ContractChanges = nil
	changed.ReopenedAcceptance = nil
	changed.ReopenedVerification = nil
	raw, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip ContractRecoveryPlan
	if err := json.Unmarshal(raw, &roundtrip); err != nil {
		t.Fatal(err)
	}
	a, _ := recoveryPlanHash(changed)
	b, _ := recoveryPlanHash(roundtrip)
	if a != b {
		t.Fatal("legacy plan hash changed on round trip")
	}
	roundtrip.SHA256 = b
	raw, err = json.Marshal(roundtrip)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeContractRecovery(RunEvent{SchemaVersion: eventStoreSchemaVersion, BranchID: "legacy-child", Payload: raw}); err != nil {
		t.Fatalf("legacy description-free event rejected: %v", err)
	}
	branch, err := c.ApplyContractRecovery(t.Context(), source, "explained", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == string(EventContractRecovery) && event.BranchID == branch {
			stored, err := decodeContractRecovery(event)
			if err != nil || !reflect.DeepEqual(stored.Tasks, plan.Tasks) || !reflect.DeepEqual(stored.ContractChanges, plan.ContractChanges) {
				t.Fatalf("replay lost descriptions: %v", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing persisted recovery preview")
	}
}

func TestRecoveryAcceptanceItems(t *testing.T) {
	spec := AcceptanceSpec{Criteria: []AcceptanceCriterion{{ID: "report"}}, Commands: []string{"private command"}, Verifications: []VerificationSpec{{Type: VerifyCommandExit}}, RequiredWorkers: []string{"reviewer"}, RequiredArtifacts: []string{"private path"}, RequireNoUnresolvedTasks: true}
	want := []string{"criterion:report", "command[0]", "verification[0]", "required_worker:reviewer", "required_artifact[0]", "no_unresolved_tasks"}
	if got := recoveryAcceptanceItems(spec); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := recoveryAcceptanceItems(AcceptanceSpec{}); len(got) != 0 {
		t.Fatalf("invented acceptance: %v", got)
	}
}
