package systemone_test

import (
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/systemone"
)

func TestChoiceRequestMapsOptionsToOpaqueKeys(t *testing.T) {
	for _, count := range []int{2, 21} {
		t.Run(twoDigits(count), func(t *testing.T) {
			keys := opaqueKeys("o", count)
			server := newRecordingServer(t, http.StatusOK, choiceResponse(keys, keys[0], 0.6, 0.2))
			request := choiceRequest(count)
			if _, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			body := server.onlyRequestBody(t)
			question := body["questions"].(map[string]any)["decision"].(map[string]any)
			if question["type"] != "choice" || question["instructions"] != request.Spec.Question {
				t.Fatalf("question = %#v", question)
			}
			criteria := question["criteria"].(map[string]any)
			if len(criteria) != count {
				t.Fatalf("criteria = %#v", criteria)
			}
			for index, option := range request.Spec.Options {
				want := option.ID
				if option.Description != "" {
					want += ": " + option.Description
				}
				if criteria[keys[index]] != want {
					t.Fatalf("criteria[%s] = %#v, want %q", keys[index], criteria[keys[index]], want)
				}
			}
			assertCriteriaOrder(t, server.recorded()[0].body, keys)
		})
	}
}

func TestBooleanRequestUsesNoulWithoutCriteria(t *testing.T) {
	server := newRecordingServer(t, http.StatusOK, noulResponse("0.9"))
	if _, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), booleanRequest()); err != nil {
		t.Fatal(err)
	}
	question := server.onlyRequestBody(t)["questions"].(map[string]any)["decision"].(map[string]any)
	if !reflect.DeepEqual(question, map[string]any{"type": "noul", "instructions": "Should this task be retried?"}) {
		t.Fatalf("question = %#v", question)
	}
}

func TestIntegerRangeRequestUsesChoiceNeverScore(t *testing.T) {
	tests := []struct {
		name             string
		minimum, maximum int64
	}{
		{name: "negative to positive", minimum: -2, maximum: 2},
		{name: "twenty-one values", minimum: 0, maximum: 20},
		{name: "int64 lower edge", minimum: math.MinInt64, maximum: math.MinInt64 + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			count := int(test.maximum - test.minimum + 1)
			keys := opaqueKeys("i", count)
			server := newRecordingServer(t, http.StatusOK, choiceResponse(keys, keys[0], 0.5, 0.1))
			request := integerRequest(test.minimum, test.maximum)
			if _, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			question := server.onlyRequestBody(t)["questions"].(map[string]any)["decision"].(map[string]any)
			if question["type"] != "choice" {
				t.Fatalf("type = %#v", question["type"])
			}
			criteria := question["criteria"].(map[string]any)
			for offset, key := range keys {
				want := json.Number(criteria[key].(string))
				if got, err := want.Int64(); err != nil || got != test.minimum+int64(offset) {
					t.Fatalf("criteria[%s] = %#v", key, criteria[key])
				}
			}
			assertCriteriaOrder(t, server.recorded()[0].body, keys)
		})
	}
}

func TestSingleValueIntegerRangeIsRejectedBeforeHTTP(t *testing.T) {
	server := newRecordingServer(t, http.StatusOK, choiceResponse([]string{"i00", "i01"}, "i00", 1, 1))
	_, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), integerRequest(5, 5))
	assertErrorKind(t, err, decisionrt.ErrorBackendFailure)
	if len(server.recorded()) != 0 {
		t.Fatalf("requests = %d, want none", len(server.recorded()))
	}
}

func TestStateCarriesOnlyCanonicalContext(t *testing.T) {
	tests := []struct {
		name    string
		context map[string]any
		want    map[string]any
	}{
		{name: "nil context", context: nil, want: map[string]any{}},
		{name: "empty context", context: map[string]any{}, want: map[string]any{}},
		{
			name:    "values",
			context: map[string]any{"failure_class": "transient", "attempt": int8(2), "ratio": float64(3), "exact": json.Number("2.50"), "retry": true},
			want:    map[string]any{"failure_class": "transient", "attempt": json.Number("2"), "ratio": json.Number("3"), "exact": json.Number("2.5"), "retry": true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newRecordingServer(t, http.StatusOK, noulResponse("0.2"))
			request := booleanRequest()
			request.Context = test.context
			if _, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			body := server.onlyRequestBody(t)
			if !reflect.DeepEqual(body["state"], test.want) {
				t.Fatalf("state = %#v, want %#v", body["state"], test.want)
			}
			if len(body) != 3 || body["model"] != testModel {
				t.Fatalf("top-level fields = %#v", body)
			}
			raw := string(server.recorded()[0].body)
			for _, metadata := range []string{request.Purpose, `"` + request.Spec.Version + `"`, "sha256:"} {
				if strings.Contains(raw, metadata) {
					t.Fatalf("request body leaks %q: %s", metadata, raw)
				}
			}
		})
	}
}

func TestOversizedRequestIsRejectedBeforeHTTP(t *testing.T) {
	server := newRecordingServer(t, http.StatusOK, noulResponse("0.2"))
	request := choiceRequest(21)
	for index := range request.Spec.Options {
		request.Spec.Options[index].Description = strings.Repeat("d", 1024)
	}
	request.Context = map[string]any{}
	for index := range 12 {
		request.Context["k"+twoDigits(index)] = strings.Repeat("v", 4000)
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request must stay valid for DecisionPrimitive: %v", err)
	}
	_, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), request)
	assertErrorKind(t, err, decisionrt.ErrorBackendFailure)
	if len(server.recorded()) != 0 {
		t.Fatalf("requests = %d, want none", len(server.recorded()))
	}
}

func TestInvalidRequestIsRejectedBeforeHTTP(t *testing.T) {
	server := newRecordingServer(t, http.StatusOK, noulResponse("0.2"))
	request := booleanRequest()
	request.Context = map[string]any{"bad key": "value"}
	_, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), request)
	assertErrorKind(t, err, decisionrt.ErrorInvalidRequest)
	if len(server.recorded()) != 0 {
		t.Fatalf("requests = %d, want none", len(server.recorded()))
	}
}

// assertCriteriaOrder checks that the criteria keys appear in declared order
// in the encoded body, not only in the decoded map.
func assertCriteriaOrder(t *testing.T, body []byte, keys []string) {
	t.Helper()
	raw := string(body)
	last := -1
	for _, key := range keys {
		position := strings.Index(raw, `"`+key+`":`)
		if position <= last {
			t.Fatalf("criteria key %s out of order in %s", key, raw)
		}
		last = position
	}
}
