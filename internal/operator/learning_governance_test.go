package operator

import "testing"

func TestNormalizeSnapshotClonesGovernanceCounters(t *testing.T) {
	rejected, stale, edited, unknown, conflicts := int64(1), int64(2), int64(3), int64(4), int64(5)
	snapshot := testSnapshot()
	snapshot.Learning.RejectedPromotions = &rejected
	snapshot.Learning.StalePromotions = &stale
	snapshot.Learning.AppliedEditedPromotions = &edited
	snapshot.Learning.AppliedEditUnknownPromotions = &unknown
	snapshot.Learning.OpenConflicts = &conflicts
	normalized := NormalizeSnapshot(snapshot)
	rejected, stale, edited, unknown, conflicts = 9, 9, 9, 9, 9
	got := []*int64{normalized.Learning.RejectedPromotions, normalized.Learning.StalePromotions, normalized.Learning.AppliedEditedPromotions, normalized.Learning.AppliedEditUnknownPromotions, normalized.Learning.OpenConflicts}
	for i, pointer := range got {
		if pointer == nil || *pointer != int64(i+1) {
			t.Fatalf("counter %d aliases or lost its value: %#v", i, normalized.Learning)
		}
	}
}

func TestFinalizeSnapshotRejectsNegativeGovernanceCounters(t *testing.T) {
	negative := int64(-1)
	cases := []struct {
		name string
		set  func(*LearningView)
	}{
		{name: "rejected", set: func(v *LearningView) { v.RejectedPromotions = &negative }},
		{name: "stale", set: func(v *LearningView) { v.StalePromotions = &negative }},
		{name: "applied edited", set: func(v *LearningView) { v.AppliedEditedPromotions = &negative }},
		{name: "applied edit unknown", set: func(v *LearningView) { v.AppliedEditUnknownPromotions = &negative }},
		{name: "open conflicts", set: func(v *LearningView) { v.OpenConflicts = &negative }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := testSnapshot()
			tc.set(&snapshot.Learning)
			if _, err := FinalizeSnapshot(snapshot); err == nil {
				t.Fatal("negative governance counter was accepted")
			}
		})
	}
}
