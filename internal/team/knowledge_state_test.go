package team

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestClassifyKnowledgeState(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	policy := agent.DefaultMemoryLearningPolicy()
	verified := &contextstore.ExperienceAggregate{
		VerifiedSupportCount: policy.MinConfirmedSupport,
		IndependentTaskCount: policy.MinIndependentTasks,
		LastObservedAt:       now.Add(-time.Hour),
	}
	tests := []struct {
		name      string
		authority ContextAuthority
		aggregate *contextstore.ExperienceAggregate
		policy    agent.MemoryLearningPolicy
		want      KnowledgeState
		wantOK    bool
	}{
		{name: "normative", authority: ContextAuthorityNormative, want: KnowledgeKnown, wantOK: true},
		{name: "example", authority: ContextAuthorityExample},
		{name: "unknown authority", authority: ContextAuthority("future")},
		{name: "historical without aggregate", authority: ContextAuthorityHistorical, want: KnowledgeAssumed, wantOK: true},
		{name: "historical below support", authority: ContextAuthorityHistorical, aggregate: &contextstore.ExperienceAggregate{VerifiedSupportCount: policy.MinConfirmedSupport - 1, IndependentTaskCount: policy.MinIndependentTasks}, want: KnowledgeAssumed, wantOK: true},
		{name: "historical verified", authority: ContextAuthorityHistorical, aggregate: verified, want: KnowledgeKnown, wantOK: true},
		{name: "historical stale", authority: ContextAuthorityHistorical, aggregate: &contextstore.ExperienceAggregate{VerifiedSupportCount: policy.MinConfirmedSupport, IndependentTaskCount: policy.MinIndependentTasks, LastObservedAt: now.Add(-policy.StaleAfter - time.Second)}, want: KnowledgeStale, wantOK: true},
		{name: "staleness disabled", authority: ContextAuthorityHistorical, aggregate: &contextstore.ExperienceAggregate{VerifiedSupportCount: policy.MinConfirmedSupport, IndependentTaskCount: policy.MinIndependentTasks, LastObservedAt: time.Time{}}, policy: func() agent.MemoryLearningPolicy { p := policy; p.StaleAfter = 0; return p }(), want: KnowledgeKnown, wantOK: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			activePolicy := test.policy
			if activePolicy.PolicyVersion == "" {
				activePolicy = policy
			}
			got, ok := classifyKnowledgeState(test.authority, test.aggregate, now, activePolicy, false)
			if got != test.want || ok != test.wantOK {
				t.Fatalf("classifyKnowledgeState() = (%q, %v), want (%q, %v)", got, ok, test.want, test.wantOK)
			}
			gotAgain, okAgain := classifyKnowledgeState(test.authority, test.aggregate, now, activePolicy, false)
			if gotAgain != got || okAgain != ok {
				t.Fatalf("classification changed for identical input: (%q, %v) then (%q, %v)", got, ok, gotAgain, okAgain)
			}
		})
	}
}

func TestResolveMemoryLearningPolicyStaleAfter(t *testing.T) {
	policy, err := resolveMemoryLearningPolicy(rawMemoryLearningPolicy{StaleAfter: "48h"})
	if err != nil {
		t.Fatal(err)
	}
	if policy.StaleAfter != 48*time.Hour {
		t.Fatalf("stale-after = %s, want 48h", policy.StaleAfter)
	}
	if _, err := resolveMemoryLearningPolicy(rawMemoryLearningPolicy{StaleAfter: "not-a-duration"}); err == nil {
		t.Fatal("invalid stale-after was accepted")
	}
	if err := validateMemoryLearningPolicy(func() agent.MemoryLearningPolicy { p := policy; p.StaleAfter = -time.Second; return p }()); err == nil {
		t.Fatal("negative stale-after was accepted")
	}
}

type countingExperienceRepository struct {
	*contextstore.SQLiteRepository
	aggregateCalls int
}

func (r *countingExperienceRepository) ExperienceAggregate(ctx context.Context, itemID, policyVersion string) (contextstore.ExperienceAggregate, error) {
	r.aggregateCalls++
	return r.SQLiteRepository.ExperienceAggregate(ctx, itemID, policyVersion)
}

func TestKnowledgeStateReusesRankingAggregate(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	counting := &countingExperienceRepository{SQLiteRepository: repo}
	c.contextRepo = counting
	policy := c.session.Config.MemoryLearning
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for i := range policy.MinIndependentTasks {
		_, err := repo.ApplyExperienceObservation(t.Context(), contextstore.ExperienceObservation{
			IdempotencyKey:       "support-" + string(rune('a'+i)),
			ContextItemID:        "a",
			PolicyVersion:        policy.PolicyVersion,
			ProjectID:            "project",
			TaskID:               "task-" + string(rune('a'+i)),
			ObservedAt:           now.Add(-time.Hour),
			AppliedDelta:         1,
			VerifiedSupportDelta: 1,
			PositiveWeight:       1,
			PriorAlpha:           policy.PriorAlpha,
			PriorBeta:            policy.PriorBeta,
			UtilityPercentile:    policy.UtilityPercentile,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	record := rankingItem("a", 10)
	entries, scores, aggregates, err := c.reinforceSearchResults(t.Context(), []contextstore.SearchResult{{Item: record, Score: 0.9}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if counting.aggregateCalls != 1 || aggregates[record.ID] == nil {
		t.Fatalf("aggregate calls = %d, aggregate = %#v", counting.aggregateCalls, aggregates[record.ID])
	}
	finalScores := map[string]float64{record.ID: entries[0].FinalScore}
	compiledItems := canonicalCompilerItemsScored([]contextstore.ContextItem{record}, PriorityRelevantLTM, "shared_persistent", false, scores, finalScores, aggregates, nil)
	request := validTestContextRequest()
	manifest, err := BuildContextInjectionManifest(request, CompiledContext{IncludedItems: compiledItems}, nil, "worker", now, policy)
	if err != nil {
		t.Fatal(err)
	}
	if counting.aggregateCalls != 1 {
		t.Fatalf("manifest attribution performed a second aggregate query: calls=%d", counting.aggregateCalls)
	}
	if len(manifest.Items) != 1 || manifest.Items[0].KnowledgeState != KnowledgeKnown {
		t.Fatalf("knowledge attribution = %#v", manifest.Items)
	}
}

func TestComputeTaskKnowledgeCoverage(t *testing.T) {
	manifest := &ContextInjectionManifest{Items: []ContextManifestItem{
		{ID: "known", Included: true, KnowledgeState: KnowledgeKnown},
		{ID: "assumed", Included: true, KnowledgeState: KnowledgeAssumed},
		{ID: "stale", Included: true, KnowledgeState: KnowledgeStale},
		{ID: "example", Included: true},
		{ID: "omitted", Included: false, KnowledgeState: KnowledgeKnown},
	}}
	catalog := []InvariantDefinition{
		{ID: "team", AppliesTo: []string{"internal/team/"}},
		{ID: "command", AppliesTo: []string{"cmd/hufu/main.go", "internal/team/"}},
		{ID: "other", AppliesTo: []string{"README.md"}},
	}
	touchedPaths := []string{"internal/team/task_result.go", "cmd/hufu/main.go", "docs/guide.md"}
	want := TaskKnowledgeCoverage{
		InvariantCoverage: InvariantCoverageSignal{TouchedPathCount: 3, ApplicableInvariantCount: 2, UncoveredPathCount: 1},
		OutcomeCoverage:   OutcomeCoverageSignal{IncludedItemCount: 3, KnownCount: 1, AssumedCount: 1, StaleCount: 1},
	}
	if got := ComputeTaskKnowledgeCoverage(manifest, catalog, touchedPaths); !reflect.DeepEqual(got, want) {
		t.Fatalf("coverage = %#v, want %#v", got, want)
	}
	withoutScope := ComputeTaskKnowledgeCoverage(manifest, catalog, nil)
	if withoutScope.InvariantCoverage != (InvariantCoverageSignal{}) || withoutScope.OutcomeCoverage != want.OutcomeCoverage {
		t.Fatalf("empty touched paths coverage = %#v", withoutScope)
	}
}

func TestAttachTaskKnowledgeCoverageFailsOpen(t *testing.T) {
	c, item, manifest := invariantAttestationFixture(t, "")
	result := &TaskResult{Findings: []Finding{{Summary: "preserved"}}}
	c.attachTaskKnowledgeCoverage(item.ID, 1, manifest.ModelExecutionID, result)
	if result.KnowledgeCoverage == nil {
		t.Fatal("valid manifest did not produce knowledge coverage")
	}

	tampered := manifest
	tampered.Fingerprint = "tampered"
	if err := c.taskTracker.TodoList().SetContextManifest(item.ID, &tampered); err != nil {
		t.Fatal(err)
	}
	failedOpen := &TaskResult{Findings: []Finding{{Summary: "preserved"}}}
	c.attachTaskKnowledgeCoverage(item.ID, 1, manifest.ModelExecutionID, failedOpen)
	if failedOpen.KnowledgeCoverage != nil {
		t.Fatalf("corrupt manifest produced coverage: %#v", failedOpen.KnowledgeCoverage)
	}
	if len(failedOpen.Findings) != 1 || failedOpen.Findings[0].Summary != "preserved" {
		t.Fatalf("coverage failure mutated task findings: %#v", failedOpen.Findings)
	}
}

func TestTaskKnowledgeCoverageCloneAndLegacyReplay(t *testing.T) {
	original := &TaskResult{KnowledgeCoverage: &TaskKnowledgeCoverage{OutcomeCoverage: OutcomeCoverageSignal{KnownCount: 2}}}
	cloned := cloneTaskResult(original)
	cloned.KnowledgeCoverage.OutcomeCoverage.KnownCount = 9
	if original.KnowledgeCoverage.OutcomeCoverage.KnownCount != 2 {
		t.Fatal("cloneTaskResult aliases knowledge coverage")
	}

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var replayed TaskResult
	if err := json.Unmarshal(encoded, &replayed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed.KnowledgeCoverage, original.KnowledgeCoverage) {
		t.Fatalf("coverage replay = %#v, want %#v", replayed.KnowledgeCoverage, original.KnowledgeCoverage)
	}
	var legacy TaskResult
	if err := json.Unmarshal([]byte(`{"status":"success","summary":"legacy","confidence":1,"source":"submitted"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.KnowledgeCoverage != nil {
		t.Fatalf("legacy result gained coverage: %#v", legacy.KnowledgeCoverage)
	}
}

func TestKnowledgeCoverageDoesNotAffectInvariantBlocking(t *testing.T) {
	verification := &InvariantVerificationResult{Assessments: []InvariantAssessment{{Severity: InvariantSeverityError, Status: InvariantViolated}}}
	before := HasBlockingInvariantAssessment(InvariantVerificationGate, verification)
	result := &TaskResult{InvariantVerification: verification, KnowledgeCoverage: &TaskKnowledgeCoverage{InvariantCoverage: InvariantCoverageSignal{UncoveredPathCount: 99}}}
	after := HasBlockingInvariantAssessment(InvariantVerificationGate, result.InvariantVerification)
	if !before || after != before {
		t.Fatalf("blocking assessment changed after adding coverage: before=%v after=%v", before, after)
	}
}
