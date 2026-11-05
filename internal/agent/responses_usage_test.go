package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"
)

func responsesUsageFixture(status, usage string, stream bool) string {
	output := `{"type":"function_call","id":"fc","call_id":"call","name":"answer","arguments":"{\"value\":\"ok\"}","status":"completed"}`
	extra := ""
	if status == "failed" {
		extra = `,"error":{"message":"fixture failed","code":"server_error"}`
	}
	if status == "incomplete" {
		extra = `,"incomplete_details":{"reason":"max_output_tokens"}`
	}
	if usage != "" {
		extra += `,"usage":` + usage
	}
	response := fmt.Sprintf(`{"id":"r","object":"response","status":%q,"output":[%s]%s}`, status, output, extra)
	if !stream {
		return response
	}
	event := func(kind, payload string) string { return "event: " + kind + "\ndata: " + payload + "\n\n" }
	result := event("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":"answer","arguments":""}}`)
	result += event("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"value\":\"ok\"}"}`)
	result += event("response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, output))
	return result + event("response."+status, fmt.Sprintf(`{"type":%q,"response":%s}`, "response."+status, response))
}

func invokeResponsesFixture(ctx context.Context, model fantasy.LanguageModel, entry string) error {
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("question")}, Tools: []fantasy.Tool{fantasy.FunctionTool{Name: "answer", InputSchema: map[string]any{"type": "object"}}}}
	object := fantasy.ObjectCall{Prompt: call.Prompt, SchemaName: "answer", Schema: schema.Generate(reflect.TypeFor[struct {
		Value string `json:"value"`
	}]())}
	switch entry {
	case "generate":
		_, err := model.Generate(ctx, call)
		return err
	case "generate_object":
		_, err := model.GenerateObject(ctx, object)
		return err
	case "stream":
		parts, err := model.Stream(ctx, call)
		if err != nil {
			return err
		}
		for part := range parts {
			if part.Error != nil {
				return part.Error
			}
		}
	case "stream_object":
		parts, err := model.StreamObject(ctx, object)
		if err != nil {
			return err
		}
		for part := range parts {
			if part.Error != nil {
				return part.Error
			}
		}
	default:
		return fmt.Errorf("unknown fixture entry %s", entry)
	}
	return nil
}

func TestResponsesUsagePresenceAndSettlement(t *testing.T) {
	for _, entry := range []string{"generate", "stream", "generate_object", "stream_object"} {
		for _, tc := range []struct {
			name, raw string
			want      *ProviderInvocationUsage
		}{
			{name: "absent"}, {name: "null", raw: "null"},
			{name: "zero", raw: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, want: &ProviderInvocationUsage{}},
			{name: "cached and reasoning", raw: `{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":6,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":16}`, want: &ProviderInvocationUsage{InputTokens: 6, CacheReadTokens: 4, OutputTokens: 6, TotalTokens: 16}},
			{name: "missing total", raw: `{"input_tokens":4,"output_tokens":2}`},
			{name: "negative", raw: `{"input_tokens":4,"output_tokens":-2,"total_tokens":2}`},
			{name: "cache exceeds input", raw: `{"input_tokens":4,"input_tokens_details":{"cached_tokens":5},"output_tokens":2,"total_tokens":6}`},
			{name: "reasoning exceeds output", raw: `{"input_tokens":4,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":6}`},
			{name: "overflow", raw: `{"input_tokens":9223372036854775807,"output_tokens":1,"total_tokens":0}`},
		} {
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				calls := 0
				streaming := strings.HasPrefix(entry, "stream")
				raw := responsesUsageFixture("completed", tc.raw, streaming)
				inner := responsesFixtureModel(t, "custom", func(*http.Request) (*http.Response, error) {
					calls++
					response := sdkJSONResponse(raw, 200)
					if streaming {
						response.Header.Set("Content-Type", "text/event-stream")
					}
					return response, nil
				})
				admission := &settlingAdmission{recordingAdmission: &recordingAdmission{}}
				model := NewAdmittedLanguageModel("remote/custom", inner, admission)
				if err := invokeResponsesFixture(t.Context(), model, entry); err != nil {
					t.Fatal(err)
				}
				results := admission.snapshotSettlements()
				if calls != 1 || len(results) != 1 || len(admission.commits) != 1 {
					t.Fatalf("calls=%d results=%v commits=%d", calls, results, len(admission.commits))
				}
				result := results[0].result
				if result.UsageReported != (tc.want != nil) || !reflect.DeepEqual(result.Usage, tc.want) || !result.ResponseObserved || result.Outcome != ProviderInvocationSuccess {
					t.Fatalf("result=%+v usage=%+v want=%+v", result, result.Usage, tc.want)
				}
				if results[0].request.InvocationID == "" || results[0].request.AdmissionContext.APIProtocol.Effective != "responses" {
					t.Fatalf("request=%+v", results[0].request)
				}
			})
		}
	}
}

func TestResponsesFailedAndAbandonedStreamsSettleOnce(t *testing.T) {
	usage := `{"input_tokens":4,"output_tokens":2,"total_tokens":6}`
	for _, tc := range []struct {
		name, raw   string
		stop, known bool
		outcome     ProviderInvocationOutcome
	}{
		{name: "failed", raw: responsesUsageFixture("failed", usage, true), known: true, outcome: ProviderInvocationProviderError},
		{name: "missing terminal", raw: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n", outcome: ProviderInvocationProviderError},
		{name: "consumer stop", raw: responsesUsageFixture("completed", usage, true), stop: true, known: true, outcome: ProviderInvocationStreamAbandoned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			inner := responsesFixtureModel(t, "custom", func(*http.Request) (*http.Response, error) {
				calls++
				response := sdkJSONResponse(tc.raw, 200)
				response.Header.Set("Content-Type", "text/event-stream")
				return response, nil
			})
			admission := &settlingAdmission{recordingAdmission: &recordingAdmission{}}
			model := NewAdmittedLanguageModel("remote/custom", inner, admission)
			stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("question")}})
			if err != nil {
				t.Fatal(err)
			}
			stream(func(fantasy.StreamPart) bool { return !tc.stop })
			results := admission.snapshotSettlements()
			if calls != 1 || len(results) != 1 || results[0].result.Outcome != tc.outcome || results[0].result.UsageReported != tc.known {
				t.Fatalf("calls=%d results=%+v", calls, results)
			}
		})
	}
}

type responsesCloseCounter struct {
	io.Reader
	closes int
}

func (r *responsesCloseCounter) Close() error { r.closes++; return nil }

func TestResponsesUsageObserverPreservesBytesAndBounds(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, padding    string
			duplicate, known bool
		}{
			{name: "valid", known: true},
			{name: "oversized", padding: strings.Repeat("x", maxObservedProviderBody)},
			{name: "duplicate terminal", duplicate: true},
		} {
			if !stream && tc.duplicate {
				continue
			}
			t.Run(fmt.Sprintf("stream_%t/%s", stream, tc.name), func(t *testing.T) {
				ctx := withProviderUsageObservation(t.Context())
				raw := responsesUsageFixture("completed", `{"input_tokens":1,"output_tokens":2,"total_tokens":3}`, stream)
				if tc.padding != "" {
					raw = strings.ReplaceAll(raw, `"status":"completed"`, `"padding":"`+tc.padding+`","status":"completed"`)
				}
				if tc.duplicate {
					raw += raw
				}
				source := &responsesCloseCounter{Reader: strings.NewReader(raw)}
				body := &responsesUsageBody{source: source, ctx: ctx, stream: stream}
				read, err := io.ReadAll(body)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err = body.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if string(read) != raw || source.closes != 1 {
					t.Fatalf("rewrote bytes or closed source %d times", source.closes)
				}
				result := observedProviderUsage(ctx, ProviderInvocationResult{ResponseObserved: true})
				if result.UsageReported != tc.known {
					t.Fatalf("result=%+v", result)
				}
				if len(body.buffer) != 0 {
					t.Fatal("closed accounting body retained content")
				}
			})
		}
	}
}

func TestResponsesFailureUsageDoesNotTriggerQuotaReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{200, 429} {
			t.Run(fmt.Sprintf("stream_%t/status_%d", stream, status), func(t *testing.T) {
				calls := 0
				raw := responsesUsageFixture("failed", `{"input_tokens":4,"output_tokens":2,"total_tokens":6}`, stream && status == 200)
				inner := responsesFixtureModel(t, "custom", func(*http.Request) (*http.Response, error) {
					calls++
					response := sdkJSONResponse(raw, status)
					if stream && status == 200 {
						response.Header.Set("Content-Type", "text/event-stream")
					}
					return response, nil
				})
				limiter := newCapacityLimiter(1)
				admission := &recoveryAdmission{settlingAdmission: &settlingAdmission{recordingAdmission: &recordingAdmission{limiter: limiter}}, limiter: limiter, t: t}
				model := NewAdmittedLanguageModel("remote/custom", inner, admission)
				entry := "generate"
				if stream {
					entry = "stream"
				}
				err := invokeResponsesFixture(t.Context(), model, entry)
				results := admission.snapshotSettlements()
				if err == nil || calls != 1 || admission.recoveries != 0 || len(results) != 1 {
					t.Fatalf("error=%v calls=%d recoveries=%d results=%v", err, calls, admission.recoveries, results)
				}
				result := results[0].result
				if result.Outcome != ProviderInvocationProviderError || !result.ResponseObserved || !result.UsageReported || result.Usage == nil || result.Usage.TotalTokens != 6 {
					t.Fatalf("result=%+v usage=%+v", result, result.Usage)
				}
				assertReleaseCount(t, limiter, 1)
			})
		}
	}
}

func TestResponsesIncompleteNeverExecutesTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, reason := range []string{"max_output_tokens", "content_filter", "max_messages", "steered", ""} {
			t.Run(fmt.Sprintf("stream_%t/reason_%s", stream, reason), func(t *testing.T) {
				calls, effects := 0, 0
				raw := strings.ReplaceAll(responsesUsageFixture("incomplete", `{"input_tokens":1,"output_tokens":1,"total_tokens":2}`, stream), "max_output_tokens", reason)
				model := responsesFixtureModel(t, "custom", func(*http.Request) (*http.Response, error) {
					calls++
					response := sdkJSONResponse(raw, 200)
					if stream {
						response.Header.Set("Content-Type", "text/event-stream")
					}
					return response, nil
				})
				tool := fantasy.NewAgentTool("answer", "fixture", func(context.Context, struct {
					Value string `json:"value"`
				}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					effects++
					return fantasy.NewTextResponse("done"), nil
				})
				worker := fantasy.NewAgent(model, fantasy.WithTools(tool), fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(2)))
				var err error
				if stream {
					_, err = worker.Stream(t.Context(), fantasy.AgentStreamCall{Prompt: "question"})
				} else {
					_, err = worker.Generate(t.Context(), fantasy.AgentCall{Prompt: "question"})
				}
				if err == nil || calls != 1 || effects != 0 {
					t.Fatalf("error=%v calls=%d tool effects=%d", err, calls, effects)
				}
			})
		}
	}
}

type responsesReadSignal struct {
	io.ReadCloser
	once    sync.Once
	started chan struct{}
}

func (r *responsesReadSignal) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(p)
}

func TestResponsesCancellationDrainsAccountingAndReleasesSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	var consumers sync.WaitGroup
	defer func() { cancel(); _ = reader.Close(); _ = writer.Close(); consumers.Wait() }()
	started := make(chan struct{})
	done := make(chan error, 1)
	inner := responsesFixtureModel(t, "custom", func(request *http.Request) (*http.Response, error) {
		context.AfterFunc(request.Context(), func() { _ = writer.CloseWithError(context.Canceled) })
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &responsesReadSignal{ReadCloser: reader, started: started}}, nil
	})
	limiter := newCapacityLimiter(1)
	admission := &settlingAdmission{recordingAdmission: &recordingAdmission{limiter: limiter}}
	model := NewAdmittedLanguageModel("remote/custom", inner, admission)
	stream, err := model.Stream(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("question")}})
	if err != nil {
		t.Fatal(err)
	}
	consumers.Go(func() {
		var failure error
		for part := range stream {
			if part.Error != nil {
				failure = part.Error
			}
		}
		done <- failure
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not start reading")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled accounting body did not drain")
	}
	results := admission.snapshotSettlements()
	if len(results) != 1 || results[0].result.Outcome != ProviderInvocationCancelled || results[0].result.UsageReported {
		t.Fatalf("results=%v", results)
	}
	assertReleaseCount(t, limiter, 1)
	assertNoDoubleRelease(t, limiter)
}

func TestResponsesRetriesOnlyUnobservedQuotaRejection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			calls := 0
			inner := responsesFixtureModel(t, "custom", func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return sdkJSONResponse(`{"error":{"message":"fixture quota","type":"rate_limit_exceeded"}}`, 429), nil
				}
				response := sdkJSONResponse(responsesUsageFixture("completed", `{"input_tokens":1,"output_tokens":2,"total_tokens":3}`, stream), 200)
				if stream {
					response.Header.Set("Content-Type", "text/event-stream")
				}
				return response, nil
			})
			limiter := newCapacityLimiter(1)
			admission := &recoveryAdmission{settlingAdmission: &settlingAdmission{recordingAdmission: &recordingAdmission{limiter: limiter}}, limiter: limiter, t: t}
			model := NewAdmittedLanguageModel("remote/custom", inner, admission)
			call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("question")}}
			if stream {
				parts, err := model.Stream(t.Context(), call)
				if err != nil {
					t.Fatal(err)
				}
				for part := range parts {
					if part.Error != nil {
						t.Fatal(part.Error)
					}
				}
			} else if _, err := model.Generate(t.Context(), call); err != nil {
				t.Fatal(err)
			}
			results := admission.snapshotSettlements()
			if calls != 2 || admission.recoveries != 1 || len(results) != 2 || results[0].result.UsageReported || !results[1].result.UsageReported || results[0].request.InvocationID == results[1].request.InvocationID {
				t.Fatalf("calls=%d recoveries=%d results=%v", calls, admission.recoveries, results)
			}
			assertReleaseCount(t, limiter, 2)
		})
	}
}

func TestResponsesMultilineSSEPreservesTerminalAndUsage(t *testing.T) {
	for _, entry := range []string{"stream", "stream_object"} {
		t.Run(entry, func(t *testing.T) {
			raw := responsesUsageFixture("completed", `{"input_tokens":1,"output_tokens":2,"total_tokens":3}`, true)
			raw = strings.ReplaceAll(raw, `,"response":`, ",\ndata: \"response\":")
			raw = strings.ReplaceAll(raw, "\n", "\r\n")
			inner := responsesFixtureModel(t, "custom", func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(raw)))}, nil
			})
			admission := &settlingAdmission{recordingAdmission: &recordingAdmission{}}
			model := NewAdmittedLanguageModel("remote/custom", inner, admission)
			if err := invokeResponsesFixture(t.Context(), model, entry); err != nil {
				t.Fatal(err)
			}
			results := admission.snapshotSettlements()
			if len(results) != 1 || !results[0].result.UsageReported || results[0].result.Usage.TotalTokens != 3 || results[0].result.Outcome != ProviderInvocationSuccess {
				t.Fatalf("results=%v", results)
			}
		})
	}
}
