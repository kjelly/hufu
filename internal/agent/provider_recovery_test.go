package agent

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"charm.land/fantasy"
)

type toolRecoveryModel struct {
	rejectedModel
	requests [][]byte
}

func (m *toolRecoveryModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	request, err := SerializeProviderRequest(ProviderRequest{ModelID: "test", Call: &call})
	if err != nil {
		return nil, err
	}
	m.requests = append(m.requests, request)
	switch m.calls {
	case 1:
		return &fantasy.Response{FinishReason: fantasy.FinishReasonToolCalls, Content: []fantasy.Content{fantasy.ToolCallContent{ToolCallID: "write-once", ToolName: "effect", Input: `{}`}}}, nil
	case 2:
		return nil, &fantasy.ProviderError{StatusCode: 429}
	default:
		return &fantasy.Response{FinishReason: fantasy.FinishReasonStop, Content: []fantasy.Content{fantasy.TextContent{Text: "done"}}}, nil
	}
}

func TestProviderRecoveryContinuesAgentWithoutReplayingTools(t *testing.T) {
	limiter := newCapacityLimiter(1)
	admission := &recoveryAdmission{settlingAdmission: &settlingAdmission{recordingAdmission: &recordingAdmission{limiter: limiter}}, limiter: limiter, t: t}
	inner := &toolRecoveryModel{}
	effects := 0
	tool := fantasy.NewAgentTool("effect", "Record one effect", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		effects++
		return fantasy.NewTextResponse("recorded"), nil
	})
	ag := fantasy.NewAgent(NewAdmittedLanguageModel("test", inner, admission), fantasy.WithTools(tool), fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(4)))
	if _, err := ag.Generate(t.Context(), fantasy.AgentCall{Prompt: "perform effect and report"}); err != nil {
		t.Fatal(err)
	}
	if effects != 1 || inner.calls != 3 || admission.recoveries != 1 {
		t.Fatalf("effects=%d calls=%d recoveries=%d", effects, inner.calls, admission.recoveries)
	}
	if !bytes.Equal(inner.requests[1], inner.requests[2]) {
		t.Fatal("quota resend changed the rejected request or lost tool history")
	}
}

type recoveryAdmission struct {
	*settlingAdmission
	recoveries int
	waits      int
	waitErr    error
	limiter    *capacityLimiter
	t          *testing.T
}

func (*recoveryAdmission) CanRecoverProviderRequest(ProviderRequest) bool { return true }

func (a *recoveryAdmission) WaitProviderRequest(context.Context, ProviderRequest) (func(bool) error, error) {
	a.waits++
	if a.limiter.active.Load() != 0 {
		a.t.Fatal("provider slot held during cooldown")
	}
	return nil, a.waitErr
}
func (a *recoveryAdmission) RecoverProviderRequest(_ context.Context, _ ProviderRequest, err error) (bool, error) {
	a.recoveries++
	if a.limiter.active.Load() != 0 {
		a.t.Fatal("provider slot held after rejection")
	}
	failure, ok := errors.AsType[*fantasy.ProviderError](err)
	return ok && failure.StatusCode == 429 && a.recoveries == 1, nil
}

type rejectedModel struct {
	calls       int
	failure     error
	partial     bool
	streamError bool
}

func (m *rejectedModel) failed() error {
	m.calls++
	if m.calls == 1 {
		return m.failure
	}
	return nil
}
func (m *rejectedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	err := m.failed()
	if err != nil && !m.partial {
		return nil, err
	}
	return &fantasy.Response{}, err
}
func (m *rejectedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	err := m.failed()
	if err != nil && !m.partial {
		return nil, err
	}
	return &fantasy.ObjectResponse{}, err
}
func (m *rejectedModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	err := m.failed()
	if err != nil && !m.streamError {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		}
	}, nil
}
func (m *rejectedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	err := m.failed()
	if err != nil && !m.streamError {
		return nil, err
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		if err != nil {
			yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: err})
		}
	}, nil
}
func (*rejectedModel) Provider() string { return "test" }
func (*rejectedModel) Model() string    { return "test" }

func recoveryMethods() map[string]func(context.Context, fantasy.LanguageModel) error {
	return map[string]func(context.Context, fantasy.LanguageModel) error{
		"generate": func(ctx context.Context, m fantasy.LanguageModel) error {
			_, err := m.Generate(ctx, fantasy.Call{})
			return err
		},
		"object": func(ctx context.Context, m fantasy.LanguageModel) error {
			_, err := m.GenerateObject(ctx, fantasy.ObjectCall{})
			return err
		},
		"stream": func(ctx context.Context, m fantasy.LanguageModel) error {
			s, err := m.Stream(ctx, fantasy.Call{})
			if err == nil && s != nil {
				for part := range s {
					if part.Error != nil {
						err = part.Error
					}
				}
			}
			return err
		},
		"object stream": func(ctx context.Context, m fantasy.LanguageModel) error {
			s, err := m.StreamObject(ctx, fantasy.ObjectCall{})
			if err == nil && s != nil {
				for part := range s {
					if part.Error != nil {
						err = part.Error
					}
				}
			}
			return err
		},
	}
}

func TestProviderRecoveryReAdmitsAndSettlesEveryRejectedCall(t *testing.T) {
	for name, invoke := range recoveryMethods() {
		t.Run(name, func(t *testing.T) {
			limiter := newCapacityLimiter(1)
			base := &recordingAdmission{limiter: limiter}
			admission := &recoveryAdmission{settlingAdmission: &settlingAdmission{recordingAdmission: base}, limiter: limiter, t: t}
			inner := &rejectedModel{failure: &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}}
			model := NewAdmittedLanguageModel("test", inner, admission)
			if err := invoke(t.Context(), model); err != nil {
				t.Fatal(err)
			}
			if inner.calls != 2 || admission.waits != 2 || admission.recoveries != 1 || len(base.requests) != 2 || len(base.commits) != 2 {
				t.Fatalf("calls=%d waits=%d recoveries=%d admissions=%d commits=%d", inner.calls, admission.waits, admission.recoveries, len(base.requests), len(base.commits))
			}
			settled := admission.snapshotSettlements()
			if len(settled) != 2 || settled[0].result.Outcome != ProviderInvocationProviderError || settled[1].result.Outcome != ProviderInvocationSuccess {
				t.Fatalf("settlements=%#v", settled)
			}
			if settled[0].request.InvocationID == settled[1].request.InvocationID {
				t.Fatal("resend reused invocation identity")
			}
			if limiter.active.Load() != 0 || limiter.releaseCount.Load() != 2 {
				t.Fatal("provider slots leaked")
			}
		})
	}
}

func TestProviderRecoveryNeverReplaysObservedOrLocallyRejectedCalls(t *testing.T) {
	for _, scenario := range []string{"partial response", "stream error", "admission", "commit", "other provider error", "cancelled wait"} {
		t.Run(scenario, func(t *testing.T) {
			limiter := newCapacityLimiter(1)
			failure := &fantasy.ProviderError{StatusCode: 429}
			base := &recordingAdmission{limiter: limiter}
			admission := &recoveryAdmission{settlingAdmission: &settlingAdmission{recordingAdmission: base}, limiter: limiter, t: t}
			inner := &rejectedModel{failure: failure}
			method := "generate"
			wantCalls := 1
			switch scenario {
			case "partial response":
				inner.partial = true
			case "stream error":
				method = "stream"
				inner.streamError = true
			case "admission":
				base.err = failure
				wantCalls = 0
			case "commit":
				base.commitErr = failure
				wantCalls = 0
			case "other provider error":
				inner.failure = errors.New("transport failed")
			case "cancelled wait":
				admission.waitErr = context.Canceled
				wantCalls = 0
			}
			err := recoveryMethods()[method](t.Context(), NewAdmittedLanguageModel("test", inner, admission))
			if err == nil || inner.calls != wantCalls || admission.recoveries != 0 {
				t.Fatalf("err=%v calls=%d recoveries=%d", err, inner.calls, admission.recoveries)
			}
			if limiter.active.Load() != 0 {
				t.Fatal("provider slot leaked")
			}
			if scenario == "admission" && admission.waits != 0 {
				t.Fatal("invalid request entered quota cooldown")
			}

		})
	}
}
