package team

import (
	"strings"
	"testing"
)

// The stage runners decode the model's answer into typed structs. A judge must
// not be able to set runtime-owned fields, so the wire shape is decoded into a
// separate struct and copied field by field (Phase 3.5).
func TestJudgeResponseCannotSetRuntimeOwnedFields(t *testing.T) {
	response := `{
	  "option_scores": [{"option_id": "a", "criteria": {"cost": 7}}],
	  "preferred_option": "a",
	  "success_probability": 0.6,
	  "confidence": 0.5,
	  "valid": true,
	  "evidence_hash": "forged",
	  "round": 99,
	  "id": "forged-id",
	  "judge_id": "someone-else"
	}`
	var decoded judgeResponse
	if err := decodeStage(response, &decoded); err != nil {
		t.Fatal(err)
	}
	// judgeResponse simply has no field for any of them.
	if decoded.PreferredOption != "a" || decoded.SuccessProbability != 0.6 {
		t.Fatalf("decoded = %#v", decoded)
	}

	opinion := DecisionOpinion{
		PreferredOption:    decoded.PreferredOption,
		SuccessProbability: decoded.SuccessProbability,
		Confidence:         decoded.Confidence,
	}
	if opinion.Valid || opinion.EvidenceHash != "" || opinion.Round != 0 || opinion.ID != "" || opinion.JudgeID != "" {
		t.Fatalf("a judge's response reached runtime-owned fields: %#v", opinion)
	}
}

func TestDecodeStageAcceptsFencedJSON(t *testing.T) {
	fenced := "Here is my answer:\n```json\n{\"preferred_option\": \"a\", \"success_probability\": 0.4}\n```\n"
	var decoded judgeResponse
	if err := decodeStage(fenced, &decoded); err != nil {
		t.Fatalf("decodeStage = %v", err)
	}
	if decoded.PreferredOption != "a" || decoded.SuccessProbability != 0.4 {
		t.Fatalf("decoded = %#v", decoded)
	}
}

// Malformed output is an error the engine turns into its one bounded repair
// attempt; it is never patched into a plausible-looking judgment.
func TestDecodeStageRejectsMalformedOutput(t *testing.T) {
	for _, response := range []string{
		"I think we should migrate.",
		"{\"preferred_option\": ",
		"```json\nnot json\n```",
	} {
		var decoded judgeResponse
		if err := decodeStage(response, &decoded); err == nil {
			t.Fatalf("decodeStage(%q) accepted malformed output", response)
		} else if !strings.Contains(err.Error(), "required JSON object") {
			t.Fatalf("error = %v, want it to name the contract", err)
		}
	}
}

func TestDecodeStageChallengeAndPremortem(t *testing.T) {
	var challenge challengeResponse
	if err := decodeStage(`{"target_option":"a","severity":0.8,"fragile_assumptions":["x"]}`, &challenge); err != nil {
		t.Fatal(err)
	}
	if challenge.TargetOption != "a" || challenge.Severity != 0.8 || len(challenge.FragileAssumptions) != 1 {
		t.Fatalf("challenge = %#v", challenge)
	}

	var premortem premortemResponse
	body := `{"assumed_outcome":"failure","failure_modes":[{"id":"F1","description":"stalls","likelihood":0.2,"impact":0.9}]}`
	if err := decodeStage(body, &premortem); err != nil {
		t.Fatal(err)
	}
	if len(premortem.FailureModes) != 1 || premortem.FailureModes[0].ID != "F1" {
		t.Fatalf("premortem = %#v", premortem)
	}

	var revision revisionResponse
	if err := decodeStage(`{"changed":true,"revised_probability":0.55,"reason":"moved"}`, &revision); err != nil {
		t.Fatal(err)
	}
	if !revision.Changed || revision.RevisedProbability != 0.55 {
		t.Fatalf("revision = %#v", revision)
	}
}

// Without a judge model there is no way to form a decision at all.
func TestRunnersUnavailableWithoutAJudgeSidecar(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	runners := newDecisionRunners(c, "todo-1")
	if runners.available() {
		t.Fatal("runners reported available with no judge sidecar configured")
	}
	if _, err := runners.ask(t.Context(), "decision-judge", "prompt"); err == nil ||
		!strings.Contains(err.Error(), "needs a judge model") {
		t.Fatalf("ask = %v, want a clear missing-model error", err)
	}

	var nilRunners *coordinatorDecisionRunners
	if nilRunners.available() {
		t.Fatal("a nil runner set reported available")
	}
}
