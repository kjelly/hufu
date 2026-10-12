package evalharness

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestContextDeliveryCountsOnlyModelVisibleFragments(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"<!-- hufu-context authority=historical id=context:visible -->\nactual content"},{"role":"assistant","content":"<!-- hufu-context authority=historical id=context:answer -->\nanswer content","tool_calls":[{"function":{"arguments":"id=context:argument"}}]}],"tools":[{"description":"<!-- hufu-context authority=historical id=context:schema -->"}]}`)
	_, fragments, err := decodeContextRequest(body)
	if err != nil || len(fragments) != 1 || !strings.Contains(fragments["visible"], "actual content") {
		t.Fatalf("visible fragments = %v, error %v", fragments, err)
	}
}

func TestContextDeliveryPairingDistinguishesModelAndToolPayloads(t *testing.T) {
	base := `{"model":"a","messages":[{"role":"user","content":"same input"}],"tools":[{"type":"function","function":{"name":"view"}}]}`
	hash, _, err := decodeContextRequest([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{strings.Replace(base, `"model":"a"`, `"model":"b"`, 1), strings.Replace(base, `"name":"view"`, `"name":"write"`, 1)} {
		other, _, err := decodeContextRequest([]byte(changed))
		if err != nil || other == hash {
			t.Fatal("distinct execution payloads collided in transport pairing")
		}
	}
}

func TestContextDeliveryForbidsUnmarkedContentInEveryRequest(t *testing.T) {
	capture := newContextDeliveryCapture()
	capture.received = []receivedContextRequest{
		{hash: "first", visibleText: "safe"},
		{hash: "later", visibleText: "forbidden sentinel without a marker"},
	}
	findings, _ := capture.evaluate(ExpectSpec{ForbiddenContext: []string{"forbidden"}}, []ContextItemFixture{{ID: "forbidden", Content: "forbidden sentinel"}}, nil, nil)
	if !containsDimension(findings, "forbidden-context") {
		t.Fatalf("unmarked later leakage was missed: %+v", findings)
	}
	data := []byte(`{"messages":[{"role":"assistant","content":"forbidden sentinel"},{"role":"tool","content":"forbidden sentinel"}],"tools":[{"description":"forbidden sentinel"}]}`)
	if text := providerVisibleText(data); text != "" {
		t.Fatalf("non-input content was counted: %q", text)
	}
}

func TestContextDeliveryMetricsIncludeUnseededOptionalItems(t *testing.T) {
	observed := capturedContextRequest{metadata: team.InvocationMetadata{ParentRequestID: "r", ModelExecutionID: "m"}}
	received := receivedContextRequest{fragments: map[string]string{"good": "good content", "noise": "irrelevant content"}}
	manifest := team.ContextInjectionManifest{RequestID: "r", ModelExecutionID: "m", Items: []team.ContextManifestItem{
		{ID: "good", Included: true, Source: "shared_persistent"},
		{ID: "noise", Included: true, Source: "worker_persistent"},
	}}
	_, metrics := evaluateContextRequest(ContextDeliveryExpect{Relevant: []string{"good"}}, observed, received, nil, nil, []team.ContextInjectionManifest{manifest})
	if metrics.DeliveryPrecision == nil || *metrics.DeliveryPrecision != .5 || metrics.IrrelevantOptionalTokens == 0 {
		t.Fatalf("precision hid unseeded optional memory: %+v", metrics)
	}
}

func TestContextDeliveryParallelRetriesKeepStreamOrdinals(t *testing.T) {
	capture := newContextDeliveryCapture()
	var workers sync.WaitGroup
	for index := range 12 {
		workers.Go(func() {
			metadata := team.InvocationMetadata{RunID: "run", AgentName: "worker", TaskID: fmt.Sprint(index), Attempt: 2, ModelExecutionID: fmt.Sprint(index), ParentRequestID: fmt.Sprint(index)}
			for _, id := range []string{fmt.Sprintf("first-%d", index), fmt.Sprintf("first-%d", index), fmt.Sprintf("second-%d", index)} {
				capture.recordObserved(metadata, agent.ProviderTransportObservation{InvocationID: id, ModelID: "test", RequestBody: []byte(`{"model":"test","messages":[{"role":"user","content":"same input"}]}`)})
			}
		})
	}
	workers.Wait()
	if len(capture.findings) != 0 || len(capture.observed) != 36 {
		t.Fatalf("capture: %+v", capture.findings)
	}
	byTask := make(map[string][]int)
	for _, request := range capture.observed {
		byTask[request.metadata.TaskID] = append(byTask[request.metadata.TaskID], request.step)
	}
	for task, steps := range byTask {
		if fmt.Sprint(steps) != "[1 1 2]" {
			t.Fatalf("task %s steps %v", task, steps)
		}
	}
}

func TestContextDeliveryStageTimingRequiresAllStages(t *testing.T) {
	capture := newContextDeliveryCapture()
	observed := capturedContextRequest{metadata: team.InvocationMetadata{ParentRequestID: "r", ModelExecutionID: "m"}}
	metrics := ContextRequestMetrics{MetricReasons: map[string]string{}}
	for _, stage := range []string{"routing", "compilation"} {
		capture.observeSelection(team.ContextSelectionObservation{RequestID: "r", ModelExecutionID: "m", Source: stage, Elapsed: time.Millisecond})
	}
	capture.addRetrievalMetrics(&metrics, ContextDeliveryExpect{}, observed)
	if metrics.SelectionLatencyMS != nil || metrics.MetricReasons["selection_latency_ms"] != "stage_not_observed" {
		t.Fatalf("unknown timing became zero: %+v", metrics)
	}
	capture.observeSelection(team.ContextSelectionObservation{RequestID: "r", ModelExecutionID: "m", Source: "manifest", Elapsed: 2 * time.Millisecond})
	capture.observeSelection(team.ContextSelectionObservation{RequestID: "r", ModelExecutionID: "m", Source: "private_retrieval", Elapsed: time.Millisecond})
	capture.addRetrievalMetrics(&metrics, ContextDeliveryExpect{}, observed)
	if metrics.SelectionLatencyMS == nil || *metrics.SelectionLatencyMS != 5 || metrics.MetricReasons["selection_latency_ms"] != "" {
		t.Fatalf("timing sum: %+v", metrics)
	}
}

func TestContextDeliveryCannotBeSatisfiedByAnEmptyMarkerOrWrongManifest(t *testing.T) {
	observed := capturedContextRequest{metadata: team.InvocationMetadata{AgentName: "worker", TaskID: "1", Attempt: 1, ModelExecutionID: "model-1", ParentRequestID: "request-1"}, step: 1}
	received := receivedContextRequest{fragments: map[string]string{"required": "", "memory": "sentinel"}}
	want := ContextDeliveryExpect{Agent: "worker", Include: []string{"memory"}, RequiredFragments: map[string]string{"required": "goal sentinel"}}
	fixtures := []ContextItemFixture{{ID: "memory", Content: "sentinel"}}
	manifest := team.ContextInjectionManifest{RequestID: "request-1", ModelExecutionID: "model-1", Items: []team.ContextManifestItem{{ID: "memory", Included: false}}}
	findings, metrics := evaluateContextRequest(want, observed, received, fixtures, nil, []team.ContextInjectionManifest{manifest})
	if !containsDimension(findings, "required-fragment") || !containsDimension(findings, "attribution") || metrics.RequiredDeliveryRecall == nil || *metrics.RequiredDeliveryRecall != 0 {
		t.Fatalf("false-positive delivery: findings=%+v metrics=%+v", findings, metrics)
	}
}

func TestContextDeliveryRequiredSentinelNeedsRequiredManifestAttribution(t *testing.T) {
	observed := capturedContextRequest{metadata: team.InvocationMetadata{ParentRequestID: "r", ModelExecutionID: "m"}}
	received := receivedContextRequest{fragments: map[string]string{"current_task": "goal sentinel"}}
	want := ContextDeliveryExpect{RequiredFragments: map[string]string{"current_task": "goal sentinel"}}
	manifest := team.ContextInjectionManifest{RequestID: "r", ModelExecutionID: "m", Items: []team.ContextManifestItem{{ID: "current_task", Included: false, Reason: team.ContextOmittedBudget}}}
	findings, metrics := evaluateContextRequest(want, observed, received, nil, nil, []team.ContextInjectionManifest{manifest})
	if !containsDimension(findings, "required-attribution") || metrics.RequiredDeliveryRecall == nil || *metrics.RequiredDeliveryRecall != 0 {
		t.Fatalf("unattributed sentinel satisfied required delivery: %+v, %+v", findings, metrics)
	}
}

func TestContextDeliveryPairingUsesInvocationIdentityRatherThanArrivalOrder(t *testing.T) {
	capture := newContextDeliveryCapture()
	capture.observed = []capturedContextRequest{
		{metadata: team.InvocationMetadata{AgentName: "a", TaskID: "1", Attempt: 1, ParentRequestID: "ra", ModelExecutionID: "ma"}, hash: "a", step: 1},
		{metadata: team.InvocationMetadata{AgentName: "b", TaskID: "2", Attempt: 1, ParentRequestID: "rb", ModelExecutionID: "mb"}, hash: "b", step: 1},
	}
	capture.received = []receivedContextRequest{
		{hash: "b", fragments: map[string]string{"b": "content b"}},
		{hash: "a", fragments: map[string]string{"a": "content a"}},
	}
	expect := ExpectSpec{ContextDelivery: []ContextDeliveryExpect{{Agent: "a", Include: []string{"a"}}, {Agent: "b", Include: []string{"b"}}}}
	tasks := []*team.TodoItem{
		{ID: "1", ContextManifests: []team.ContextInjectionManifest{{RequestID: "ra", ModelExecutionID: "ma", Items: []team.ContextManifestItem{{ID: "a", Included: true}}}}},
		{ID: "2", ContextManifests: []team.ContextInjectionManifest{{RequestID: "rb", ModelExecutionID: "mb", Items: []team.ContextManifestItem{{ID: "b", Included: true}}}}},
	}
	findings, metrics := capture.evaluate(expect, nil, tasks, nil)
	if len(findings) != 0 || len(metrics.Requests) != 2 || metrics.Requests[0].Agent != "a" {
		t.Fatalf("pairing = %+v, metrics %+v", findings, metrics)
	}
	if metrics.Requests[0].DeliveryPrecision != nil {
		t.Fatal("zero denominator was rendered as a numeric precision")
	}
}

func containsDimension(findings []EvalFinding, dimension string) bool {
	for _, finding := range findings {
		if finding.Dimension == dimension {
			return true
		}
	}
	return false
}
