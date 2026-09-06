package team

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
	"golang.org/x/sys/unix"
)

// The cross-run decision index
// (docs/hufu-decision-aware-runtime-spec.md §49.2 entry condition).
//
// A decision's own record lives in the run that formed it, and its events live
// in that run's event log. Neither is reachable once the process exits, which
// is why outcome resolution had no entry point. The index is the missing
// addressing layer: one append-only file per workspace, listing every decision
// any run has formed, so `hufu decision resolve` can find one later.
//
// It is a projection, not a source of truth. The DecisionRecord artifact and
// the event log remain canonical; the index only says where to look and what
// has been resolved. It is append-only for the same reason the event log is:
// a crash mid-write costs at most the last line, never the history.

const (
	decisionsDir      = "decisions"
	decisionIndexFile = "index.jsonl"

	// DecisionIndexSchemaVersion versions one index row.
	DecisionIndexSchemaVersion = 2
)

// DecisionIndexEntry is one decision's cross-run row.
type DecisionIndexEntry struct {
	SchemaVersion int    `json:"schema_version"`
	DecisionID    string `json:"decision_id"`
	RunID         string `json:"run_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	Profile       string `json:"profile,omitempty"`
	EvidenceHash  string `json:"evidence_hash,omitempty"`

	Question              string       `json:"question,omitempty"`
	FinalOption           string       `json:"final_option,omitempty"`
	Probability           float64      `json:"probability,omitempty"`
	FinalizationMode      string       `json:"finalization_mode,omitempty"`
	FinalizationIdentity  string       `json:"finalization_identity,omitempty"`
	FinalizationReason    string       `json:"finalization_reason,omitempty"`
	FinalizationResultRef *ArtifactRef `json:"finalization_result_ref,omitempty"`
	FinalizationStale     bool         `json:"finalization_stale,omitempty"`
	FinalizationWarnings  []string     `json:"finalization_warnings,omitempty"`
	FinalizationOutcome   string       `json:"finalization_outcome,omitempty"`

	// ForecastRequired and FalsificationConditions record what would have to be
	// observed for this decision to be judged wrong, so a later resolution has
	// something to resolve against (spec §40).
	ForecastRequired        bool     `json:"forecast_required,omitempty"`
	FalsificationConditions []string `json:"falsification_conditions,omitempty"`

	// RecordRef addresses the durable DecisionRecord artifact. RecordDigest and
	// RecordPath remain readable compatibility projections for schema-v1 rows.
	RecordRef    ArtifactRef `json:"record_ref,omitempty"`
	RecordDigest string      `json:"record_digest,omitempty"`
	RecordPath   string      `json:"record_path,omitempty"`

	Stale       bool      `json:"stale,omitempty"`
	StaleReason string    `json:"stale_reason,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`

	// SourceCount and IndependenceGroupCount carry the evidence independence
	// view forward, so cross-run observability does not have to re-open every
	// run's event log to answer how well-sourced a decision was (§28.2).
	SourceCount            int `json:"source_count,omitempty"`
	IndependenceGroupCount int `json:"independence_group_count,omitempty"`

	// Assumptions carries the decision's typed assumptions and their current
	// status, so an operator can check one after the run that formed the
	// decision has exited (spec §18.1 source 3).
	Assumptions []DecisionAssumption `json:"assumptions,omitempty"`

	// AssumptionNotes records why an operator changed an assumption's status,
	// so a transition is explainable and not just observable.
	AssumptionNotes []string `json:"assumption_notes,omitempty"`

	// Outcome is set by resolution. Its presence is what makes a decision
	// resolved; the decision record itself is never edited (spec §35).
	Outcome *DecisionOutcomeRecord `json:"outcome,omitempty"`

	// IndexedAt is when this row was written, used to project the latest row
	// per decision from the append-only file.
	IndexedAt time.Time `json:"indexed_at"`
}

// Resolved reports whether an outcome has been recorded for this decision.
func (e DecisionIndexEntry) Resolved() bool { return e.Outcome != nil }

// ValidateSchemaVersion rejects rows written by a newer index implementation.
// Schema v1 remains readable because its digest/path fields are retained as a
// compatibility projection of the now-full RecordRef.
func (e DecisionIndexEntry) ValidateSchemaVersion() error {
	switch e.SchemaVersion {
	case 1, DecisionIndexSchemaVersion:
		return nil
	default:
		return fmt.Errorf("decision index entry %s has unsupported schema version %d", e.DecisionID, e.SchemaVersion)
	}
}

// EffectiveRecordRef returns the full record reference when present and a
// legacy digest/path compatibility reference otherwise.
func (e DecisionIndexEntry) EffectiveRecordRef() ArtifactRef {
	if strings.TrimSpace(e.RecordRef.ID) != "" || strings.TrimSpace(e.RecordRef.SHA256) != "" || strings.TrimSpace(e.RecordRef.Path) != "" {
		return e.RecordRef
	}
	return ArtifactRef{SHA256: e.RecordDigest, Path: e.RecordPath}
}

// normalizeDecisionIndexEntry keeps the new full reference and the legacy
// digest/path projection in lockstep. It is intentionally lossless for old
// rows and rejects contradictory aliases before a row can be persisted or
// projected.
func normalizeDecisionIndexEntry(entry DecisionIndexEntry) (DecisionIndexEntry, error) {
	if err := entry.ValidateSchemaVersion(); err != nil {
		return DecisionIndexEntry{}, err
	}
	if entry.RecordRef.ID != "" || entry.RecordRef.SHA256 != "" || entry.RecordRef.Path != "" {
		if entry.RecordDigest != "" && entry.RecordRef.SHA256 != entry.RecordDigest {
			return DecisionIndexEntry{}, fmt.Errorf("decision index entry %s has conflicting record digest fields", entry.DecisionID)
		}
		if entry.RecordPath != "" && entry.RecordRef.Path != entry.RecordPath {
			return DecisionIndexEntry{}, fmt.Errorf("decision index entry %s has conflicting record path fields", entry.DecisionID)
		}
		if entry.RecordDigest == "" {
			entry.RecordDigest = entry.RecordRef.SHA256
		}
		if entry.RecordPath == "" {
			entry.RecordPath = entry.RecordRef.Path
		}
		return entry, nil
	}
	if entry.RecordDigest != "" || entry.RecordPath != "" {
		entry.RecordRef = ArtifactRef{SHA256: entry.RecordDigest, Path: entry.RecordPath}
	}
	return entry, nil
}

// DecisionIndex is an append-only index file scoped to one workspace.
type DecisionIndex struct {
	path          string
	lockPath      string
	journal       decisionJournal
	artifactStore ArtifactStore
	mu            *sync.Mutex
	journalMu     sync.RWMutex
}

var decisionIndexProcessLocks sync.Map // map[string]*sync.Mutex, keyed by stable lock path

func decisionIndexLockFor(path string) *sync.Mutex {
	if lock, ok := decisionIndexProcessLocks.Load(path); ok {
		return lock.(*sync.Mutex)
	}
	created := &sync.Mutex{}
	actual, _ := decisionIndexProcessLocks.LoadOrStore(path, created)
	return actual.(*sync.Mutex)
}

func (i *DecisionIndex) withExclusiveLock(fn func() error) error {
	if i == nil || i.mu == nil || strings.TrimSpace(i.lockPath) == "" {
		return fmt.Errorf("decision index: workspace lock is unavailable")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	lockFile, err := os.OpenFile(i.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("decision index: opening workspace lock %s: %w", i.lockPath, err)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return fmt.Errorf("decision index: acquiring workspace lock (fail closed): %w", err)
	}
	fnErr := fn()
	unlockErr := unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	closeErr := lockFile.Close()
	if fnErr != nil {
		return fnErr
	}
	if unlockErr != nil {
		return fmt.Errorf("decision index: releasing workspace lock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("decision index: closing workspace lock: %w", closeErr)
	}
	return nil
}

// RebuildFromJournal reconstructs the derived index from finalized decision
// events and their later lifecycle events. Existing outcomes are retained as
// a separate projection because resolution is intentionally not part of the
// decision record.
func (i *DecisionIndex) RebuildFromJournal(ctx context.Context, journal decisionJournal) error {
	if i == nil || journal == nil {
		return fmt.Errorf("decision index rebuild: canonical event journal is unavailable")
	}
	return i.withExclusiveLock(func() error {
		if _, branchScoped := branchScopedJournal(journal); branchScoped {
			return fmt.Errorf("decision index rebuild: branch-scoped journal is read-only")
		}
		previous, err := i.listFile()
		if err != nil {
			return err
		}
		outcomes := make(map[string]*DecisionOutcomeRecord, len(previous))
		for _, entry := range previous {
			if entry.Outcome != nil {
				copyOutcome := *entry.Outcome
				outcomes[entry.DecisionID] = &copyOutcome
			}
		}
		events, err := journal.ReadEvents(ctx)
		if err != nil {
			return fmt.Errorf("decision index rebuild: reading events: %w", err)
		}
		ids := map[string]struct{}{}
		for _, event := range events {
			if event.Type != agent.EventDecisionFinalized {
				continue
			}
			if len(event.Payload) == 0 {
				return fmt.Errorf("decision index rebuild: finalized event has empty payload")
			}
			var payload decisionEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decision index rebuild: decode finalized event: %w", err)
			}
			if payload.Record == nil || payload.DecisionID == "" {
				return fmt.Errorf("decision index rebuild: finalized event is incomplete")
			}
			ids[payload.DecisionID] = struct{}{}
		}
		var rebuilt []DecisionIndexEntry
		for id := range ids {
			state, projectErr := projectDecision(ctx, journal, id)
			if projectErr != nil {
				return fmt.Errorf("decision index rebuild: projecting %s: %w", id, projectErr)
			}
			if state.Record == nil {
				return fmt.Errorf("decision index rebuild: finalized decision %s has no record", id)
			}
			if err := validateFinalizedDecisionState(ctx, i.artifactStoreSnapshot(), state); err != nil {
				return fmt.Errorf("decision index rebuild: validating %s: %w", id, err)
			}
			question := state.FinalizationQuestion
			if question == "" {
				question = state.Packet.Question
			}
			entry := IndexEntryFor(*state.Record, question, state.FinalizationForecastRequired, state.FinalizedRecordRef)
			if state.Record.Stale {
				entry.Stale, entry.StaleReason = true, state.Record.StaleReason
			}
			entry.Outcome = outcomes[id]
			rebuilt = append(rebuilt, entry)
		}
		sort.Slice(rebuilt, func(a, b int) bool { return rebuilt[a].DecisionID < rebuilt[b].DecisionID })
		var data []byte
		for _, entry := range rebuilt {
			line, marshalErr := json.Marshal(entry)
			if marshalErr != nil {
				return fmt.Errorf("decision index rebuild: encoding %s: %w", entry.DecisionID, marshalErr)
			}
			data = append(data, line...)
			data = append(data, '\n')
		}
		if err := AtomicWriteFile(i.path, data, 0o644); err != nil {
			return fmt.Errorf("decision index rebuild: %w", err)
		}
		return nil
	})
}

// SetJournal binds the canonical event journal used for lifecycle changes.
// The index remains a derived projection; callers must configure this before
// recording assumption checks.
func (i *DecisionIndex) SetJournal(journal decisionJournal) {
	if i != nil {
		i.journalMu.Lock()
		i.journal = journal
		i.journalMu.Unlock()
	}
}

// SetEventJournal binds an EventJournal without exposing the internal adapter
// used by the decision reducer.
func (i *DecisionIndex) SetEventJournal(journal EventJournal) {
	if i != nil && journal != nil {
		i.journalMu.Lock()
		i.journal = journal
		i.journalMu.Unlock()
	}
}

// SetArtifactStore binds the canonical content-addressed store used to verify
// finalized decision records and finalization results before projection.
func (i *DecisionIndex) SetArtifactStore(store ArtifactStore) {
	if i != nil {
		i.journalMu.Lock()
		i.artifactStore = store
		i.journalMu.Unlock()
	}
}

func (i *DecisionIndex) artifactStoreSnapshot() ArtifactStore {
	if i == nil {
		return nil
	}
	i.journalMu.RLock()
	defer i.journalMu.RUnlock()
	return i.artifactStore
}

// BindDecisionIndexEventStore connects the index projection to the workspace
// canonical event store for operator lifecycle commands.
func BindDecisionIndexEventStore(i *DecisionIndex, store *EventStore) {
	if i != nil && store != nil {
		i.journalMu.Lock()
		i.journal = eventStoreJournal{store: store}
		i.journalMu.Unlock()
	}
}

func (i *DecisionIndex) journalSnapshot() decisionJournal {
	if i == nil {
		return nil
	}
	i.journalMu.RLock()
	defer i.journalMu.RUnlock()
	return i.journal
}

// DecisionIndexPath returns the index file's location for a workspace.
func DecisionIndexPath(workspace string) string {
	return filepath.Join(workspace, logsDir, decisionsDir, decisionIndexFile)
}

// OpenDecisionIndex opens (and creates) the index for a workspace.
func OpenDecisionIndex(workspace string) (*DecisionIndex, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("decision index: empty workspace")
	}
	path := DecisionIndexPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("decision index: creating %s: %w", filepath.Dir(path), err)
	}
	lockPath := filepath.Join(filepath.Dir(path), ".lock")
	index := &DecisionIndex{path: path, lockPath: lockPath, mu: decisionIndexLockFor(lockPath)}
	if err := index.withExclusiveLock(func() error { return nil }); err != nil {
		return nil, err
	}
	return index, nil
}

// Path returns the index file path.
func (i *DecisionIndex) Path() string { return i.path }

// Append writes one row. Later rows for the same decision supersede earlier
// ones on read, so an update never rewrites history in place.
func (i *DecisionIndex) Append(entry DecisionIndexEntry) error {
	if i == nil {
		return fmt.Errorf("decision index is unavailable")
	}
	if strings.TrimSpace(entry.DecisionID) == "" {
		return fmt.Errorf("decision index: entry requires a decision id")
	}
	if entry.SchemaVersion == 0 {
		entry.SchemaVersion = DecisionIndexSchemaVersion
	}
	var err error
	if entry, err = normalizeDecisionIndexEntry(entry); err != nil {
		return err
	}
	if entry.IndexedAt.IsZero() {
		entry.IndexedAt = time.Now().UTC()
	}
	entry = entry.Redacted()

	return i.withExclusiveLock(func() error { return i.appendEntryUnlocked(entry) })
}

func (i *DecisionIndex) appendEntryUnlocked(entry DecisionIndexEntry) error {
	entry = entry.Redacted()
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("decision index: encoding entry: %w", err)
	}
	file, err := os.OpenFile(i.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("decision index: opening %s: %w", i.path, err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("decision index: appending to %s: %w", i.path, err)
	}
	// The index is the only way to find a decision after the process exits;
	// losing the tail of it to a page cache is not acceptable, and a Close
	// error on a write path can still mean the bytes never landed.
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("decision index: syncing %s: %w", i.path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("decision index: closing %s: %w", i.path, err)
	}
	return nil
}

// List projects the file into one row per decision, latest write winning,
// sorted by creation time and then decision ID for a stable listing.
//
// A truncated final line is skipped rather than failing the whole read: a
// crash during append must not make every earlier decision unreachable.
func (i *DecisionIndex) List() ([]DecisionIndexEntry, error) {
	if i == nil {
		return nil, fmt.Errorf("decision index is unavailable")
	}
	var entries []DecisionIndexEntry
	err := i.withExclusiveLock(func() error {
		var err error
		if journal, branchScoped := branchScopedJournal(i.journalSnapshot()); branchScoped {
			entries, err = i.listVisibleFromJournal(journal)
		} else {
			entries, err = i.listFileValidated(context.Background())
		}
		return err
	})
	return entries, err
}

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
func (i *DecisionIndex) listFile() ([]DecisionIndexEntry, error) {
	file, err := os.Open(i.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("decision index: opening %s: %w", i.path, err)
	}
	defer func() { _ = file.Close() }()

	latest := map[string]DecisionIndexEntry{}
	order := []string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry DecisionIndexEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
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
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("decision index: reading %s: %w", i.path, err)
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

// Get returns one decision's current row.
func (i *DecisionIndex) Get(decisionID string) (DecisionIndexEntry, bool, error) {
	entries, err := i.List()
	if err != nil {
		return DecisionIndexEntry{}, false, err
	}
	for _, entry := range entries {
		if entry.DecisionID == decisionID {
			return entry, true, nil
		}
	}
	return DecisionIndexEntry{}, false, nil
}

// Pending returns the decisions that have no recorded outcome yet.
func (i *DecisionIndex) Pending() ([]DecisionIndexEntry, error) {
	entries, err := i.List()
	if err != nil {
		return nil, err
	}
	var out []DecisionIndexEntry
	for _, entry := range entries {
		if !entry.Resolved() {
			out = append(out, entry)
		}
	}
	return out, nil
}

// Resolve records an outcome for a decision. It appends a superseding row; the
// DecisionRecord artifact the row points at is never touched (spec §35).
func (i *DecisionIndex) Resolve(decisionID string, outcome DecisionOutcomeRecord) (DecisionIndexEntry, error) {
	var updated DecisionIndexEntry
	err := i.withExclusiveLock(func() error {
		entries, err := i.listFileValidated(context.Background())
		if err != nil {
			return err
		}
		var found bool
		for _, candidate := range entries {
			if candidate.DecisionID == decisionID {
				updated, found = candidate, true
				break
			}
		}
		if !found {
			return fmt.Errorf("decision %q is not in the index at %s", decisionID, i.path)
		}
		if updated.Resolved() {
			return fmt.Errorf("decision %q was already resolved as %q at %s",
				decisionID, updated.Outcome.ResolvedOutcome, updated.Outcome.ResolvedAt.Format(time.RFC3339))
		}
		outcome.DecisionID = decisionID
		if outcome.Forecast == 0 {
			outcome.Forecast = updated.Probability
		}
		if err := ValidateOutcome(&outcome); err != nil {
			return err
		}
		if outcome.ResolvedAt.IsZero() {
			outcome.ResolvedAt = time.Now().UTC()
		}
		updated.Outcome = &outcome
		updated.IndexedAt = time.Time{}
		return i.appendEntryUnlocked(updated)
	})
	if err != nil {
		return updated, err
	}
	return updated, nil
}

// IndexEntryFor builds an index row from a finalized decision record.
func IndexEntryFor(record DecisionRecord, question string, forecastRequired bool, recordRef ArtifactRef) DecisionIndexEntry {
	entry := DecisionIndexEntry{
		SchemaVersion:           DecisionIndexSchemaVersion,
		DecisionID:              record.ID,
		RunID:                   record.RunID,
		TaskID:                  record.TaskID,
		Profile:                 record.Profile,
		EvidenceHash:            record.EvidenceHash,
		Question:                question,
		FinalOption:             record.FinalOption,
		Probability:             record.Probability,
		FinalizationMode:        record.FinalizationMode,
		FinalizationIdentity:    record.FinalizationIdentity,
		FinalizationReason:      record.FinalizationReason,
		FinalizationStale:       record.FinalizationStale,
		FinalizationWarnings:    append([]string(nil), record.FinalizationWarnings...),
		FinalizationOutcome:     record.FinalizationOutcome,
		ForecastRequired:        forecastRequired,
		FalsificationConditions: record.FalsificationConditions,
		Assumptions:             record.Assumptions,
		SourceCount:             record.SourceCount,
		IndependenceGroupCount:  record.IndependenceGroupCount,
		RecordRef:               recordRef,
		RecordDigest:            recordRef.SHA256,
		RecordPath:              recordRef.Path,
		Stale:                   record.Stale,
		StaleReason:             record.StaleReason,
		CreatedAt:               record.CreatedAt,
	}
	if record.FinalizationResultRef != nil {
		ref := *record.FinalizationResultRef
		entry.FinalizationResultRef = &ref
	}
	return entry.Redacted()
}

// Redacted returns a presentation-safe copy of an index projection. Canonical
// finalization events retain the runtime decision trail; index, JSON, report,
// and TUI consumers must never turn a free-text finalization reason into a
// credential disclosure channel.
func (e DecisionIndexEntry) Redacted() DecisionIndexEntry {
	e.DecisionID = utils.RedactSecrets(e.DecisionID)
	e.RunID = utils.RedactSecrets(e.RunID)
	e.TaskID = utils.RedactSecrets(e.TaskID)
	e.Profile = utils.RedactSecrets(e.Profile)
	e.EvidenceHash = utils.RedactSecrets(e.EvidenceHash)
	e.Question = utils.RedactSecrets(e.Question)
	e.FinalOption = utils.RedactSecrets(e.FinalOption)
	e.FinalizationMode = utils.RedactSecrets(e.FinalizationMode)
	e.FinalizationIdentity = utils.RedactSecrets(e.FinalizationIdentity)
	e.FinalizationReason = utils.RedactSecrets(e.FinalizationReason)
	e.FinalizationWarnings = redactDecisionStrings(e.FinalizationWarnings)
	e.FinalizationOutcome = utils.RedactSecrets(e.FinalizationOutcome)
	if e.FinalizationResultRef != nil {
		ref := redactArtifactRef(*e.FinalizationResultRef)
		e.FinalizationResultRef = &ref
	}
	e.FalsificationConditions = redactDecisionStrings(e.FalsificationConditions)
	e.RecordRef = redactArtifactRef(e.RecordRef)
	e.RecordDigest = utils.RedactSecrets(e.RecordDigest)
	e.RecordPath = utils.RedactSecrets(e.RecordPath)
	e.StaleReason = utils.RedactSecrets(e.StaleReason)
	e.Assumptions = redactDecisionAssumptions(e.Assumptions)
	e.AssumptionNotes = redactDecisionStrings(e.AssumptionNotes)
	if e.Outcome != nil {
		outcome := redactDecisionOutcome(*e.Outcome)
		e.Outcome = &outcome
	}
	return e
}

// RedactedDecisionIndexEntries makes a presentation-safe copy of a decision
// projection slice without changing its canonical ordering.
func RedactedDecisionIndexEntries(entries []DecisionIndexEntry) []DecisionIndexEntry {
	redacted := make([]DecisionIndexEntry, len(entries))
	for idx, entry := range entries {
		redacted[idx] = entry.Redacted()
	}
	return redacted
}

func redactDecisionStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	redacted := make([]string, len(values))
	for idx, value := range values {
		redacted[idx] = utils.RedactSecrets(value)
	}
	return redacted
}

func redactArtifactRef(ref ArtifactRef) ArtifactRef {
	ref.ID = utils.RedactSecrets(ref.ID)
	ref.Kind = utils.RedactSecrets(ref.Kind)
	ref.Role = utils.RedactSecrets(ref.Role)
	ref.Path = utils.RedactSecrets(ref.Path)
	ref.Description = utils.RedactSecrets(ref.Description)
	ref.Type = utils.RedactSecrets(ref.Type)
	ref.SHA256 = utils.RedactSecrets(ref.SHA256)
	ref.MediaType = utils.RedactSecrets(ref.MediaType)
	ref.RunID = utils.RedactSecrets(ref.RunID)
	ref.TaskID = utils.RedactSecrets(ref.TaskID)
	ref.Agent = utils.RedactSecrets(ref.Agent)
	ref.Provider = utils.RedactSecrets(ref.Provider)
	ref.ToolCallID = utils.RedactSecrets(ref.ToolCallID)
	return ref
}

func redactArtifactRefs(refs []ArtifactRef) []ArtifactRef {
	if len(refs) == 0 {
		return nil
	}
	redacted := make([]ArtifactRef, len(refs))
	for idx, ref := range refs {
		redacted[idx] = redactArtifactRef(ref)
	}
	return redacted
}

func redactDecisionAssumptions(values []DecisionAssumption) []DecisionAssumption {
	if len(values) == 0 {
		return nil
	}
	redacted := make([]DecisionAssumption, len(values))
	for idx, assumption := range values {
		assumption.ID = utils.RedactSecrets(assumption.ID)
		assumption.Statement = utils.RedactSecrets(assumption.Statement)
		assumption.Status = utils.RedactSecrets(assumption.Status)
		assumption.EvidenceRefs = redactArtifactRefs(assumption.EvidenceRefs)
		redacted[idx] = assumption
	}
	return redacted
}

func redactDecisionOutcome(outcome DecisionOutcomeRecord) DecisionOutcomeRecord {
	outcome.DecisionID = utils.RedactSecrets(outcome.DecisionID)
	outcome.ResolvedOutcome = utils.RedactSecrets(outcome.ResolvedOutcome)
	outcome.SuccessCriteria = redactDecisionStrings(outcome.SuccessCriteria)
	outcome.ObservedEvidence = redactArtifactRefs(outcome.ObservedEvidence)
	outcome.Lessons = redactDecisionStrings(outcome.Lessons)
	outcome.Notes = utils.RedactSecrets(outcome.Notes)
	outcome.ResolvedBy = utils.RedactSecrets(outcome.ResolvedBy)
	outcome.UnverifiedEvidence = redactDecisionStrings(outcome.UnverifiedEvidence)
	outcome.VerificationSummary = utils.RedactSecrets(outcome.VerificationSummary)
	return outcome
}

// fileArtifactResolver adapts an ArtifactStore to ArtifactResolver so outcome
// verification can check that cited evidence really exists.
type fileArtifactResolver struct {
	byDigest map[string]ArtifactRef
}

// NewArtifactResolver builds a resolver from a set of known artifacts.
func NewArtifactResolver(artifacts []ArtifactRef) ArtifactResolver {
	byDigest := make(map[string]ArtifactRef, len(artifacts))
	for _, artifact := range artifacts {
		if digest := strings.TrimSpace(artifact.SHA256); digest != "" {
			byDigest[digest] = artifact
		}
	}
	return fileArtifactResolver{byDigest: byDigest}
}

// ResolveDigest implements ArtifactResolver.
func (r fileArtifactResolver) ResolveDigest(sha256 string) (ArtifactRef, bool) {
	artifact, ok := r.byDigest[strings.TrimSpace(sha256)]
	return artifact, ok
}

// WorkspaceArtifacts lists every artifact the workspace's content-addressed
// store holds, so outcome evidence can be checked against real bytes.
func WorkspaceArtifacts(workspace string) ([]ArtifactRef, error) {
	root := filepath.Join(workspace, logsDir, "artifacts", "meta")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("decision index: reading artifact metadata in %s: %w", root, err)
	}

	var artifacts []ArtifactRef
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		var ref ArtifactRef
		if err := json.Unmarshal(data, &ref); err != nil {
			continue
		}
		if strings.TrimSpace(ref.SHA256) != "" {
			artifacts = append(artifacts, ref)
		}
	}
	sort.Slice(artifacts, func(a, b int) bool { return artifacts[a].SHA256 < artifacts[b].SHA256 })
	return artifacts, nil
}

// CheckAssumption records an operator's assumption check, the third status
// source (spec §18.1). It appends a superseding row; the DecisionRecord the row
// points at is never edited.
//
// An operator may only report supported or contradicted, exactly like the other
// two sources: `unknown` is the absence of a check and `stale` is the runtime's
// own conclusion.
func (i *DecisionIndex) CheckAssumption(decisionID, assumptionID, status, note string) (DecisionIndexEntry, DecisionAssumption, error) {
	var entry DecisionIndexEntry
	var assumption DecisionAssumption
	err := i.withExclusiveLock(func() error {
		var err error
		entry, assumption, err = i.checkAssumptionUnlocked(decisionID, assumptionID, status, note)
		return err
	})
	return entry, assumption, err
}

func (i *DecisionIndex) checkAssumptionUnlocked(decisionID, assumptionID, status, note string) (DecisionIndexEntry, DecisionAssumption, error) {
	if strings.TrimSpace(decisionID) == "" || strings.TrimSpace(assumptionID) == "" {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision and assumption IDs cannot be blank")
	}
	entries, err := i.listFileValidated(context.Background())
	if err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	var entry DecisionIndexEntry
	found := false
	for _, candidate := range entries {
		if candidate.DecisionID == decisionID {
			entry, found = candidate, true
			break
		}
	}
	if !found {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision %q is not in the index at %s", decisionID, i.path)
	}
	if entry.Stale {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision %q is stale and cannot accept assumption checks", decisionID)
	}
	if !ValidCheckStatus(status) {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf(
			"%q is not a reportable assumption status (want supported or contradicted)", status)
	}
	if len(entry.Assumptions) == 0 {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision %q declares no assumptions", decisionID)
	}
	if previous := assumptionStatusBefore(entry.Assumptions, assumptionID); previous == status {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("assumption %q already has status %q", assumptionID, status)
	}

	at := time.Now().UTC()
	updated, assumption, err := ApplyAssumptionTransition(entry.Assumptions, AssumptionTransition{
		DecisionID:   decisionID,
		AssumptionID: assumptionID,
		To:           status,
		Source:       AssumptionSourceOperator,
		At:           at,
	})
	if err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	journal := i.journalSnapshot()
	if journal == nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision index: canonical event journal is unavailable")
	}
	transition := AssumptionTransition{DecisionID: decisionID, AssumptionID: assumptionID,
		From: assumptionStatusBefore(entry.Assumptions, assumptionID), To: status,
		Source: AssumptionSourceOperator, At: at}
	if err := appendDecisionEvent(context.Background(), journal, AssumptionStatusEvent(status), decisionEvent{
		DecisionID: decisionID, AssumptionID: transition.AssumptionID, From: transition.From,
		To: transition.To, Source: transition.Source, Note: note, At: transition.At,
		IdempotencyKey: assumptionTransitionEventKey(transition),
		Reason:         fmt.Sprintf("operator assumption check: %s", strings.TrimSpace(note)),
	}); err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	entry.Assumptions = updated
	entry.AssumptionNotes = appendAssumptionNote(entry.AssumptionNotes, assumptionID, status, note)
	if status == AssumptionContradicted && assumption.Critical {
		reason := fmt.Sprintf("%s: critical assumption %s contradicted", ReasonAssumptionInvalidated, assumptionID)
		if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionInvalidated, decisionEvent{
			DecisionID:     decisionID,
			Reason:         reason,
			IdempotencyKey: decisionStageEventKey(decisionID, "invalidated", reason),
		}); err != nil {
			return DecisionIndexEntry{}, DecisionAssumption{}, err
		}
		if err := RequestReplan(context.Background(), journal, decisionID, CheckpointDecision{
			Action: CheckpointReplan, Reason: ReasonAssumptionInvalidated, Detail: reason,
		}); err != nil {
			return DecisionIndexEntry{}, DecisionAssumption{}, err
		}
		entry.Stale = true
		entry.StaleReason = reason
	}
	entry.IndexedAt = time.Time{}
	if err := i.appendEntryUnlocked(entry); err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	if err := persistDecisionIndexSessionProjection(i.path, entry); err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	if err := recordDecisionIndexTaskProjection(i.path, entry); err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	return entry, assumption, nil
}

// persistDecisionIndexSessionProjection keeps the restart-facing session
// projection aligned with operator changes. Missing session.json is normal for
// a workspace addressed before its first coordinator run.
func persistDecisionIndexSessionProjection(indexPath string, entry DecisionIndexEntry) error {
	workspace := filepath.Dir(filepath.Dir(filepath.Dir(indexPath)))
	session, err := loadSessionQuiet(workspace)
	if err != nil {
		return fmt.Errorf("load session for decision projection: %w", err)
	}
	if session == nil {
		return nil
	}
	for n := range session.DecisionProjections {
		if session.DecisionProjections[n].DecisionID == entry.DecisionID {
			session.DecisionProjections[n] = entry
			return SaveSession(workspace, session)
		}
	}
	session.DecisionProjections = append(session.DecisionProjections, entry)
	return SaveSession(workspace, session)
}

func assumptionStatusBefore(assumptions []DecisionAssumption, id string) string {
	for _, assumption := range assumptions {
		if assumption.ID == id {
			return assumption.EffectiveStatus()
		}
	}
	return AssumptionUnknown
}

// CriticalAssumptionContradicted returns the ID of a contradicted critical
// assumption on this decision, or an empty string.
func (e DecisionIndexEntry) CriticalAssumptionContradicted() string {
	return CriticalContradiction(e.Assumptions)
}

// appendAssumptionNote records one operator transition in a readable line.
func appendAssumptionNote(notes []string, assumptionID, status, note string) []string {
	line := fmt.Sprintf("%s: %s -> %s", time.Now().UTC().Format(time.RFC3339), assumptionID, status)
	if trimmed := strings.TrimSpace(note); trimmed != "" {
		line += " (" + trimmed + ")"
	}
	return append(notes, line)
}
