package systemone_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/systemone"
)

const testModel = "nimble"

// recordingServer serves one fixed response and records every request.
type recordingServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	body     string
}

type recordedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func newRecordingServer(t *testing.T, status int, body string) *recordingServer {
	t.Helper()
	server := &recordingServer{status: status, body: body}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		server.mu.Lock()
		server.requests = append(server.requests, recordedRequest{
			method: request.Method, path: request.URL.Path, header: request.Header.Clone(), body: data,
		})
		status, body := server.status, server.body
		server.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *recordingServer) endpoint() string {
	return s.URL + "/v1/systemone"
}

func (s *recordingServer) recorded() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

// onlyRequestBody returns the decoded body of the single recorded request.
func (s *recordingServer) onlyRequestBody(t *testing.T) map[string]any {
	t.Helper()
	requests := s.recorded()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	decoder := json.NewDecoder(strings.NewReader(string(requests[0].body)))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode request body %q: %v", requests[0].body, err)
	}
	return body
}

func mustNew(t *testing.T, config systemone.Config) decisionrt.Backend {
	t.Helper()
	if config.Model == "" {
		config.Model = testModel
	}
	backend, err := systemone.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func choiceRequest(optionCount int) decisionrt.Request {
	options := make([]decisionrt.Option, 0, optionCount)
	for index := range optionCount {
		option := decisionrt.Option{ID: "opt-" + string(rune('a'+index))}
		if index%2 == 0 {
			option.Description = "Option " + option.ID
		}
		options = append(options, option)
	}
	return decisionrt.Request{
		Purpose: "route@v1",
		Spec: decisionrt.Spec{
			ID: "route", Version: "v1", Kind: decisionrt.KindChoice, Question: "Select the execution class", Options: options,
		},
	}
}

func booleanRequest() decisionrt.Request {
	return decisionrt.Request{
		Purpose: "retry@v1",
		Spec:    decisionrt.Spec{ID: "retry", Version: "v1", Kind: decisionrt.KindBoolean, Question: "Should this task be retried?"},
	}
}

func integerRequest(minimum, maximum int64) decisionrt.Request {
	return decisionrt.Request{
		Purpose: "reviewers@v1",
		Spec: decisionrt.Spec{
			ID: "reviewers", Version: "v1", Kind: decisionrt.KindIntegerRange, Question: "How many reviewers?",
			Range: &decisionrt.IntegerRange{Min: minimum, Max: maximum},
		},
	}
}

// choiceResponse builds a native choice answer that gives probability to
// the first key and spreads the rest evenly over the other keys.
func choiceResponse(keys []string, selected string, first, confidence float64) string {
	probabilities := make(map[string]float64, len(keys))
	for index, key := range keys {
		if index == 0 {
			probabilities[key] = first
		} else {
			probabilities[key] = (1 - first) / float64(len(keys)-1)
		}
	}
	answer := map[string]any{"type": "choice", "choice": selected, "probabilities": probabilities, "confidence": confidence}
	encoded, err := json.Marshal(map[string]any{"model": testModel, "answers": map[string]any{"decision": answer}})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func noulResponse(probability string) string {
	return `{"model":"nimble","answers":{"decision":{"type":"noul","noul":` + probability + `}},"usage":{"input_tokens":152,"output_tokens":1}}`
}

func opaqueKeys(prefix string, count int) []string {
	keys := make([]string, 0, count)
	for index := range count {
		keys = append(keys, prefix+twoDigits(index))
	}
	return keys
}

func twoDigits(index int) string {
	return string(rune('0'+index/10)) + string(rune('0'+index%10))
}

func assertErrorKind(t *testing.T, err error, want decisionrt.ErrorKind) {
	t.Helper()
	typed, ok := errors.AsType[*decisionrt.RuntimeError](err)
	if !ok || typed.Kind != want {
		t.Fatalf("error = %#v, want kind %s", err, want)
	}
}

// errorChainText joins the messages of err and everything it wraps.
func errorChainText(err error) string {
	var parts []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		parts = append(parts, current.Error())
	}
	return strings.Join(parts, " | ")
}
