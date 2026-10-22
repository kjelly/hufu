package inspect

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestRequestAcceptanceProjectionDoesNotExposeRequestOrQuotedOutput(t *testing.T) {
	review := &team.RequestAcceptanceResult{SnapshotID: "snapshot", SnapshotHash: "hash", RequestCovered: true,
		Contract: team.RequestAcceptanceContract{Request: "private request", Requirements: []team.RequestRequirement{
			{ID: "requirement-1", Statement: "private statement", SourceQuote: "private request", Required: true, Verification: "observed"},
		}},
		Checks: []team.RequestAcceptanceCheck{{ID: "requirement-1", Status: "blocked", Reason: "private reason",
			Delivery: &team.RequestDeliveryAssessment{Match: "unknown", Extent: "partial", Source: "private invalid source", Gaps: []string{"private gap"}, Limitations: []string{"private contextual limitation"}},
			Evidence: []team.RequestAcceptanceProof{{Quote: "private tool output", ArtifactID: "opaque-evidence-ref"},
				{Quote: "private final answer", SourceID: "candidate-response", ArtifactHash: "response-digest"}}}},
		MaterialReview: &team.RequestMaterialReview{ProofHash: "private proof digest", Checks: []team.RequestMaterialCheck{{ID: "requirement-1", Adequate: false, Form: "indirect", Reason: "private material reason"}}},
	}
	summary := summarizeRequestAcceptance(review)
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private") || !strings.Contains(string(data), "opaque-evidence-ref") || !strings.Contains(string(data), "response-digest") || summary.Checks[0].Status != "blocked" || !summary.Checks[0].Required {
		t.Fatalf("unsafe or incomplete inspect summary: %s", data)
	}
	if summary.Checks[0].DeliveryMatch != "unknown" || summary.Checks[0].DeliveryExtent != "partial" || summary.Checks[0].DeliverySource != "unknown" || summary.Checks[0].DeliveryGapCount != 1 || summary.Checks[0].DeliveryLimitationCount != 1 {
		t.Fatalf("missing delivery status projection: %s", data)
	}
	if summary.Checks[0].MaterialForm != "indirect" || summary.Checks[0].MaterialAdequate == nil || *summary.Checks[0].MaterialAdequate {
		t.Fatalf("missing material projection: %s", data)
	}
}
