package team

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
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
	index := newIndex(t)
	record := fullDecisionRecord()
	if err := index.Append(IndexEntryFor(record, "Ship?", true, ArtifactRef{SHA256: "rec-digest"})); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Resolve(record.ID, DecisionOutcomeRecord{ResolvedOutcome: OutcomeFailed}); err != nil {
		t.Fatal(err)
	}

	entry, found, err := index.Get(record.ID)
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if entry.FinalOption != record.FinalOption || entry.EvidenceHash != record.EvidenceHash {
		t.Fatalf("resolution changed what was decided: %#v", entry)
	}
	if entry.RecordDigest != "rec-digest" {
		t.Fatalf("RecordDigest = %q, want the record artifact still addressed", entry.RecordDigest)
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

// A finalized decision is listed so it can be found after the run exits. A
// decision blocked by a gate was never made and must not appear.
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
