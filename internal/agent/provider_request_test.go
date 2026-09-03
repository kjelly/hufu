package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

type rejectingAdmission struct {
	calls int
	err   error
}

func (a *rejectingAdmission) AdmitProviderRequest(context.Context, ProviderRequest) error {
	a.calls++
	return a.err
}

type countingLanguageModel struct {
	calls int
}

func (m *countingLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	return &fantasy.Response{}, nil
}

func (m *countingLanguageModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls++
	return nil, nil
}

func (m *countingLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.calls++
	return &fantasy.ObjectResponse{}, nil
}

func (m *countingLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	m.calls++
	return nil, nil
}

func (*countingLanguageModel) Provider() string { return "test" }

func (*countingLanguageModel) Model() string { return "test-model" }

func TestAdmissionRejectsOversizedWorkerBeforeProvider(t *testing.T) {
	assertAdmissionRejectsBeforeProvider(t)
}

func TestAdmissionRejectsOversizedDirectAgentBeforeProvider(t *testing.T) {
	assertAdmissionRejectsBeforeProvider(t)
}

func TestAdmissionRejectsOversizedRepairBeforeProvider(t *testing.T) {
	assertAdmissionRejectsBeforeProvider(t)
}

func TestAdmissionRejectsOversizedSidecarBeforeProvider(t *testing.T) {
	assertAdmissionRejectsBeforeProvider(t)
}

func assertAdmissionRejectsBeforeProvider(t *testing.T) {
	t.Helper()
	providerErr := errors.New("context request cannot fit")
	admission := &rejectingAdmission{err: providerErr}
	model := &countingLanguageModel{}
	wrapped := NewAdmittedLanguageModel("test-model", model, admission)
	_, err := wrapped.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("large request")}})
	if !errors.Is(err, providerErr) {
		t.Fatalf("Generate error = %v, want admission error", err)
	}
	if admission.calls != 1 || model.calls != 0 {
		t.Fatalf("admission calls=%d provider calls=%d, want 1 and 0", admission.calls, model.calls)
	}
}

func TestAdmissionCountsSerializedToolsAndMultimodalParts(t *testing.T) {
	request := ProviderRequest{
		ModelID: "qwen3:8b",
		Call: &fantasy.Call{
			Prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: "inspect this image"},
				fantasy.FilePart{Filename: "image.png", MediaType: "image/png", Data: []byte("binary-image-data")},
			}}},
			Tools: []fantasy.Tool{fantasy.FunctionTool{
				Name:        "large_tool",
				Description: "a tool with a schema",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}},
			}},
		},
	}
	serialized, err := SerializeProviderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(serialized)
	for _, expected := range []string{"large_tool", "properties", "image_url", "YmluYXJ5LWltYWdlLWRhdGE="} {
		if !strings.Contains(wire, expected) {
			t.Fatalf("serialized request lacks %q: %s", expected, wire)
		}
	}
	textOnly := request
	textOnly.Call = &fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("inspect this image")}}
	textTokens, err := CountProviderRequestTokens(textOnly)
	if err != nil {
		t.Fatal(err)
	}
	fullTokens, err := CountProviderRequestTokens(request)
	if err != nil {
		t.Fatal(err)
	}
	if fullTokens <= textTokens {
		t.Fatalf("full serialized request tokens=%d, text-only=%d; tool/media were not counted", fullTokens, textTokens)
	}
}

type recordingAdmission struct {
	mu       sync.Mutex
	requests []ProviderRequest
	limiter  InvocationLimiter
	err      error
}

func (a *recordingAdmission) AdmitProviderRequest(_ context.Context, request ProviderRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, request)
	return a.err
}

func (a *recordingAdmission) AcquireProviderInvocation(ctx context.Context, modelID string) (func(), error) {
	if a.limiter == nil {
		return nil, nil
	}
	return a.limiter.AcquireProviderInvocation(ctx, modelID)
}

type capacityLimiter struct {
	slots          chan struct{}
	active         atomic.Int32
	doubleReleases atomic.Int32
	releaseCount   atomic.Int32
}

func newCapacityLimiter(capacity int) *capacityLimiter {
	return &capacityLimiter{slots: make(chan struct{}, capacity)}
}

func (l *capacityLimiter) AcquireProviderInvocation(ctx context.Context, _ string) (func(), error) {
	select {
	case l.slots <- struct{}{}:
		l.active.Add(1)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var released atomic.Bool
	return func() {
		if !released.CompareAndSwap(false, true) {
			l.doubleReleases.Add(1)
			return
		}
		<-l.slots
		l.active.Add(-1)
		l.releaseCount.Add(1)
	}, nil
}

type streamLanguageModel struct {
	streamFn        func(context.Context) fantasy.StreamResponse
	streamErr       error
	objectStreamFn  func(context.Context) fantasy.ObjectStreamResponse
	objectStreamErr error
}

func (*streamLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{}, nil
}

func (m *streamLanguageModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	if m.streamFn == nil {
		return nil, m.streamErr
	}
	return m.streamFn(ctx), m.streamErr
}

func (*streamLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return &fantasy.ObjectResponse{}, nil
}

func (m *streamLanguageModel) StreamObject(ctx context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	if m.objectStreamFn == nil {
		return nil, m.objectStreamErr
	}
	return m.objectStreamFn(ctx), m.objectStreamErr
}

func (*streamLanguageModel) Provider() string { return "test" }

func (*streamLanguageModel) Model() string { return "test-model" }

func assertNoDoubleRelease(t *testing.T, limiter *capacityLimiter) {
	t.Helper()
	if got := limiter.doubleReleases.Load(); got != 0 {
		t.Fatalf("release called %d times after the first release", got)
	}
}

func assertReleaseCount(t *testing.T, limiter *capacityLimiter, want int32) {
	t.Helper()
	if got := limiter.releaseCount.Load(); got != want {
		t.Fatalf("release count = %d, want %d", got, want)
	}
}

func TestBoundAdmissionContextCoversAllLanguageModelMethods(t *testing.T) {
	admission := &recordingAdmission{}
	model := &countingLanguageModel{}
	bound := ProviderAdmissionContext{
		ModelID:       "local/model",
		ContextWindow: 32_768, MaxOutputTokens: 1_024,
		SafetyMarginTokens: 128, ProviderIdentity: "local",
		ProviderBaseURL: "http://localhost:11434/v1", ContextWindowSource: "provider_runtime",
	}
	wrapped := NewAdmittedLanguageModelWithContext("local/model", model, admission, bound)
	if _, err := wrapped.Generate(t.Context(), fantasy.Call{}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Stream(t.Context(), fantasy.Call{}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.GenerateObject(t.Context(), fantasy.ObjectCall{}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.StreamObject(t.Context(), fantasy.ObjectCall{}); err != nil {
		t.Fatal(err)
	}
	if len(admission.requests) != 4 || model.calls != 4 {
		t.Fatalf("admission calls=%d provider calls=%d, want four each", len(admission.requests), model.calls)
	}
	for index, request := range admission.requests {
		if request.ModelID != bound.ModelID || request.AdmissionContext != bound {
			t.Errorf("method %d provider request = %#v, want model/context %q/%#v", index, request, bound.ModelID, bound)
		}
	}
}

func TestCreateAgentUsesInvocationModelForProviderAdmission(t *testing.T) {
	const selectedModel = "openai/gpt-4o"
	var wireModel string
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable in this environment: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("provider path = %q, want /v1/chat/completions", r.URL.Path)
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode provider request: %v", err)
		}
		wireModel = request.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"selected","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	providerBaseURL := server.URL + "/v1"
	provider, err := NewOpenAICompatibleProvider(providerBaseURL, "", "openai")
	if err != nil {
		t.Fatal(err)
	}
	admission := &recordingAdmission{}
	bound := ProviderAdmissionContext{
		ModelID: selectedModel, ProviderIdentity: "openai", ProviderBaseURL: providerBaseURL,
		Bound: true, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	}
	created, err := CreateAgent(t.Context(), provider, AgentConfig{
		Def:               &AgentDef{Name: "worker", System: "system", Generation: GenerationParams{Model: "local/qwen3"}},
		TeamConfig:        &TeamConfig{},
		Admission:         admission,
		AdmissionContext:  bound,
		InvocationModelID: selectedModel,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunAgent(t.Context(), created, "task"); err != nil {
		t.Fatalf("run selected invocation: %v", err)
	}
	if wireModel != "gpt-4o" {
		t.Fatalf("wire model = %q, want selected model basename", wireModel)
	}
	if len(admission.requests) != 1 || admission.requests[0].ModelID != selectedModel {
		t.Fatalf("admission requests = %#v, want selected model %q", admission.requests, selectedModel)
	}
	if admission.requests[0].AdmissionContext != bound {
		t.Fatalf("admission context = %#v, want selected provider/profile %#v", admission.requests[0].AdmissionContext, bound)
	}
}

func TestAdmittedStreamHoldsProviderSlotUntilExhaustion(t *testing.T) {
	limiter := newCapacityLimiter(1)
	admission := &recordingAdmission{limiter: limiter}
	mayFinish := make(chan struct{})
	firstPart := make(chan struct{})
	model := &streamLanguageModel{streamFn: func(context.Context) fantasy.StreamResponse {
		return func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "chunk"}) {
				return
			}
			<-mayFinish
		}
	}}
	wrapped := NewAdmittedLanguageModel("local/model", model, admission)

	first, err := wrapped.Stream(t.Context(), fantasy.Call{})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan struct{})
	go func() {
		for range first {
			close(firstPart)
		}
		close(firstDone)
	}()
	select {
	case <-firstPart:
	case <-time.After(time.Second):
		t.Fatal("first stream did not begin iterating")
	}

	secondResult := make(chan struct {
		stream fantasy.StreamResponse
		err    error
	})
	secondCtx, cancelSecond := context.WithTimeout(t.Context(), time.Second)
	defer cancelSecond()
	go func() {
		stream, streamErr := wrapped.Stream(secondCtx, fantasy.Call{})
		secondResult <- struct {
			stream fantasy.StreamResponse
			err    error
		}{stream, streamErr}
	}()
	select {
	case <-secondResult:
		t.Fatal("second same-provider stream bypassed the capacity-one slot")
	case <-time.After(50 * time.Millisecond):
	}

	close(mayFinish)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first stream did not release after exhaustion")
	}
	var second fantasy.StreamResponse
	select {
	case result := <-secondResult:
		if result.err != nil {
			t.Fatalf("second stream: %v", result.err)
		}
		second = result.stream
	case <-time.After(time.Second):
		t.Fatal("second stream did not acquire after first exhaustion")
	}
	for range second {
	}
	assertReleaseCount(t, limiter, 2)
	assertNoDoubleRelease(t, limiter)
}

func TestAdmittedStreamObjectHoldsProviderSlotUntilExhaustion(t *testing.T) {
	limiter := newCapacityLimiter(1)
	admission := &recordingAdmission{limiter: limiter}
	mayFinish := make(chan struct{})
	firstPart := make(chan struct{})
	model := &streamLanguageModel{objectStreamFn: func(context.Context) fantasy.ObjectStreamResponse {
		return func(yield func(fantasy.ObjectStreamPart) bool) {
			if !yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeObject, Object: map[string]any{"ok": true}}) {
				return
			}
			<-mayFinish
		}
	}}
	wrapped := NewAdmittedLanguageModel("local/model", model, admission)

	first, err := wrapped.StreamObject(t.Context(), fantasy.ObjectCall{})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan struct{})
	go func() {
		for range first {
			close(firstPart)
		}
		close(firstDone)
	}()
	select {
	case <-firstPart:
	case <-time.After(time.Second):
		t.Fatal("first object stream did not begin iterating")
	}

	secondResult := make(chan struct {
		stream fantasy.ObjectStreamResponse
		err    error
	})
	secondCtx, cancelSecond := context.WithTimeout(t.Context(), time.Second)
	defer cancelSecond()
	go func() {
		stream, streamErr := wrapped.StreamObject(secondCtx, fantasy.ObjectCall{})
		secondResult <- struct {
			stream fantasy.ObjectStreamResponse
			err    error
		}{stream, streamErr}
	}()
	select {
	case <-secondResult:
		t.Fatal("second same-provider object stream bypassed the capacity-one slot")
	case <-time.After(50 * time.Millisecond):
	}

	close(mayFinish)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first object stream did not release after exhaustion")
	}
	var second fantasy.ObjectStreamResponse
	select {
	case result := <-secondResult:
		if result.err != nil {
			t.Fatalf("second object stream: %v", result.err)
		}
		second = result.stream
	case <-time.After(time.Second):
		t.Fatal("second object stream did not acquire after first exhaustion")
	}
	for range second {
	}
	assertReleaseCount(t, limiter, 2)
	assertNoDoubleRelease(t, limiter)
}

func TestAdmittedStreamCancellationReleasesUniteratedSlot(t *testing.T) {
	limiter := newCapacityLimiter(1)
	admission := &recordingAdmission{limiter: limiter}
	innerContexts := make(chan context.Context, 2)
	model := &streamLanguageModel{streamFn: func(ctx context.Context) fantasy.StreamResponse {
		innerContexts <- ctx
		return func(func(fantasy.StreamPart) bool) {}
	}}
	wrapped := NewAdmittedLanguageModel("local/model", model, admission)

	firstContext, cancelFirst := context.WithCancel(t.Context())
	if _, err := wrapped.Stream(firstContext, fantasy.Call{}); err != nil {
		t.Fatal(err)
	}
	firstInnerContext := <-innerContexts
	secondResult := make(chan error)
	secondCtx, cancelSecond := context.WithTimeout(t.Context(), time.Second)
	defer cancelSecond()
	go func() {
		_, err := wrapped.Stream(secondCtx, fantasy.Call{})
		secondResult <- err
	}()
	select {
	case err := <-secondResult:
		t.Fatalf("second stream acquired before cancellation, err=%v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancelFirst()
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("second stream after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second stream did not acquire after caller cancellation")
	}
	select {
	case <-firstInnerContext.Done():
	case <-time.After(time.Second):
		t.Fatal("inner stream context was not canceled")
	}
	assertReleaseCount(t, limiter, 1)
	assertNoDoubleRelease(t, limiter)
}

func TestAdmittedStreamEarlyBreakCancelsBeforeRelease(t *testing.T) {
	limiter := newCapacityLimiter(1)
	admission := &recordingAdmission{limiter: limiter}
	canceledBeforeReturn := make(chan struct{})
	model := &streamLanguageModel{streamFn: func(ctx context.Context) fantasy.StreamResponse {
		return func(yield func(fantasy.StreamPart) bool) {
			if yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "chunk"}) {
				return
			}
			select {
			case <-ctx.Done():
				close(canceledBeforeReturn)
			default:
			}
		}
	}}
	wrapped := NewAdmittedLanguageModel("local/model", model, admission)
	stream, err := wrapped.Stream(t.Context(), fantasy.Call{})
	if err != nil {
		t.Fatal(err)
	}
	stream(func(fantasy.StreamPart) bool { return false })
	select {
	case <-canceledBeforeReturn:
	case <-time.After(time.Second):
		t.Fatal("early break did not cancel the inner stream before it returned")
	}
	if got := limiter.active.Load(); got != 0 {
		t.Fatalf("active slots after early break = %d, want zero", got)
	}
	assertReleaseCount(t, limiter, 1)
	assertNoDoubleRelease(t, limiter)
}

func TestAdmittedStreamReleasesOnInnerStreamCreationError(t *testing.T) {
	limiter := newCapacityLimiter(1)
	admission := &recordingAdmission{limiter: limiter}
	wantErr := errors.New("stream setup failed")
	model := &streamLanguageModel{streamErr: wantErr}
	wrapped := NewAdmittedLanguageModel("local/model", model, admission)
	if _, err := wrapped.Stream(t.Context(), fantasy.Call{}); !errors.Is(err, wantErr) {
		t.Fatalf("stream error = %v, want %v", err, wantErr)
	}
	if got := limiter.active.Load(); got != 0 {
		t.Fatalf("active slots after stream creation error = %d, want zero", got)
	}
	assertReleaseCount(t, limiter, 1)
	assertNoDoubleRelease(t, limiter)
}
