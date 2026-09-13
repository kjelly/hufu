package improve

import (
	"bytes"
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestHandoffSchemaRejectsUntypedOrMissingRefs(t *testing.T) {
	scope := HandoffScope{ProjectID: "project", TeamID: "team"}
	if _, err := NewImprovementHandoff(HandoffSkill, scope, ArtifactRef{ID: "proposal", Revision: "revision"}, nil); err == nil {
		t.Fatal("untyped proposal ref was accepted")
	}
	handoff := newContractHandoff(t)
	handoff.Status = HandoffCandidateReady
	if err := handoff.Validate(); err == nil {
		t.Fatal("candidate_ready handoff without candidate ref was accepted")
	}
	handoff = newContractHandoff(t)
	handoff.StatusReason = "source text or sk-secret must not be persisted"
	if err := handoff.Validate(); err == nil {
		t.Fatal("free-form status reason was accepted")
	}
	handoff = newContractHandoff(t)
	handoff.ID = "handoff-arbitrary"
	if err := handoff.Validate(); err == nil {
		t.Fatal("handoff id unrelated to immutable identity was accepted")
	}
	handoff = newContractHandoff(t)
	handoff.StatusReason = HandoffReasonReviewApproved
	if err := handoff.Validate(); err == nil {
		t.Fatal("status reason for a different lifecycle state was accepted")
	}
	handoff = newContractHandoff(t)
	handoff.Status = HandoffCandidateReady
	handoff.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate", Revision: "candidate-revision"}
	handoff.StatusReason = HandoffReasonSkillCandidatePrepared
	if err := handoff.Validate(); err == nil {
		t.Fatal("candidate reason for a different handoff kind was accepted")
	}
}

func TestHandoffCannotCrossProjectOrTeamScope(t *testing.T) {
	_, err := NewImprovementHandoff(
		HandoffConsolidation,
		HandoffScope{ProjectID: "project", TeamID: "team"},
		ArtifactRef{Kind: "consolidation_proposal", ID: "proposal", Revision: "revision"},
		[]SourceBinding{{Ref: ArtifactRef{Kind: "context_item", ID: "source", Revision: "hash"}, ContentHash: "hash", ProjectID: "other-project", TeamID: "team"}},
	)
	if err == nil {
		t.Fatal("cross-project source binding was accepted")
	}
}

func TestHandoffTransitionMatrix(t *testing.T) {
	statuses := []HandoffStatus{
		HandoffProposed, HandoffCandidateReady, HandoffBenchmarkBound, HandoffEvaluated,
		HandoffEligibleForReview, HandoffApproved, HandoffAdopted, HandoffMonitoring,
		HandoffRollbackRecommended, HandoffRejected, HandoffStale,
	}
	allowed := map[[2]HandoffStatus]bool{
		{HandoffProposed, HandoffCandidateReady}:        true,
		{HandoffCandidateReady, HandoffBenchmarkBound}:  true,
		{HandoffBenchmarkBound, HandoffEvaluated}:       true,
		{HandoffEvaluated, HandoffEligibleForReview}:    true,
		{HandoffEvaluated, HandoffRejected}:             true,
		{HandoffEligibleForReview, HandoffApproved}:     true,
		{HandoffEligibleForReview, HandoffRejected}:     true,
		{HandoffApproved, HandoffAdopted}:               true,
		{HandoffAdopted, HandoffMonitoring}:             true,
		{HandoffMonitoring, HandoffMonitoring}:          true,
		{HandoffMonitoring, HandoffRollbackRecommended}: true,
		{HandoffProposed, HandoffStale}:                 true,
		{HandoffCandidateReady, HandoffStale}:           true,
		{HandoffBenchmarkBound, HandoffStale}:           true,
		{HandoffEvaluated, HandoffStale}:                true,
		{HandoffEligibleForReview, HandoffStale}:        true,
		{HandoffApproved, HandoffStale}:                 true,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			if got, want := validHandoffTransition(from, to), allowed[[2]HandoffStatus{from, to}]; got != want {
				t.Fatalf("transition %s -> %s = %t, want %t", from, to, got, want)
			}
		}
	}
}

func TestHandoffCannotAdoptWithoutEvaluation(t *testing.T) {
	handoff := newContractHandoff(t)
	handoff.Status = HandoffAdopted
	handoff.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate", Revision: "candidate-revision"}
	handoff.Benchmark = &BenchmarkBinding{Ref: ArtifactRef{Kind: "benchmark_fixture", ID: "benchmark", Revision: "benchmark-revision"}, Name: "benchmark", Category: "memory", Cases: 1}
	handoff.Adoption = &ArtifactRef{Kind: "memory_policy_activation", ID: "candidate", Revision: "candidate-revision"}
	if err := handoff.Validate(); err == nil {
		t.Fatal("adoption without experiment and evaluation refs was accepted")
	}
}

func TestHandoffEligibleForReviewIsNotApprovalOrAdoption(t *testing.T) {
	store := NewHandoffStore(t.TempDir())
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	handoff := newContractHandoff(t)
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	eligible := advanceContractHandoffToEligible(t, store, handoff)
	if eligible.Status != HandoffEligibleForReview || eligible.Adoption != nil {
		t.Fatalf("eligible handoff bypassed review boundary: %#v", eligible)
	}
	directAdoption := eligible
	directAdoption.Status = HandoffAdopted
	directAdoption.Adoption = &ArtifactRef{Kind: "memory_policy_activation", ID: "candidate", Revision: "candidate-revision"}
	if _, err := store.Transition(t.Context(), eligible.ID, eligible.Revision, directAdoption); err == nil {
		t.Fatal("eligible handoff skipped explicit approval")
	}
}

func TestHandoffArtifactsAndEventsContainNoContentOrSecrets(t *testing.T) {
	handoff := newContractHandoff(t)
	data, err := marshalHandoff(handoff)
	if err != nil {
		t.Fatal(err)
	}
	event := handoffEvent("handoff_created", handoff, handoff.Revision)
	secret := []byte("sk-secret-source-content")
	if bytes.Contains(data, secret) || bytes.Contains(event.Payload, secret) {
		t.Fatal("handoff artifact or audit event persisted source content")
	}
	if bytes.Contains(event.Payload, []byte("status_reason")) || bytes.Contains(event.Payload, []byte("sources")) {
		t.Fatalf("handoff audit event contains non-minimal payload: %s", event.Payload)
	}
}

func TestHandoffCreateRequiresInitialState(t *testing.T) {
	store := NewHandoffStore(t.TempDir())
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	handoff := newContractHandoff(t)
	handoff.Status = HandoffCandidateReady
	handoff.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate", Revision: "candidate-revision"}
	if _, err := store.Create(t.Context(), handoff); err == nil {
		t.Fatal("store created a handoff that did not start at proposed revision 1")
	}
}

func TestHandoffAuditRetryUsesDurableRecord(t *testing.T) {
	store := NewHandoffStore(t.TempDir())
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	handoff := newContractHandoff(t)
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	var retried team.RunEvent
	store.audit = func(_ context.Context, event team.RunEvent) error {
		retried = event
		return nil
	}
	if err := store.RetryAudit(t.Context(), handoff.ID); err != nil {
		t.Fatal(err)
	}
	if retried.Type != "handoff_created" || retried.IdempotencyKey != "handoff:"+handoff.ID+":handoff_created:1" {
		t.Fatalf("retried event = %#v", retried)
	}
	if err := store.RetryAudit(t.Context(), "missing-handoff"); err == nil {
		t.Fatal("audit retry accepted a caller-only handoff")
	}
}

func newContractHandoff(t *testing.T) ImprovementHandoff {
	t.Helper()
	handoff, err := NewImprovementHandoff(
		HandoffMemoryPolicy,
		HandoffScope{ProjectID: "project", TeamID: "team"},
		ArtifactRef{Kind: "memory_policy_proposal", ID: "proposal", Revision: "proposal-revision"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return handoff
}

func advanceContractHandoffToEligible(t *testing.T, store *HandoffStore, handoff ImprovementHandoff) ImprovementHandoff {
	t.Helper()
	next := handoff
	next.Status = HandoffCandidateReady
	next.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate", Revision: "candidate-revision"}
	next, err := store.Transition(t.Context(), handoff.ID, handoff.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	next.Status = HandoffBenchmarkBound
	next.Benchmark = &BenchmarkBinding{Ref: ArtifactRef{Kind: "benchmark_fixture", ID: "benchmark", Revision: "benchmark-revision"}, Name: "benchmark", Category: "memory", Cases: 1}
	next, err = store.Transition(t.Context(), handoff.ID, next.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	report := &ArtifactRef{Kind: "experiment_report", ID: "experiment", Revision: "experiment-revision"}
	next.Status = HandoffEvaluated
	next.Experiment = report
	next.Evaluation = EvaluationState{Decision: "eligible_for_review", Status: "passed", Report: report}
	next, err = store.Transition(t.Context(), handoff.ID, next.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	next.Status = HandoffEligibleForReview
	next, err = store.Transition(t.Context(), handoff.ID, next.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	return next
}
