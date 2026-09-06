package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

type recordingFinalizer struct {
	coordinatorCalls int
	judgeCalls       int
	coordinatorReq   CoordinatorFinalizationRequest
	judgeReq         JudgeFinalizationRequest
	coordinatorWire  FinalizationWireResult
	judgeWire        FinalizationWireResult
}

func (f *recordingFinalizer) RunCoordinatorFinalization(_ context.Context, req CoordinatorFinalizationRequest) (FinalizationWireResult, error) {
	f.coordinatorCalls++
	f.coordinatorReq = req
	return f.coordinatorWire, nil
}

func (f *recordingFinalizer) RunJudgeFinalization(_ context.Context, req JudgeFinalizationRequest) (FinalizationWireResult, error) {
	f.judgeCalls++
	f.judgeReq = req
	return f.judgeWire, nil
}

func TestDecisionFinalizationModesPersistRuntimeOwnedResult(t *testing.T) {
	for _, tt := range []struct {
		name          string
		mode          string
		judgeID       string
		wire          FinalizationWireResult
		wantOption    string
		wantIdentity  string
		wantOutcome   string
		wantCoordCall int
		wantJudgeCall int
	}{
		{"aggregate", agent.FinalizationAggregate, "", FinalizationWireResult{}, "migrate", FinalizationIdentityAggregate, FinalizationOutcomeSelected, 0, 0},
		{"coordinator", agent.FinalizationCoordinator, "", FinalizationWireResult{OptionID: "wait", Reason: "capacity is not ready"}, "wait", FinalizationIdentityCoordinator, FinalizationOutcomeOverride, 1, 0},
		{"named judge", agent.FinalizationJudge, "judge-2", FinalizationWireResult{OptionID: "migrate"}, "migrate", "judge-2", FinalizationOutcomeSelected, 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			journal := &memoryJournal{}
			finalizer := &recordingFinalizer{coordinatorWire: tt.wire, judgeWire: tt.wire}
			policy := enginePolicy(2)
			policy.Finalization = FinalizationPolicy{Mode: tt.mode, JudgeID: tt.judgeID}
			record, err := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{
				CoordinatorFinalizer: finalizer, JudgeFinalizer: finalizer,
			}).Run(context.Background(), engineRequest(policy))
			if err != nil {
				t.Fatalf("Run = %v", err)
			}
			if record.FinalOption != tt.wantOption || record.FinalizationMode != tt.mode || record.FinalizationIdentity != tt.wantIdentity || record.FinalizationOutcome != tt.wantOutcome {
				t.Fatalf("record finalization = %#v", record)
			}
			if finalizer.coordinatorCalls != tt.wantCoordCall || finalizer.judgeCalls != tt.wantJudgeCall {
				t.Fatalf("finalizer calls coordinator=%d judge=%d, want %d/%d", finalizer.coordinatorCalls, finalizer.judgeCalls, tt.wantCoordCall, tt.wantJudgeCall)
			}
			types := journal.typesOf()
			resultAt, finalAt := -1, -1
			for idx, eventType := range types {
				if eventType == agent.EventDecisionFinalizationResult {
					resultAt = idx
				}
				if eventType == agent.EventDecisionFinalized {
					finalAt = idx
				}
			}
			if resultAt < 0 || finalAt < 0 || resultAt >= finalAt {
				t.Fatalf("event order = %v, want durable result before decision_finalized", types)
			}
		})
	}
}

func TestDecisionFinalizationPolicyPreflightPrecedesModelAndReferenceStages(t *testing.T) {
	for _, tt := range []struct {
		name      string
		finalizer FinalizationPolicy
		want      string
	}{
		{
			name:      "coordinator finalizer is absent",
			finalizer: FinalizationPolicy{Mode: agent.FinalizationCoordinator},
			want:      "coordinator finalization requires a coordinator finalizer",
		},
		{
			name:      "named judge finalizer is absent",
			finalizer: FinalizationPolicy{Mode: agent.FinalizationJudge, JudgeID: "judge-1"},
			want:      "judge finalization requires a named judge finalizer",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewFileArtifactStore(workspace, workspace)
			if err != nil {
				t.Fatal(err)
			}
			journal := &memoryJournal{}
			proposer := &stubProposer{}
			reference := &referenceDraftRunner{draft: validReferenceDraft()}
			// This runner is the model/sidecar-capable judging seam. The
			// preflight must reject before it is dispatched.
			sidecar := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
				return scoredOpinion(8, 4, "migrate", .8), nil
			})
			policy := enginePolicy(1)
			policy.OptionProposal = OptionProposalPolicy{Enabled: true, MaxOptions: 2}
			policy.OutsideView.Required = true
			policy.OutsideView.ReferenceEvidence = true
			policy.Finalization = tt.finalizer
			req := engineRequest(policy)
			req.Options = nil // Require the proposer if preflight is misplaced.

			_, err = newTestEngineWithStages(journal, sidecar, nil, DecisionServices{
				Store:             store,
				Proposer:          proposer,
				ReferenceEvidence: reference,
			}).Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run error = %v, want %q", err, tt.want)
			}
			if proposer.calls != 0 || reference.calls != 0 || len(sidecar.dispatched()) != 0 {
				t.Fatalf("preflight dispatched proposer/reference/sidecar = %d/%d/%v", proposer.calls, reference.calls, sidecar.dispatched())
			}
			for _, eventType := range journal.typesOf() {
				switch eventType {
				case agent.EventDecisionReferenceStarted, agent.EventDecisionReferenceCompleted, agent.EventDecisionReferenceFailed:
					t.Fatalf("preflight published reference event %q", eventType)
				}
			}
			artifacts, err := WorkspaceArtifacts(workspace)
			if err != nil {
				t.Fatal(err)
			}
			for _, artifact := range artifacts {
				if artifact.Kind == "reference_evidence" || artifact.Kind == "reference_evidence_result" {
					t.Fatalf("preflight published reference artifact %#v", artifact)
				}
			}
		})
	}
}

func TestFinalizationRequestIsHistoryFreeAndRejectsForgedWireFields(t *testing.T) {
	journal := &memoryJournal{}
	finalizer := &recordingFinalizer{coordinatorWire: FinalizationWireResult{OptionID: "migrate"}}
	policy := enginePolicy(1)
	policy.Finalization = FinalizationPolicy{Mode: agent.FinalizationCoordinator}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		opinion := scoredOpinion(8, 4, "migrate", .8)
		opinion.KeyAssumptions = []string{"STAGE4-HISTORY-CANARY"}
		return opinion, nil
	})
	if _, err := newTestEngineWithStages(journal, runner, nil, DecisionServices{CoordinatorFinalizer: finalizer}).Run(context.Background(), engineRequest(policy)); err != nil {
		t.Fatalf("Run = %v", err)
	}
	encoded, err := cloneFinalizationRequest(finalizer.coordinatorReq)
	if err != nil {
		t.Fatal(err)
	}
	if !encoded.Packet.Sealed || len(encoded.Aggregates) == 0 {
		t.Fatalf("finalizer request missing sealed aggregate evidence: %#v", encoded)
	}
	if text := finalizationJSON(t, encoded); strings.Contains(text, "STAGE4-HISTORY-CANARY") || strings.Contains(text, "opinions") {
		t.Fatalf("finalizer request leaked opinion/history: %s", text)
	}
	if _, err := decodeFinalizationWireResult(`{"option_id":"migrate","reason":"","identity":"forged"}`); err == nil {
		t.Fatal("wire decoder accepted runtime-owned identity")
	}
}

func TestFinalizationRejectsForgedOptionReasonAndJudge(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy DecisionPolicy
		wire   FinalizationWireResult
		want   string
	}{
		{"unsealed option", func() DecisionPolicy {
			p := enginePolicy(1)
			p.Finalization = FinalizationPolicy{Mode: agent.FinalizationCoordinator}
			return p
		}(), FinalizationWireResult{OptionID: "forged", Reason: "no"}, "outside sealed options"},
		{"unneeded reason", func() DecisionPolicy {
			p := enginePolicy(1)
			p.Finalization = FinalizationPolicy{Mode: agent.FinalizationCoordinator}
			return p
		}(), FinalizationWireResult{OptionID: "migrate", Reason: "forged"}, "without diverging"},
		{"missing override reason", func() DecisionPolicy {
			p := enginePolicy(1)
			p.Finalization = FinalizationPolicy{Mode: agent.FinalizationCoordinator}
			return p
		}(), FinalizationWireResult{OptionID: "wait"}, "mandatory override reason"},
		{"judge not admitted", func() DecisionPolicy {
			p := enginePolicy(1)
			p.Finalization = FinalizationPolicy{Mode: agent.FinalizationJudge, JudgeID: "judge-2"}
			return p
		}(), FinalizationWireResult{OptionID: "migrate"}, "not one of the configured judges"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			finalizer := &recordingFinalizer{coordinatorWire: tt.wire, judgeWire: tt.wire}
			_, err := newTestEngineWithStages(&memoryJournal{}, spreadRunner(), nil, DecisionServices{CoordinatorFinalizer: finalizer, JudgeFinalizer: finalizer}).Run(context.Background(), engineRequest(tt.policy))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run error = %v, want %q", err, tt.want)
			}
		})
	}
}

type failDecisionRecordStore struct {
	ArtifactStore
	fail bool
}

func (s *failDecisionRecordStore) Put(ctx context.Context, req PutArtifactRequest) (ArtifactPutResult, error) {
	if req.Kind == "decision_record" && s.fail {
		s.fail = false
		return ArtifactPutResult{}, errors.New("injected decision record failure")
	}
	return s.ArtifactStore.Put(ctx, req)
}

func TestFinalizationResumeReusesDurableResultWithoutRecall(t *testing.T) {
	base, err := NewFileArtifactStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	journal := &memoryJournal{}
	finalizer := &recordingFinalizer{coordinatorWire: FinalizationWireResult{OptionID: "wait", Reason: "defer"}}
	policy := enginePolicy(1)
	policy.Finalization = FinalizationPolicy{Mode: agent.FinalizationCoordinator}
	req := engineRequest(policy)
	if _, err := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{Store: &failDecisionRecordStore{ArtifactStore: base, fail: true}, CoordinatorFinalizer: finalizer}).Run(context.Background(), req); err == nil || !strings.Contains(err.Error(), "injected decision record failure") {
		t.Fatalf("first Run error = %v, want injected persistence failure", err)
	}
	if finalizer.coordinatorCalls != 1 || journal.count(agent.EventDecisionFinalizationResult) != 1 {
		t.Fatalf("first run calls/events = %d/%d", finalizer.coordinatorCalls, journal.count(agent.EventDecisionFinalizationResult))
	}
	record, err := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{Store: base, CoordinatorFinalizer: finalizer}).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("resume Run = %v", err)
	}
	if record.FinalOption != "wait" || finalizer.coordinatorCalls != 1 || journal.count(agent.EventDecisionFinalizationResult) != 1 {
		t.Fatalf("resume recalled finalizer or changed result: record=%#v calls=%d events=%d", record, finalizer.coordinatorCalls, journal.count(agent.EventDecisionFinalizationResult))
	}
}

func TestFinalizationSchemaV1ReadAndProjectionCompatibility(t *testing.T) {
	legacy := DecisionRecord{SchemaVersion: 1, ID: "legacy-1", EvidenceHash: "evidence", FinalOption: "wait", FinalizationMode: agent.FinalizationAggregate}
	if err := legacy.ValidateSchemaVersion(); err != nil {
		t.Fatalf("legacy schema was rejected: %v", err)
	}
	result, ok := LegacyFinalizationResult(legacy)
	if !ok || result.Outcome != FinalizationOutcomeLegacy || result.Identity != FinalizationIdentityAggregate {
		t.Fatalf("legacy finalization projection = %#v, %t", result, ok)
	}
	entry := IndexEntryFor(legacy, "legacy question", false, ArtifactRef{})
	if entry.SchemaVersion != DecisionIndexSchemaVersion || entry.FinalOption != "wait" || entry.FinalizationMode != agent.FinalizationAggregate {
		t.Fatalf("legacy index projection = %#v", entry)
	}
}

func finalizationJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
