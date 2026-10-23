package team

import (
	"fmt"
	"strings"
	"testing"
)

func TestPrimaryPrimitiveReplayDoesNotRegenerate(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) {
			h := newJudgeRoutingHarness(t)
			c := h.coordinator
			if interrupted {
				c.eventJournal = primitiveFailingJournal{EventJournal: c.EventJournal(), kind: EventDecisionPrimitiveSettled}
			}
			runners := newDecisionRunners(c, "task-1")
			request := judgeRequestFor("judge-1", nil)
			first, err := runners.RunJudge(t.Context(), request)
			if (err != nil) != interrupted {
				t.Fatalf("first judgment: %v", err)
			}
			second, err := runners.RunJudge(t.Context(), request)
			if interrupted {
				if err == nil || !strings.Contains(err.Error(), "decision_interrupted") {
					t.Fatalf("unsettled primitive replay: %v", err)
				}
			} else if err != nil || second.PreferredOption != first.PreferredOption {
				t.Fatalf("settled replay: %#v %v", second, err)
			}
			if h.legacyJudge.count(stageJudge) != 1 || h.legacyJudge.count(stagePrimitive) != 1 {
				t.Fatalf("generation=%d primitive=%d; replay must invoke neither", h.legacyJudge.count(stageJudge), h.legacyJudge.count(stagePrimitive))
			}
		})
	}
}

func TestPrimaryPrimitiveFailureDoesNotEnterLegacyRepair(t *testing.T) {
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return DecisionOpinion{}, &runtimeDecisionFailure{fmt.Errorf("decision_timeout")}
	})
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngineWithStages(&memoryJournal{}, runner, nil, DecisionServices{Store: store})
	if _, err := engine.Run(t.Context(), engineRequest(enginePolicy(1))); err == nil {
		t.Fatal("primitive failure accepted")
	}
	if got := len(runner.dispatched()); got != 1 {
		t.Fatalf("primitive failure dispatched %d times, want one", got)
	}
}
