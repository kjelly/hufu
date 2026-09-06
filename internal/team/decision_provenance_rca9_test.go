package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ArtifactRef is deliberately safe to use as a comparable protocol value.
// In particular, origin metadata belongs in immutableArtifactMetadata rather
// than being added as a slice-bearing field to the public reference.
func TestArtifactRefRemainsComparable(t *testing.T) {
	refs := map[ArtifactRef]struct{}{{ID: "artifact", SHA256: "digest"}: {}}
	if _, ok := refs[ArtifactRef{ID: "artifact", SHA256: "digest"}]; !ok {
		t.Fatal("equivalent ArtifactRef was not comparable as a map key")
	}
}

func TestArtifactOriginMetadataRoundTripLegacyAndConflict(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}

	put, err := store.Put(context.Background(), PutArtifactRequest{
		ID: "origin-roundtrip", Kind: "evidence", Path: "evidence.json", Content: []byte("evidence"),
		RetrievalURL: "https://final.example.test/evidence", ParentSourceIDs: []string{"parent-b", " parent-a ", ""},
	})
	if err != nil {
		t.Fatalf("Put with origin: %v", err)
	}
	origin, err := store.TrustedArtifactMetadata(context.Background(), put.ArtifactRef)
	if err != nil {
		t.Fatalf("TrustedArtifactMetadata: %v", err)
	}
	wantOrigin := ArtifactOriginMetadata{RetrievalURL: "https://final.example.test/evidence", ParentSourceIDs: []string{"parent-a", "parent-b"}}
	if !reflect.DeepEqual(origin, wantOrigin) {
		t.Fatalf("origin = %#v, want %#v", origin, wantOrigin)
	}

	legacyPut, err := store.Put(context.Background(), PutArtifactRequest{
		ID: "origin-legacy", Kind: "evidence", Path: "legacy.json", Content: []byte("legacy"),
	})
	if err != nil {
		t.Fatalf("Put legacy fixture: %v", err)
	}
	legacyBytes, err := json.Marshal(immutableArtifactMetadata{ArtifactRef: legacyPut.ArtifactRef})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "meta", legacyPut.ID+".json"), legacyBytes, 0o644); err != nil {
		t.Fatalf("write legacy metadata: %v", err)
	}
	legacyOrigin, err := store.TrustedArtifactMetadata(context.Background(), legacyPut.ArtifactRef)
	if err != nil {
		t.Fatalf("legacy TrustedArtifactMetadata: %v", err)
	}
	if !reflect.DeepEqual(legacyOrigin, ArtifactOriginMetadata{}) {
		t.Fatalf("legacy origin = %#v, want unavailable origin", legacyOrigin)
	}

	_, err = store.Put(context.Background(), PutArtifactRequest{
		ID: "origin-roundtrip", Kind: "evidence", Path: "evidence.json", Content: []byte("evidence"),
		RetrievalURL: "https://different.example.test/evidence", ParentSourceIDs: []string{"parent-a", "parent-b"},
	})
	if err == nil || !strings.Contains(err.Error(), "metadata conflicts") {
		t.Fatalf("conflicting origin Put = %v, want immutable metadata conflict", err)
	}
}

type trustedOriginTestStore struct {
	ArtifactStore
	mu      sync.Mutex
	origins map[string]ArtifactOriginMetadata
	calls   []ArtifactRef
	err     error
}

func (s *trustedOriginTestStore) TrustedArtifactMetadata(_ context.Context, ref ArtifactRef) (ArtifactOriginMetadata, error) {
	s.mu.Lock()
	s.calls = append(s.calls, ref)
	s.mu.Unlock()
	if s.err != nil {
		return ArtifactOriginMetadata{}, s.err
	}
	return s.origins[ref.ID], nil
}

func (s *trustedOriginTestStore) resolverCalls() []ArtifactRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ArtifactRef(nil), s.calls...)
}

func TestResolveDecisionEvidenceUsesTrustedOriginForEveryEvidenceClass(t *testing.T) {
	workspace := t.TempDir()
	base, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]ArtifactRef, 3)
	for i, id := range []string{"direct", "base-rate", "assumption"} {
		put, putErr := base.Put(context.Background(), PutArtifactRequest{ID: id, Kind: "evidence", Path: id + ".json", Content: []byte(id)})
		if putErr != nil {
			t.Fatal(putErr)
		}
		refs[i] = put.ArtifactRef
	}
	store := &trustedOriginTestStore{
		ArtifactStore: base,
		origins: map[string]ArtifactOriginMetadata{
			"direct":     {RetrievalURL: "https://publisher.example.test/final/direct", ParentSourceIDs: []string{"root-direct"}},
			"base-rate":  {RetrievalURL: "https://other.example.test/final/base-rate", ParentSourceIDs: []string{"root-base"}},
			"assumption": {ParentSourceIDs: []string{"root-assumption"}},
		},
	}
	req := engineRequest(enginePolicy(1))
	req.Artifacts = []ArtifactRef{refs[0]}
	req.BaseRates = []BaseRateEvidence{{
		ReferenceClass: "operations", Metric: "success", SampleSize: 10,
		Distribution: DistributionSummary{Mean: .8, Median: .8, P10: .5, P90: .95}, Source: refs[1],
	}}
	req.Assumptions = []DecisionAssumption{{ID: "A1", Statement: "assumption", EvidenceRefs: []ArtifactRef{refs[2]}}}
	req.Provenance = []EvidenceProvenance{{SourceID: "forged-advisory", SourceType: EvidenceSourceDeclared, ParentSourceIDs: []string{"direct"}}}
	admitted := cloneDecisionRequest(req)
	if err := (&decisionEngine{services: DecisionServices{Store: store}}).resolveDecisionEvidence(context.Background(), &admitted); err != nil {
		t.Fatalf("resolveDecisionEvidence: %v", err)
	}
	if calls := store.resolverCalls(); len(calls) != 3 {
		t.Fatalf("resolver calls = %d, want one for each evidence class", len(calls))
	}
	if len(admitted.trustedProvenance) != 3 {
		t.Fatalf("trusted provenance = %#v, want direct/base-rate/assumption sources", admitted.trustedProvenance)
	}
	byID := make(map[string]EvidenceProvenance, len(admitted.trustedProvenance))
	for _, source := range admitted.trustedProvenance {
		byID[source.SourceID] = source
	}
	if _, forged := byID["forged-advisory"]; forged {
		t.Fatal("model-declared advisory provenance was promoted to trusted provenance")
	}
	if got := byID["https://publisher.example.test/final/direct"]; !reflect.DeepEqual(got.ParentSourceIDs, []string{"root-direct"}) {
		t.Fatalf("direct trusted parents = %#v", got.ParentSourceIDs)
	}
	if got := byID["assumption"]; !reflect.DeepEqual(got.ParentSourceIDs, []string{"root-assumption"}) {
		t.Fatalf("assumption trusted parents = %#v", got.ParentSourceIDs)
	}
}

func TestResolveDecisionEvidenceFailsClosedOnTrustedOriginResolverError(t *testing.T) {
	workspace := t.TempDir()
	base, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	put, err := base.Put(context.Background(), PutArtifactRequest{ID: "resolver-error", Kind: "evidence", Path: "evidence.json", Content: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	store := &trustedOriginTestStore{ArtifactStore: base, err: errors.New("origin metadata unavailable")}
	req := engineRequest(enginePolicy(1))
	req.Artifacts = []ArtifactRef{put.ArtifactRef}
	err = (&decisionEngine{services: DecisionServices{Store: store}}).resolveDecisionEvidence(context.Background(), &req)
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionOutsideViewMissing) || !strings.Contains(err.Error(), "origin metadata unavailable") {
		t.Fatalf("resolver error = %v, want fail-closed outside-view error", err)
	}
	if len(req.trustedProvenance) != 0 {
		t.Fatalf("trusted provenance survived resolver failure: %#v", req.trustedProvenance)
	}
}

func TestTrustedGroupingSurvivesReplayWithoutAdvisoryParentPromotion(t *testing.T) {
	original := []EvidenceProvenance{
		{SourceID: "https://publisher.example.test/a", SourceType: EvidenceSourceURL, ContentHash: "same", ParentSourceIDs: []string{"root"}, DeclaredParentSourceIDs: []string{"forged"}},
		{SourceID: "https://mirror.example.test/b", SourceType: EvidenceSourceURL, ContentHash: "same"},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var replayed []EvidenceProvenance
	if err := json.Unmarshal(encoded, &replayed); err != nil {
		t.Fatal(err)
	}
	want := GroupEvidence(original, EvidenceIndependencePolicy{WarnSharedOrigin: true})
	got := GroupEvidence(replayed, EvidenceIndependencePolicy{WarnSharedOrigin: true})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed grouping changed: got %#v want %#v", got, want)
	}
	if got.IndependenceGroupCount != 1 {
		t.Fatalf("replayed grouping count = %d, want one shared content group", got.IndependenceGroupCount)
	}
}

type recordingArtifactPutStore struct {
	ArtifactStore
	mu       sync.Mutex
	requests []PutArtifactRequest
}

func (s *recordingArtifactPutStore) Put(ctx context.Context, request PutArtifactRequest) (ArtifactPutResult, error) {
	s.mu.Lock()
	request.ParentSourceIDs = append([]string(nil), request.ParentSourceIDs...)
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	return s.ArtifactStore.Put(ctx, request)
}

func (s *recordingArtifactPutStore) putRequests() []PutArtifactRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PutArtifactRequest(nil), s.requests...)
}

func TestReferenceEvidencePublisherDoesNotPromoteAdvisorySourceToArtifactOrigin(t *testing.T) {
	workspace := t.TempDir()
	base, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingArtifactPutStore{ArtifactStore: base}
	draft := validReferenceDraft()
	draft.Entries[0].Source.URL = "https://model-declared.example/source"
	draft.Entries[0].Source.DeclaredParentSourceIDs = []string{"model-parent"}
	refRunner := &referenceDraftRunner{draft: draft}
	engine := &decisionEngine{services: DecisionServices{Store: store, Journal: &memoryJournal{}, ReferenceEvidence: refRunner}, counter: 1}
	req := engineRequest(enginePolicy(1))
	req.DecisionID = "publisher-provenance"
	_, _, provenance, _, err := engine.runReferenceEvidence(context.Background(), req, decisionState{})
	if err != nil {
		t.Fatalf("runReferenceEvidence: %v", err)
	}
	if refRunner.calls != 1 {
		t.Fatalf("reference producer calls = %d, want one", refRunner.calls)
	}
	if len(provenance) != 2 {
		t.Fatalf("reference provenance = %#v, want runtime artifact plus advisory declaration", provenance)
	}
	declaredID, err := referenceDeclaredSourceID(draft.Entries[0].Source)
	if err != nil {
		t.Fatalf("referenceDeclaredSourceID: %v", err)
	}
	var runtime, advisory *EvidenceProvenance
	for i := range provenance {
		source := &provenance[i]
		switch source.SourceType {
		case EvidenceSourceArtifact:
			runtime = source
		case EvidenceSourceDeclared:
			advisory = source
		}
	}
	if runtime == nil || runtime.SourceID == "" || runtime.ContentHash == "" || len(runtime.ParentSourceIDs) != 0 || len(runtime.DeclaredParentSourceIDs) != 0 {
		t.Fatalf("runtime provenance = %#v, want runtime artifact identity without advisory origin", runtime)
	}
	if advisory == nil || advisory.SourceID != declaredID || !reflect.DeepEqual(advisory.DeclaredParentSourceIDs, []string{"model-parent"}) || len(advisory.ParentSourceIDs) != 0 {
		t.Fatalf("advisory provenance = %#v, want declaration retained without trusted parents", advisory)
	}
	if advisory.SourceID == draft.Entries[0].Source.URL {
		t.Fatalf("model-declared URL was promoted to advisory source identity: %#v", advisory)
	}
	requests := store.putRequests()
	if len(requests) != len(draft.Entries)+1 {
		t.Fatalf("artifact Put calls = %d, want %d entry plus result publications", len(requests), len(draft.Entries)+1)
	}
	for _, request := range requests {
		if request.RetrievalURL != "" || len(request.ParentSourceIDs) != 0 {
			t.Fatalf("publisher copied model/request advisory origin into PutArtifactRequest: %#v", request)
		}
	}
}
