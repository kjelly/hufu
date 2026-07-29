package team

import "testing"

func TestValidateObjectiveVerificationRequiresContractVerify(t *testing.T) {
	err := validateObjectiveVerification([]TaskDef{{
		Agent: "helper",
		Goal:  "Run drive script",
		Execution: ExecutionContract{
			Kind:                 ExecutionKindInteractive,
			RequiresVerification: true,
		},
	}})
	if err == nil {
		t.Fatal("expected interactive task with requires_verification=true and missing verify to be rejected")
	}
}

func TestValidateObjectiveVerificationAllowsVerifiedContract(t *testing.T) {
	err := validateObjectiveVerification([]TaskDef{{
		Agent:  "helper",
		Goal:   "Run drive script",
		Verify: `jq -e '.status == "success"' casts/flow.result.json`,
		Execution: ExecutionContract{
			Kind:                 ExecutionKindInteractive,
			RequiresVerification: true,
		},
	}})
	if err != nil {
		t.Fatalf("verified interactive task rejected: %v", err)
	}
}
