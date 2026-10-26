package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

type rejectedEvidenceJournal struct {
	EventJournal
	cause          error
	observations   int
	rejectNext     bool
	rejectAllAfter bool
}

func (j *rejectedEvidenceJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if event.Type == string(EventArtifactEvidenceObserved) {
		j.observations++
		j.rejectNext = j.rejectAllAfter
		return RunEvent{}, j.cause
	}
	if j.rejectNext {
		return RunEvent{}, j.cause
	}
	return j.EventJournal.Append(ctx, event)
}

func TestPolicyGateAccountsForEvidenceCaptureFailure(t *testing.T) {
	for _, tt := range []struct {
		name           string
		checkpointStop bool
		checkpointFail bool
	}{
		{name: "capture failure"},
		{name: "checkpoint stops dispatch", checkpointStop: true},
		{name: "checkpoint persistence failure", checkpointStop: true, checkpointFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newArtifactKeysFixture(t)
			stop := StopPolicy{}
			if tt.checkpointStop {
				stop = StopPolicy{CheckpointEvery: 1, KillCriteria: []KillCriterion{{ID: "calls", Kind: agent.KillKindToolCalls, Threshold: 1}}}
			}
			if err := f.c.armDiscipline(f.ctx, f.item.ID, TaskDef{Agent: "worker"}, disciplinePolicy(CommitGatePolicy{}, stop, ReplanPolicy{}), &DecisionRecord{ID: "capture-failure"}); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("injected evidence persistence failure")
			journal := &rejectedEvidenceJournal{EventJournal: f.c.EventJournal(), cause: cause, rejectAllAfter: tt.checkpointFail}
			f.c.SetEventJournal(journal)
			sequence := newTaskToolSequence([]string{"view", "view", submitResultToolName}, nil, "", nil)
			recorder := new(executedToolCallRecorder)
			ctx := withExecutedToolCallRecorder(context.WithValue(f.ctx, taskToolSequenceKey{}, sequence), recorder)
			call := fantasy.ToolCall{ID: "capture-read", Name: "view", Input: fmt.Sprintf(`{"artifact_ref":%q}`, f.ref.ID)}
			response, err := f.view.Run(ctx, call)
			if strings.Contains(response.Content, "bounded source evidence") || len(tools.ViewArtifactReads(response)) != 0 {
				t.Fatalf("uncommitted evidence escaped: response=%#v err=%v", response, err)
			}
			switch {
			case tt.checkpointFail:
				if _, ok := errors.AsType[checkpointPersistenceError](err); !ok {
					t.Fatalf("checkpoint error lost: %v", err)
				}
			case tt.checkpointStop:
				if outcome, ok := asCheckpointControlError(err); !ok || outcome.Action != CheckpointStop {
					t.Fatalf("checkpoint stop lost: %v", err)
				}
				if got := todoByID(t, f.c, f.item.ID).Status; got != TaskBlocked {
					t.Fatalf("checkpoint task status=%v", got)
				}
			default:
				if !errors.Is(err, cause) || response.Content != "" || response.IsError {
					t.Fatalf("capture failure lost or returned content: response=%#v err=%v", response, err)
				}
			}
			discipline := f.c.disciplineFor(f.item.ID)
			if discipline.toolCalls != 1 || discipline.failures != 1 || journal.observations != 1 || len(recorder.snapshot()) != 1 {
				t.Fatalf("read accounting: calls=%d failures=%d captures=%d executed=%d", discipline.toolCalls, discipline.failures, journal.observations, len(recorder.snapshot()))
			}
			if !sequence.failed || sequence.failure == nil || sequence.failure.Position != 1 || sequence.failure.Tool != "view" {
				t.Fatalf("closed sequence lost capture failure: %#v", sequence.failure)
			}
			// A second read must be denied before execution, even though the
			// closed sequence originally contained another view slot.
			response, err = f.view.Run(ctx, fantasy.ToolCall{ID: "retry-read", Name: "view", Input: call.Input})
			if err != nil || !response.IsError || len(recorder.snapshot()) != 1 || journal.observations != 1 {
				t.Fatalf("failed read was replayed: response=%#v err=%v", response, err)
			}
			events, err := f.c.eventStore.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == string(EventArtifactEvidenceObserved) || event.Type == string(EventToolObservationRecorded) {
					t.Fatalf("failed capture published usable evidence: %s", event.Type)
				}
			}
		})
	}
}
