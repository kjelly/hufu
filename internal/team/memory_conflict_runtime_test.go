package team

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestClassifyKnowledgeStateConflicting(t *testing.T) {
	policy := agent.DefaultMemoryLearningPolicy()
	now := time.Now()
	supported := &contextstore.ExperienceAggregate{VerifiedSupportCount: 10, IndependentTaskCount: 10, LastObservedAt: now}
	cases := []struct {
		name        string
		authority   ContextAuthority
		aggregate   *contextstore.ExperienceAggregate
		conflicting bool
		want        KnowledgeState
		wantOK      bool
	}{
		{name: "historical conflicting beats known", authority: ContextAuthorityHistorical, aggregate: supported, conflicting: true, want: KnowledgeConflicting, wantOK: true},
		{name: "historical conflicting without aggregate", authority: ContextAuthorityHistorical, conflicting: true, want: KnowledgeConflicting, wantOK: true},
		{name: "historical without conflict stays known", authority: ContextAuthorityHistorical, aggregate: supported, want: KnowledgeKnown, wantOK: true},
		{name: "normative ignores conflict", authority: ContextAuthorityNormative, conflicting: true, want: KnowledgeKnown, wantOK: true},
		{name: "example stays unclassified", authority: ContextAuthorityExample, conflicting: true, want: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyKnowledgeState(tc.authority, tc.aggregate, now, policy, tc.conflicting)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("classifyKnowledgeState = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func saveRuntimeConflict(t *testing.T, repo *contextstore.SQLiteRepository, aID, bID string) {
	t.Helper()
	a, err := repo.Get(context.Background(), aID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.Get(context.Background(), bID)
	if err != nil {
		t.Fatal(err)
	}
	j := contextstore.PairJudgment{ProjectID: "project", TeamID: "team", ItemAID: a.ID, ItemBID: b.ID, ItemAContentHash: a.ContentHash, ItemBContentHash: b.ContentHash, Verdict: contextstore.PairVerdictContradicts, JudgePolicyVersion: contextstore.CurrentConflictJudgePolicyVersion, JudgeModel: "judge"}
	contextstore.NormalizePairJudgment(&j)
	if _, _, err = repo.SavePairJudgment(context.Background(), j, "operator", false); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorOpenConflictsForItems(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningObserve)
	saveRuntimeConflict(t, repo, "a", "b")
	items := []contextstore.ContextItem{rankingItem("a", 10), rankingItem("base-a", 10)}
	got := c.openConflictsForItems(t.Context(), items)
	if len(got) != 1 || len(got["a"]) != 1 {
		t.Fatalf("openConflictsForItems = %v, want only a", got)
	}
}

type failingConflictLookupRepository struct{ *contextstore.SQLiteRepository }

func (failingConflictLookupRepository) OpenConflictsForItems(context.Context, string, string, []string) (map[string][]string, error) {
	return nil, errors.New("conflict table locked")
}

func TestOpenConflictLookupFailureDegradesWithoutFailing(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningObserve)
	c.contextRepo = failingConflictLookupRepository{repo}
	if got := c.openConflictsForItems(t.Context(), []contextstore.ContextItem{rankingItem("a", 10)}); got != nil {
		t.Fatalf("failed lookup = %v, want nil attribution", got)
	}
}

func conflictCompileInput(bundle *CanonicalContextBundle) WorkerContextInput {
	return WorkerContextInput{
		Goal:            "choose the storage engine",
		CanonicalMemory: bundle,
		ModelContext:    ModelContextSpec{ModelID: "test", ContextWindow: 200000, MaxOutputTokens: 1000, SafetyMarginTokens: 100},
	}
}

func includedIDs(compiled CompiledContext) []string {
	ids := make([]string, 0, len(compiled.IncludedItems))
	for _, item := range compiled.IncludedItems {
		ids = append(ids, item.ID)
	}
	return ids
}

func manifestStates(t *testing.T, compiled CompiledContext) map[string]KnowledgeState {
	t.Helper()
	manifest, err := BuildContextInjectionManifest(validTestContextRequest(), compiled, nil, "worker", time.Now(), agent.DefaultMemoryLearningPolicy())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]KnowledgeState{}
	for _, item := range manifest.Items {
		if item.Included {
			states[item.ID] = item.KnowledgeState
		}
	}
	return states
}

func TestConflictMarkingDoesNotChangePrompt(t *testing.T) {
	records := []contextstore.ContextItem{rankingItem("a", 10), rankingItem("b", 20)}
	plain := &CanonicalContextBundle{SharedPersistent: records}
	marked := &CanonicalContextBundle{SharedPersistent: records, SharedPersistentConflicts: map[string][]string{"a": {"conflict-1"}, "b": {"conflict-1"}}}
	base, err := CompileWorkerContext(t.Context(), conflictCompileInput(plain))
	if err != nil {
		t.Fatal(err)
	}
	withConflicts, err := CompileWorkerContext(t.Context(), conflictCompileInput(marked))
	if err != nil {
		t.Fatal(err)
	}
	if base.Prompt != withConflicts.Prompt || base.Fingerprint != withConflicts.Fingerprint || base.UsedTokens != withConflicts.UsedTokens {
		t.Fatal("conflict attribution changed the compiled prompt")
	}
	if !reflect.DeepEqual(includedIDs(base), includedIDs(withConflicts)) || len(base.OmittedItems) != len(withConflicts.OmittedItems) {
		t.Fatalf("conflict attribution changed inclusion: %v vs %v", includedIDs(base), includedIDs(withConflicts))
	}
	if !strings.Contains(base.Prompt, "procedure a") {
		t.Fatalf("fixture memory missing from prompt:\n%s", base.Prompt)
	}
	plainStates, markedStates := manifestStates(t, base), manifestStates(t, withConflicts)
	for _, id := range []string{"a", "b"} {
		if plainStates[id] == KnowledgeConflicting || markedStates[id] != KnowledgeConflicting {
			t.Fatalf("state of %s = %q/%q, want non-conflicting then conflicting", id, plainStates[id], markedStates[id])
		}
	}
}

func TestManifestMarksConflictEvenWhenCounterpartOmitted(t *testing.T) {
	bundle := &CanonicalContextBundle{SharedPersistent: []contextstore.ContextItem{rankingItem("a", 10)}, SharedPersistentConflicts: map[string][]string{"a": {"conflict-with-unselected"}}}
	compiled, err := CompileWorkerContext(t.Context(), conflictCompileInput(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if states := manifestStates(t, compiled); states["a"] != KnowledgeConflicting {
		t.Fatalf("states = %v, want a conflicting", states)
	}
}

func TestTaskKnowledgeCoverageCountsConflicting(t *testing.T) {
	manifest := &ContextInjectionManifest{Items: []ContextManifestItem{
		{ID: "a", Included: true, KnowledgeState: KnowledgeConflicting},
		{ID: "b", Included: true, KnowledgeState: KnowledgeKnown},
		{ID: "c", Included: false, KnowledgeState: KnowledgeConflicting},
	}}
	coverage := ComputeTaskKnowledgeCoverage(manifest, nil, nil)
	if coverage.OutcomeCoverage.ConflictingCount != 1 || coverage.OutcomeCoverage.IncludedItemCount != 2 {
		t.Fatalf("coverage = %+v", coverage.OutcomeCoverage)
	}
}

func TestCoverageJSONUnchangedWithoutConflicts(t *testing.T) {
	data, err := json.Marshal(OutcomeCoverageSignal{IncludedItemCount: 1, KnownCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "conflicting_count") {
		t.Fatalf("coverage JSON without conflicts = %s, want no conflicting_count", data)
	}
}
