package team

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func newIndex(t *testing.T) *DecisionIndex {
	t.Helper()
	index, err := OpenDecisionIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func indexEntry(id string, created time.Time) DecisionIndexEntry {
	return DecisionIndexEntry{
		DecisionID: id, RunID: "run-1", Profile: "standard",
		Question: "Ship?", FinalOption: "ship", Probability: 0.7,
		CreatedAt: created,
	}
}

// A missing index reads as empty rather than as an error: a workspace that has
// never formed a decision is a normal state.
func TestDecisionIndexMissingFileIsEmpty(t *testing.T) {
	index := newIndex(t)
	entries, err := index.List()
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("List = %v, want empty", entries)
	}
	if _, found, err := index.Get("dec-1"); err != nil || found {
		t.Fatalf("Get = %v, %v, want not found", found, err)
	}
}

func TestDecisionIndexAppendAndProject(t *testing.T) {
	index := newIndex(t)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	if err := index.Append(indexEntry("dec-2", base.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := index.Append(indexEntry("dec-1", base)); err != nil {
		t.Fatal(err)
	}

	entries, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("List returned %d entries, want 2", len(entries))
	}
	// Listing is ordered by formation time, not by write order.
	if entries[0].DecisionID != "dec-1" || entries[1].DecisionID != "dec-2" {
		t.Fatalf("order = %s, %s; want dec-1 then dec-2", entries[0].DecisionID, entries[1].DecisionID)
	}
	if entries[0].SchemaVersion != DecisionIndexSchemaVersion || entries[0].IndexedAt.IsZero() {
		t.Fatalf("entry was not stamped: %#v", entries[0])
	}
}

func TestDecisionIndexConcurrentAppendPreservesEveryRow(t *testing.T) {
	index := newIndex(t)
	const count = 128
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			entry := indexEntry("concurrent-decision-"+strings.TrimSpace(time.Duration(i).String()), time.Unix(int64(i), 0).UTC())
			if err := index.Append(entry); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	entries, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != count {
		t.Fatalf("concurrent index entries = %d, want %d", len(entries), count)
	}
	data, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != count {
		t.Fatalf("concurrent index lines = %d, want %d", lines, count)
	}
}

func TestDecisionIndexConcurrentIndependentOpenResolveIsMonotonic(t *testing.T) {
	workspace := t.TempDir()
	first, err := OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Append(indexEntry("monotonic", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	second, err := OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, index := range []*DecisionIndex{first, second} {
		wg.Add(1)
		go func(index *DecisionIndex) {
			defer wg.Done()
			_, err := index.Resolve("monotonic", DecisionOutcomeRecord{ResolvedOutcome: OutcomeSucceeded})
			errs <- err
		}(index)
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "already resolved") {
			t.Fatalf("unexpected concurrent resolve error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent resolve successes = %d, want exactly one", successes)
	}
	entries, err := first.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Resolved() {
		t.Fatalf("monotonic projection = %#v", entries)
	}
}

func TestDecisionIndexCrossProcessResolve(t *testing.T) {
	if os.Getenv("HUFU_DECISION_INDEX_HELPER") == "1" {
		index, err := OpenDecisionIndex(os.Getenv("HUFU_DECISION_INDEX_WORKSPACE"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = index.Resolve("cross-process", DecisionOutcomeRecord{ResolvedOutcome: OutcomeSucceeded})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	workspace := t.TempDir()
	index, err := OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Append(indexEntry("cross-process", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDecisionIndexCrossProcessResolve$")
	cmd.Env = append(os.Environ(), "HUFU_DECISION_INDEX_HELPER=1", "HUFU_DECISION_INDEX_WORKSPACE="+workspace)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process resolver: %v\n%s", err, output)
	}
	entries, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Resolved() {
		t.Fatalf("cross-process projection = %#v", entries)
	}
}

// A later row supersedes an earlier one for the same decision; the file is
// never rewritten in place.
func TestDecisionIndexLatestRowWins(t *testing.T) {
	index := newIndex(t)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	if err := index.Append(indexEntry("dec-1", base)); err != nil {
		t.Fatal(err)
	}
	updated := indexEntry("dec-1", base)
	updated.Stale = true
	updated.StaleReason = "critical assumption contradicted"
	if err := index.Append(updated); err != nil {
		t.Fatal(err)
	}

	entries, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("List returned %d entries, want the projection to collapse to 1", len(entries))
	}
	if !entries[0].Stale {
		t.Fatal("the later row did not supersede the earlier one")
	}

	// Both rows are still on disk: the index is append-only.
	data, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != 2 {
		t.Fatalf("index file has %d lines, want both rows retained", lines)
	}
}

// A crash during append truncates the last line. That must cost the last write,
// never every earlier decision.
func TestDecisionIndexSkipsTruncatedTail(t *testing.T) {
	index := newIndex(t)
	if err := index.Append(indexEntry("dec-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(index.Path(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"decision_id":"dec-2","run_i`); err != nil {
		t.Fatal(err)
	}
	file.Close()

	entries, err := index.List()
	if err != nil {
		t.Fatalf("List = %v, want the intact rows to still be readable", err)
	}
	if len(entries) != 1 || entries[0].DecisionID != "dec-1" {
		t.Fatalf("List = %#v, want only the intact dec-1", entries)
	}
}

func TestDecisionIndexPending(t *testing.T) {
	index := newIndex(t)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if err := index.Append(indexEntry("dec-1", base)); err != nil {
		t.Fatal(err)
	}
	if err := index.Append(indexEntry("dec-2", base.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Resolve("dec-1", DecisionOutcomeRecord{ResolvedOutcome: OutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}

	pending, err := index.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].DecisionID != "dec-2" {
		t.Fatalf("Pending = %#v, want only dec-2", pending)
	}
}

func TestDecisionIndexResolve(t *testing.T) {
	index := newIndex(t)
	entry := indexEntry("dec-1", time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
	if err := index.Append(entry); err != nil {
		t.Fatal(err)
	}

	updated, err := index.Resolve("dec-1", DecisionOutcomeRecord{
		ResolvedOutcome: OutcomeSucceeded, ResolvedBy: "ops",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Resolved() || updated.Outcome.ResolvedOutcome != OutcomeSucceeded {
		t.Fatalf("Resolve = %#v", updated)
	}
	// The decision's own forecast is what the outcome resolves against.
	if updated.Outcome.Forecast != entry.Probability {
		t.Fatalf("Forecast = %v, want the decision's probability %v", updated.Outcome.Forecast, entry.Probability)
	}
	if updated.Outcome.ResolvedAt.IsZero() {
		t.Fatal("ResolvedAt was not stamped")
	}

	// An outcome is recorded once.
	if _, err := index.Resolve("dec-1", DecisionOutcomeRecord{ResolvedOutcome: OutcomeFailed}); err == nil {
		t.Fatal("a second resolution was accepted")
	}
	if _, err := index.Resolve("dec-9", DecisionOutcomeRecord{ResolvedOutcome: OutcomeFailed}); err == nil {
		t.Fatal("an unknown decision was resolved")
	}
	if _, err := index.Resolve("dec-1", DecisionOutcomeRecord{ResolvedOutcome: "went-fine"}); err == nil {
		t.Fatal("an undeclared outcome classification was accepted")
	}
}

// Resolving records a separate outcome; it never edits what was decided.
func TestResolveDoesNotTouchTheDecisionRecord(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	index := newIndex(t)
	index.SetJournal(journal)
	index.SetArtifactStore(store)
	state, err := projectDecision(context.Background(), journal, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.CanonicalRecord == nil || state.FinalizedRecordRef.ID == "" {
		t.Fatalf("fixture state = %#v, want canonical v2 record and artifact reference", state)
	}
	question := state.FinalizationQuestion
	if question == "" {
		question = state.Packet.Question
	}
	canonicalRecord := *state.CanonicalRecord
	finalizationResultRef := derefArtifact(canonicalRecord.FinalizationResultRef)
	if err := index.Append(IndexEntryFor(canonicalRecord, question, state.FinalizationForecastRequired, state.FinalizedRecordRef)); err != nil {
		t.Fatal(err)
	}
	indexBefore, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	recordBytesBefore, err := os.ReadFile(filepath.Join(store.root, "data", state.FinalizedRecordRef.ID))
	if err != nil {
		t.Fatal(err)
	}
	recordMetadataBefore, err := os.ReadFile(filepath.Join(store.root, "meta", state.FinalizedRecordRef.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.Resolve(record.ID, DecisionOutcomeRecord{ResolvedOutcome: OutcomeFailed}); err != nil {
		t.Fatal(err)
	}

	entry, found, err := index.Get(record.ID)
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if entry.Outcome == nil || entry.Outcome.ResolvedOutcome != OutcomeFailed {
		t.Fatalf("resolution outcome was not appended: %#v", entry)
	}
	if entry.FinalOption != canonicalRecord.FinalOption || entry.EvidenceHash != canonicalRecord.EvidenceHash {
		t.Fatalf("resolution changed what was decided: %#v", entry)
	}
	if entry.RecordDigest != state.FinalizedRecordRef.SHA256 || entry.RecordPath != state.FinalizedRecordRef.Path {
		t.Fatalf("record artifact identity changed: %#v", entry)
	}
	recordBytesAfter, err := os.ReadFile(filepath.Join(store.root, "data", state.FinalizedRecordRef.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recordBytesAfter, recordBytesBefore) {
		t.Fatal("resolution changed the canonical decision record bytes")
	}
	recordMetadataAfter, err := os.ReadFile(filepath.Join(store.root, "meta", state.FinalizedRecordRef.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recordMetadataAfter, recordMetadataBefore) {
		t.Fatal("resolution changed the canonical decision record metadata")
	}
	stateAfter, err := projectDecision(context.Background(), journal, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stateAfter.CanonicalRecord == nil || !reflect.DeepEqual(*stateAfter.CanonicalRecord, canonicalRecord) {
		t.Fatalf("resolution changed the canonical final record: before=%#v after=%#v", canonicalRecord, stateAfter.CanonicalRecord)
	}
	if !sameArtifactIdentity(stateAfter.FinalizedRecordRef, state.FinalizedRecordRef) ||
		!sameArtifactIdentity(derefArtifact(stateAfter.CanonicalRecord.FinalizationResultRef), finalizationResultRef) {
		t.Fatalf("resolution changed final artifact identity: before record=%#v result=%#v after record=%#v result=%#v",
			state.FinalizedRecordRef, finalizationResultRef, stateAfter.FinalizedRecordRef, derefArtifact(stateAfter.CanonicalRecord.FinalizationResultRef))
	}
	indexAfter, err := os.ReadFile(index.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(indexAfter, indexBefore) {
		t.Fatal("resolution did not append after the original index row")
	}
	if lines := strings.Count(strings.TrimSpace(string(indexAfter)), "\n") + 1; lines != 2 {
		t.Fatalf("index lines after resolution = %d, want original plus outcome", lines)
	}
}

func TestDecisionIndexResolveRejectsCorruptOrMissingCanonicalV2State(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, store *FileArtifactStore, ref ArtifactRef)
	}{
		{
			name: "corrupt record bytes",
			mutate: func(t *testing.T, store *FileArtifactStore, ref ArtifactRef) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(store.root, "data", ref.ID), []byte(`{"id":"corrupt"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "remove record metadata",
			mutate: func(t *testing.T, store *FileArtifactStore, ref ArtifactRef) {
				t.Helper()
				if err := os.Remove(filepath.Join(store.root, "meta", ref.ID+".json")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			index := newIndex(t)
			index.SetJournal(journal)
			index.SetArtifactStore(store)
			state, err := projectDecision(context.Background(), journal, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if state.CanonicalRecord == nil || state.FinalizedRecordRef.ID == "" {
				t.Fatalf("fixture state = %#v, want canonical v2 record and artifact reference", state)
			}
			question := state.FinalizationQuestion
			if question == "" {
				question = state.Packet.Question
			}
			if err := index.Append(IndexEntryFor(*state.CanonicalRecord, question, state.FinalizationForecastRequired, state.FinalizedRecordRef)); err != nil {
				t.Fatal(err)
			}
			indexBefore, err := os.ReadFile(index.Path())
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(t, store, state.FinalizedRecordRef)

			if _, err := index.Resolve(record.ID, DecisionOutcomeRecord{ResolvedOutcome: OutcomeFailed}); err == nil {
				t.Fatal("resolution accepted invalid canonical v2 state")
			}
			indexAfter, err := os.ReadFile(index.Path())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(indexAfter, indexBefore) {
				t.Fatal("failed resolution appended an outcome")
			}
		})
	}
}

func TestDecisionIndexResolveSupportsSchemaV1FinalizedRecord(t *testing.T) {
	journal := &memoryJournal{}
	record := appendFinalizedDecisionForTest(t, journal, "legacy-resolve", "run-legacy", "task-legacy", "wait")
	index := newIndex(t)
	index.SetJournal(journal)
	if err := index.Append(IndexEntryFor(record, "Which branch?", false, ArtifactRef{})); err != nil {
		t.Fatal(err)
	}
	updated, err := index.Resolve(record.ID, DecisionOutcomeRecord{ResolvedOutcome: OutcomeSucceeded})
	if err != nil {
		t.Fatalf("schema-v1 resolution: %v", err)
	}
	if updated.Outcome == nil || updated.Outcome.ResolvedOutcome != OutcomeSucceeded {
		t.Fatalf("schema-v1 resolution = %#v", updated)
	}
}

func TestIndexEntryFor(t *testing.T) {
	record := fullDecisionRecord()
	record.FalsificationConditions = []string{"ingest lag exceeds 5m"}
	entry := IndexEntryFor(record, "Ship?", true, ArtifactRef{SHA256: "digest", Path: "decisions/dec-1.json"})

	if entry.DecisionID != record.ID || entry.RunID != record.RunID || entry.Profile != record.Profile {
		t.Fatalf("entry = %#v", entry)
	}
	if !entry.ForecastRequired || len(entry.FalsificationConditions) != 1 {
		t.Fatalf("forecast metadata lost: %#v", entry)
	}
	if entry.RecordDigest != "digest" || entry.RecordPath != "decisions/dec-1.json" {
		t.Fatalf("record artifact not addressed: %#v", entry)
	}
}
func TestEngineListsFinalizedDecisions(t *testing.T) {
	index := newIndex(t)
	journal := &memoryJournal{}
	engine := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{Index: index})

	req := engineRequest(enginePolicy(2))
	req.DecisionID = "dec-listed"
	if _, err := engine.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	entries, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].DecisionID != "dec-listed" {
		t.Fatalf("index = %#v, want the finalized decision listed", entries)
	}
	if entries[0].Question != req.Question || entries[0].FinalOption == "" {
		t.Fatalf("entry lost decision context: %#v", entries[0])
	}

	blocked := engineRequest(enginePolicy(2))
	blocked.DecisionID = "dec-blocked"
	blocked.Policy.OutsideView.Required = true
	if _, err := engine.Run(context.Background(), blocked); err == nil {
		t.Fatal("the outside-view gate did not block")
	}
	entries, err = index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("index = %#v, want a blocked decision to stay unlisted", entries)
	}
}

func TestDecisionIndexPathIsWorkspaceScoped(t *testing.T) {
	workspace := t.TempDir()
	if got := DecisionIndexPath(workspace); !strings.HasSuffix(got, "logs/decisions/index.jsonl") {
		t.Fatalf("DecisionIndexPath = %q", got)
	}
	if _, err := OpenDecisionIndex("  "); err == nil {
		t.Fatal("an empty workspace was accepted")
	}
}

func appendFinalizedDecisionForTest(t *testing.T, journal decisionJournal, id, runID, taskID, profile string) DecisionRecord {
	t.Helper()
	record := DecisionRecord{SchemaVersion: 1, ID: id, RunID: runID, TaskID: taskID, Profile: profile, FinalOption: profile}
	if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionFinalized, decisionEvent{
		DecisionID: id, RunID: runID, TaskID: taskID, Attempt: 1, Profile: profile,
		Record: &record, Question: "Which branch?",
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestBranchScopedDecisionIndexProjectsVisibleFinalizedRecordsOnly(t *testing.T) {
	workspace := t.TempDir()
	index, err := OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	left, err := tree.CreateBranch("left", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	right, err := tree.CreateBranch("right", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	raw := &memoryJournal{}
	tree.ActiveBranch = left.ID
	leftJournal, err := newBranchScopedDecisionJournal(raw, tree)
	if err != nil {
		t.Fatal(err)
	}
	leftRecord := appendFinalizedDecisionForTest(t, leftJournal, "shared", "run-left", "task-left", "left")
	leftOnly := appendFinalizedDecisionForTest(t, leftJournal, "left-only", "run-left", "task-left-2", "left-only")
	tree.ActiveBranch = right.ID
	rightJournal, err := newBranchScopedDecisionJournal(raw, tree)
	if err != nil {
		t.Fatal(err)
	}
	rightRecord := appendFinalizedDecisionForTest(t, rightJournal, "shared", "run-right", "task-right", "right")
	if err := index.Append(IndexEntryFor(leftRecord, "Which branch?", false, ArtifactRef{})); err != nil {
		t.Fatal(err)
	}
	leftOutcome := DecisionOutcomeRecord{DecisionID: leftOnly.ID, ResolvedOutcome: OutcomeSucceeded}
	if err := ValidateOutcome(&leftOutcome); err != nil {
		t.Fatal(err)
	}
	leftOnlyEntry := IndexEntryFor(leftOnly, "Which branch?", false, ArtifactRef{})
	leftOnlyEntry.Outcome = &leftOutcome
	if err := index.Append(leftOnlyEntry); err != nil {
		t.Fatal(err)
	}
	rightEntry := IndexEntryFor(rightRecord, "Which branch?", false, ArtifactRef{})
	rightOutcome := DecisionOutcomeRecord{DecisionID: rightRecord.ID, ResolvedOutcome: OutcomeFailed}
	if err := ValidateOutcome(&rightOutcome); err != nil {
		t.Fatal(err)
	}
	rightEntry.Outcome = &rightOutcome
	if err := index.Append(rightEntry); err != nil {
		t.Fatal(err)
	}
	orphan := indexEntry("orphan", time.Now().UTC())
	if err := index.Append(orphan); err != nil {
		t.Fatal(err)
	}

	tree.ActiveBranch = left.ID
	index.SetEventJournal(leftJournal)
	leftEntries, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(leftEntries) != 2 {
		t.Fatalf("left entries = %#v, want the two visible finalized records", leftEntries)
	}
	var shared, leftOnlyEntryGot DecisionIndexEntry
	for _, entry := range leftEntries {
		switch entry.DecisionID {
		case "shared":
			shared = entry
		case "left-only":
			leftOnlyEntryGot = entry
		case "orphan":
			t.Fatal("bound index exposed an index-only orphan")
		}
	}
	if shared.Profile != "left" || shared.Outcome != nil {
		t.Fatalf("left shared entry = %#v, want left record without right outcome", shared)
	}
	if leftOnlyEntryGot.Outcome == nil || leftOnlyEntryGot.Outcome.ResolvedOutcome != OutcomeSucceeded {
		t.Fatalf("left-only outcome = %#v, want matching legacy metadata", leftOnlyEntryGot.Outcome)
	}

	tree.ActiveBranch = right.ID
	index.SetEventJournal(rightJournal)
	rightEntries, err := index.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rightEntries) != 1 || rightEntries[0].DecisionID != "shared" || rightEntries[0].Profile != "right" ||
		rightEntries[0].Outcome == nil || rightEntries[0].Outcome.ResolvedOutcome != OutcomeFailed {
		t.Fatalf("right entries = %#v, want right divergent record and outcome", rightEntries)
	}
}

func TestCoordinatorDecisionIndexEntriesUseBoundBranchProjection(t *testing.T) {
	workspace := t.TempDir()
	if _, err := OpenDecisionIndex(workspace); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	branch, err := tree.CreateBranch("feature", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = branch.ID
	journal, err := newBranchScopedDecisionJournal(&memoryJournal{}, tree)
	if err != nil {
		t.Fatal(err)
	}
	appendFinalizedDecisionForTest(t, journal, "visible", "run-feature", "task-feature", "feature")
	c := &Coordinator{eventJournal: journal, session: &TeamSession{Workspace: workspace}}
	entries, err := c.DecisionIndexEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].DecisionID != "visible" {
		t.Fatalf("coordinator entries = %#v, want visible branch record only", entries)
	}
}
