package inspect

import (
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestObservationTracePreservesOriginalAndDeliveredContentReferences(t *testing.T) {
	digest, delivered := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		kind    team.EventType
		payload any
		refs    []string
	}{
		{team.EventToolObservationRecorded, team.ToolObservationPayload{Tool: "source__read", Artifact: team.ArtifactRef{ID: "observation-id", SHA256: digest}}, []string{"observation-id", digest}},
		{team.EventToolObservationRead, team.ObservationReadPayload{ObservationID: "source-event", SHA256: digest, ContentSHA256: delivered}, []string{"source-event", digest, delivered}},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			entry := eventTraceEntry(IndexedEvent{Ordinal: 5, Event: team.RunEvent{ID: "event-read", Type: string(tc.kind), Payload: jsonBytes(t, tc.payload), TaskID: "2", Attempt: 1}})
			for _, ref := range tc.refs {
				if !slices.Contains(entry.Refs, ref) {
					t.Fatalf("lost observation trace reference %q: %+v", ref, entry)
				}
			}
			if tc.kind == team.EventToolObservationRead && entry.Status != "historical" {
				t.Fatalf("read presented as fresh: %+v", entry)
			}
		})
	}
}
