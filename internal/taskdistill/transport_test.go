package taskdistill

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
)

func TestTransportRecoveryPreservesPromptBudgetAndCheckpoints(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		failures, budget, wantCalls int
		partial                     bool
		want                        error
	}{
		{"recovers", 1, 3, 2, false, nil},
		{"bounded", 3, 8, 2, false, ErrTransport},
		{"budget exhausted", 1, 1, 1, false, ErrCallBudget},
		{"partial response", 1, 3, 1, true, ErrTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			first := ""
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
			g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
				calls++
				if first == "" {
					first = prompt
				} else if first != prompt {
					t.Fatal("transport resend changed or split the frozen input")
				}
				if calls <= tc.failures {
					partial := ""
					if tc.partial {
						partial = "{"
					}
					return partial, NewTransportError(&url.Error{Op: "Post", URL: "http://fixture/.hufu-provider/private-capability", Err: io.ErrUnexpectedEOF})
				}
				return discardedReply(t, requestedSources(t, prompt)), nil
			}
			draft, err := GenerateBatched(t.Context(), g, []Source{{ID: "source", Content: strings.Repeat("e", 4096)}}, "", tc.budget)
			if calls != tc.wantCalls || tc.want != nil && !errors.Is(err, tc.want) || tc.want == nil && err != nil {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if err != nil {
				if len(g.checkpoints) != 0 || len(draft.Decisions) != 0 || strings.Contains(err.Error(), "private-capability") {
					t.Fatal("interrupted output cached, split, or exposed relay capability")
				}
			} else {
				if len(g.checkpoints) != 1 {
					t.Fatal("complete result not checkpointed")
				}
				if _, err := GenerateBatched(t.Context(), g, []Source{{ID: "source", Content: strings.Repeat("e", 4096)}}, "", 1); err != nil || calls != tc.wantCalls {
					t.Fatal("completed checkpoint resent")
				}
			}
		})
	}
}

func TestTransportRecoveryDoesNotReplayCancellationOrUnmarkedFailures(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, errors.New("admission rejected")} {
		calls := 0
		generator := generatorFunc(func(context.Context, string) (string, error) { calls++; return "", cause })
		if _, err := GenerateBatched(t.Context(), generator, []Source{{ID: "source", Content: strings.Repeat("e", 4096)}}, "", 4); !errors.Is(err, cause) || calls != 1 {
			t.Fatalf("non-transport failure replayed: calls=%d err=%v", calls, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	generator := generatorFunc(func(context.Context, string) (string, error) { calls++; cancel(); return "", NewTransportError(io.EOF) })
	if _, err := GenerateBatched(ctx, generator, []Source{{ID: "source"}}, "", 4); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled resend: calls=%d err=%v", calls, err)
	}
}
