package team

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestRequestAcceptanceEvidenceRules(t *testing.T) {
	sources := []requestAcceptanceSource{
		{ID: requestAcceptanceFinalResponseSource, Kind: "response"},
		{ID: "observation", Kind: "observed"},
		{ID: "worker-response", Kind: "response"},
	}
	for _, verification := range []string{"observed", "response"} {
		t.Run(verification, func(t *testing.T) {
			contract := RequestAcceptanceContract{Requirements: []RequestRequirement{{ID: "requirement-1", Verification: verification}}}
			rules := requestAcceptanceEvidenceRules(contract, sources)
			if len(rules) != 1 || rules[0].RequirementID != "requirement-1" {
				t.Fatalf("rules = %#v", rules)
			}
			if verification == "observed" {
				if len(rules[0].AllowedSourceIDs) != 1 || rules[0].AllowedSourceIDs[0] != "observation" || len(rules[0].RequiredSourceIDs) != 0 {
					t.Fatalf("observed rule admitted response evidence: %#v", rules[0])
				}
			} else if len(rules[0].AllowedSourceIDs) != len(sources) || len(rules[0].RequiredSourceIDs) != 1 || rules[0].RequiredSourceIDs[0] != requestAcceptanceFinalResponseSource {
				t.Fatalf("response rule lost final delivery: %#v", rules[0])
			}
		})
	}
}

func TestRequestAcceptanceMixedEvidenceRepair(t *testing.T) {
	for _, repaired := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid_repair", true: "observed_only_repair"}[repaired], func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			snapshot := freezeRequestFixture(t, c, "Retrieve and analyze the document", "observed")
			item := completedRequestEvidence(t, c, "The document was retrieved.", false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil || len(sources) == 0 {
				t.Fatalf("sources=%#v error=%v", sources, err)
			}
			calls := 0
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				calls++
				if operation != "review" || !strings.Contains(prompt, `"evidence_rules"`) {
					t.Fatal("review omitted requirement-specific source eligibility")
				}
				if calls == 2 && !strings.Contains(prompt, "remove EVERY ineligible source") {
					t.Fatal("repair did not explain mixed-source correction")
				}
				proofs := []any{map[string]any{"source_id": sources[0].ID, "quote_index": 0}}
				if calls == 1 || !repaired {
					proofs = append(proofs, map[string]any{"source_id": requestAcceptanceFinalResponseSource, "quote_index": 0})
				}
				return json.Marshal(map[string]any{"request_covered": true, "checks": []any{map[string]any{
					"id": "requirement-1", "status": "satisfied", "reason": "Document retrieved and analyzed.",
					"delivery": fixtureRequestDeliveryAssessment(false), "evidence": proofs,
				}}})
			})))
			acceptance, err := c.runAcceptance(t.Context(), "The document analysis is delivered here.")
			if calls != 2 || (err == nil && acceptance.IsPassed()) != repaired {
				t.Fatalf("calls=%d acceptance=%#v error=%v", calls, acceptance, err)
			}
			if !repaired {
				if acceptance.RequestAcceptance.Error == "" {
					t.Fatal("invalid repair was persisted without its validation error")
				}
				return
			}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true,
				Response: "The document analysis is delivered here.", RunInputs: snapshot, Acceptance: acceptance}
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestAcceptanceFinalResponseEvidenceAndOfflineReplay(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Explain the findings", "response")
	item := completedRequestEvidence(t, c, "Supporting observation only", false)
	response := "The findings show a discrepancy, with uncertainty about its cause."
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
		if operation != "review" || !strings.Contains(prompt, `"id":"candidate-response"`) || !strings.Contains(prompt, response) {
			t.Fatal("review omitted the source for the actual delivered response")
		}
		return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "a discrepancy, with uncertainty"), nil
	})))
	acceptance, err := c.runAcceptance(t.Context(), response)
	if err != nil || !acceptance.IsPassed() {
		t.Fatalf("final response acceptance=%#v error=%v", acceptance, err)
	}
	result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true,
		Response: response + FormatRequestAcceptance(acceptance.RequestAcceptance), RunInputs: snapshot, Acceptance: acceptance}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
		t.Fatal(err)
	}
	acceptance.RequestAcceptance.Checks[0].Evidence[0].Quote = "invented final analysis"
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err == nil {
		t.Fatal("offline replay accepted a quotation absent from the final response")
	}
}

func TestRequestAcceptanceResponseCannotSubstituteForObservationOrFinalDelivery(t *testing.T) {
	for _, verification := range []string{"observed", "response"} {
		t.Run(verification, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			snapshot := freezeRequestFixture(t, c, "Explain the findings", verification)
			completedRequestEvidence(t, c, "Supporting observation only", false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			source := requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}
			if verification == "response" {
				// Worker output alone must not establish delivery to the user.
				source = sources[0]
			}
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
				return passingRequestReview(source, "Supporting observation only"), nil
			})))
			acceptance, err := c.runAcceptance(t.Context(), "Supporting observation only")
			if err == nil || acceptance.IsPassed() {
				t.Fatalf("response source crossed its evidence boundary: %#v error=%v", acceptance, err)
			}
		})
	}
}

func TestRequestAcceptanceProofRepairIsBoundedAndCannotUpgradeVerdicts(t *testing.T) {
	for _, kind := range []string{"correct quote", "still invalid", "upgrade missing", "negative coverage"} {
		t.Run(kind, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			freezeRequestFixture(t, c, "Explain findings", "response")
			calls := 0
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				calls++
				if operation != "review" || !strings.Contains(prompt, "The **verified** finding") {
					t.Fatal("review repair lost its original response")
				}
				raw := passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "The verified finding")
				if calls == 1 && kind == "negative coverage" {
					return []byte(strings.Replace(string(raw), `"request_covered":true`, `"request_covered":false`, 1)), nil
				}
				if calls == 1 && kind == "upgrade missing" {
					return []byte(strings.Replace(string(raw), `"status":"satisfied"`, `"status":"missing"`, 1)), nil
				}
				if calls == 2 {
					if !strings.Contains(prompt, "Rejected review") || !strings.Contains(prompt, "candidate-response; copy a short exact substring") {
						t.Fatal("proof repair omitted exact validation feedback")
					}
					if kind != "still invalid" {
						return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "The **verified** finding"), nil
					}
				}
				return raw, nil
			})))
			acceptance, err := c.runAcceptance(t.Context(), "The **verified** finding is explained here.")
			if kind == "correct quote" {
				if err != nil || !acceptance.IsPassed() || calls != 2 {
					t.Fatalf("quote repair failed: calls=%d acceptance=%#v error=%v", calls, acceptance, err)
				}
				return
			}
			wantCalls := 2
			if kind == "negative coverage" || kind == "upgrade missing" {
				wantCalls = 1
			}
			if err == nil || acceptance.IsPassed() || calls != wantCalls {
				t.Fatalf("invalid proof or upgraded verdict passed: calls=%d acceptance=%#v error=%v", calls, acceptance, err)
			}
		})
	}
}

func TestRequestAcceptanceProofRepairSelectsCorrectSourceKind(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Retrieve the source document", "observed")
	completedRequestEvidence(t, c, "The source document was returned.", false)
	sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
	if err != nil || len(sources) == 0 {
		t.Fatalf("sources=%#v error=%v", sources, err)
	}
	calls := 0
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, _ string, prompt string) ([]byte, error) {
		calls++
		sourceID := requestAcceptanceFinalResponseSource
		if calls == 2 {
			if !strings.Contains(prompt, "requires observed evidence") {
				t.Fatal("repair omitted the source-kind validation feedback")
			}
			sourceID = sources[0].ID
		}
		return json.Marshal(map[string]any{"request_covered": true, "checks": []any{map[string]any{
			"id": "requirement-1", "status": "satisfied", "reason": "The document is available.",
			"delivery": fixtureRequestDeliveryAssessment(false),
			"evidence": []any{map[string]any{"source_id": sourceID, "quote_index": 0}},
		}}})
	})))
	acceptance, err := c.runAcceptance(t.Context(), "I retrieved the source document.")
	if err != nil || !acceptance.IsPassed() || calls != 2 || acceptance.RequestAcceptance.Checks[0].Evidence[0].Kind != "observed" {
		t.Fatalf("source-kind repair failed: calls=%d acceptance=%#v error=%v", calls, acceptance, err)
	}
}

func TestRequestAcceptanceProofRepairCannotUpgradeOptionalNegative(t *testing.T) {
	c := requestAcceptanceFixture(t)
	request := "Optionally explain findings"
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
		if operation == "contract" {
			return faithfulRequestContract(), nil
		}
		return []byte(strings.Replace(string(requestExtraction(request, request, "response")), `"required":true`, `"required":false`, 1)), nil
	})))
	ctx, end := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"request_covered":true,"checks":[{"id":"requirement-1","status":"missing","reason":"No explanation","evidence":[{"source_id":"candidate-response","quote":"invented"}]}]}`), nil
		}
		return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "The finding is explained."), nil
	})))
	acceptance, err := c.runAcceptance(ctx, "The finding is explained.")
	if err == nil || acceptance.IsPassed() || calls != 2 || !strings.Contains(err.Error(), "upgraded a negative verdict") {
		t.Fatalf("optional negative verdict was upgraded: calls=%d acceptance=%#v error=%v", calls, acceptance, err)
	}
}

func TestRequestAcceptanceIndexedProofBindsExactUnicodeSourceAndAudits(t *testing.T) {
	for _, verification := range []string{"observed", "response"} {
		t.Run(verification, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			snapshot := freezeRequestFixture(t, c, "Explain the findings", verification)
			const suffix = "中文**原文**\nExact source quotation."
			text := strings.Repeat("界", 384) + suffix
			item := completedRequestEvidence(t, c, text, false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil || len(sources) == 0 {
				t.Fatalf("sources=%#v error=%v", sources, err)
			}
			sourceID := sources[0].ID
			if verification == "response" {
				sourceID = requestAcceptanceFinalResponseSource
			}
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				if operation != "review" || !strings.Contains(prompt, `"quote_options"`) || !strings.Contains(prompt, `"index":1`) {
					t.Fatal("review did not receive indexed source text")
				}
				return json.Marshal(map[string]any{"request_covered": true, "checks": []any{map[string]any{
					"id": "requirement-1", "status": "satisfied", "reason": "The selected source supports the requirement.",
					"delivery": fixtureRequestDeliveryAssessment(verification == "response"),
					"evidence": []any{map[string]any{"source_id": sourceID, "quote_index": 1}},
				}}})
			})))
			acceptance, err := c.runAcceptance(t.Context(), text)
			if err != nil || !acceptance.IsPassed() {
				t.Fatalf("indexed acceptance=%#v error=%v", acceptance, err)
			}
			proof := acceptance.RequestAcceptance.Checks[0].Evidence[0]
			if proof.Quote != suffix || proof.QuoteIndex != nil {
				t.Fatalf("runtime did not bind the exact UTF-8 quotation: %#v", proof)
			}
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true,
				Response: text + FormatRequestAcceptance(acceptance.RequestAcceptance), RunInputs: snapshot, Acceptance: acceptance}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestAcceptanceIndexedProofRejectsInvalidSelection(t *testing.T) {
	for _, evidence := range []string{
		`{"source_id":"candidate-response","quote_index":-1}`,
		`{"source_id":"candidate-response","quote_index":999}`,
		`{"source_id":"candidate-response","quote_index":0,"quote":"invented"}`,
		`{"source_id":"foreign-source","quote_index":0}`,
	} {
		c := requestAcceptanceFixture(t)
		freezeRequestFixture(t, c, "Explain findings", "response")
		c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
			return []byte(`{"request_covered":true,"checks":[{"id":"requirement-1","status":"satisfied","reason":"ok","evidence":[` + evidence + `]}]}`), nil
		})))
		acceptance, err := c.runAcceptance(t.Context(), "The finding is explained.")
		if err == nil || acceptance.IsPassed() {
			t.Fatalf("invalid selection accepted: %s; result=%#v", evidence, acceptance)
		}
	}
}

func TestRequestAcceptanceCoordinatorFinishBoundsRepairAndPersistsPartial(t *testing.T) {
	c := requestAcceptanceFixture(t)
	freezeRequestFixture(t, c, "Retrieve the complete source", "observed")
	completedRequestEvidence(t, c, "source unavailable", false)
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		return []byte(`{"request_covered":true,"checks":[{"id":"requirement-1","status":"blocked","reason":"source unavailable","evidence":[]}]}`), nil
	})))
	tool := &finishTool{coordinator: c}
	for attempt := range 3 {
		response, err := tool.Run(t.Context(), fantasy.ToolCall{Input: `{"response":"Only partial material is available."}`})
		if err != nil {
			t.Fatal(err)
		}
		if response.IsError != (attempt < 2) {
			t.Fatalf("finish attempt=%d response=%#v", attempt, response)
		}
	}
	result := c.LastRunResult()
	if result == nil || result.Outcome != RunOutcomePartial || result.StopReason != StopReasonAcceptanceFailed || result.GoalSatisfied || c.selfHealingAttempts != 2 {
		t.Fatalf("missing source escaped bounded acceptance: %#v attempts=%d", result, c.selfHealingAttempts)
	}
}

func TestRequestAcceptanceRuntimeVerifierEvidenceIsBoundToReceipt(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Run the verification suite", "observed")
	item := completedRequestEvidence(t, c, "work completed", false)
	verification := &VerificationResult{Command: "fixture-verifier", ExitCode: 0, Stdout: "all requested checks passed", Fingerprint: "verified-attempt"}
	if err := c.taskTracker.TodoList().SetVerificationResult(item.ID, verification); err != nil {
		t.Fatal(err)
	}
	item = c.todoItemByID(item.ID)
	sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
	if err != nil || len(sources) == 0 || sources[0].Tool != "runtime_verification" {
		t.Fatalf("runtime verification unavailable: %#v %v", sources, err)
	}
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		return passingRequestReview(sources[0], "all requested checks passed"), nil
	})))
	acceptance, err := c.runAcceptance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true, RunInputs: snapshot, Acceptance: acceptance}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
		t.Fatal(err)
	}
	item.VerifyResult.ExitCode = 1
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err == nil {
		t.Fatal("failed runtime verifier accepted as successful evidence")
	}
}

// A typed result's verification entry carries a time.Duration. Its response
// text must still render for the reviewer and for offline quote replay.
func TestRequestAcceptanceResponseSourceRendersVerification(t *testing.T) {
	for _, tc := range []struct {
		name         string
		verification VerificationResult
	}{
		{name: "zero_duration", verification: VerificationResult{Command: "go test ./...", ExitCode: 0}},
		{name: "duration", verification: VerificationResult{Command: "go test ./...", ExitCode: 0, Duration: 1500 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			snapshot := freezeRequestFixture(t, c, "Explain the findings", "response")
			item := completedRequestEvidence(t, c, "Supporting observation only", false)
			item.TypedResult.Verification = []VerificationResult{tc.verification}
			if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
				t.Fatal(err)
			}
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil {
				t.Fatalf("response source failed to render: %v", err)
			}
			const quote = "comparison is explained with its limits"
			var response *requestAcceptanceSource
			for i := range sources {
				if sources[i].Kind == "response" && sources[i].TaskID == item.ID {
					response = &sources[i]
				}
			}
			if response == nil || !strings.Contains(response.Text, quote) || !strings.Contains(response.Text, `"verification"`) {
				t.Fatalf("response source=%+v", response)
			}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			result := &RunResult{RunID: c.executionRunID, RunInputs: snapshot}
			proof := RequestAcceptanceProof{Kind: "response", TaskID: item.ID, ArtifactID: response.ArtifactID, ArtifactHash: response.ArtifactHash, Quote: quote}
			if err := validateRequestAcceptanceProof(t.Context(), result, []*TodoItem{c.todoItemByID(item.ID)}, store, proof); err != nil {
				t.Fatalf("offline replay rejected the rendered response quote: %v", err)
			}
			proof.Quote = "invented analysis"
			if err := validateRequestAcceptanceProof(t.Context(), result, []*TodoItem{c.todoItemByID(item.ID)}, store, proof); err == nil {
				t.Fatal("offline replay accepted a quotation absent from the typed result")
			}
		})
	}
}
