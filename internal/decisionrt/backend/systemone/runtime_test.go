package systemone_test

import (
	"errors"
	"net/http"
	"sync"
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

func TestConcurrentUse(t *testing.T) {
	keys := opaqueKeys("o", 3)
	server := newRecordingServer(t, http.StatusOK, choiceResponse(keys, keys[0], 0.8, 0.5))
	backend := mustNew(t, systemone.Config{Endpoint: server.endpoint()})
	errorsChannel := make(chan error, 32)
	var wait sync.WaitGroup
	for range 32 {
		wait.Go(func() {
			result, err := backend.Decide(t.Context(), choiceRequest(3))
			if err == nil && (result.Value.Choice != "opt-a" || len(result.Candidates) != 3) {
				err = errors.New("unexpected decision")
			}
			errorsChannel <- err
		})
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(server.recorded()) != 32 {
		t.Fatalf("requests = %d", len(server.recorded()))
	}
}
