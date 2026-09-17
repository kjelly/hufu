package team

import (
	"encoding/json"
	"testing"
)

func TestTerminalEntryPointInventoryIsClosedAndComplete(t *testing.T) {
	points := TerminalEntryPoints()
	if len(points) != 19 {
		t.Fatalf("entry points = %d, want 19", len(points))
	}

	seen := make(map[TerminalEntryPoint]struct{}, len(points))
	for _, point := range points {
		if _, duplicate := seen[point]; duplicate {
			t.Fatalf("duplicate terminal entry point %q", point)
		}
		seen[point] = struct{}{}
		policy, ok := TerminalEntryPolicyFor(point)
		if !ok {
			t.Fatalf("terminal entry point %q has no policy", point)
		}
		switch policy.PrimaryMode {
		case TerminalPrimaryStart, TerminalPrimaryResumeOnly, TerminalPrimaryForbidden:
		default:
			t.Fatalf("terminal entry point %q has invalid mode %q", point, policy.PrimaryMode)
		}
	}

	if _, ok := TerminalEntryPolicyFor("unknown"); ok {
		t.Fatal("unknown terminal entry point unexpectedly has a policy")
	}
}

func TestTerminalEntryPointPrimaryModes(t *testing.T) {
	for _, point := range []TerminalEntryPoint{
		TerminalEntryFinishTool,
		TerminalEntryCoordinatorEOF,
		TerminalEntryDirectAgent,
		TerminalEntryFastRoute,
		TerminalEntryReusedWork,
		TerminalEntryEmbedded,
	} {
		policy, _ := TerminalEntryPolicyFor(point)
		if policy.PrimaryMode != TerminalPrimaryStart {
			t.Errorf("%s mode = %q, want start", point, policy.PrimaryMode)
		}
	}

	resume, _ := TerminalEntryPolicyFor(TerminalEntryResumeCompletion)
	if resume.PrimaryMode != TerminalPrimaryResumeOnly {
		t.Errorf("resume mode = %q, want resume_only", resume.PrimaryMode)
	}
}

func TestTerminalPreparationProofKeepsNullableFields(t *testing.T) {
	proof := TerminalPreparationProof{
		SchemaVersion:  1,
		Kind:           "terminal_preparation_proof",
		LogicalRunID:   "ldr_01010101010101010101010101010101",
		ExecutionRunID: "run-1",
		BranchID:       "main",
		Action:         TerminalPreparationCommitTerminal,
		ReasonCodes:    []string{"decision_not_ready"},
	}

	data, err := json.Marshal(proof)
	if err != nil {
		t.Fatalf("marshal proof: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode proof: %v", err)
	}
	for _, field := range []string{"support_revision_digest", "primary_binding_event_id"} {
		value, ok := got[field]
		if !ok {
			t.Fatalf("proof omitted %q", field)
		}
		if value != nil {
			t.Fatalf("proof %q = %#v, want null", field, value)
		}
	}
}
