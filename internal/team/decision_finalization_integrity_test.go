package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/runtimestore"
)

// A finalized decision is durable in two places: the journal (a finalization
// result event followed by a finalized event carrying the record and its
// result reference) and the artifact store (result and record bytes with
// their metadata). Replay, index projection and assumption mutation must fail
// closed whenever those sources disagree, and a failed validation must never
// rewrite the state it rejected.

type finalizationRefMutation struct {
	name   string
	mutate func(*ArtifactRef)
}

// finalizationRefMutations forges every ArtifactRef field in turn.
func finalizationRefMutations() []finalizationRefMutation {
	return []finalizationRefMutation{
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

// finalizationSparseRef keeps only the identity fields a lenient reader might
// accept in place of a fully resolved reference.
var finalizationSparseRef = finalizationRefMutation{name: "sparse", mutate: func(ref *ArtifactRef) {
	*ref = ArtifactRef{ID: ref.ID, SHA256: ref.SHA256}
}}

func finalizationFullArtifactRef() ArtifactRef {
	return ArtifactRef{
		ID: "artifact-id", Kind: "artifact-kind", Role: "artifact-role", Path: "artifact/path",
		Description: "artifact description", Type: "artifact-type", SHA256: "artifact-digest",
		Bytes: 11, ByteSize: 11, MediaType: "application/octet-stream", RunID: "run-id",
		TaskID: "task-id", Attempt: 3, Agent: "agent", Provider: "provider", ToolCallID: "tool-call",
		CreatedAt: time.Date(2026, time.January, 2, 3, 4, 5, 6, time.FixedZone("+08", 8*60*60)),
	}
}

// finalizationResume replays a decision through a fresh engine.
func finalizationResume(journal *memoryJournal, store ArtifactStore, id string) (*DecisionRecord, error) {
	return NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), id)
}

// finalizationIndexFixture is a finalized decision projected into an index
// that validates against both the journal and the artifact store.
type finalizationIndexFixture struct {
	journal *memoryJournal
	store   *FileArtifactStore
	record  *DecisionRecord
	index   *DecisionIndex
	entry   DecisionIndexEntry
}

func newFinalizationIndexFixture(t *testing.T) finalizationIndexFixture {
	t.Helper()
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
	return finalizationIndexFixture{journal: journal, store: store, record: record, index: index, entry: entry}
}

func (f finalizationIndexFixture) indexLog(t *testing.T) string {
	t.Helper()
	data, err := readDecisionIndexLog(f.index)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writeEntry replaces the index log with one forged row.
func (f finalizationIndexFixture) writeEntry(t *testing.T, entry DecisionIndexEntry) {
	t.Helper()
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDecisionIndexLog(f.index, append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
}

// assertReadsFail checks that every index read surfaces the validation error.
func (f finalizationIndexFixture) assertReadsFail(t *testing.T, want string) {
	t.Helper()
	_, listErr := f.index.List()
	_, _, getErr := f.index.Get(f.record.ID)
	_, pendingErr := f.index.Pending()
	for name, err := range map[string]error{"list": listErr, "get": getErr, "pending": pendingErr} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s error = %v, want %q", name, err, want)
		}
	}
}

func TestFinalizationRunReplaysCanonicalFinalizedState(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	req := engineRequest(enginePolicy(1))
	req.DecisionID = record.ID
	got, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("v2 Run replay: %v", err)
	}
	if got.ID != record.ID || got.SchemaVersion != DecisionRecordSchemaVersion || got.FinalizationResultRef == nil {
		t.Fatalf("v2 Run replay = %#v, want the finalized v%d record", got, DecisionRecordSchemaVersion)
	}
}

func TestFinalizationReplayRejectsEveryReferenceMutation(t *testing.T) {
	locations := []struct {
		name      string
		eventType string
		ref       func(*decisionEvent) *ArtifactRef
	}{
		{"result event", agent.EventDecisionFinalizationResult, func(p *decisionEvent) *ArtifactRef { return &p.FinalizationResultRef }},
		{"record embedded", agent.EventDecisionFinalized, func(p *decisionEvent) *ArtifactRef { return p.Record.FinalizationResultRef }},
	}
	for _, location := range locations {
		for _, mutation := range append(finalizationRefMutations(), finalizationSparseRef) {
			t.Run(location.name+"/"+mutation.name, func(t *testing.T) {
				journal, store, record := stage4FinalizedReplayFixture(t)
				mutateDecisionEventPayload(t, journal, location.eventType, func(payload *decisionEvent) {
					ref := location.ref(payload)
					if ref == nil {
						t.Fatal("fixture has no finalization-result reference")
					}
					mutation.mutate(ref)
				})
				if _, err := finalizationResume(journal, store, record.ID); err == nil {
					t.Fatalf("replay accepted forged %s reference", mutation.name)
				}
			})
		}
	}
}

func TestFinalizationReplayRejectsArtifactIntegrityFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(store *FileArtifactStore, result, record ArtifactRef) error
		wantErr string
	}{
		{name: "missing result metadata", corrupt: func(store *FileArtifactStore, result, _ ArtifactRef) error {
			return os.Remove(filepath.Join(store.root, "meta", result.ID+".json"))
		}},
		{name: "missing result bytes", corrupt: func(store *FileArtifactStore, result, _ ArtifactRef) error {
			return os.Remove(filepath.Join(store.root, "data", result.ID))
		}},
		{name: "altered result bytes", corrupt: func(store *FileArtifactStore, result, _ ArtifactRef) error {
			return os.WriteFile(filepath.Join(store.root, "data", result.ID), []byte(`{"schema_version":1}`), 0o644)
		}},
		{name: "altered record bytes", corrupt: func(store *FileArtifactStore, _, record ArtifactRef) error {
			return os.WriteFile(filepath.Join(store.root, "data", record.ID), []byte(`{"id":"altered"}`), 0o644)
		}, wantErr: "decision record artifact"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			state, err := projectDecision(context.Background(), journal, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.corrupt(store, *record.FinalizationResultRef, state.FinalizedRecordRef); err != nil {
				t.Fatal(err)
			}
			_, err = finalizationResume(journal, store, record.ID)
			if err == nil {
				t.Fatal("replay accepted the corrupted artifact store")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("replay error = %v, want failure containing %q", err, tt.wantErr)
			}
		})
	}
}

// A result artifact that decodes to a valid but different result must not be
// accepted just because its reference is internally consistent.
func TestFinalizationReplayRejectsAlternateDecodedResultContent(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	data, err := os.ReadFile(filepath.Join(store.root, "data", record.FinalizationResultRef.ID))
	if err != nil {
		t.Fatal(err)
	}
	var alternateResult DecisionFinalizationResult
	if err := json.Unmarshal(data, &alternateResult); err != nil {
		t.Fatal(err)
	}
	alternateResult.Warnings = []string{"alternate decoded content"}
	alternateResultData, err := json.Marshal(alternateResult)
	if err != nil {
		t.Fatal(err)
	}
	resultPut, err := store.Put(context.Background(), PutArtifactRequest{
		Kind: FinalizationResultKind, Role: FinalizationResultRole,
		Path: "decisions/finalization/alternate.json", MediaType: FinalizationResultMediaType,
		Content: alternateResultData,
	})
	if err != nil {
		t.Fatal(err)
	}
	alternateRecord := *record
	alternateRecord.FinalizationResultRef = &resultPut.ArtifactRef
	recordData, err := json.MarshalIndent(alternateRecord, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	recordPut, err := store.Put(context.Background(), PutArtifactRequest{
		Kind: "decision_record", Role: "decision", Path: "decisions/alternate-record.json",
		MediaType: "application/json", Content: recordData,
	})
	if err != nil {
		t.Fatal(err)
	}
	mutateDecisionEventPayload(t, journal, agent.EventDecisionFinalizationResult, func(payload *decisionEvent) {
		payload.FinalizationResultRef = resultPut.ArtifactRef
	})
	mutateDecisionEventPayload(t, journal, agent.EventDecisionFinalized, func(payload *decisionEvent) {
		payload.Record = &alternateRecord
		payload.RecordRef = recordPut.ArtifactRef
	})
	if _, err := finalizationResume(journal, store, record.ID); err == nil || !strings.Contains(err.Error(), "does not match finalization event") {
		t.Fatalf("alternate decoded content error = %v", err)
	}
}

func TestFinalizationReplayRejectsConflictingDuplicateResultEvent(t *testing.T) {
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
	if _, err := finalizationResume(journal, store, record.ID); err == nil || !strings.Contains(err.Error(), "conflicts with an earlier result") {
		t.Fatalf("altered duplicate error = %v", err)
	}
}

// finalizationDelegatingStore wraps a file store so a test can observe how an
// alternate ArtifactStore implementation's Resolve output is treated.
type finalizationDelegatingStore struct {
	inner         *FileArtifactStore
	sparseResolve bool
}

func (s finalizationDelegatingStore) Put(ctx context.Context, req PutArtifactRequest) (ArtifactPutResult, error) {
	return s.inner.Put(ctx, req)
}

func (s finalizationDelegatingStore) Verify(ctx context.Context, ref ArtifactRef) error {
	return s.inner.Verify(ctx, ref)
}

func (s finalizationDelegatingStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	return s.inner.Open(ctx, id)
}

func (s finalizationDelegatingStore) Resolve(ctx context.Context, ref ArtifactRef) (ArtifactRef, error) {
	resolved, err := s.inner.Resolve(ctx, ref)
	if err != nil || !s.sparseResolve {
		return resolved, err
	}
	return ArtifactRef{ID: resolved.ID, SHA256: resolved.SHA256}, nil
}

func (s finalizationDelegatingStore) ListByTask(ctx context.Context, taskID string) ([]ArtifactRef, error) {
	return s.inner.ListByTask(ctx, taskID)
}

var _ ArtifactStore = finalizationDelegatingStore{}

func TestFinalizationReplayRequiresFullyResolvedReferenceFromAnyStore(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	if _, err := finalizationResume(journal, finalizationDelegatingStore{inner: store}, record.ID); err != nil {
		t.Fatalf("alternate full artifact store replay: %v", err)
	}
	if _, err := finalizationResume(journal, finalizationDelegatingStore{inner: store, sparseResolve: true}, record.ID); err == nil {
		t.Fatal("replay accepted a store that resolved a sparse reference")
	}
}

// The record schema version is discriminated on its own: the outer event
// store schema neither upgrades a schema-zero record nor downgrades a v2 one.
func TestFinalizationReplayIsIndependentOfOuterEventSchema(t *testing.T) {
	for _, outerSchema := range []int{eventStoreLegacySchemaVersion, eventStoreSchemaVersion} {
		t.Run(fmt.Sprintf("outer-v%d", outerSchema), func(t *testing.T) {
			journal := &memoryJournal{}
			zero := DecisionRecord{SchemaVersion: 0, ID: "schema-zero", FinalOption: "wait"}
			raw, err := json.Marshal(decisionEvent{DecisionID: zero.ID, Record: &zero})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Append(context.Background(), RunEvent{SchemaVersion: outerSchema, Type: agent.EventDecisionFinalized, Payload: raw}); err != nil {
				t.Fatal(err)
			}
			if _, err := finalizationResume(journal, nil, zero.ID); err == nil {
				t.Fatal("schema-zero record was accepted")
			}

			journal, store, record := stage4FinalizedReplayFixture(t)
			journal.mu.Lock()
			for i := range journal.events {
				journal.events[i].SchemaVersion = outerSchema
			}
			journal.mu.Unlock()
			got, err := finalizationResume(journal, store, record.ID)
			if err != nil {
				t.Fatalf("v2 replay with outer schema %d: %v", outerSchema, err)
			}
			if got.SchemaVersion != DecisionRecordSchemaVersion || got.FinalizationResultRef == nil {
				t.Fatalf("v2 replay with outer schema %d = %#v", outerSchema, got)
			}
		})
	}
}

func TestSameArtifactRefRejectsEveryFieldMutation(t *testing.T) {
	base := finalizationFullArtifactRef()
	for _, mutation := range append(finalizationRefMutations(), finalizationSparseRef) {
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

func TestFinalizationIndexValidatesEntriesAndKeepsStaleStateOnRebuild(t *testing.T) {
	f := newFinalizationIndexFixture(t)
	if entries, err := f.index.List(); err != nil || len(entries) != 1 {
		t.Fatalf("valid index list = %#v, %v", entries, err)
	}
	if entry, found, err := f.index.Get(f.record.ID); err != nil || !found || entry.FinalOption != f.record.FinalOption {
		t.Fatalf("valid index get = %#v, %t, %v", entry, found, err)
	}
	if pending, err := f.index.Pending(); err != nil || len(pending) != 1 {
		t.Fatalf("valid index pending = %#v, %v", pending, err)
	}

	if err := appendDecisionEvent(context.Background(), f.journal, agent.EventDecisionInvalidated, decisionEvent{DecisionID: f.record.ID, Reason: "stale test"}); err != nil {
		t.Fatal(err)
	}
	if err := f.index.RebuildFromJournal(context.Background(), f.journal); err != nil {
		t.Fatal(err)
	}
	entries, err := f.index.List()
	if err != nil || len(entries) != 1 || !entries[0].Stale {
		t.Fatalf("stale valid index = %#v, %v", entries, err)
	}
}

func TestFinalizationIndexRejectsForgedRecordIdentity(t *testing.T) {
	tests := []struct {
		name    string
		forge   func(entry *DecisionIndexEntry)
		wantErr string
	}{
		{
			name: "record reference digest substitution",
			forge: func(entry *DecisionIndexEntry) {
				entry.RecordRef.SHA256 = "forged-record-digest"
				entry.RecordDigest = entry.RecordRef.SHA256
			},
			wantErr: "canonical record artifact",
		},
		{
			name:    "record digest alias conflict",
			forge:   func(entry *DecisionIndexEntry) { entry.RecordDigest = "forged-record-digest" },
			wantErr: "conflicting record digest fields",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFinalizationIndexFixture(t)
			var entry DecisionIndexEntry
			if err := json.Unmarshal([]byte(strings.TrimSpace(f.indexLog(t))), &entry); err != nil {
				t.Fatal(err)
			}
			tt.forge(&entry)
			f.writeEntry(t, entry)
			f.assertReadsFail(t, tt.wantErr)
		})
	}
}

func TestFinalizationIndexRejectsForgedResultReference(t *testing.T) {
	for _, mutation := range finalizationRefMutations() {
		t.Run(mutation.name, func(t *testing.T) {
			f := newFinalizationIndexFixture(t)
			if f.entry.FinalizationResultRef == nil {
				t.Fatal("index fixture has no finalization-result reference")
			}
			forged := f.entry
			forged.FinalizationResultRef = new(ArtifactRef)
			*forged.FinalizationResultRef = *f.entry.FinalizationResultRef
			mutation.mutate(forged.FinalizationResultRef)
			f.writeEntry(t, forged)
			before := f.indexLog(t)
			if _, err := f.index.List(); err == nil || !strings.Contains(err.Error(), "finalization-result artifact") {
				t.Fatalf("index accepted forged %s reference: %v", mutation.name, err)
			}
			// Validation must report the forged row, never repair or rewrite it.
			if after := f.indexLog(t); after != before {
				t.Fatal("index validation rewrote its forged row")
			}
		})
	}
}

// A rebuild that fails validation, whether the journal or the artifact store
// is at fault, must leave the previous index untouched.
func TestFinalizationIndexRebuildIsAtomic(t *testing.T) {
	type corruption struct {
		name    string
		corrupt func(t *testing.T, f finalizationIndexFixture)
	}
	var tests []corruption
	for _, mutation := range finalizationRefMutations() {
		tests = append(tests, corruption{name: "journal/" + mutation.name, corrupt: func(t *testing.T, f finalizationIndexFixture) {
			mutateDecisionEventPayload(t, f.journal, agent.EventDecisionFinalizationResult, func(payload *decisionEvent) {
				mutation.mutate(&payload.FinalizationResultRef)
			})
		}})
	}
	tests = append(tests, corruption{name: "store/missing result metadata", corrupt: func(t *testing.T, f finalizationIndexFixture) {
		if err := os.Remove(filepath.Join(f.store.root, "meta", f.record.FinalizationResultRef.ID+".json")); err != nil {
			t.Fatal(err)
		}
	}})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFinalizationIndexFixture(t)
			before := f.indexLog(t)
			tt.corrupt(t, f)
			if err := f.index.RebuildFromJournal(context.Background(), f.journal); err == nil {
				t.Fatalf("rebuild accepted %s", tt.name)
			}
			if after := f.indexLog(t); after != before {
				t.Fatal("failed rebuild replaced the previous index")
			}
		})
	}
}

// finalizationAssumptionFixture is a finalized decision with one critical
// assumption, projected into the index, the session and the task journal.
type finalizationAssumptionFixture struct {
	workspace string
	store     *FileArtifactStore
	journal   *memoryJournal
	index     *DecisionIndex
	req       DecisionRequest
	record    *DecisionRecord
}

func newFinalizationAssumptionFixture(t *testing.T) finalizationAssumptionFixture {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	journal := &memoryJournal{}
	index, err := OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	req := engineRequest(enginePolicy(1))
	req.Assumptions = []DecisionAssumption{{ID: "A1", Statement: "traffic stays flat", Critical: true}}
	record, err := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{Store: store, Index: index}).Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if record.SchemaVersion != DecisionRecordSchemaVersion || record.FinalizationResultRef == nil {
		t.Fatalf("assumption fixture record = %#v, want valid v2 finalization", record)
	}
	entries, err := index.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("assumption fixture index = %#v, %v", entries, err)
	}
	session := NewSession()
	session.Tasks = []*TodoItem{{ID: req.TaskID, Status: TaskDone, DecisionAssumptions: cloneDecisionAssumptions(req.Assumptions)}}
	session.DecisionProjections = append([]DecisionIndexEntry(nil), entries...)
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	if err := recordDecisionIndexTaskProjection(index.workspace, entries[0]); err != nil {
		t.Fatal(err)
	}
	return finalizationAssumptionFixture{workspace: workspace, store: store, journal: journal, index: index, req: req, record: record}
}

// projections snapshots the index log, session document and task journal.
func (f finalizationAssumptionFixture) projections(t *testing.T) [3]string {
	t.Helper()
	index, err := readDecisionIndexLog(f.index)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := getRuntimeDocument(t, f.workspace, runtimestore.DocumentSession)
	taskJournal, err := readTaskJournalLog(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	return [3]string{string(index), string(session), string(taskJournal)}
}

func TestAssumptionMutationFailsClosedOnFinalizationIntegrity(t *testing.T) {
	f := newFinalizationAssumptionFixture(t)
	before := f.projections(t)
	if err := os.Remove(filepath.Join(f.store.root, "data", f.record.FinalizationResultRef.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.index.List(); err == nil {
		t.Fatal("index list accepted missing finalization bytes")
	}
	if _, _, err := f.index.CheckAssumption(f.record.ID, "A1", AssumptionSupported, "verified"); err == nil || !strings.Contains(err.Error(), "finalization") {
		t.Fatalf("assumption mutation error = %v", err)
	}
	for _, eventType := range []string{
		agent.EventAssumptionSupported,
		agent.EventAssumptionContradicted,
		agent.EventAssumptionStale,
		agent.EventDecisionInvalidated,
		agent.EventReplanRequested,
	} {
		if got := f.journal.count(eventType); got != 0 {
			t.Fatalf("failed assumption mutation appended %s events = %d, want zero", eventType, got)
		}
	}
	if after := f.projections(t); after != before {
		t.Fatalf("failed assumption mutation changed a projection:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestAssumptionMutationProjectsSupportedStatus(t *testing.T) {
	f := newFinalizationAssumptionFixture(t)
	entry, assumption, err := f.index.CheckAssumption(f.record.ID, "A1", AssumptionSupported, "verified")
	if err != nil {
		t.Fatalf("valid v2 assumption mutation = %v", err)
	}
	if assumption.EffectiveStatus() != AssumptionSupported || entry.Assumptions[0].EffectiveStatus() != AssumptionSupported {
		t.Fatalf("valid v2 assumption projection = %#v, %#v", entry, assumption)
	}
	if f.journal.count(agent.EventAssumptionSupported) != 1 || f.journal.count(agent.EventAssumptionContradicted) != 0 || f.journal.count(agent.EventDecisionInvalidated) != 0 || f.journal.count(agent.EventReplanRequested) != 0 {
		t.Fatalf("valid v2 assumption events = %v", f.journal.typesOf())
	}
	updatedSession, err := loadSessionQuiet(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if updatedSession == nil || len(updatedSession.DecisionProjections) != 1 || updatedSession.DecisionProjections[0].Assumptions[0].EffectiveStatus() != AssumptionSupported {
		t.Fatalf("valid v2 session projection = %#v", updatedSession)
	}
	taskJournal, err := readTaskJournalLog(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(taskJournal), `"op":"decision_assumptions"`) || !strings.Contains(string(taskJournal), `"status":"supported"`) {
		t.Fatalf("valid v2 task projection = %s", taskJournal)
	}
}
