package team

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestDecisionEngineRunsBuiltInAndResumesWithoutTeamConfig(t *testing.T) {
	policy, metadata, err := agent.BuiltInDecisionProfileCatalog().Resolve(agent.DecisionProfileRef{Name: agent.DecisionProfileBuiltinStandardV1})
	if err != nil {
		t.Fatal(err)
	}
	policy, err = agent.NormalizeDecisionPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := agent.DecisionPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}

	store, req := genericStandardDecisionFixture(t, policy, metadata, digest, "decision-generic-resume")
	journal := &memoryJournal{store: store}
	reference := &referenceDraftRunner{draft: genericReferenceDraft()}
	interrupted := newRecordingRunner(func(judgeID string, _ int) (DecisionOpinion, error) {
		if judgeID == "judge-3" {
			return DecisionOpinion{}, fmt.Errorf("simulated interruption after envelope anchoring")
		}
		return genericStandardOpinion(), nil
	})
	first := NewDecisionEngine(genericStandardServices(journal, store, interrupted, reference))
	if _, err := first.Run(t.Context(), req); err == nil {
		t.Fatal("first engine unexpectedly finalized")
	}
	state, err := projectDecision(t.Context(), journal, req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.EnvelopeRef.ID == "" || len(state.OpinionsForRound(1)) != 2 {
		t.Fatalf("interrupted state = envelope %#v, opinions %d", state.EnvelopeRef, len(state.OpinionsForRound(1)))
	}

	recoveredRunner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return genericStandardOpinion(), nil
	})
	second := NewDecisionEngine(genericStandardServices(journal, store, recoveredRunner, &referenceDraftRunner{draft: genericReferenceDraft()}))
	recovered, err := second.Resume(t.Context(), req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveredRunner.dispatched(); !reflect.DeepEqual(got, []string{"judge-3"}) {
		t.Fatalf("resume judge dispatches = %v, want only unfinished judge-3", got)
	}
	if reference.calls != 1 {
		t.Fatalf("reference stage calls before interruption = %d, want 1", reference.calls)
	}

	uninterruptedStore, uninterruptedReq := genericStandardDecisionFixture(t, policy, metadata, digest, "decision-generic-control")
	uninterruptedJournal := &memoryJournal{store: uninterruptedStore}
	uninterruptedRunner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return genericStandardOpinion(), nil
	})
	control, err := NewDecisionEngine(genericStandardServices(
		uninterruptedJournal, uninterruptedStore, uninterruptedRunner,
		&referenceDraftRunner{draft: genericReferenceDraft()},
	)).Run(t.Context(), uninterruptedReq)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.FinalOption != control.FinalOption || recovered.Probability != control.Probability || len(recovered.Aggregates) != len(control.Aggregates) {
		t.Fatalf("recovered result differs from control: recovered=%#v control=%#v", recovered, control)
	}
}

func TestDecisionEngineMetadataFreeDirectInlineCompatibility(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	req := engineRequest(enginePolicy(1))
	req.DecisionID = "decision-inline-compatibility"
	if _, err := newTestEngine(journal, runner, nil).Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	state, err := projectDecision(t.Context(), journal, req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := loadDecisionRunEnvelope(t.Context(), journal.store, state.EnvelopeRef)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.ProfileOrigin != agent.DecisionProfileOriginRequestInline || envelope.PolicyDigest == "" {
		t.Fatalf("metadata-free direct call materialized as %#v", envelope.profileIdentity())
	}
}

func genericStandardDecisionFixture(
	t *testing.T,
	policy DecisionPolicy,
	metadata agent.DecisionProfileMetadata,
	digest string,
	decisionID string,
) (*FileArtifactStore, DecisionRequest) {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	contractEnvelope, contractBytes, err := BuildRequestContract("choose rollout", "Which rollout?", agent.RequestContractConfig{
		Enabled: true, Objective: "choose a safe rollout",
		SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "the rollout is reversible and verified"}},
		Constraints:     []agent.RequestConstraint{{ID: "C1", Statement: "avoid downtime"}},
	}, 1, time.Unix(1_700_000_000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	contractRef, err := PersistRequestContract(t.Context(), store, contractEnvelope, contractBytes)
	if err != nil {
		t.Fatal(err)
	}
	contract := contractEnvelope.RequestContract()
	req := DecisionRequest{
		DecisionID: decisionID, RunID: "run-" + decisionID, TaskID: "task-" + decisionID, Attempt: 1,
		Profile: agent.DecisionProfileBuiltinStandardV1, Policy: policy,
		ProfileOrigin: metadata.Origin, ProfileVersion: metadata.Version, ProfileRef: metadata.Ref, PolicyDigest: digest,
		Question: "Which rollout should we choose?",
		Options: []DecisionOption{
			{ID: "execute", Kind: OptionExecute, Title: "Roll out now"},
			{ID: "reduce", Kind: OptionReduceScope, Title: "Canary first"},
			{ID: "defer", Kind: OptionDefer, Title: "Do nothing now"},
			{ID: "gather", Kind: OptionRequestInfo, Title: "Gather more evidence"},
		},
		Facts: map[string]any{"service": "payments"},
		Role:  "You are an independent systems reviewer.", Contract: &contract, RequireRequestContract: true,
		RequestContractRef: contractRef.ID, RequestContractRevision: contractEnvelope.Revision, RequestContractArtifact: contractRef,
	}
	return store, req
}

func genericStandardServices(journal *memoryJournal, store ArtifactStore, judges JudgeRunner, reference ReferenceEvidenceRunner) DecisionServices {
	return DecisionServices{
		Journal: journal, Store: store, Judges: judges, ReferenceEvidence: reference,
		Premortems: &stubPremortem{},
		Challengers: &stubChallenger{respond: func(string) (DecisionChallenge, error) {
			return DecisionChallenge{TargetOption: "execute", StrongestCountercase: "rollback may fail", Severity: 0.4}, nil
		}},
		Revisions: &stubReviser{},
		Budget:    budgetWith(100*defaultJudgeTokenEstimate, 0),
	}
}

func genericReferenceDraft() ReferenceEvidenceDraft {
	first := validReferenceDraft().Entries[0]
	second := first
	second.ReferenceClass = "canary rollouts"
	second.Source = ReferenceSourceDeclaration{Name: "independent canary report", Locator: "/reports/canary"}
	return ReferenceEvidenceDraft{SchemaVersion: ReferenceEvidenceSchemaVersion, Entries: []ReferenceBaseRateDraft{first, second}}
}

func genericStandardOpinion() DecisionOpinion {
	return DecisionOpinion{
		OptionScores: []OptionScore{
			{OptionID: "execute", Overall: 9},
			{OptionID: "reduce", Overall: 7},
			{OptionID: "defer", Overall: 4},
			{OptionID: "gather", Overall: 5},
		},
		PreferredOption: "execute", SuccessProbability: 0.8,
	}
}
