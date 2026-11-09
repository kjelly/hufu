package evalharness

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// These suites exercise delivery alongside their ordinary lifecycle assertions
// in TestRunAllEvalSuites, rather than running the same configuration twice.
var lifecycleDeliverySuites = []string{"core-lifecycle", "retry-recovery", "memory-learning", "memory-fresh-session", "memory-fresh-session-off", "plan-first-dependency", "verbatim-dependency", "legacy-execution-replay"}

func addLifecycleDeliveryExpectations(fixture *SuiteFixture) {
	for index := range fixture.Cases {
		c := &fixture.Cases[index]
		c.Expect.ContextDelivery = append(c.Expect.ContextDelivery, ContextDeliveryExpect{
			Agent: "coordinator", Purpose: "coordinator_start", Include: []string{"current_task"},
			RequiredFragments: map[string]string{"current_task": c.Prompt},
		})
		var agents []string
		for _, task := range c.Expect.Tasks {
			if (task.Status != "done" && task.Status != "error") || slices.Contains(agents, task.Agent) {
				continue
			}
			agents = append(agents, task.Agent)
			c.Expect.ContextDelivery = append(c.Expect.ContextDelivery, ContextDeliveryExpect{
				Agent: task.Agent, Purpose: "task_execution", Include: []string{"current_task"}, RequiredFragments: map[string]string{"current_task": "## Goal"},
			})
			if task.Attempts != nil && *task.Attempts > 1 {
				c.Expect.ContextDelivery = append(c.Expect.ContextDelivery, ContextDeliveryExpect{
					Agent: task.Agent, Purpose: "task_retry", Attempt: 2, Include: []string{"current_task", "retry_failure_context"}, RequiredFragments: map[string]string{"current_task": "## Goal"},
				})
			}
		}
	}
}

func TestContextDeliveryExtraModelsUseAdmittedTargetsAndParentManifests(t *testing.T) {
	for _, mode := range []string{"legacy", "active"} {
		t.Run(mode, func(t *testing.T) { testExtraModelProviderDelivery(t, mode, false) })
	}
	t.Run("active_request_retry", func(t *testing.T) { testExtraModelProviderDelivery(t, "active", true) })
}

func testExtraModelProviderDelivery(t *testing.T, mode string, retry bool) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: eval-extra-context\nresult-transport: tool\nmodel: eval-harness-model\nmemory-learning:\n  mode: observe\ncontext-routing:\n  query-mode: "+mode+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := "---\nname: worker\nrole: worker\ntools: view\nmodel: qwen3:8b\nextra-models: [qwen3:14b]\nmax-retries: 1\nrecovery: retry\n---\nComplete the assigned task and submit_result.\n"
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte(worker), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &SuiteFixture{Version: 1, Name: "extra-context", Team: dir, Mode: "deterministic", Path: filepath.Join(dir, "cases.yaml")}
	provider := newScriptedProvider(&ProviderFixture{Steps: []ProviderStep{
		{ToolCall: &ToolCallStep{Name: "agent", Arguments: `{"tasks":[{"agent":"worker","goal":"inspect router","constraints":"migration sentinel"}]}`}},
		{ToolCall: &ToolCallStep{Name: "submit_result", Arguments: `{"status":"success","summary":"complete"}`}},
		{ToolCall: &ToolCallStep{Name: "submit_result", Arguments: `{"status":"success","summary":"complete"}`}},
		{Match: &StepMatch{Contains: "runtime_judge"}, Content: `{"token":"A0"}`},
		{Match: &StepMatch{Contains: "grafts"}, Content: `{"reason":"both complete","grafts":[]}`},
		{ToolCall: &ToolCallStep{Name: "finish", Arguments: `{"response":"done"}`}},
	}})
	c := CaseFixture{ID: "parallel-extra-provider-context", Prompt: "inspect router", Expect: ExpectSpec{RunOutcome: "unverified", TaskCount: new(1), Tasks: []TaskExpect{{Agent: "worker", Status: "done"}}, ProviderCallCount: new(6), ContextDelivery: []ContextDeliveryExpect{{Agent: "worker", Include: []string{"current_task"}, RequiredFragments: map[string]string{"current_task": "inspect router"}}}}}
	c.ContextItems = []ContextItemFixture{{ID: "extra-target", Content: "migration sentinel useful background"}, {ID: "foreign", Content: "migration sentinel forbidden", Scope: "other_worker"}}
	for index := range 8 {
		c.ContextItems = append(c.ContextItems, ContextItemFixture{ID: fmt.Sprintf("noise-%d", index), Content: fmt.Sprintf("inspect router noise %d", index)})
	}
	c.Expect.ForbiddenContext = []string{"foreign"}
	if mode == "active" {
		c.Expect.ContextDelivery[0].Include = append(c.Expect.ContextDelivery[0].Include, "extra-target")
		c.Expect.MemoryAggregates = []MemoryAggregateExpect{{ContextItemID: "extra-target", PolicyVersion: "memory-policy-v1", MinExposureCount: new(2), AppliedCount: new(0), PositiveWeight: new(0.0)}}
	} else {
		c.Expect.ContextDelivery[0].Exclude = []string{"extra-target"}
	}
	var mu sync.Mutex
	entered := 0
	extraFailed := false
	both := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if request.Model == "qwen3:8b" || request.Model == "qwen3:14b" {
				mu.Lock()
				fail := retry && request.Model == "qwen3:14b" && !extraFailed
				if fail {
					extraFailed = true
				}
				entered++
				if entered == 2 {
					close(both)
				}
				mu.Unlock()
				select {
				case <-both:
				case <-time.After(3 * time.Second):
					http.Error(w, "extra-model provider calls did not overlap", 500)
					return
				case <-r.Context().Done():
					return
				}
				if fail {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":{"message":"temporary fixture outage","type":"server_error"}}`)
					return
				}
			}
		}
		provider.ServeHTTP(w, r)
	})
	calls, workerCalls := 6, 2
	if retry {
		calls, workerCalls = 7, 3
		c.Expect.ProviderCallCount = new(calls)
	}
	result, err := runCaseWithHandler(t.Context(), fixture, c, handler, provider.scriptFindings)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatalf("admitted extra-model delivery failed: %+v", result.Findings)
	}
	if result.Metrics.ContextDelivery.ProviderCalls != calls || len(result.Metrics.ContextDelivery.Requests) != workerCalls {
		t.Fatalf("missing extra-model receiver evidence: %+v", result.Metrics.ContextDelivery)
	}
	var left, right *ContextRequestMetrics
	for index := range result.Metrics.ContextDelivery.Requests {
		request := &result.Metrics.ContextDelivery.Requests[index]
		if request.ModelID == "qwen3:8b" {
			left = request
		}
		if request.ModelID == "qwen3:14b" {
			right = request
		}
	}
	if left == nil || right == nil || !left.ManifestMatched || !right.ManifestMatched || left.ModelID == right.ModelID || left.ModelExecutionID == right.ModelExecutionID {
		t.Fatal("parallel leaf target/manifest identities collided")
	}
	if entered != workerCalls {
		t.Fatal("two concurrent leaves did not execute")
	}
	if retry {
		var resends []*ContextRequestMetrics
		for index := range result.Metrics.ContextDelivery.Requests {
			request := &result.Metrics.ContextDelivery.Requests[index]
			if request.ModelID == "qwen3:14b" {
				resends = append(resends, request)
			}
		}
		if len(resends) != 2 || resends[0].Attempt != 1 || resends[1].Attempt != 1 ||
			resends[0].ModelExecutionID != resends[1].ModelExecutionID || !resends[0].ManifestMatched || !resends[1].ManifestMatched ||
			!slices.Equal(resends[0].DeliveredIDs, resends[1].DeliveredIDs) {
			t.Fatal("provider resend changed the worker attempt, admitted target or manifest")
		}
	}
	if mode == "active" && (left.ManifestSchema != 3 || right.ManifestSchema != 3 || left.QueryAttribution == nil || right.QueryAttribution == nil) {
		t.Fatal("production active mode missing from extra-model requests")
	}
}
