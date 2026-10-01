package systemone_test

import (
	"net/http"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/systemone"
)

// TestRuntimeAcceptsMappedResults runs the adapter through NewRuntime, whose
// result validation stays authoritative after mapping.
func TestRuntimeAcceptsMappedResults(t *testing.T) {
	tests := []struct {
		name    string
		request decisionrt.Request
		body    string
		policy  decisionrt.AcceptancePolicy
		status  decisionrt.Status
		reason  string
	}{
		{name: "choice", request: choiceRequest(21), body: choiceResponse(opaqueKeys("o", 21), "o00", 0.8, 0.1), status: decisionrt.StatusDecided},
		{name: "integer", request: integerRequest(-10, 10), body: choiceResponse(opaqueKeys("i", 21), "i00", 0.4, 0.1), status: decisionrt.StatusDecided},
		{name: "boolean certain false", request: booleanRequest(), body: noulResponse("0"), status: decisionrt.StatusDecided},
		{name: "boolean certain true", request: booleanRequest(), body: noulResponse("1"), status: decisionrt.StatusDecided},
		{
			name: "below minimum confidence", request: choiceRequest(2), body: choiceResponse(opaqueKeys("o", 2), "o00", 0.55, 0.9),
			policy: decisionrt.AcceptancePolicy{MinConfidence: new(0.6)}, status: decisionrt.StatusAbstained, reason: "low_confidence",
		},
		{
			name: "at minimum confidence", request: choiceRequest(2), body: choiceResponse(opaqueKeys("o", 2), "o00", 0.75, 0.1),
			policy: decisionrt.AcceptancePolicy{MinConfidence: new(0.75)}, status: decisionrt.StatusDecided,
		},
		{
			name: "calibration required", request: booleanRequest(), body: noulResponse("0.99"),
			policy: decisionrt.AcceptancePolicy{MinConfidence: new(0.0), RequireCalibratedConfidence: true}, status: decisionrt.StatusAbstained, reason: "low_confidence",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newRecordingServer(t, http.StatusOK, test.body)
			runtime, err := decisionrt.NewRuntime(decisionrt.RuntimeConfig{
				Primary: mustNew(t, systemone.Config{Endpoint: server.endpoint()}), Policy: test.policy,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, receipt, err := runtime.Decide(t.Context(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.status || result.ReasonCode != test.reason || result.Backend != "systemone" || result.Model != testModel || result.FallbackUsed {
				t.Fatalf("result = %#v", result)
			}
			if test.status == decisionrt.StatusDecided && result.ConfidenceSemantics != decisionrt.ConfidenceRaw {
				t.Fatalf("confidence semantics = %s", result.ConfidenceSemantics)
			}
			if receipt.Backend != "systemone" || receipt.Model != testModel || receipt.Status != test.status {
				t.Fatalf("receipt = %#v", receipt)
			}
		})
	}
}
