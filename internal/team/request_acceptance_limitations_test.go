package team

import (
	"context"
	"strings"
	"testing"
)

func TestRequestAcceptanceContextualLimitationsDoNotInventRequirements(t *testing.T) {
	for _, response := range []bool{false, true} {
		t.Run(map[bool]string{false: "relevant source parts", true: "analysis with limitations"}[response], func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			verification := "observed"
			if response {
				verification = "response"
			}
			snapshot := freezeRequestFixture(t, c, "Compare the reasoning in the requested material", verification)
			const answer = "The supplied passages conflict on their assumptions; the conclusions are limited to the published text."
			item := completedRequestEvidence(t, c, "The relevant source passages were obtained. Unrelated private identifiers are redacted by the publisher.", false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil || len(sources) == 0 {
				t.Fatalf("sources=%#v error=%v", sources, err)
			}
			sourceID := sources[0].ID
			assessment := RequestDeliveryAssessment{Match: "requested", Extent: "relevant_parts", Source: "primary", Gaps: []string{},
				Limitations: []string{"Private source identifiers outside the requested passages are redacted.", "A future revision and additional background material are unavailable."}}
			if response {
				sourceID = requestAcceptanceFinalResponseSource
				// not_applicable is a contract with no material-completeness
				// restriction, not an exact-match obligation on this observation.
				assessment.Extent = "full"
			}
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
				return assessedRequestReview(sourceID, assessment), nil
			})))
			acceptance, err := c.runAcceptance(t.Context(), answer)
			if err != nil || !acceptance.IsPassed() {
				t.Fatalf("contextual uncertainty became a new obligation: %#v error=%v", acceptance.RequestAcceptance, err)
			}
			formatted := FormatRequestAcceptance(acceptance.RequestAcceptance)
			if !strings.Contains(formatted, "Limitation: Private source identifiers") || strings.Contains(formatted, "  Gap:") {
				t.Fatalf("report lost or promoted a caveat: %s", formatted)
			}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true,
				RunInputs: snapshot, Acceptance: acceptance, Response: answer + formatted}
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestAcceptanceLimitationsCannotWaiveMissingRequiredMaterial(t *testing.T) {
	for _, scenario := range []string{"target", "extent", "source", "blocking gap"} {
		t.Run(scenario, func(t *testing.T) {
			requirement := RequestRequirement{ID: "requirement-1", Verification: "observed", Delivery: &RequestDelivery{
				Target: "the requested item", Extent: "full", Source: "primary", AllowedSubstitutes: []RequestDeliverySubstitute{},
			}}
			assessment := &RequestDeliveryAssessment{Match: "requested", Extent: "full", Source: "primary", Gaps: []string{}, Limitations: []string{"Calling a missing obligation a caveat does not satisfy it."}}
			switch scenario {
			case "target":
				assessment.Match = "none"
			case "extent":
				assessment.Extent = "partial"
			case "source":
				assessment.Source = "secondary"
			case "blocking gap":
				assessment.Gaps = []string{"A specifically required section could not be obtained."}
			}
			if err := validateRequestDeliveryAssessment(requirement, RequestAcceptanceCheck{Status: "satisfied", Delivery: assessment}); err == nil {
				t.Fatal("a contextual-limitations field waived an unmet delivery obligation")
			}
		})
	}
}

func TestRequestAcceptanceLimitationNotesRemainBounded(t *testing.T) {
	requirement := RequestRequirement{ID: "requirement-1", Delivery: &RequestDelivery{Target: "analysis", Extent: "not_applicable", Source: "any"}}
	for _, notes := range [][]string{{""}, {strings.Repeat("x", 1025)}, {"1", "2", "3", "4", "5", "6", "7", "8", "9"}} {
		assessment := &RequestDeliveryAssessment{Match: "requested", Extent: "full", Source: "not_applicable", Gaps: []string{}, Limitations: notes}
		if err := validateRequestDeliveryAssessment(requirement, RequestAcceptanceCheck{Status: "satisfied", Delivery: assessment}); err == nil {
			t.Fatalf("invalid limitation notes accepted: %#v", notes)
		}
	}
}

func TestRequestAcceptanceUnconstrainedResponseCanonicalizesPolicyMarker(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Explain the observed differences", "response")
	const answer = "The differences are explained with appropriate limits."
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		return assessedRequestReview(requestAcceptanceFinalResponseSource, RequestDeliveryAssessment{
			Match: "requested", Extent: "not_applicable", Source: "any", Gaps: []string{}, Limitations: []string{"Further background is unavailable."},
		}), nil
	})))
	acceptance, err := c.runAcceptance(t.Context(), answer)
	if err != nil || !acceptance.IsPassed() || acceptance.RequestAcceptance.Checks[0].Delivery.Source != "not_applicable" {
		t.Fatalf("irrelevant provenance policy caused a failure or was not canonicalized: %#v error=%v", acceptance.RequestAcceptance, err)
	}
	result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true,
		RunInputs: snapshot, Acceptance: acceptance, Response: answer + FormatRequestAcceptance(acceptance.RequestAcceptance)}
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRequestAcceptancePolicyMarkerDoesNotEstablishMaterialProvenance(t *testing.T) {
	for _, verification := range []string{"observed", "response"} {
		requirement := RequestRequirement{ID: "requirement-1", Verification: verification, Delivery: &RequestDelivery{Target: "source", Extent: "full", Source: "primary"}}
		assessment := &RequestDeliveryAssessment{Match: "requested", Extent: "full", Source: "any", Gaps: []string{}}
		canonicalizeRequestDeliveryAssessment(requirement, assessment)
		if err := validateRequestDeliveryAssessment(requirement, RequestAcceptanceCheck{Status: "satisfied", Delivery: assessment}); err == nil || !strings.Contains(err.Error(), "delivery.source must be") {
			t.Fatalf("permission marker established constrained provenance: %#v error=%v", assessment, err)
		}
	}
	requirement := RequestRequirement{ID: "requirement-1", Verification: "response", Delivery: &RequestDelivery{
		Target: "analysis", Extent: "not_applicable", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{{Extent: "full", Source: "primary"}},
	}}
	assessment := &RequestDeliveryAssessment{Match: "allowed_substitute", SubstituteIndex: new(0), Extent: "full", Source: "any", Gaps: []string{}}
	canonicalizeRequestDeliveryAssessment(requirement, assessment)
	if assessment.Source != "any" {
		t.Fatal("the unconstrained main target waived a selected alternative's provenance restriction")
	}
}
