package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"charm.land/fantasy"
)

func TestProviderErrorOutcomeSurvivesConsumerStop(t *testing.T) {
	for _, object := range []bool{false, true} {
		for _, stop := range []bool{false, true} {
			for _, cancelled := range []bool{false, true} {
				t.Run(fmt.Sprintf("object_%t/stop_%t/cancel_%t", object, stop, cancelled), func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					admission := &settlingAdmission{recordingAdmission: &recordingAdmission{}}
					failure := errors.New("provider fixture error")
					inner := &streamLanguageModel{
						streamFn: func(context.Context) fantasy.StreamResponse {
							return func(yield func(fantasy.StreamPart) bool) {
								yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: failure})
							}
						},
						objectStreamFn: func(context.Context) fantasy.ObjectStreamResponse {
							return func(yield func(fantasy.ObjectStreamPart) bool) {
								yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: failure})
							}
						},
					}
					model := NewAdmittedLanguageModel("fixture", inner, admission)
					consume := func() bool {
						if cancelled {
							cancel()
						}
						return !stop
					}
					if object {
						stream, err := model.StreamObject(ctx, fantasy.ObjectCall{})
						if err != nil {
							t.Fatal(err)
						}
						stream(func(fantasy.ObjectStreamPart) bool { return consume() })
					} else {
						stream, err := model.Stream(ctx, fantasy.Call{})
						if err != nil {
							t.Fatal(err)
						}
						stream(func(fantasy.StreamPart) bool { return consume() })
					}
					want := ProviderInvocationProviderError
					if cancelled {
						want = ProviderInvocationCancelled
					}
					results := admission.snapshotSettlements()
					if len(results) != 1 || results[0].result.Outcome != want {
						t.Fatalf("settlements=%v want outcome %s", results, want)
					}
				})
			}
		}
	}
}

type usageObservingModel struct {
	resultLanguageModel
	usage     *ProviderInvocationUsage
	duplicate bool
}

func (m *usageObservingModel) observe(ctx context.Context) {
	ObserveProviderResponseUsage(ctx, m.usage)
	if m.duplicate {
		ObserveProviderResponseUsage(ctx, m.usage)
	}
}

func (m *usageObservingModel) Generate(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	m.observe(ctx)
	return &fantasy.Response{}, nil // SDK value fields cannot represent absence.
}

func (m *usageObservingModel) GenerateObject(ctx context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.observe(ctx)
	return &fantasy.ObjectResponse{}, nil
}

func (m *usageObservingModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		m.observe(ctx)
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish})
	}, nil
}

func (m *usageObservingModel) StreamObject(ctx context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		m.observe(ctx)
		yield(fantasy.ObjectStreamPart{})
	}, nil
}

func TestProviderUsagePresenceSurvivesSDKValueFields(t *testing.T) {
	calls := map[string]func(context.Context, fantasy.LanguageModel) error{
		"generate": func(ctx context.Context, model fantasy.LanguageModel) error {
			_, err := model.Generate(ctx, fantasy.Call{})
			return err
		},
		"generate_object": func(ctx context.Context, model fantasy.LanguageModel) error {
			_, err := model.GenerateObject(ctx, fantasy.ObjectCall{})
			return err
		},
		"stream": func(ctx context.Context, model fantasy.LanguageModel) error {
			stream, err := model.Stream(ctx, fantasy.Call{})
			if err == nil {
				for range stream {
				}
			}
			return err
		},
		"stream_object": func(ctx context.Context, model fantasy.LanguageModel) error {
			stream, err := model.StreamObject(ctx, fantasy.ObjectCall{})
			if err == nil {
				for range stream {
				}
			}
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			for _, test := range []struct {
				name      string
				usage     *ProviderInvocationUsage
				duplicate bool
				known     bool
			}{
				{"absent", nil, false, false},
				{"explicit_zero", &ProviderInvocationUsage{}, false, true},
				{"explicit_positive", &ProviderInvocationUsage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}, false, true},
				{"conflicting_observations", &ProviderInvocationUsage{}, true, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					admission := &settlingAdmission{recordingAdmission: &recordingAdmission{}}
					inner := &usageObservingModel{usage: test.usage, duplicate: test.duplicate}
					model := NewAdmittedLanguageModel("test", inner, admission)
					// Repeated calls share a parent context but never the usage recorder.
					for range 2 {
						if err := call(t.Context(), model); err != nil {
							t.Fatal(err)
						}
					}
					settlements := admission.snapshotSettlements()
					if len(settlements) != 2 {
						t.Fatalf("got %d settlements", len(settlements))
					}
					for _, settlement := range settlements {
						if (settlement.result.Usage != nil) != test.known || settlement.result.UsageReported != test.known {
							t.Fatalf("usage presence lost: %+v", settlement.result)
						}
						if test.known && *settlement.result.Usage != *test.usage {
							t.Fatal("transport usage was replaced by SDK defaults")
						}
					}
				})
			}
		})
	}
}
