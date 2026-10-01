package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
)

// decisionRTSystemOneServer serves one fixed native response and records the
// requests the real registry and adapter send.
type decisionRTSystemOneServer struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
}

func newDecisionRTSystemOneServer(t *testing.T, status int, body string) *decisionRTSystemOneServer {
	t.Helper()
	server := &decisionRTSystemOneServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		server.mu.Lock()
		server.bodies = append(server.bodies, string(data))
		server.headers = append(server.headers, request.Header.Clone())
		server.mu.Unlock()
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *decisionRTSystemOneServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

type decisionRTSystemOneRun struct {
	stdout bytes.Buffer
	stderr bytes.Buffer
	err    error
}

func runDecisionRTSystemOne(environment map[string]string, args ...string) *decisionRTSystemOneRun {
	run := &decisionRTSystemOneRun{}
	command := newDecisionRTCommand(decisionRTDeps{
		registryFactory: NewDefaultRegistry,
		stdin:           strings.NewReader(""), stdout: &run.stdout, stderr: &run.stderr,
		stdinIsTerminal: func() bool { return true },
		getenv:          func(key string) string { return environment[key] },
	})
	command.SetArgs(args)
	run.err = command.Execute()
	return run
}

func systemOneChoiceArgs(server *decisionRTSystemOneServer, extra ...string) []string {
	args := append(validDecisionRTChoiceArgs(),
		"--backend", "systemone", "--systemone-model", "nimble", "--systemone-url", server.URL+"/v1/systemone")
	return append(args, extra...)
}

const decisionRTSystemOneChoiceBody = `{"model":"nimble","answers":{"decision":{"type":"choice","choice":"o01","probabilities":{"o00":0.375,"o01":0.625},"confidence":0.05}},"usage":{"input_tokens":159,"output_tokens":1}}`

func TestDecisionRTSystemOneDecidesWithRawConfidence(t *testing.T) {
	server := newDecisionRTSystemOneServer(t, http.StatusOK, decisionRTSystemOneChoiceBody)
	run := runDecisionRTSystemOne(nil, systemOneChoiceArgs(server, "--json")...)
	assertDecisionRTExitCode(t, run.err, 0)
	var result decisionrt.Result
	if err := json.Unmarshal(run.stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout %q: %v", run.stdout.String(), err)
	}
	want := decisionrt.Result{
		Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "large"},
		Candidates:          []decisionrt.Candidate{{Value: "small", Probability: 0.375}, {Value: "large", Probability: 0.625}},
		Confidence:          0.625,
		ConfidenceSemantics: decisionrt.ConfidenceRaw, Backend: "systemone", Model: "nimble",
	}
	if !jsonEqual(t, result, want) {
		t.Fatalf("result = %#v\nwant %#v", result, want)
	}
	if run.stderr.Len() != 0 {
		t.Fatalf("stderr = %q", run.stderr.String())
	}
}

func TestDecisionRTSystemOneReceipt(t *testing.T) {
	server := newDecisionRTSystemOneServer(t, http.StatusOK, decisionRTSystemOneChoiceBody)
	run := runDecisionRTSystemOne(nil, systemOneChoiceArgs(server, "--receipt", "--no-fallback")...)
	assertDecisionRTExitCode(t, run.err, 0)
	var envelope decisionRTReceiptEnvelope
	if err := json.Unmarshal(run.stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	receipt := envelope.Receipt
	if receipt.Backend != "systemone" || receipt.Model != "nimble" || receipt.FallbackUsed || receipt.Status != decisionrt.StatusDecided ||
		receipt.SchemaVersion != 1 || !strings.HasPrefix(receipt.RequestDigest, "sha256:") || envelope.Result.Backend != "systemone" {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestDecisionRTSystemOneAcceptancePolicyAbstainsWithoutFallback(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "below minimum confidence", args: []string{"--min-confidence", "0.7"}},
		{name: "calibration required", args: []string{"--require-calibrated"}},
		{name: "calibration required with no-fallback", args: []string{"--require-calibrated", "--no-fallback"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newDecisionRTSystemOneServer(t, http.StatusOK, decisionRTSystemOneChoiceBody)
			run := runDecisionRTSystemOne(nil, systemOneChoiceArgs(server, append(test.args, "--json")...)...)
			assertDecisionRTExitCode(t, run.err, 3)
			var result decisionrt.Result
			if err := json.Unmarshal(run.stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Backend != "systemone" || result.FallbackUsed || result.ReasonCode != "low_confidence" || result.Model != "nimble" {
				t.Fatalf("result = %#v", result)
			}
			if server.requestCount() != 1 {
				t.Fatalf("requests = %d", server.requestCount())
			}
		})
	}
	server := newDecisionRTSystemOneServer(t, http.StatusOK, decisionRTSystemOneChoiceBody)
	run := runDecisionRTSystemOne(nil, systemOneChoiceArgs(server, "--min-confidence", "0.6")...)
	assertDecisionRTExitCode(t, run.err, 0)
	if run.stdout.String() != "large\n" {
		t.Fatalf("stdout = %q", run.stdout.String())
	}
}

func TestDecisionRTSystemOneErrorsMapToExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		args     func(*decisionRTSystemOneServer) []string
		wantCode int
		requests int
	}{
		{name: "invalid output", status: http.StatusOK, body: `{"answers":{}}`, wantCode: 4, requests: 1},
		{name: "server error", status: http.StatusInternalServerError, body: `{"error":"boom"}`, wantCode: 4, requests: 1},
		{name: "unknown model", status: http.StatusNotFound, body: `{"error":"model not found"}`, wantCode: 5, requests: 1},
		{
			name: "missing model", status: http.StatusOK, body: decisionRTSystemOneChoiceBody, wantCode: 5,
			args: func(server *decisionRTSystemOneServer) []string {
				return append(validDecisionRTChoiceArgs(), "--backend", "systemone", "--systemone-url", server.URL+"/v1/systemone")
			},
		},
		{
			name: "invalid URL", status: http.StatusOK, body: decisionRTSystemOneChoiceBody, wantCode: 5,
			args: func(server *decisionRTSystemOneServer) []string {
				return append(validDecisionRTChoiceArgs(), "--backend", "systemone", "--systemone-model", "nimble", "--systemone-url", server.URL+"/v1/systemone?x=1")
			},
		},
		{
			name: "single-value integer range", status: http.StatusOK, body: decisionRTSystemOneChoiceBody, wantCode: 4,
			args: func(server *decisionRTSystemOneServer) []string {
				return []string{"integer", "--id", "n", "--version", "v1", "--purpose", "n@v1", "--question", "How many?",
					"--min", "2", "--max", "2", "--backend", "systemone", "--systemone-model", "nimble", "--systemone-url", server.URL + "/v1/systemone"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newDecisionRTSystemOneServer(t, test.status, test.body)
			args := systemOneChoiceArgs(server)
			if test.args != nil {
				args = test.args(server)
			}
			run := runDecisionRTSystemOne(map[string]string{"HUFU_SYSTEMONE_API_KEY": "sk-secret-key"}, args...)
			assertDecisionRTExitCode(t, run.err, test.wantCode)
			if run.stdout.Len() != 0 || run.stderr.Len() == 0 {
				t.Fatalf("stdout=%q stderr=%q", run.stdout.String(), run.stderr.String())
			}
			if strings.Contains(run.stderr.String(), "sk-secret-key") || strings.Contains(run.stderr.String(), test.body) {
				t.Fatalf("stderr leaks: %q", run.stderr.String())
			}
			if server.requestCount() != test.requests {
				t.Fatalf("requests = %d, want %d", server.requestCount(), test.requests)
			}
		})
	}
}

func TestDecisionRTSystemOneBooleanIntegerAndAPIKey(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		args   []string
		stdout string
	}{
		{
			name: "boolean", body: `{"answers":{"decision":{"type":"noul","noul":0.97}}}`, stdout: "true\n",
			args: []string{"boolean", "--id", "retry", "--version", "v1", "--purpose", "retry@v1", "--question", "Retry?", "--context", "failure_class=transient"},
		},
		{
			name: "integer", body: `{"answers":{"decision":{"type":"choice","choice":"i03","probabilities":{"i00":0.1,"i01":0.1,"i02":0.1,"i03":0.6,"i04":0.1},"confidence":0.4}}}`, stdout: "1\n",
			args: []string{"integer", "--id", "n", "--version", "v1", "--purpose", "n@v1", "--question", "How many?", "--min", "-2", "--max", "2"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newDecisionRTSystemOneServer(t, http.StatusOK, test.body)
			args := append(test.args, "--backend", "systemone", "--systemone-model", "nimble", "--systemone-url", server.URL+"/v1/systemone")
			run := runDecisionRTSystemOne(map[string]string{"HUFU_SYSTEMONE_API_KEY": "sk-env", "HUFU_PROVIDER_API_KEY": "sk-provider"}, args...)
			assertDecisionRTExitCode(t, run.err, 0)
			if run.stdout.String() != test.stdout {
				t.Fatalf("stdout = %q", run.stdout.String())
			}
			if got := server.headers[0].Get("Authorization"); got != "Bearer sk-env" {
				t.Fatalf("Authorization = %q", got)
			}
		})
	}
}

func jsonEqual(t *testing.T, got, want any) bool {
	t.Helper()
	left, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left, right)
}
