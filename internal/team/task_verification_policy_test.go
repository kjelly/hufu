package team

import "testing"

func TestValidateObjectiveVerificationRequiresTrecDriveVerify(t *testing.T) {
	err := validateObjectiveVerification([]TaskDef{{Agent: "helper", Goal: "Run trec drive --script flow.drive"}})
	if err == nil {
		t.Fatal("expected trec drive task without verify to be rejected")
	}
}

func TestValidateObjectiveVerificationAllowsVerifiedTrecDrive(t *testing.T) {
	err := validateObjectiveVerification([]TaskDef{{Agent: "helper", Goal: "Run trec drive --script flow.drive", Verify: `jq -e '.status == "success"' casts/flow.result.json`}})
	if err != nil {
		t.Fatalf("verified trec drive task rejected: %v", err)
	}
}
