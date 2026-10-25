package sidecar

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
)

func TestSelectCandidatePreservesCompleteDomain(t *testing.T) {
	for _, count := range []int{0, 1, 20, 21, 100, 256} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			candidates := make([]TeamSummary, count)
			for i := range candidates {
				candidates[i] = TeamSummary{Name: fmt.Sprintf("worker-%d", i), Description: "complete evidence"}
			}
			calls := 0
			s := &Sidecar{}
			s.SetDecisionExecutor(func(_ context.Context, in DecisionInvocation) (decisionrt.Result, error) {
				calls++
				input, ok := in.Input.(struct {
					Prompt     string
					Candidates []TeamSummary
				})
				if !ok || input.Prompt != "task" || !reflect.DeepEqual(input.Candidates, candidates) {
					t.Fatal("decision input changed the candidate snapshot")
				}
				if len(in.Options) != count+1 || in.Options[0].ID != "none" {
					t.Fatal("decision domain omitted candidates or abstention")
				}
				for i := range candidates {
					if in.Options[i+1].ID != fmt.Sprintf("candidate_%d", i) {
						t.Fatal("decision domain changed candidate order")
					}
				}
				return decisionrt.Result{Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: in.Options[count].ID}}, nil
			})
			index, err := s.selectCandidate(t.Context(), "runtime_agent_match", "Choose a worker", "task", candidates)
			wantCalls := 1
			if count == 0 {
				wantCalls = 0
			}
			if err != nil || index != count-1 || calls != wantCalls {
				t.Fatalf("index=%d calls=%d error=%v", index, calls, err)
			}
		})
	}
}

func TestSelectCandidateRejectsUnsupportedDecision(t *testing.T) {
	failure := errors.New("inference failed")
	for _, test := range []struct {
		name   string
		result decisionrt.Result
		err    error
	}{
		{name: "none", result: decisionrt.Result{Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "none"}}},
		{name: "unknown option", result: decisionrt.Result{Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "candidate_21"}}},
		{name: "abstained", result: decisionrt.Result{Status: decisionrt.StatusAbstained}},
		{name: "invalid status", result: decisionrt.Result{Status: "unknown"}},
		{name: "executor failure", err: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Sidecar{}
			s.SetDecisionExecutor(func(context.Context, DecisionInvocation) (decisionrt.Result, error) {
				return test.result, test.err
			})
			index, err := s.selectCandidate(t.Context(), "runtime_agent_match", "Choose", "task", make([]TeamSummary, 21))
			if index != -1 || (err == nil) != (test.name == "none") {
				t.Fatalf("index=%d error=%v", index, err)
			}
			if test.err != nil && !errors.Is(err, failure) {
				t.Fatalf("executor error lost: %v", err)
			}
		})
	}
}
