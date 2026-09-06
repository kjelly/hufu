package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func rca15ArtifactRefMutations() []struct {
	name   string
	mutate func(*ArtifactRef)
} {
	return []struct {
		name   string
		mutate func(*ArtifactRef)
	}{
		{name: "id", mutate: func(ref *ArtifactRef) { ref.ID = "forged-id" }},
		{name: "kind", mutate: func(ref *ArtifactRef) { ref.Kind = "forged-kind" }},
		{name: "role", mutate: func(ref *ArtifactRef) { ref.Role = "forged-role" }},
		{name: "path", mutate: func(ref *ArtifactRef) { ref.Path = "forged/path" }},
		{name: "description", mutate: func(ref *ArtifactRef) { ref.Description = "forged description" }},
		{name: "type", mutate: func(ref *ArtifactRef) { ref.Type = "forged-type" }},
		{name: "sha256", mutate: func(ref *ArtifactRef) { ref.SHA256 = "forged-digest" }},
		{name: "bytes", mutate: func(ref *ArtifactRef) { ref.Bytes++ }},
		{name: "byte_size", mutate: func(ref *ArtifactRef) { ref.ByteSize++ }},
		{name: "media_type", mutate: func(ref *ArtifactRef) { ref.MediaType = "forged/media" }},
		{name: "run_id", mutate: func(ref *ArtifactRef) { ref.RunID = "forged-run" }},
		{name: "task_id", mutate: func(ref *ArtifactRef) { ref.TaskID = "forged-task" }},
		{name: "attempt", mutate: func(ref *ArtifactRef) { ref.Attempt++ }},
		{name: "agent", mutate: func(ref *ArtifactRef) { ref.Agent = "forged-agent" }},
		{name: "provider", mutate: func(ref *ArtifactRef) { ref.Provider = "forged-provider" }},
		{name: "tool_call_id", mutate: func(ref *ArtifactRef) { ref.ToolCallID = "forged-tool-call" }},
		{name: "created_at", mutate: func(ref *ArtifactRef) { ref.CreatedAt = ref.CreatedAt.Add(time.Second) }},
	}
}

func rca15FullyPopulatedArtifactRef() ArtifactRef {
	return ArtifactRef{
		ID: "artifact-id", Kind: "artifact-kind", Role: "artifact-role", Path: "artifact/path",
		Description: "artifact description", Type: "artifact-type", SHA256: "artifact-digest",
		Bytes: 11, ByteSize: 11, MediaType: "application/octet-stream", RunID: "run-id",
		TaskID: "task-id", Attempt: 3, Agent: "agent", Provider: "provider", ToolCallID: "tool-call",
		CreatedAt: time.Date(2026, time.January, 2, 3, 4, 5, 6, time.FixedZone("+08", 8*60*60)),
	}
}

func TestRCA15StrictArtifactRefEqualityRejectsEveryZeroedField(t *testing.T) {
	base := rca15FullyPopulatedArtifactRef()
	for _, mutation := range rca15ArtifactRefMutations() {
		t.Run(mutation.name, func(t *testing.T) {
			forged := base
			mutation.mutate(&forged)
			if sameArtifactRef(base, forged) {
				t.Fatalf("accepted forged %s reference: %#v", mutation.name, forged)
			}
		})
	}
	if !sameArtifactRef(base, base) {
		t.Fatal("rejected identical full artifact references")
	}
	instantEquivalent := base
	instantEquivalent.CreatedAt = base.CreatedAt.UTC()
	if !sameArtifactRef(base, instantEquivalent) {
		t.Fatal("rejected equal CreatedAt instants with different locations")
	}
	zeroTimestamp := base
	zeroTimestamp.CreatedAt = time.Time{}
	if sameArtifactRef(base, zeroTimestamp) {
		t.Fatal("accepted zero CreatedAt in place of a non-zero timestamp")
	}
}

func TestRCA15RejectsEveryFinalizationResultReferenceMutationDuringReplay(t *testing.T) {
	for _, mutation := range rca15ArtifactRefMutations() {
		t.Run(mutation.name, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			mutateDecisionEventPayload(t, journal, agent.EventDecisionFinalizationResult, func(payload *decisionEvent) {
				mutation.mutate(&payload.FinalizationResultRef)
			})
			if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID); err == nil {
				t.Fatalf("replay accepted forged finalization-result %s reference", mutation.name)
			}
		})
	}
}

func TestRCA15RejectsEveryEmbeddedFinalizationResultReferenceMutationDuringReplay(t *testing.T) {
	for _, mutation := range rca15ArtifactRefMutations() {
		t.Run(mutation.name, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			mutateDecisionEventPayload(t, journal, agent.EventDecisionFinalized, func(payload *decisionEvent) {
				if payload.Record == nil || payload.Record.FinalizationResultRef == nil {
					t.Fatal("fixture has no embedded finalization-result reference")
				}
				mutation.mutate(payload.Record.FinalizationResultRef)
			})
			if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID); err == nil {
				t.Fatalf("replay accepted forged embedded %s reference", mutation.name)
			}
		})
	}
}

func TestRCA15RejectsCanonicallySparseFinalizationReferences(t *testing.T) {
	for _, location := range []string{"result-event", "record-embedded"} {
		t.Run(location, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			mutateDecisionEventPayload(t, journal, map[string]string{
				"result-event":    agent.EventDecisionFinalizationResult,
				"record-embedded": agent.EventDecisionFinalized,
			}[location], func(payload *decisionEvent) {
				sparse := ArtifactRef{ID: record.FinalizationResultRef.ID, SHA256: record.FinalizationResultRef.SHA256}
				if location == "result-event" {
					payload.FinalizationResultRef = sparse
					return
				}
				payload.Record.FinalizationResultRef = &sparse
			})
			if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID); err == nil {
				t.Fatal("replay accepted a canonically sparse finalization reference")
			}
		})
	}
}

func TestRCA15RejectsAlteredDuplicateFinalizationResultReference(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	journal.mu.Lock()
	var original RunEvent
	for _, event := range journal.events {
		if event.Type == agent.EventDecisionFinalizationResult {
			original = event
			break
		}
	}
	journal.mu.Unlock()
	if original.Type == "" {
		t.Fatal("fixture has no finalization-result event")
	}
	var payload decisionEvent
	if err := json.Unmarshal(original.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.FinalizationResultRef.Description = "altered duplicate"
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), RunEvent{
		Type: agent.EventDecisionFinalizationResult, IdempotencyKey: original.IdempotencyKey,
		RunID: original.RunID, TaskID: original.TaskID, Attempt: original.Attempt, Payload: encoded,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID); err == nil || !strings.Contains(err.Error(), "conflicts with an earlier result") {
		t.Fatalf("altered duplicate error = %v", err)
	}
}

func TestRCA15FinalizationResultIndexIdentityRejectsEveryReferenceMutation(t *testing.T) {
	for _, mutation := range rca15ArtifactRefMutations() {
		t.Run(mutation.name, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			index := newIndex(t)
			index.SetEventJournal(journal)
			index.SetArtifactStore(store)
			state, err := projectDecision(context.Background(), journal, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			entry := IndexEntryFor(*record, "Should we migrate now?", false, state.FinalizedRecordRef)
			if err := index.Append(entry); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(index.Path())
			if err != nil {
				t.Fatal(err)
			}
			if entry.FinalizationResultRef == nil {
				t.Fatal("index fixture has no finalization-result reference")
			}
			mutation.mutate(entry.FinalizationResultRef)
			encoded, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(index.Path(), append(encoded, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := index.List(); err == nil || !strings.Contains(err.Error(), "finalization-result artifact") {
				t.Fatalf("index accepted forged %s reference: %v", mutation.name, err)
			}
			if after, readErr := os.ReadFile(index.Path()); readErr != nil || string(after) == string(data) {
				// The direct forged write is expected; this assertion prevents the
				// validation path from silently rewriting or repairing the row.
				if readErr != nil {
					t.Fatal(readErr)
				}
				t.Fatal("index validation unexpectedly rewrote its forged row")
			}
		})
	}
}

func TestRCA15RebuildAtomicOnEveryFinalizationReferenceMutation(t *testing.T) {
	for _, mutation := range rca15ArtifactRefMutations() {
		t.Run(mutation.name, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			index := newIndex(t)
			index.SetEventJournal(journal)
			index.SetArtifactStore(store)
			state, err := projectDecision(context.Background(), journal, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := index.Append(IndexEntryFor(*record, "Should we migrate now?", false, state.FinalizedRecordRef)); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(index.Path())
			if err != nil {
				t.Fatal(err)
			}
			mutateDecisionEventPayload(t, journal, agent.EventDecisionFinalizationResult, func(payload *decisionEvent) {
				mutation.mutate(&payload.FinalizationResultRef)
			})
			if err := index.RebuildFromJournal(context.Background(), journal); err == nil {
				t.Fatalf("rebuild accepted forged %s reference", mutation.name)
			}
			after, err := os.ReadFile(index.Path())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("failed rebuild replaced the previous index")
			}
		})
	}
}

func TestRCA15AlternateArtifactStoreRequiresFullResolvedReference(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	fullStore := delegatingArtifactStore{inner: store}
	if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: fullStore}).Resume(context.Background(), record.ID); err != nil {
		t.Fatalf("alternate full artifact store replay: %v", err)
	}
	sparseStore := delegatingArtifactStore{inner: store, sparseResolve: true}
	if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: sparseStore}).Resume(context.Background(), record.ID); err == nil {
		t.Fatal("replay accepted alternate store that resolved a sparse reference")
	}
}

func TestRCA15V2PositiveAcrossExplicitOuterEventSchemas(t *testing.T) {
	for _, outerSchema := range []int{eventStoreLegacySchemaVersion, eventStoreSchemaVersion} {
		t.Run(fmt.Sprintf("outer-v%d", outerSchema), func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			journal.mu.Lock()
			for idx := range journal.events {
				journal.events[idx].SchemaVersion = outerSchema
			}
			journal.mu.Unlock()
			got, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID)
			if err != nil {
				t.Fatalf("v2 replay with outer schema %d: %v", outerSchema, err)
			}
			if got.SchemaVersion != DecisionRecordSchemaVersion || got.FinalizationResultRef == nil {
				t.Fatalf("v2 replay with outer schema %d = %#v", outerSchema, got)
			}
		})
	}
}

func TestRCA15ExplicitV1RecordWithZeroReferenceRemainsReadable(t *testing.T) {
	for _, outerSchema := range []int{eventStoreLegacySchemaVersion, eventStoreSchemaVersion} {
		t.Run(fmt.Sprintf("outer-v%d", outerSchema), func(t *testing.T) {
			journal := &memoryJournal{}
			record := DecisionRecord{SchemaVersion: 1, ID: "legacy-zero-ref", FinalOption: "wait"}
			payload, err := json.Marshal(decisionEvent{DecisionID: record.ID, Record: &record, RecordRef: ArtifactRef{}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Append(context.Background(), RunEvent{SchemaVersion: outerSchema, Type: agent.EventDecisionFinalized, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			got, err := NewDecisionEngine(DecisionServices{Journal: journal}).Resume(context.Background(), record.ID)
			if err != nil || got.SchemaVersion != 1 {
				t.Fatalf("v1 zero-reference replay = %#v, %v", got, err)
			}
		})
	}
}

type delegatingArtifactStore struct {
	inner         *FileArtifactStore
	sparseResolve bool
}

func (s delegatingArtifactStore) Put(ctx context.Context, req PutArtifactRequest) (ArtifactPutResult, error) {
	return s.inner.Put(ctx, req)
}

func (s delegatingArtifactStore) Verify(ctx context.Context, ref ArtifactRef) error {
	return s.inner.Verify(ctx, ref)
}

func (s delegatingArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	return s.inner.Open(ctx, id)
}

func (s delegatingArtifactStore) Resolve(ctx context.Context, ref ArtifactRef) (ArtifactRef, error) {
	resolved, err := s.inner.Resolve(ctx, ref)
	if err != nil || !s.sparseResolve {
		return resolved, err
	}
	return ArtifactRef{ID: resolved.ID, SHA256: resolved.SHA256}, nil
}

func (s delegatingArtifactStore) ListByTask(ctx context.Context, taskID string) ([]ArtifactRef, error) {
	return s.inner.ListByTask(ctx, taskID)
}

var _ ArtifactStore = delegatingArtifactStore{}
