package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
)

func materialVerdict(form string, adequate bool) []byte {
	value, _ := json.Marshal(struct {
		Checks []RequestMaterialCheck `json:"checks"`
	}{[]RequestMaterialCheck{{"requirement-1", adequate, form, "Independent inspection of the actual material."}}})
	return value
}

func TestRequestAcceptanceMaterialVetoDoesNotAcceptRelatedCoverage(t *testing.T) {
	for _, form := range []string{"indirect", "unknown", "authorized_substitute"} {
		t.Run(form, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			freezeRequestFixture(t, c, "Analyze the reasoning in the source document", "observed")
			const observation = "Official announcement summarizes the document; the document itself was not obtained."
			completedRequestEvidence(t, c, observation, false)
			sources, err := c.requestAcceptanceSources(t.Context(), c.RunInputSnapshot())
			if err != nil {
				t.Fatal(err)
			}
			calls := map[string]int{}
			c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				calls[operation]++
				if operation == "review" {
					return assessedRequestReview(sources[0].ID, RequestDeliveryAssessment{Match: "requested", Extent: "relevant_parts", Source: "primary", Gaps: []string{}, Limitations: []string{"The document itself was not obtained."}}), nil
				}
				if operation != "evidence" {
					t.Fatalf("unexpected operation %s", operation)
				}
				if !strings.Contains(prompt, observation) || strings.Contains(prompt, `"limitations"`) {
					t.Fatal("material review lost observations or inherited optimistic caveats")
				}
				// Even a contradictory true verdict cannot authorize indirect
				// material or an alternative absent from the frozen contract.
				return materialVerdict(form, true), nil
			}))
			acceptance, err := c.runAcceptance(t.Context(), "A useful provisional analysis.")
			if err == nil || acceptance.IsPassed() || calls["review"] != 1 || calls["evidence"] != 1 {
				t.Fatalf("material absence was waived or retried: %#v calls=%v error=%v", acceptance, calls, err)
			}
			r := acceptance.RequestAcceptance
			if r.Checks[0].Status != "missing" || r.MaterialReview == nil || r.MaterialReview.Checks[0].Adequate || r.MaterialReview.ProofHash != r.materialProofHash() {
				t.Fatalf("missing bound negative material verdict: %#v", r)
			}
			if !strings.Contains(FormatRequestAcceptance(r), "Material equivalence: "+form+"; adequate=false") {
				t.Fatal("report hid material veto")
			}
		})
	}
}

func TestRequestAcceptanceMaterialSuccessRequiresBoundIndependentReview(t *testing.T) {
	for _, form := range []string{"direct", "faithful_copy", "authorized_substitute"} {
		t.Run(form, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			request := "Read the source document; a summary is acceptable if unavailable"
			delivery := RequestDelivery{Target: "source document", Extent: "relevant_parts", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{{Description: "summary", SourceQuote: "a summary is acceptable if unavailable", Extent: "relevant_parts", Source: "any"}}}
			snapshot := freezeDeliveryFixture(t, c, request, delivery)
			item := completedRequestEvidence(t, c, "The actual document's relevant passages were obtained.", false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			assessment := fixtureRequestDeliveryAssessment(false)
			if form == "authorized_substitute" {
				assessment.Match, assessment.SubstituteIndex = "allowed_substitute", new(0)
			}
			c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
				if operation == "review" {
					return assessedRequestReview(sources[0].ID, assessment), nil
				}
				if operation != "evidence" {
					t.Fatalf("unexpected operation %s", operation)
				}
				return materialVerdict(form, true), nil
			}))
			acceptance, err := c.runAcceptance(t.Context())
			if err != nil || !acceptance.IsPassed() {
				t.Fatalf("material rejected: %#v %v", acceptance, err)
			}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true, RunInputs: snapshot, Acceptance: acceptance}
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
				t.Fatal(err)
			}
			original, _ := json.Marshal(acceptance.RequestAcceptance)
			for _, mutation := range []func(*RequestAcceptanceResult){
				func(r *RequestAcceptanceResult) { r.MaterialReview = nil },
				func(r *RequestAcceptanceResult) { r.MaterialReview.ProofHash = strings.Repeat("0", 64) },
				func(r *RequestAcceptanceResult) { r.Checks[0].Delivery.Source = "primary" },
				func(r *RequestAcceptanceResult) { r.Checks[0].Evidence[0].Quote += " altered" },
				func(r *RequestAcceptanceResult) { r.MaterialReview.Checks[0].Form = "indirect" },
			} {
				var changed RequestAcceptanceResult
				if err := json.Unmarshal(original, &changed); err != nil {
					t.Fatal(err)
				}
				mutation(&changed)
				acceptance.RequestAcceptance = &changed
				if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err == nil {
					t.Fatal("offline audit accepted absent or altered material review")
				}
			}
		})
	}
}

func TestRequestAcceptanceMaterialReviewFailsClosedWithoutRetry(t *testing.T) {
	for _, scenario := range []string{"provider", "malformed", "missing boolean", "duplicate", "negative"} {
		t.Run(scenario, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			freezeRequestFixture(t, c, "Read the requested document", "observed")
			completedRequestEvidence(t, c, "The requested contents.", false)
			sources, err := c.requestAcceptanceSources(t.Context(), c.RunInputSnapshot())
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
				if operation == "review" {
					return assessedRequestReview(sources[0].ID, fixtureRequestDeliveryAssessment(false)), nil
				}
				if operation != "evidence" {
					t.Fatalf("unexpected operation %s", operation)
				}
				calls++
				switch scenario {
				case "provider":
					return nil, errors.New("provider unavailable")
				case "malformed":
					return []byte(`{"checks":`), nil
				case "missing boolean":
					return []byte(`{"checks":[{"id":"requirement-1","form":"direct","reason":"missing flag"}]}`), nil
				case "duplicate":
					return []byte(`{"checks":[{"id":"requirement-1","adequate":true,"form":"direct","reason":"a"},{"id":"requirement-1","adequate":true,"form":"direct","reason":"b"}]}`), nil
				default:
					return materialVerdict("direct", false), nil
				}
			}))
			acceptance, err := c.runAcceptance(t.Context())
			if err == nil || acceptance.IsPassed() || calls != 1 {
				t.Fatalf("review failed open or retried: %#v %v calls=%d", acceptance, err, calls)
			}
		})
	}
}

func TestRequestAcceptanceResponseDoesNotAcquireMaterialObligation(t *testing.T) {
	c := requestAcceptanceFixture(t)
	freezeRequestFixture(t, c, "Explain the competing views", "response")
	c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
		if operation != "review" {
			t.Fatalf("response-only task invoked %s", operation)
		}
		return assessedRequestReview(requestAcceptanceFinalResponseSource, fixtureRequestDeliveryAssessment(true)), nil
	}))
	acceptance, err := c.runAcceptance(t.Context(), "The competing views.")
	if err != nil || !acceptance.IsPassed() || acceptance.RequestAcceptance.MaterialReview != nil {
		t.Fatalf("response acquired material gate: %#v %v", acceptance, err)
	}
}

func TestRequestAcceptanceMaterialReviewCannotUpgradeOptionalNegative(t *testing.T) {
	r := &RequestAcceptanceResult{Contract: RequestAcceptanceContract{Version: 3, Requirements: []RequestRequirement{{ID: "requirement-1", Verification: "observed", Delivery: &RequestDelivery{Extent: "relevant_parts"}}}}, Checks: []RequestAcceptanceCheck{{ID: "requirement-1", Status: "missing", Delivery: &RequestDeliveryAssessment{Match: "requested"}}}}
	if err := bindRequestMaterialReview(materialVerdict("direct", true), r); err == nil {
		t.Fatal("optional negative upgraded")
	}
	if r.Checks[0].Status != "missing" {
		t.Fatal("negative parent verdict changed")
	}
}

func TestRequestAcceptanceMaterialGatePreservesLegacyVersionTwo(t *testing.T) {
	legacy := []byte(`{"version":2,"request":"read document","requirements":[{"id":"requirement-1","statement":"read document","source_quote":"read document","required":true,"verification":"observed","delivery":{"target":"document","extent":"relevant_parts","source":"any","allowed_substitutes":[]}}]}`)
	snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{requestAcceptanceDefinitionVersion(2)}, []RunInputAssignment{{
		Name: requestAcceptanceInput, RawValue: legacy, Source: RunInputSourceResolver, ResolverID: "hufu-request-acceptance", ResolverVersion: "2",
	}}, "run-legacy", "invocation", "generic")
	if err != nil {
		t.Fatal(err)
	}
	contract, err := RequestAcceptanceContractFromSnapshot(snapshot)
	if err != nil || contract.Version != 2 || contract.Requirements[0].Delivery.Extent != "relevant_parts" {
		t.Fatalf("historical v2 contract changed: %#v %v", contract, err)
	}
	r := &RequestAcceptanceResult{Contract: *contract, Checks: []RequestAcceptanceCheck{{ID: "requirement-1", Status: "satisfied"}}}
	if err := r.validateMaterialReview(); err != nil {
		t.Fatal(err)
	}
	r.MaterialReview = &RequestMaterialReview{}
	if err := r.validateMaterialReview(); err == nil {
		t.Fatal("legacy result falsely certified under new gate")
	}
	if _, err := validateAndCanonicalizeRunInput(requestAcceptanceDefinition().Schema, legacy); err == nil {
		t.Fatal("v3 schema admitted legacy obligations")
	}
}

func TestRequestAcceptanceMaterialGateSkipsActionsAndPrimaryFailures(t *testing.T) {
	for _, action := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary negative", true: "action only"}[action], func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			delivery := RequestDelivery{Target: "requested work", Extent: "relevant_parts", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{}}
			if action {
				delivery.Extent = "not_applicable"
			}
			freezeDeliveryFixture(t, c, "Perform the requested work", delivery)
			completedRequestEvidence(t, c, "The requested work was performed.", false)
			sources, err := c.requestAcceptanceSources(t.Context(), c.RunInputSnapshot())
			if err != nil {
				t.Fatal(err)
			}
			c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
				if operation != "review" {
					t.Fatalf("unexpected material call for %s", operation)
				}
				raw := assessedRequestReview(sources[0].ID, fixtureRequestDeliveryAssessment(false))
				if !action {
					raw = []byte(strings.ReplaceAll(string(raw), `"satisfied"`, `"missing"`))
				}
				return raw, nil
			}))
			acceptance, err := c.runAcceptance(t.Context())
			if action && (err != nil || !acceptance.IsPassed()) {
				t.Fatalf("action acquired material gate: %#v %v", acceptance, err)
			}
			if !action && (err == nil || acceptance.IsPassed()) {
				t.Fatal("negative primary review was upgraded")
			}
		})
	}
}
