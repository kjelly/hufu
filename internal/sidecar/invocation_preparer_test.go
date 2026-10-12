package sidecar

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

type invocationDiagnosticKey struct{}

type invocationContextAgent struct {
	usageAgent
	calls  int
	ctx    context.Context
	prompt string
}

func (a *invocationContextAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	a.calls++
	a.ctx, a.prompt = ctx, call.Prompt
	return a.usageAgent.Generate(ctx, call)
}

func TestInvocationPromptPreparerCarriesBoundContextAndHonorsProjection(t *testing.T) {
	a := &invocationContextAgent{}
	s := &Sidecar{agent: a, modelID: "test"}
	s.SetInvocationBinder(func(ctx context.Context, _ string) (context.Context, agent.ProviderAdmissionContext, error) {
		return context.WithValue(ctx, boundSnapshotKey{}, true), agent.ProviderAdmissionContext{Bound: true}, nil
	})
	preparer := func(label string) func(context.Context, string, string) (context.Context, string, error) {
		return func(ctx context.Context, _, prompt string) (context.Context, string, error) {
			if ctx.Value(boundSnapshotKey{}) != true {
				t.Fatal("invocation preparation preceded binding")
			}
			return context.WithValue(ctx, invocationDiagnosticKey{}, label), label + ":" + prompt, nil
		}
	}
	s.SetInvocationPromptPreparers(preparer("durable"), preparer("projection"))
	usage := 0
	s.SetUsageObserver(func(*fantasy.AgentResult) { usage++ })
	for _, mode := range []sidecarPreparationMode{sidecarDurablePreparation, sidecarProjectionPreparation} {
		if _, err := s.generateWithOptions(t.Context(), "input", JudgeProfile, sidecarInvocationOptions{preparationMode: mode}); err != nil {
			t.Fatal(err)
		}
		want := "durable"
		if mode == sidecarProjectionPreparation {
			want = "projection"
		}
		if a.ctx.Value(invocationDiagnosticKey{}) != want || a.prompt != want+":input" {
			t.Fatalf("model did not receive invocation-local preparation: %q", a.prompt)
		}
	}
	if usage != 1 {
		t.Fatalf("projection added durable usage: %d", usage)
	}
	s.SetPromptPreparer(func(_ context.Context, _, _ string) (string, error) {
		return "", errors.New("required compilation rejected")
	})
	if _, err := s.generate(t.Context(), "input", JudgeProfile); err == nil || a.calls != 2 {
		t.Fatal("legacy override or fail-before-model contract was lost")
	}
}
