package team

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPrimaryEvidenceCollectionIsDeterministicRedactedAndContinuesAfterOverflow(t *testing.T) {
	ctx := context.Background()
	store := newPrimaryEvidenceTestStore(t)
	policyRef := putPrimaryEvidenceTestDecisionRef(t, store, "redaction-policy", "application/json", []byte(`{"version":1}`))
	limitsRef := putPrimaryEvidenceTestDecisionRef(t, store, "evidence-limits", "application/json", []byte(`{"version":1}`))
	records := []PrimaryEvidenceSourceRecord{
		primaryEvidenceTestRecord(t, store, "source-required", "application/json", []byte(`{"api_key":"super-secret-credential","value":7}`), "fact", "event-1", 1),
		primaryEvidenceTestRecord(t, store, "source-too-large", "text/plain", []byte(strings.Repeat("large evidence line\n", 12)), "artifact_text", "event-2", 2),
		primaryEvidenceTestRecord(t, store, "source-small", "text/plain", []byte("small\n"), "artifact_text", "event-3", 3),
	}
	records[0].MandatoryReasons = []string{"request_input"}
	candidates, err := PrimaryEvidenceCandidatesFromSources(ctx, store, records)
	if err != nil {
		t.Fatal(err)
	}
	request := primaryEvidenceTestRequest(store, policyRef, limitsRef, candidates)
	request.Limits.MaxItems = 8
	request.Limits.MaxViewBytes = 65
	request.Limits.MaxItemViewBytes = 256
	request.Limits.MaxContextInputTokens = 256
	request.Limits.ReservedFramingTokens = 64

	first, err := CollectPrimaryDecisionEvidence(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	reversed := slices.Clone(candidates)
	slices.Reverse(reversed)
	request.Candidates = reversed
	second, err := CollectPrimaryDecisionEvidence(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := CanonicalDecisionJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := CanonicalDecisionJSON(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("evidence changed with candidate order\nfirst: %s\nsecond: %s", firstJSON, secondJSON)
	}
	if strings.Contains(string(firstJSON), "super-secret-credential") || !strings.Contains(string(firstJSON), "[REDACTED]") {
		t.Fatalf("evidence was not safely redacted: %s", firstJSON)
	}
	if len(first.Items) != 2 || first.Items[1].Content != "small\n" {
		t.Fatalf("selected items = %#v, want mandatory plus small item after overflow", first.Items)
	}
	if first.Coverage.ExcludedReasonCounts.Budget != 1 {
		t.Fatalf("budget exclusions = %d, want 1", first.Coverage.ExcludedReasonCounts.Budget)
	}
}

func TestPrimaryEvidenceMandatoryOverflowFailsClosed(t *testing.T) {
	store := newPrimaryEvidenceTestStore(t)
	policyRef := putPrimaryEvidenceTestDecisionRef(t, store, "redaction-policy", "application/json", []byte(`{}`))
	limitsRef := putPrimaryEvidenceTestDecisionRef(t, store, "evidence-limits", "application/json", []byte(`{}`))
	record := primaryEvidenceTestRecord(t, store, "source-required", "text/plain", []byte(strings.Repeat("required\n", 20)), "artifact_text", "event-1", 1)
	record.MandatoryReasons = []string{"required_criterion"}
	candidates, err := PrimaryEvidenceCandidatesFromSources(context.Background(), store, []PrimaryEvidenceSourceRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	request := primaryEvidenceTestRequest(store, policyRef, limitsRef, candidates)
	request.Limits.MaxItemViewBytes = 32
	_, err = CollectPrimaryDecisionEvidence(context.Background(), request)
	if !errors.Is(err, ErrDecisionEvidenceMandatoryOverflow) {
		t.Fatalf("error = %v, want mandatory overflow", err)
	}
}

func TestPrimaryEvidenceSourceAdapterRejectsDuplicateJSONKeys(t *testing.T) {
	store := newPrimaryEvidenceTestStore(t)
	record := primaryEvidenceTestRecord(t, store, "source-duplicate", "application/json", []byte(`{"value":1,"value":2}`), "fact", "event-1", 1)
	_, err := PrimaryEvidenceCandidatesFromSources(context.Background(), store, []PrimaryEvidenceSourceRecord{record})
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
		t.Fatalf("error = %v, want duplicate-key rejection", err)
	}
}

func TestPrimaryEvidenceLongLineBecomesMetadataOnly(t *testing.T) {
	store := newPrimaryEvidenceTestStore(t)
	policyRef := putPrimaryEvidenceTestDecisionRef(t, store, "redaction-policy", "application/json", []byte(`{}`))
	limitsRef := putPrimaryEvidenceTestDecisionRef(t, store, "evidence-limits", "application/json", []byte(`{}`))
	record := primaryEvidenceTestRecord(t, store, "source-long-line", "text/plain", []byte(strings.Repeat("x", primaryEvidenceMaxLineBytes+1)), "artifact_text", "event-1", 1)
	candidates, err := PrimaryEvidenceCandidatesFromSources(context.Background(), store, []PrimaryEvidenceSourceRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	request := primaryEvidenceTestRequest(store, policyRef, limitsRef, candidates)
	request.Limits.MaxItemViewBytes = 256
	evidence, err := CollectPrimaryDecisionEvidence(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Items) != 1 || evidence.Items[0].Source.SourceKind != "artifact_metadata" || evidence.Items[0].Extraction.Algorithm != "metadata_only_v1" {
		t.Fatalf("metadata-only item = %#v", evidence.Items)
	}
}

func TestPrimaryEvidenceUnknownProvenanceDoesNotMeetMinimum(t *testing.T) {
	store := newPrimaryEvidenceTestStore(t)
	policyRef := putPrimaryEvidenceTestDecisionRef(t, store, "redaction-policy", "application/json", []byte(`{}`))
	limitsRef := putPrimaryEvidenceTestDecisionRef(t, store, "evidence-limits", "application/json", []byte(`{}`))
	record := primaryEvidenceTestRecord(t, store, "source-model", "text/plain", []byte("reported"), "advisory_summary", "event-1", 1)
	record.ProvenanceAuthority = "model_reported"
	candidates, err := PrimaryEvidenceCandidatesFromSources(context.Background(), store, []PrimaryEvidenceSourceRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	request := primaryEvidenceTestRequest(store, policyRef, limitsRef, candidates)
	request.Limits.KnownIndependenceMinimum = 1
	_, err = CollectPrimaryDecisionEvidence(context.Background(), request)
	if !errors.Is(err, ErrDecisionEvidenceInsufficient) {
		t.Fatalf("error = %v, want insufficient evidence", err)
	}
}

func TestPrimaryEvidenceFailedAssertionIsMandatoryNegativeEvidence(t *testing.T) {
	store := newPrimaryEvidenceTestStore(t)
	policyRef := putPrimaryEvidenceTestDecisionRef(t, store, "redaction-policy", "application/json", []byte(`{}`))
	limitsRef := putPrimaryEvidenceTestDecisionRef(t, store, "evidence-limits", "application/json", []byte(`{}`))
	records := []PrimaryEvidenceSourceRecord{
		primaryEvidenceTestRecord(t, store, "source-fact", "application/json", []byte(`{"value":"positive"}`), "fact", "event-1", 1),
		primaryEvidenceTestRecord(t, store, "source-failed", "application/json", []byte(`{"assertion":"did not hold"}`), "assertion", "event-2", 2),
	}
	records[1].Verification = PrimaryEvidenceVerificationV1{State: "failed", Scope: "specific_assertion"}
	candidates, err := PrimaryEvidenceCandidatesFromSources(context.Background(), store, records)
	if err != nil {
		t.Fatal(err)
	}
	request := primaryEvidenceTestRequest(store, policyRef, limitsRef, candidates)
	request.Limits.MaxItems = 1
	evidence, err := CollectPrimaryDecisionEvidence(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Items) != 1 || evidence.Items[0].Verification.State != "failed" || !slices.Contains(evidence.Items[0].MandatoryReasons, "failed_assertion") {
		t.Fatalf("selected evidence = %#v, want mandatory failed assertion", evidence.Items)
	}
}

func TestPrimaryEvidenceRejectsInvalidBaseRateAndCircularParents(t *testing.T) {
	store := newPrimaryEvidenceTestStore(t)
	policyRef := putPrimaryEvidenceTestDecisionRef(t, store, "redaction-policy", "application/json", []byte(`{}`))
	limitsRef := putPrimaryEvidenceTestDecisionRef(t, store, "evidence-limits", "application/json", []byte(`{}`))
	sourceRef := putPrimaryEvidenceTestDecisionRef(t, store, "base-source", "application/json", []byte(`{"rows":10}`))
	badRate := map[string]any{
		"reference_class": "deployments", "metric": "success", "sample_size": 10,
		"distribution": map[string]any{"mean": 0.5, "median": 0.5, "p10": 0.9, "p90": 0.1},
		"source":       sourceRef, "limitations": []string{},
	}
	badRateJSON, err := json.Marshal(badRate)
	if err != nil {
		t.Fatal(err)
	}
	record := primaryEvidenceTestRecord(t, store, "source-rate", "application/json", badRateJSON, "base_rate", "event-1", 1)
	record.BaseRate = true
	candidates, err := PrimaryEvidenceCandidatesFromSources(context.Background(), store, []PrimaryEvidenceSourceRecord{record})
	if err == nil || !strings.Contains(err.Error(), "typed base rate is invalid") {
		t.Fatalf("error = %v, want typed base-rate rejection", err)
	}

	request := primaryEvidenceTestRequest(store, policyRef, limitsRef, candidates)
	left := primaryEvidenceTestCandidate("event-left", "left")
	right := primaryEvidenceTestCandidate("event-right", "right")
	left.Source.DerivedParentItemIDs = []string{right.Source.SourceEventID}
	right.Source.DerivedParentItemIDs = []string{left.Source.SourceEventID}
	leftID, _ := primaryEvidenceItemID(left.Source)
	rightID, _ := primaryEvidenceItemID(right.Source)
	left.Source.DerivedParentItemIDs = []string{rightID}
	right.Source.DerivedParentItemIDs = []string{leftID}
	request.Candidates = []PrimaryEvidenceCandidate{left, right}
	_, err = CollectPrimaryDecisionEvidence(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error = %v, want provenance cycle", err)
	}
}

func TestAdaptPrimaryDecisionEvidenceNamespacesFactsAndResolvesViews(t *testing.T) {
	store := newPrimaryEvidenceTestStore(t)
	policyRef := putPrimaryEvidenceTestDecisionRef(t, store, "redaction-policy", "application/json", []byte(`{}`))
	limitsRef := putPrimaryEvidenceTestDecisionRef(t, store, "evidence-limits", "application/json", []byte(`{}`))
	records := []PrimaryEvidenceSourceRecord{
		primaryEvidenceTestRecord(t, store, "source-a", "application/json", []byte(`{"value":1}`), "fact", "event-1", 1),
		primaryEvidenceTestRecord(t, store, "source-b", "application/json", []byte(`{"value":2}`), "fact", "event-2", 2),
	}
	for index := range records {
		records[index].JSONPointer = "/value"
	}
	candidates, err := PrimaryEvidenceCandidatesFromSources(context.Background(), store, records)
	if err != nil {
		t.Fatal(err)
	}
	request := primaryEvidenceTestRequest(store, policyRef, limitsRef, candidates)
	evidence, err := CollectPrimaryDecisionEvidence(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := AdaptPrimaryDecisionEvidence(context.Background(), store, evidence, PrimaryEvidencePacketInput{
		ID: "packet-1", Question: "Which option?", Options: []DecisionOption{{ID: "go", Kind: OptionExecute}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Facts) != 2 || len(packet.Artifacts) != 2 || packet.AvailableMetadata == nil {
		t.Fatalf("adapted packet = %#v", packet)
	}
	values := make([]string, 0, len(packet.Facts))
	for key, value := range packet.Facts {
		if !strings.Contains(key, "#/value") {
			t.Fatalf("fact key %q is not namespaced by item and pointer", key)
		}
		values = append(values, value.(json.Number).String())
	}
	slices.Sort(values)
	if !reflect.DeepEqual(values, []string{"1", "2"}) {
		t.Fatalf("fact values = %v", values)
	}
}

func newPrimaryEvidenceTestStore(t *testing.T) *FileArtifactStore {
	t.Helper()
	store, err := NewFileArtifactStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func putPrimaryEvidenceTestArtifact(t *testing.T, store ArtifactStore, id, mediaType string, content []byte) ArtifactRef {
	t.Helper()
	result, err := store.Put(context.Background(), PutArtifactRequest{ID: id, Kind: "test", Role: "evidence", Path: "evidence/" + id, MediaType: mediaType, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return result.ArtifactRef
}

func putPrimaryEvidenceTestDecisionRef(t *testing.T, store ArtifactStore, id, mediaType string, content []byte) DecisionArtifactRef {
	t.Helper()
	ref := putPrimaryEvidenceTestArtifact(t, store, id, mediaType, content)
	result, err := decisionArtifactRefFromArtifact(ref)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func primaryEvidenceTestRecord(t *testing.T, store ArtifactStore, id, mediaType string, content []byte, kind, eventID string, order uint64) PrimaryEvidenceSourceRecord {
	t.Helper()
	return PrimaryEvidenceSourceRecord{
		SourceKind: kind, SourceArtifact: putPrimaryEvidenceTestArtifact(t, store, id, mediaType, content), SourceEventID: eventID,
		ProvenanceAuthority: "runtime_observed", Verification: PrimaryEvidenceVerificationV1{State: "not_checked", Scope: "none"},
		EpistemicStatus: "reported", SourceEventOrder: order, BaseRate: kind == "base_rate", Assumption: kind == "assumption",
	}
}

func primaryEvidenceTestRequest(store ArtifactStore, policyRef, limitsRef DecisionArtifactRef, candidates []PrimaryEvidenceCandidate) PrimaryEvidenceCollectionRequest {
	return PrimaryEvidenceCollectionRequest{
		LogicalRunID: "ldr_0123456789abcdef0123456789abcdef", BranchID: "main", Generation: 1,
		RequirementDigest: strings.Repeat("a", 64), SupportCursor: DecisionSupportCursor{EventID: "event-cursor", EventHash: strings.Repeat("b", 64), LineageDigest: strings.Repeat("c", 64)},
		SupportRevisionDigest: strings.Repeat("d", 64), RedactionPolicyRef: policyRef, LimitsRef: limitsRef,
		ExecutionRunIDs: []string{"run-1"}, Candidates: candidates, Store: store,
		Limits: PrimaryEvidenceLimits{MaxCandidates: 100, MaxAggregateSourceBytes: 1 << 20, MaxItems: 10, MaxViewBytes: 1 << 16, MaxItemViewBytes: 1 << 15, MaxContextInputTokens: 1 << 17, ReservedFramingTokens: 1024},
	}
}

func primaryEvidenceTestCandidate(eventID, content string) PrimaryEvidenceCandidate {
	hash := strings.Repeat("a", 64)
	if eventID == "event-right" {
		hash = strings.Repeat("b", 64)
	}
	return PrimaryEvidenceCandidate{
		Source:        PrimaryEvidenceSourceV1{SourceKind: "fact", SourceArtifact: DecisionArtifactRef{ID: "artifact-" + eventID, SHA256: hash, MediaType: "application/json", SizeBytes: uint64(len(content))}, SourceEventID: eventID, ProvenanceAuthority: "runtime_observed"},
		ContentFormat: "json", Content: `"` + content + `"`, Verification: PrimaryEvidenceVerificationV1{State: "not_checked", Scope: "none"}, EpistemicStatus: "reported", RootIdentities: []string{"root-" + eventID},
	}
}
