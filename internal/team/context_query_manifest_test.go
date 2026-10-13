package team

import (
	"bytes"
	json "encoding/json/v2"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestContextQueryManifestKeepsLegacyBytesAndBindsEffectiveSources(t *testing.T) {
	plan, sources, row := querySelectionFixture()
	selection, err := SelectContextQuerySources(t.Context(), &querySelectionRepo{rows: []contextstore.SearchResult{row}}, plan, sources, stableQueryRevision)
	if err != nil {
		t.Fatal(err)
	}
	legacy := ContextInjectionManifest{SchemaVersion: 2, RequestHash: "v1-hash", RequestID: "request", RunID: "run", Agent: "worker", CreatedAt: time.Now()}
	legacy.Fingerprint = contextManifestFingerprint(legacy)
	before, _ := json.Marshal(legacy)
	active, err := WithContextQueryAttribution(legacy, selection.Attribution)
	if err != nil || active.SchemaVersion != 3 || active.RequestHash != legacy.RequestHash || active.Fingerprint == legacy.Fingerprint {
		t.Fatalf("invalid schema3 upgrade: %+v, %v", active, err)
	}
	after, _ := json.Marshal(legacy)
	if !bytes.Equal(before, after) || bytes.Contains(before, []byte("query_attribution")) {
		t.Fatal("query helper modified legacy serialization")
	}
	if err := ValidateContextQueryManifest(active); err != nil {
		t.Fatal(err)
	}
	clone := cloneContextInjectionManifest(&active)
	clone.Query.Sources[0].Fallback = QueryFallbackSearchFailed
	if err := ValidateContextQueryManifest(*clone); err == nil {
		t.Fatal("source mutation survived fingerprint/attribution validation")
	}
	if err := ValidateContextQueryManifest(active); err != nil {
		t.Fatal("manifest clone mutated original source attribution")
	}
	changed := selection.Attribution
	changed.Sources = append([]ContextQuerySourceAttribution(nil), changed.Sources...)
	changed.Sources[0].Mode = QueryModeLegacy
	changed.Sources[0].Fallback = QueryFallbackNoMatches
	changed.Sources[0].SupplementalHash = ""
	changed.Mode = QueryModeMixed
	if hashQueryAttribution(changed) == selection.CompositeHash {
		t.Fatal("actual fallback mode was omitted from composite hash")
	}
}
