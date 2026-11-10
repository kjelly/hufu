package team

import (
	"encoding/json/v2"
	"testing"
)

func TestHufuCodeReviewCritiqueEnvelopeMatchesBoundEvidence(t *testing.T) {
	compiled := loadHufuCodeReviewTeam(t).ResultContracts["schemas/critique-v2.json"]
	for _, tc := range []struct {
		name, status                  string
		unverified, sourceGaps, valid bool
	}{
		{name: "settled", status: "success", valid: true},
		{name: "unverified", status: "completed_with_gaps", unverified: true, valid: true},
		{name: "concealed", status: "success", unverified: true},
		{name: "inherited gaps", status: "completed_with_gaps", sourceGaps: true, valid: true},
		{name: "settled with inherited gaps", status: "success", sourceGaps: true, valid: true},
		{name: "invented gap status", status: "completed_with_gaps"},
		{name: "blocked", status: "blocked", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := "confirmed"
			if tc.unverified {
				verdict = "unverified"
			}
			gaps := []any{}
			if tc.sourceGaps {
				gaps = append(gaps, map[string]any{"description": "original limitation"})
			}
			raw, err := json.Marshal(map[string]any{"record": map[string]any{"decisions": []any{map[string]any{"verdict": verdict}}, "input_review": []any{map[string]any{"record": map[string]any{"gaps": gaps}}}}})
			if err != nil {
				t.Fatal(err)
			}
			result := &TaskResult{Status: tc.status, StructuredPayload: &ResultPayload{Value: raw}}
			if tc.status == "blocked" {
				result.StructuredPayload = nil
			}
			err = validateResultEnvelope(compiled, result)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
