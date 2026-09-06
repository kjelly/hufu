package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestRCA4V2RunAndResumeUseCanonicalFinalizedState(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	engine := NewDecisionEngine(DecisionServices{Journal: journal, Store: store})
	req := engineRequest(enginePolicy(1))
	req.DecisionID = record.ID
	got, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("v2 Run replay: %v", err)
	}
	if got.ID != record.ID || got.SchemaVersion != DecisionRecordSchemaVersion {
		t.Fatalf("v2 Run replay = %#v", got)
	}
	got, err = engine.Resume(context.Background(), record.ID)
	if err != nil {
		t.Fatalf("v2 Resume replay: %v", err)
	}
	if got.ID != record.ID || got.FinalizationResultRef == nil {
		t.Fatalf("v2 Resume replay = %#v", got)
	}
}

func TestRCA4RejectsPairedFinalizationReferenceSubstitution(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	other := *record.FinalizationResultRef
	other.ID = "different-result"
	other.SHA256 = "different-result"
	mutateDecisionEventPayload(t, journal, agent.EventDecisionFinalized, func(payload *decisionEvent) {
		payload.Record.FinalizationResultRef = &other
	})
	_, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID)
	if err == nil || !strings.Contains(err.Error(), "does not match its finalization result") {
		t.Fatalf("paired reference substitution error = %v", err)
	}
}

func TestRCA4RejectsFinalizationArtifactIntegrityFailures(t *testing.T) {
	tests := []struct {
		name string
		edit func(*FileArtifactStore, ArtifactRef) error
	}{
		{name: "missing metadata", edit: func(store *FileArtifactStore, ref ArtifactRef) error {
			return os.Remove(filepath.Join(store.root, "meta", ref.ID+".json"))
		}},
		{name: "missing bytes", edit: func(store *FileArtifactStore, ref ArtifactRef) error {
			return os.Remove(filepath.Join(store.root, "data", ref.ID))
		}},
		{name: "altered bytes", edit: func(store *FileArtifactStore, ref ArtifactRef) error {
			return os.WriteFile(filepath.Join(store.root, "data", ref.ID), []byte(`{"schema_version":1}`), 0o644)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			if err := tt.edit(store, *record.FinalizationResultRef); err != nil {
				t.Fatal(err)
			}
			if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID); err == nil {
				t.Fatal("corrupt finalization artifact was accepted")
			}
		})
	}
}

func TestRCA4RejectsAlternateValidDecodedFinalizationContent(t *testing.T) {
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
	if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID); err == nil || !strings.Contains(err.Error(), "does not match finalization event") {
		t.Fatalf("alternate decoded content error = %v", err)
	}
}

func TestRCA4RecordSchemaIsIndependentOfOuterEventSchema(t *testing.T) {
	for _, outerSchema := range []int{eventStoreLegacySchemaVersion, eventStoreSchemaVersion} {
		t.Run("schema0-outer-"+string(rune('0'+outerSchema)), func(t *testing.T) {
			journal := &memoryJournal{}
			record := DecisionRecord{SchemaVersion: 0, ID: "schema-zero", FinalOption: "wait"}
			raw, err := json.Marshal(decisionEvent{DecisionID: record.ID, Record: &record})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Append(context.Background(), RunEvent{SchemaVersion: outerSchema, Type: agent.EventDecisionFinalized, Payload: raw}); err != nil {
				t.Fatal(err)
			}
			if _, err := NewDecisionEngine(DecisionServices{Journal: journal}).Resume(context.Background(), record.ID); err == nil {
				t.Fatal("schema-zero record was accepted")
			}
		})
	}
}

func TestRCA4LegacyV1ReadsUnderBothOuterSchemas(t *testing.T) {
	for _, outerSchema := range []int{eventStoreLegacySchemaVersion, eventStoreSchemaVersion} {
		t.Run("v1-outer-"+string(rune('0'+outerSchema)), func(t *testing.T) {
			journal := &memoryJournal{}
			record := DecisionRecord{SchemaVersion: 1, ID: "legacy-schema", FinalOption: "wait"}
			raw, err := json.Marshal(decisionEvent{DecisionID: record.ID, Record: &record})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Append(context.Background(), RunEvent{SchemaVersion: outerSchema, Type: agent.EventDecisionFinalized, Payload: raw}); err != nil {
				t.Fatal(err)
			}
			got, err := NewDecisionEngine(DecisionServices{Journal: journal}).Resume(context.Background(), record.ID)
			if err != nil || got.SchemaVersion != 1 {
				t.Fatalf("v1 replay = %#v, %v", got, err)
			}
		})
	}
}

func TestRCA4V2RemainsV2WithLegacyOuterEvents(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	journal.mu.Lock()
	for n := range journal.events {
		journal.events[n].SchemaVersion = eventStoreLegacySchemaVersion
	}
	journal.mu.Unlock()
	got, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID)
	if err != nil || got.SchemaVersion != DecisionRecordSchemaVersion {
		t.Fatalf("v2 legacy-outer replay = %#v, %v", got, err)
	}
}

func TestRCA4IndexBoundariesValidateAndPreserveStaleState(t *testing.T) {
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
	if entries, err := index.List(); err != nil || len(entries) != 1 {
		t.Fatalf("valid index list = %#v, %v", entries, err)
	}
	if entry, found, err := index.Get(record.ID); err != nil || !found || entry.FinalOption != record.FinalOption {
		t.Fatalf("valid index get = %#v, %t, %v", entry, found, err)
	}
	if pending, err := index.Pending(); err != nil || len(pending) != 1 {
		t.Fatalf("valid index pending = %#v, %v", pending, err)
	}

	if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionInvalidated, decisionEvent{DecisionID: record.ID, Reason: "stale test"}); err != nil {
		t.Fatal(err)
	}
	if err := index.RebuildFromJournal(context.Background(), journal); err != nil {
		t.Fatal(err)
	}
	entries, err := index.List()
	if err != nil || len(entries) != 1 || !entries[0].Stale {
		t.Fatalf("stale valid index = %#v, %v", entries, err)
	}
}

func TestRCA4RebuildIsAtomicOnFinalizationIntegrityFailure(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	index := newIndex(t)
	index.SetEventJournal(journal)
	index.SetArtifactStore(store)
	if err := index.RebuildFromJournal(context.Background(), journal); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.root, "meta", record.FinalizationResultRef.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if err := index.RebuildFromJournal(context.Background(), journal); err == nil {
		t.Fatal("rebuild accepted missing finalization metadata")
	}
	after, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed rebuild replaced the previous index")
	}
}

func TestRCA4IndexRejectsLifecycleProjectionIdentitySubstitution(t *testing.T) {
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
	data, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	var entry DecisionIndexEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatal(err)
	}
	entry.RecordRef.SHA256 = "forged-record-digest"
	entry.RecordDigest = entry.RecordRef.SHA256
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index.Path(), append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"list", "get", "pending"} {
		t.Run(operation, func(t *testing.T) {
			var err error
			switch operation {
			case "list":
				_, err = index.List()
			case "get":
				_, _, err = index.Get(record.ID)
			case "pending":
				_, err = index.Pending()
			}
			if err == nil || !strings.Contains(err.Error(), "canonical record artifact") {
				t.Fatalf("%s error = %v", operation, err)
			}
		})
	}
}

func TestRCA4IndexRejectsRecordDigestAliasConflict(t *testing.T) {
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
	data, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	var entry DecisionIndexEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatal(err)
	}
	entry.RecordDigest = "forged-record-digest"
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index.Path(), append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"list", "get", "pending"} {
		t.Run(operation, func(t *testing.T) {
			var err error
			switch operation {
			case "list":
				_, err = index.List()
			case "get":
				_, _, err = index.Get(record.ID)
			case "pending":
				_, err = index.Pending()
			}
			if err == nil || !strings.Contains(err.Error(), "conflicting record digest fields") {
				t.Fatalf("%s error = %v", operation, err)
			}
		})
	}
}

func TestRCA4AssumptionMutationValidatesFinalizedStateFirst(t *testing.T) {
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
	engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{Store: store, Index: index})
	record, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := index.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("assumption fixture index = %#v, %v", entries, err)
	}
	if record.SchemaVersion != DecisionRecordSchemaVersion || record.FinalizationResultRef == nil {
		t.Fatalf("assumption fixture record = %#v, want valid v2 finalization", record)
	}
	session := NewSession()
	session.Tasks = []*TodoItem{{ID: req.TaskID, Status: TaskDone, DecisionAssumptions: cloneDecisionAssumptions(req.Assumptions)}}
	session.DecisionProjections = append([]DecisionIndexEntry(nil), entries...)
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	if err := recordDecisionIndexTaskProjection(index.Path(), entries[0]); err != nil {
		t.Fatal(err)
	}
	beforeIndex, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := os.ReadFile(filepath.Join(workspace, sessionFile))
	if err != nil {
		t.Fatal(err)
	}
	beforeTaskJournal, err := os.ReadFile(taskJournalPath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.root, "data", record.FinalizationResultRef.ID)); err != nil {
		t.Fatal(err)
	}
	t.Logf("removed result data %s", record.FinalizationResultRef.ID)
	if _, err := index.List(); err == nil {
		t.Fatal("index list accepted missing finalization bytes")
	}
	if _, _, err := index.CheckAssumption(record.ID, "A1", AssumptionSupported, "verified"); err == nil || !strings.Contains(err.Error(), "finalization") {
		t.Fatalf("assumption mutation error = %v", err)
	}
	for _, eventType := range []string{
		agent.EventAssumptionSupported,
		agent.EventAssumptionContradicted,
		agent.EventAssumptionStale,
		agent.EventDecisionInvalidated,
		agent.EventReplanRequested,
	} {
		if got := journal.count(eventType); got != 0 {
			t.Fatalf("failed assumption mutation appended %s events = %d, want zero", eventType, got)
		}
	}
	afterIndex, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	afterSession, err := os.ReadFile(filepath.Join(workspace, sessionFile))
	if err != nil {
		t.Fatal(err)
	}
	afterTaskJournal, err := os.ReadFile(taskJournalPath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterIndex) != string(beforeIndex) {
		t.Fatal("failed assumption mutation appended an index projection")
	}
	if string(afterSession) != string(beforeSession) {
		t.Fatal("failed assumption mutation changed the session projection")
	}
	if string(afterTaskJournal) != string(beforeTaskJournal) {
		t.Fatal("failed assumption mutation changed the task projection")
	}
}

func TestRCA5ValidV2AssumptionMutationSucceeds(t *testing.T) {
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
		t.Fatalf("valid control record = %#v, want valid v2 finalization", record)
	}
	entries, err := index.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("valid control index = %#v, %v", entries, err)
	}
	session := NewSession()
	session.Tasks = []*TodoItem{{ID: req.TaskID, Status: TaskDone, DecisionAssumptions: cloneDecisionAssumptions(req.Assumptions)}}
	session.DecisionProjections = append([]DecisionIndexEntry(nil), entries...)
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	if err := recordDecisionIndexTaskProjection(index.Path(), entries[0]); err != nil {
		t.Fatal(err)
	}
	entry, assumption, err := index.CheckAssumption(record.ID, "A1", AssumptionSupported, "verified")
	if err != nil {
		t.Fatalf("valid v2 assumption mutation = %v", err)
	}
	if assumption.EffectiveStatus() != AssumptionSupported || entry.Assumptions[0].EffectiveStatus() != AssumptionSupported {
		t.Fatalf("valid v2 assumption projection = %#v, %#v", entry, assumption)
	}
	if journal.count(agent.EventAssumptionSupported) != 1 || journal.count(agent.EventAssumptionContradicted) != 0 || journal.count(agent.EventDecisionInvalidated) != 0 || journal.count(agent.EventReplanRequested) != 0 {
		t.Fatalf("valid v2 assumption events = %v", journal.typesOf())
	}
	updatedSession, err := loadSessionQuiet(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if updatedSession == nil || len(updatedSession.DecisionProjections) != 1 || updatedSession.DecisionProjections[0].Assumptions[0].EffectiveStatus() != AssumptionSupported {
		t.Fatalf("valid v2 session projection = %#v", updatedSession)
	}
	taskJournal, err := os.ReadFile(taskJournalPath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(taskJournal), `"op":"decision_assumptions"`) || !strings.Contains(string(taskJournal), `"status":"supported"`) {
		t.Fatalf("valid v2 task projection = %s", taskJournal)
	}
}

func TestRCA4RecordArtifactBytesArePartOfFinalizedIntegrity(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	state, err := projectDecision(context.Background(), journal, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "data", state.FinalizedRecordRef.ID), []byte(`{"id":"altered"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID); err == nil || !strings.Contains(err.Error(), "decision record artifact") {
		t.Fatalf("altered record artifact error = %v", err)
	}
}
