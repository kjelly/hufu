package team

import (
	"testing"
	"time"
)

func manifestFixture(attempt int, requestID string, trigger ContextTrigger, at time.Time) ContextInjectionManifest {
	return ContextInjectionManifest{
		SchemaVersion:    2,
		RequestID:        requestID,
		TaskID:           "1",
		Attempt:          attempt,
		Agent:            "deployer",
		ModelExecutionID: "model-execution-1",
		Trigger:          trigger,
		CreatedAt:        at,
	}
}

// TestCanonicalTaskShadowIgnoresManifestOrder guards E-32: the live projection
// appends context manifests as they are written, while event replay merges
// them sorted by attempt and request ID. The same set in a different order
// must not read as an event-store projection mismatch on resume.
func TestCanonicalTaskShadowIgnoresManifestOrder(t *testing.T) {
	base := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)
	dispatch := manifestFixture(1, "ctx-1-58fa", "task_dispatch", base)
	failureA := manifestFixture(1, "ctx-1-0aaa", "tool_failure", base.Add(time.Minute))
	failureB := manifestFixture(1, "ctx-1-ffff", "tool_failure", base.Add(2*time.Minute))
	retry := manifestFixture(2, "ctx-1-1234", "task_dispatch", base.Add(3*time.Minute))
	changed := failureA
	changed.Trigger = "task_dispatch"

	cases := []struct {
		name      string
		live      []ContextInjectionManifest
		replayed  []ContextInjectionManifest
		wantMatch bool
	}{
		{name: "insertion order vs replay order", live: []ContextInjectionManifest{dispatch, failureA, failureB}, replayed: mergeContextInjectionManifests(nil, []ContextInjectionManifest{dispatch, failureA, failureB}), wantMatch: true},
		{name: "attempts in reverse", live: []ContextInjectionManifest{retry, failureB, dispatch}, replayed: []ContextInjectionManifest{dispatch, failureB, retry}, wantMatch: true},
		{name: "same order", live: []ContextInjectionManifest{dispatch, failureA}, replayed: []ContextInjectionManifest{dispatch, failureA}, wantMatch: true},
		{name: "different manifest content", live: []ContextInjectionManifest{dispatch, failureA}, replayed: []ContextInjectionManifest{dispatch, changed}, wantMatch: false},
		{name: "missing manifest", live: []ContextInjectionManifest{dispatch, failureA, failureB}, replayed: []ContextInjectionManifest{dispatch, failureB}, wantMatch: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := &TodoItem{ID: "1", Agent: "deployer", Status: TaskBlocked, ContextManifests: tc.live}
			replayed := &TodoItem{ID: "1", Agent: "deployer", Status: TaskBlocked, ContextManifests: tc.replayed}
			err := compareSingleTaskProjection(live, replayed)
			if gotMatch := err == nil; gotMatch != tc.wantMatch {
				t.Fatalf("compareSingleTaskProjection() = %v, want match=%v", err, tc.wantMatch)
			}
			if gotSameKey := taskTransitionEventKey(live) == taskTransitionEventKey(replayed); gotSameKey != tc.wantMatch {
				t.Fatalf("taskTransitionEventKey equality = %v, want %v", gotSameKey, tc.wantMatch)
			}
		})
	}
}

func TestNormalizeContextManifestsDoesNotReorderTheSource(t *testing.T) {
	base := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)
	source := []ContextInjectionManifest{
		manifestFixture(1, "ctx-1-ffff", "tool_failure", base),
		manifestFixture(1, "ctx-1-0aaa", "task_dispatch", base.Add(time.Second)),
	}
	normalized := normalizeContextManifests(source)
	if normalized[0].RequestID != "ctx-1-0aaa" || normalized[1].RequestID != "ctx-1-ffff" {
		t.Fatalf("normalized order = %s, %s; want canonical request-ID order", normalized[0].RequestID, normalized[1].RequestID)
	}
	if source[0].RequestID != "ctx-1-ffff" {
		t.Fatal("normalizing the shadow reordered the live projection's slice")
	}
}
