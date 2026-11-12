package inspect

import (
	"github.com/kjelly/hufu/internal/team"
	"testing"
)

func TestRequestAcceptanceSummaryDistinguishesUnavailableVerdicts(t *testing.T) {
	for _, available := range []bool{false, true} {
		review := &team.RequestAcceptanceResult{Contract: team.RequestAcceptanceContract{Requirements: []team.RequestRequirement{{ID: "a", Required: true}, {ID: "b", Required: true}}}, Checks: []team.RequestAcceptanceCheck{{ID: "a", Status: "satisfied"}}}
		if !available {
			review.Error = "context deadline exceeded"
		}
		result := summarizeRequestAcceptance(review)
		expected := "missing"
		if !available {
			expected = "unavailable"
		}
		if result.Error != review.Error || result.Checks[0].Status != "satisfied" || result.Checks[1].Status != expected {
			t.Fatalf("summary %+v", result)
		}
	}
}
