package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecisionOutputCompletedInvariant(t *testing.T) {
	logical := "ldr_0123456789abcdef0123456789abcdef"
	identifier := "value-1"
	digest := strings.Repeat("a", 64)
	ref := &DecisionArtifactRef{ID: "record-1", SHA256: digest, MediaType: "application/json", SizeBytes: 10}
	value := NewDecisionOutputV1("decision_run_result")
	value.LogicalRunID = &logical
	value.ExecutionRunID = &identifier
	value.BranchID = &identifier
	value.TerminalPersisted = true
	value.LogicalDisposition = "closed"
	value.Outcome = "completed"
	value.ExitCode = 0
	value.GoalSatisfied = true
	value.Primary = DecisionOutputPrimaryV1{State: "bound", TaskID: &identifier, Generation: 1, DecisionID: &identifier, BindingEventID: &identifier, RecordRef: ref, BaseEvidenceRef: ref, SealedEvidenceHash: &digest}
	value.Acceptance = DecisionOutputAcceptanceV1{DecisionProcess: "passed", Team: "not_configured", Combined: "passed"}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Primary.BindingEventID = nil
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "success invariant") {
		t.Fatalf("Validate() = %v, want success invariant failure", err)
	}
}

func TestDecisionOutputNullableFieldsAreExplicit(t *testing.T) {
	value := NewDecisionOutputV1("decision_run_preview")
	value.Outcome = "preview"
	value.ExitCode = 0
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"logical_run_id":null`, `"execution_run_id":null`, `"branch_id":null`, `"team":null`, `"profile":null`, `"coverage":null`, `"continuation":null`, `"view":null`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("JSON missing explicit nullable field %s: %s", field, data)
		}
	}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionRecordViewRedactsBoundsAndRoundsForecastHalfEven(t *testing.T) {
	record := DecisionRecord{
		SchemaVersion: 3, FinalOption: "ship", Probability: 0.1234565,
		Options:    []DecisionOption{{ID: "ship", Kind: OptionExecute, Title: "password=super-secret-value"}},
		Opinions:   []DecisionOpinion{{ID: "opinion-1", Valid: true, PreferredOption: "ship"}},
		Aggregates: []DecisionAggregate{{MeanProbability: map[string]float64{"ship": 0.1234565}}},
	}
	view, _, err := DecisionRecordView(record)
	if err != nil {
		t.Fatal(err)
	}
	if view.ForecastPPM == nil || *view.ForecastPPM != 123456 {
		t.Fatalf("forecast = %v, want round-half-even 123456", view.ForecastPPM)
	}
	if strings.Contains(view.Options[0].Title, "super-secret-value") {
		t.Fatalf("record view leaked secret: %q", view.Options[0].Title)
	}
}

func TestRedactDecisionOutputPreservesNumericTelemetry(t *testing.T) {
	value := NewDecisionOutputV1("decision_run_preview")
	value.Outcome = "preview"
	value.ExitCode = 0
	value.Budget = DecisionOutputBudgetV1{UsedTokens: 41, ReservedTokens: 2, UsedDurationMS: 9}
	value.Warnings = []DecisionOutputMessageV1{{Code: "warning", Message: "api_token=secret-for-output", FieldPath: nil}}
	redacted, err := RedactDecisionOutput(value)
	if err != nil {
		t.Fatal(err)
	}
	if redacted.Budget != value.Budget || !redacted.Redaction.Applied || strings.Contains(redacted.Warnings[0].Message, "secret-for-output") {
		t.Fatalf("redacted output = %#v", redacted)
	}
}

func TestDecisionOutputRejectsInconsistentCoverage(t *testing.T) {
	value := NewDecisionOutputV1("decision_run_result")
	value.Coverage = &DecisionOutputCoverageV1{
		InventoryRef:   DecisionArtifactRef{ID: "inventory", SHA256: strings.Repeat("a", 64), MediaType: "application/json", SizeBytes: 1},
		InventoryCount: 2,
		SelectedCount:  1,
	}
	if err := value.Validate(); err == nil || !strings.Contains(err.Error(), "coverage counts") {
		t.Fatalf("Validate() error = %v", err)
	}
}
