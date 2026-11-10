package team

import (
	"testing"
	"time"
)

func TestResultCollectionEvidenceIgnoresDisplayTimingsAndManifestOrder(t *testing.T) {
	source := &TodoItem{ID: "source", Status: TaskDone, TypedResult: &TaskResult{Summary: "accepted"}}
	source.ContextManifests = []ContextInjectionManifest{{RequestID: "b", ModelCalled: true}, {RequestID: "a", ModelCalled: true}}
	before, err := collectionDigest(canonicalCollectionSources([]*TodoItem{source}))
	if err != nil {
		t.Fatal(err)
	}
	source.StartedAt, source.EndedAt = time.Unix(1, 0), time.Unix(2, 0)
	source.ModelTime, source.ToolTime = time.Second, time.Millisecond
	source.ContextManifests[0], source.ContextManifests[1] = source.ContextManifests[1], source.ContextManifests[0]
	after, err := collectionDigest(canonicalCollectionSources([]*TodoItem{source}))
	if err != nil || before != after {
		t.Fatal("display timing changed accepted evidence identity", err)
	}
	source.TypedResult.Summary = "changed"
	after, err = collectionDigest(canonicalCollectionSources([]*TodoItem{source}))
	if err != nil || before == after {
		t.Fatal("changed accepted result retained evidence identity", err)
	}
}

func TestResultCollectionRestoresOnlyAuthenticatedLegacyTimings(t *testing.T) {
	for _, kind := range []string{"valid", "missing_snapshot", "wrong_hash", "changed_result", "changed_receipt", "changed_contract", "changed_manifest", "current_revision"} {
		t.Run(kind, func(t *testing.T) {
			original := &TodoItem{ID: "source", Status: TaskDone, StartedAt: time.Unix(1, 0), EndedAt: time.Unix(2, 0), ModelTime: time.Second, TypedResult: &TaskResult{Summary: "accepted"}}
			original.ContextManifests = []ContextInjectionManifest{{RequestID: "b", ModelCalled: true}, {RequestID: "a", ModelCalled: true}}
			digest, err := collectionDigest([]*TodoItem{original})
			if err != nil {
				t.Fatal(err)
			}
			current := canonicalCollectionSources([]*TodoItem{original})[0]
			cp := &ResultCollectionCheckpoint{Attempt: 1, EvidenceSHA256: digest}
			consumer := &TodoItem{ID: "consumer", EvidenceFrom: []string{original.ID}, ExecutionReceipt: &ExecutionReceipt{Attempt: 1, ResultCollection: cp}}
			cached := []*TodoItem{original}
			switch kind {
			case "missing_snapshot":
				cached = nil
			case "wrong_hash":
				cp.EvidenceSHA256 = "invalid"
			case "changed_result":
				current.TypedResult.Summary = "changed"
			case "changed_receipt":
				current.ExecutionReceipt = &ExecutionReceipt{RunID: "different"}
			case "changed_contract":
				current.ContractHash = "different"
			case "current_revision":
				cp.EvidenceRevision = 1
			case "changed_manifest":
				current.ContextManifests[0].ModelCalled = false
			}
			restoreLegacyCollectionEvidence([]*TodoItem{current, consumer}, cached)
			if kind == "valid" {
				restored, err := collectionDigest([]*TodoItem{current})
				if err != nil || restored != digest {
					t.Fatal("original evidence hash was not preserved", err)
				}
			} else if !current.StartedAt.IsZero() || current.ModelTime != 0 {
				t.Fatal("unverified snapshot changed canonical source timing")
			}
		})
	}
}
