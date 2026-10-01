package systemone_test

import (
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/systemone"
)

func TestChoiceResultUsesSelectedProbabilityAsRawConfidence(t *testing.T) {
	// Provider confidence (0.00014) differs from the selected probability,
	// as Ollama 0.35 reports for a near-even two-way choice.
	server := newRecordingServer(t, http.StatusOK,
		`{"model":"nimble-alias","answers":{"decision":{"type":"choice","choice":"o01","probabilities":{"o00":0.4930901009524555,"o01":0.5069098990475446},"confidence":0.00013777225424671524}},"usage":{"input_tokens":159,"output_tokens":1}}`)
	result, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), choiceRequest(2))
	if err != nil {
		t.Fatal(err)
	}
	want := decisionrt.BackendResult{
		Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "opt-b"},
		Candidates: []decisionrt.Candidate{
			{Value: "opt-a", Probability: 0.4930901009524555}, {Value: "opt-b", Probability: 0.5069098990475446},
		},
		Confidence: 0.5069098990475446, ConfidenceSemantics: decisionrt.ConfidenceRaw, Model: testModel,
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("result = %#v\nwant %#v", result, want)
	}
}

func TestChoiceResultKeepsANonMaximumProviderSelection(t *testing.T) {
	keys := opaqueKeys("o", 21)
	server := newRecordingServer(t, http.StatusOK, choiceResponse(keys, keys[20], 0.9, 0.5))
	result, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), choiceRequest(21))
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.Choice != "opt-u" || len(result.Candidates) != 21 || result.Candidates[0].Value != "opt-a" || math.Abs(result.Confidence-0.1/20) > 1e-12 {
		t.Fatalf("result = %#v", result)
	}
}

func TestIntegerResultMapsBackToTypedValue(t *testing.T) {
	keys := opaqueKeys("i", 5)
	server := newRecordingServer(t, http.StatusOK, choiceResponse(keys, "i03", 0.2, 0.3))
	result, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), integerRequest(-2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.Integer == nil || *result.Value.Integer != 1 || result.Candidates[0].Value != "-2" || result.Candidates[4].Value != "2" {
		t.Fatalf("result = %#v", result)
	}
	if math.Abs(result.Confidence-0.2) > 1e-12 || result.ConfidenceSemantics != decisionrt.ConfidenceRaw {
		t.Fatalf("confidence = %v %s", result.Confidence, result.ConfidenceSemantics)
	}
}

func TestNoulResultMapping(t *testing.T) {
	tests := []struct {
		noul       string
		value      bool
		confidence float64
	}{
		{noul: "0", value: false, confidence: 1},
		{noul: "0.1", value: false, confidence: 0.9},
		{noul: "0.5", value: false, confidence: 0.5},
		{noul: "0.9", value: true, confidence: 0.9},
		{noul: "1", value: true, confidence: 1},
	}
	for _, test := range tests {
		t.Run(test.noul, func(t *testing.T) {
			server := newRecordingServer(t, http.StatusOK, noulResponse(test.noul))
			result, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), booleanRequest())
			if err != nil {
				t.Fatal(err)
			}
			if result.Value.Boolean == nil || *result.Value.Boolean != test.value || math.Abs(result.Confidence-test.confidence) > 1e-12 {
				t.Fatalf("result = %#v", result)
			}
			if len(result.Candidates) != 2 || result.Candidates[0].Value != "false" || result.Candidates[1].Value != "true" ||
				math.Abs(result.Candidates[0].Probability+result.Candidates[1].Probability-1) > 1e-12 {
				t.Fatalf("candidates = %#v", result.Candidates)
			}
			if result.ConfidenceSemantics != decisionrt.ConfidenceRaw || result.Model != testModel {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestInvalidResponsesAreInvalidOutput(t *testing.T) {
	validChoice := `"type":"choice","choice":"o00","probabilities":{"o00":0.75,"o01":0.25},"confidence":0.5`
	choiceBody := func(answer string) string {
		return `{"answers":{"decision":{` + answer + `}}}`
	}
	tests := []struct {
		name    string
		request decisionrt.Request
		body    string
	}{
		{name: "not JSON", request: choiceRequest(2), body: `not json`},
		{name: "empty body", request: choiceRequest(2), body: ``},
		{name: "top-level array", request: choiceRequest(2), body: `[]`},
		{name: "missing answers", request: choiceRequest(2), body: `{"model":"nimble"}`},
		{name: "null answers", request: choiceRequest(2), body: `{"answers":null}`},
		{name: "answers array", request: choiceRequest(2), body: `{"answers":[]}`},
		{name: "missing decision", request: choiceRequest(2), body: `{"answers":{"other":{` + validChoice + `}}}`},
		{name: "null decision", request: choiceRequest(2), body: `{"answers":{"decision":null}}`},
		{name: "extra answer", request: choiceRequest(2), body: `{"answers":{"decision":{` + validChoice + `},"other":{}}}`},
		{name: "wrong answer type", request: choiceRequest(2), body: choiceBody(strings.Replace(validChoice, `"choice","choice"`, `"score","choice"`, 1))},
		{name: "noul for choice", request: choiceRequest(2), body: choiceBody(`"type":"noul","noul":0.5`)},
		{name: "undeclared choice", request: choiceRequest(2), body: choiceBody(strings.Replace(validChoice, `"choice":"o00"`, `"choice":"opt-a"`, 1))},
		{name: "choice not string", request: choiceRequest(2), body: choiceBody(strings.Replace(validChoice, `"choice":"o00"`, `"choice":0`, 1))},
		{name: "missing confidence", request: choiceRequest(2), body: choiceBody(strings.Replace(validChoice, `,"confidence":0.5`, ``, 1))},
		{name: "null confidence", request: choiceRequest(2), body: choiceBody(strings.Replace(validChoice, `"confidence":0.5`, `"confidence":null`, 1))},
		{name: "confidence above one", request: choiceRequest(2), body: choiceBody(strings.Replace(validChoice, `"confidence":0.5`, `"confidence":1.5`, 1))},
		{name: "missing probability key", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":{"o00":1},"confidence":0.5`)},
		{name: "extra probability key", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":{"o00":0.5,"o01":0.5,"o02":0},"confidence":0.5`)},
		{name: "unknown probability key", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":{"o00":0.5,"x":0.5},"confidence":0.5`)},
		{name: "probabilities not object", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":[0.5,0.5],"confidence":0.5`)},
		{name: "sum below one", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":{"o00":0.7,"o01":0.299},"confidence":0.5`)},
		{name: "negative probability", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":{"o00":1.5,"o01":-0.5},"confidence":0.5`)},
		{name: "probability as string", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":{"o00":"0.5","o01":0.5},"confidence":0.5`)},
		{name: "non-finite probability", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":{"o00":1e400,"o01":0},"confidence":0.5`)},
		{name: "duplicate top-level key", request: choiceRequest(2), body: `{"answers":{"decision":{` + validChoice + `}},"answers":{}}`},
		{name: "escaped duplicate key", request: choiceRequest(2), body: choiceBody(validChoice + `,"confidence":0.5`)},
		{name: "nested duplicate key", request: choiceRequest(2), body: choiceBody(`"type":"choice","choice":"o00","probabilities":{"o00":0.75,"o00":0.75,"o01":0.25},"confidence":0.5`)},
		{name: "trailing JSON", request: choiceRequest(2), body: choiceBody(validChoice) + `{}`},
		{name: "malformed UTF-8", request: choiceRequest(2), body: "{\"answers\":{\"decision\":{" + validChoice + ",\"note\":\"\xff\"}}}"},
		{name: "missing noul", request: booleanRequest(), body: choiceBody(`"type":"noul"`)},
		{name: "null noul", request: booleanRequest(), body: choiceBody(`"type":"noul","noul":null`)},
		{name: "noul above one", request: booleanRequest(), body: choiceBody(`"type":"noul","noul":1.01`)},
		{name: "noul as bool", request: booleanRequest(), body: choiceBody(`"type":"noul","noul":true`)},
		{name: "choice for noul", request: booleanRequest(), body: choiceBody(validChoice)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newRecordingServer(t, http.StatusOK, test.body)
			_, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), test.request)
			assertErrorKind(t, err, decisionrt.ErrorInvalidBackendOutput)
			if strings.Contains(errorChainText(err), "o00") || strings.Contains(errorChainText(err), "nimble") {
				t.Fatalf("error repeats the response body: %v", errorChainText(err))
			}
		})
	}
}

func TestZeroValuesAndUnknownExtensionFieldsAreAccepted(t *testing.T) {
	server := newRecordingServer(t, http.StatusOK,
		`{"answers":{"decision":{"type":"choice","choice":"o01","probabilities":{"o00":0,"o01":1},"confidence":0,"extra":{"nested":[1,2]}}},"usage":{},"future":true}`)
	result, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), choiceRequest(2))
	if err != nil {
		t.Fatal(err)
	}
	if result.Value.Choice != "opt-b" || result.Confidence != 1 {
		t.Fatalf("result = %#v", result)
	}
}
