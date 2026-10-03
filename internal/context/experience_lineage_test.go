package context

import (
	"context"
	"path/filepath"
	"testing"
)

// TestListExperienceLineageOnlyLinksConfirmedPromotions pins which records
// inherit evidence: a confirmed persistent record extracted at run end, from
// each session record it cites that exists in the same project.
func TestListExperienceLineageOnlyLinksConfirmedPromotions(t *testing.T) {
	ctx := context.Background()
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	session := Scope{ProjectID: "proj", TeamID: "team", SessionID: "session-1"}
	persistent := Scope{ProjectID: "proj", TeamID: "team"}
	cites := func(ids ...string) []EvidenceRef {
		refs := make([]EvidenceRef, 0, len(ids)+1)
		for _, id := range ids {
			refs = append(refs, EvidenceRef{ItemID: id, Type: "context_item", Ref: id})
		}
		return append(refs, EvidenceRef{Type: "evidence_manifest", Ref: "manifest-1"})
	}
	promoted := SourceRef{Type: "shared_memory_candidate", Ref: PromotedSessionRecordSourceRef}
	items := []ContextItem{
		{ID: "session-a", Kind: ContextObservation, Content: "finding a", Scope: session, Lifecycle: LifecycleConfirmed},
		{ID: "session-b", Kind: ContextObservation, Content: "finding b", Scope: session, Lifecycle: LifecycleConfirmed},
		{ID: "promoted", Kind: ContextPattern, Content: "pattern a", Scope: persistent, Lifecycle: LifecycleConfirmed, Source: promoted, Evidence: cites("session-a", "session-b")},
		{ID: "still-candidate", Kind: ContextPattern, Content: "pattern c", Scope: persistent, Lifecycle: LifecycleCandidate, Source: promoted, Evidence: cites("session-a")},
		{ID: "reflexion", Kind: ContextPattern, Content: "pattern d", Scope: persistent, Lifecycle: LifecycleConfirmed, Source: SourceRef{Type: "shared_memory_candidate", Ref: "reflexion"}, Evidence: cites("session-a")},
		{ID: "missing-source", Kind: ContextPattern, Content: "pattern e", Scope: persistent, Lifecycle: LifecycleConfirmed, Source: promoted, Evidence: cites("not-stored")},
		{ID: "session-copy", Kind: ContextPattern, Content: "pattern f", Scope: session, Lifecycle: LifecycleConfirmed, Source: promoted, Evidence: cites("session-a")},
	}
	if err := repo.Append(ctx, items...); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		projectID string
		want      []ExperienceLineage
	}{
		{name: "project", projectID: "proj", want: []ExperienceLineage{{SourceID: "session-a", TargetID: "promoted"}, {SourceID: "session-b", TargetID: "promoted"}}},
		{name: "every project", projectID: "", want: []ExperienceLineage{{SourceID: "session-a", TargetID: "promoted"}, {SourceID: "session-b", TargetID: "promoted"}}},
		{name: "other project", projectID: "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.ListExperienceLineage(ctx, tc.projectID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("lineage = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("lineage[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
