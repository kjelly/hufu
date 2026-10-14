package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/runtimestore"
)

type readOnlyDecisionLineage []RunEvent

func (lineage readOnlyDecisionLineage) Append(context.Context, RunEvent) (RunEvent, error) {
	return RunEvent{}, fmt.Errorf("decision lineage projection is read-only")
}

func (lineage readOnlyDecisionLineage) ReadEvents(ctx context.Context) ([]RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]RunEvent(nil), lineage...), nil
}

// ProjectDecisionEntriesForLineage rebuilds finalized decision addressing
// facts from a caller-selected canonical lineage. It does not consult or
// mutate DecisionIndex; callers may compare the result with that disposable
// projection without treating the index as truth.
func ProjectDecisionEntriesForLineage(ctx context.Context, events []RunEvent) ([]DecisionIndexEntry, error) {
	journal := readOnlyDecisionLineage(events)
	ids := make([]string, 0)
	seen := make(map[string]bool)
	for _, event := range events {
		if event.Type != agent.EventDecisionFinalized {
			continue
		}
		var payload decisionEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("project decision entries: decode finalized event: %w", err)
		}
		if payload.DecisionID == "" || payload.Record == nil {
			return nil, fmt.Errorf("project decision entries: finalized event is incomplete")
		}
		if !seen[payload.DecisionID] {
			ids = append(ids, payload.DecisionID)
			seen[payload.DecisionID] = true
		}
	}
	sort.Strings(ids)
	entries := make([]DecisionIndexEntry, 0, len(ids))
	for _, decisionID := range ids {
		state, err := projectDecision(ctx, journal, decisionID)
		if err != nil {
			return nil, fmt.Errorf("project decision entries: %s: %w", decisionID, err)
		}
		if state.Record == nil {
			return nil, fmt.Errorf("project decision entries: finalized decision %s has no record", decisionID)
		}
		question := state.FinalizationQuestion
		if question == "" {
			question = state.Packet.Question
		}
		entry := IndexEntryFor(*state.Record, question, state.FinalizationForecastRequired, state.FinalizedRecordRef)
		if state.Record.Stale {
			entry.Stale, entry.StaleReason = true, state.Record.StaleReason
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Reading the decision index, and proving what it says.
//
// The index is a projection, so a row is only trustworthy where the durable
// journal agrees with it. These readers validate identity against the journal
// and scope visibility to the active branch, which is why listing is not a
// simple file read. Split out of decision_index.go unchanged.

func (i *DecisionIndex) listFileValidated(ctx context.Context) ([]DecisionIndexEntry, error) {
	entries, err := i.listFile()
	if err != nil {
		return nil, err
	}
	journal := i.journalSnapshot()
	store := i.artifactStoreSnapshot()
	if journal == nil {
		for _, entry := range entries {
			if entry.RecordDigest != "" || entry.FinalizationResultRef != nil {
				return nil, fmt.Errorf("decision index: canonical event journal is unavailable for %s", entry.DecisionID)
			}
		}
		return entries, nil
	}
	for _, entry := range entries {
		state, projectErr := projectDecision(ctx, journal, entry.DecisionID)
		if projectErr != nil {
			return nil, fmt.Errorf("decision index: projecting %s: %w", entry.DecisionID, projectErr)
		}
		if state.Record == nil {
			if entry.RecordDigest != "" || entry.FinalizationResultRef != nil {
				return nil, fmt.Errorf("decision index: %s has no canonical finalized record", entry.DecisionID)
			}
			continue
		}
		if err := validateFinalizedDecisionState(ctx, store, state); err != nil {
			return nil, fmt.Errorf("decision index: validating %s: %w", entry.DecisionID, err)
		}
		if err := validateDecisionIndexIdentity(entry, state); err != nil {
			return nil, fmt.Errorf("decision index: %s identity: %w", entry.DecisionID, err)
		}
	}
	return entries, nil
}

func validateDecisionIndexIdentity(entry DecisionIndexEntry, state decisionState) error {
	if state.Record == nil {
		return fmt.Errorf("canonical record is unavailable")
	}
	if entry.DecisionID != state.DecisionID || (entry.RunID != "" && entry.RunID != state.RunID) || (entry.TaskID != "" && entry.TaskID != state.TaskID) || (entry.EvidenceHash != "" && entry.EvidenceHash != state.Record.EvidenceHash) || (entry.FinalOption != "" && entry.FinalOption != state.Record.FinalOption) {
		return fmt.Errorf("index row does not match canonical record")
	}
	entryRecordRef := entry.EffectiveRecordRef()
	if state.FinalizedRecordRef.ID != "" {
		if state.Record.SchemaVersion == DecisionRecordSchemaVersion {
			if !sameArtifactRef(entryRecordRef, state.FinalizedRecordRef) {
				return fmt.Errorf("index row does not match canonical record artifact")
			}
		} else if entryRecordRef.ID != "" {
			if !sameArtifactRef(entryRecordRef, state.FinalizedRecordRef) {
				return fmt.Errorf("index row does not match canonical record artifact")
			}
		} else if entryRecordRef.SHA256 != state.FinalizedRecordRef.SHA256 || entryRecordRef.Path != state.FinalizedRecordRef.Path {
			return fmt.Errorf("index row does not match canonical record artifact")
		}
	} else if state.Record.SchemaVersion != 1 && (entryRecordRef.SHA256 != "" || entryRecordRef.Path != "") {
		return fmt.Errorf("index row contains an unbound record artifact")
	}
	if entry.RecordRef.ID != "" && entry.RecordDigest != "" && entry.RecordRef.SHA256 != entry.RecordDigest {
		return fmt.Errorf("index row has conflicting record digest compatibility fields")
	}
	if entry.RecordRef.Path != "" && entry.RecordPath != "" && entry.RecordRef.Path != entry.RecordPath {
		return fmt.Errorf("index row has conflicting record path compatibility fields")
	}
	if state.FinalizationResultRef.ID != "" {
		if entry.FinalizationResultRef == nil || !sameArtifactRef(*entry.FinalizationResultRef, state.FinalizationResultRef) {
			return fmt.Errorf("index row does not match canonical finalization-result artifact")
		}
	} else if entry.FinalizationResultRef != nil {
		return fmt.Errorf("index row contains an unbound finalization-result artifact")
	}
	if state.CanonicalRecord != nil {
		canonical := state.CanonicalRecord
		if entry.FinalizationMode != canonical.FinalizationMode || entry.FinalizationIdentity != canonical.FinalizationIdentity || entry.FinalizationReason != canonical.FinalizationReason || entry.FinalizationOutcome != canonical.FinalizationOutcome || entry.FinalizationStale != canonical.FinalizationStale || !sameDecisionStrings(entry.FinalizationWarnings, canonical.FinalizationWarnings) {
			return fmt.Errorf("index row does not match canonical finalization result")
		}
	}
	return nil
}

// listFile projects the stored rows into one row per decision, the latest
// winning. It never creates runtime.sqlite.
func (i *DecisionIndex) listFile() ([]DecisionIndexEntry, error) {
	rows, err := readDecisionIndexRows(i.workspace)
	if err != nil {
		return nil, fmt.Errorf("decision index: reading %s: %w", i.Path(), err)
	}
	latest := map[string]DecisionIndexEntry{}
	order := []string{}
	for _, row := range rows {
		var entry DecisionIndexEntry
		if err := json.Unmarshal(row, &entry); err != nil {
			continue
		}
		if entry.DecisionID == "" {
			continue
		}
		if err := entry.ValidateSchemaVersion(); err != nil {
			return nil, err
		}
		entry, err = normalizeDecisionIndexEntry(entry)
		if err != nil {
			return nil, err
		}
		entry = entry.Redacted()
		if _, seen := latest[entry.DecisionID]; !seen {
			order = append(order, entry.DecisionID)
		}
		latest[entry.DecisionID] = entry
	}

	out := make([]DecisionIndexEntry, 0, len(order))
	for _, id := range order {
		out = append(out, latest[id])
	}
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.Before(out[b].CreatedAt)
		}
		return out[a].DecisionID < out[b].DecisionID
	})
	return out, nil
}

// readDecisionIndexRows returns the stored rows in insertion order.
func readDecisionIndexRows(workspace string) ([][]byte, error) {
	store, err := runtimestore.OpenRead(context.Background(), workspace)
	if errors.Is(err, runtimestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	rows, err := store.DB().QueryContext(context.Background(), `SELECT entry FROM decision_index ORDER BY sequence`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries [][]byte
	for rows.Next() {
		var entry []byte
		if err := rows.Scan(&entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func branchScopedJournal(journal decisionJournal) (*branchScopedDecisionJournal, bool) {
	scoped, ok := journal.(*branchScopedDecisionJournal)
	return scoped, ok && scoped != nil
}

// listVisibleFromJournal rebuilds the branch-local decision projection from
// finalized canonical events. The global index is consulted only for
// resolution metadata after a visible record has been established; it never
// supplies visibility or decision content.
func (i *DecisionIndex) listVisibleFromJournal(journal *branchScopedDecisionJournal) ([]DecisionIndexEntry, error) {
	if journal == nil {
		return nil, fmt.Errorf("decision index: branch-scoped journal is unavailable")
	}
	events, err := journal.ReadEvents(context.Background())
	if err != nil {
		return nil, fmt.Errorf("decision index: reading branch events: %w", err)
	}
	global, err := i.listFile()
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0)
	seen := make(map[string]bool)
	for _, event := range events {
		if event.Type != agent.EventDecisionFinalized {
			continue
		}
		if len(event.Payload) == 0 {
			return nil, fmt.Errorf("decision index: finalized event has empty payload")
		}
		var payload decisionEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decision index: decode finalized event: %w", err)
		}
		if payload.DecisionID == "" || payload.Record == nil {
			return nil, fmt.Errorf("decision index: finalized event is incomplete")
		}
		if !seen[payload.DecisionID] {
			ids = append(ids, payload.DecisionID)
			seen[payload.DecisionID] = true
		}
	}

	entries := make([]DecisionIndexEntry, 0, len(ids))
	for _, decisionID := range ids {
		state, err := projectDecision(context.Background(), journal, decisionID)
		if err != nil {
			return nil, fmt.Errorf("decision index: projecting %s: %w", decisionID, err)
		}
		if state.Record == nil {
			return nil, fmt.Errorf("decision index: finalized decision %s has no record", decisionID)
		}
		question := state.FinalizationQuestion
		if question == "" {
			question = state.Packet.Question
		}
		if err := validateFinalizedDecisionState(context.Background(), i.artifactStoreSnapshot(), state); err != nil {
			return nil, fmt.Errorf("decision index: validating %s: %w", decisionID, err)
		}
		entry := IndexEntryFor(*state.Record, question, state.FinalizationForecastRequired, state.FinalizedRecordRef)
		if state.Record.Stale {
			entry.Stale, entry.StaleReason = true, state.Record.StaleReason
		}
		if outcome, ok := matchingDecisionOutcome(entry, global); ok {
			entry.Outcome = outcome
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(a, b int) bool {
		if !entries[a].CreatedAt.Equal(entries[b].CreatedAt) {
			return entries[a].CreatedAt.Before(entries[b].CreatedAt)
		}
		return entries[a].DecisionID < entries[b].DecisionID
	})
	return entries, nil
}

func matchingDecisionOutcome(visible DecisionIndexEntry, global []DecisionIndexEntry) (*DecisionOutcomeRecord, bool) {
	for _, candidate := range global {
		if candidate.DecisionID != visible.DecisionID {
			continue
		}
		identityMatched := false
		if visible.RecordDigest != "" {
			if candidate.RecordDigest == "" || visible.RecordDigest != candidate.RecordDigest {
				continue
			}
			identityMatched = true
		} else if candidate.RecordDigest != "" {
			continue
		}
		if visible.RunID != "" && candidate.RunID != "" {
			if visible.RunID != candidate.RunID {
				continue
			}
			identityMatched = true
		}
		if visible.TaskID != "" && candidate.TaskID != "" {
			if visible.TaskID != candidate.TaskID {
				continue
			}
			identityMatched = true
		}
		if !identityMatched || candidate.Outcome == nil {
			continue
		}
		outcome := *candidate.Outcome
		return &outcome, true
	}
	return nil, false
}
