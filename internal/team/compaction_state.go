package team

// Durable coordinator conversation compaction state. This file is the
// canonical owner of compacted coordinator history; session_history.json and
// compaction_history.json remain compatibility projections only.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	conversationCompactionStateFile     = "conversation_compaction_state.json"
	conversationCompactionStateVersion  = 1
	compactionGenerationEventType       = "coordinator_compaction_committed"
	compactionGenerationEventKeyPrefix  = "compaction-generation:"
	compactionCheckpointEventType       = "coordinator_compaction_checkpoint_attested"
	compactionCheckpointEventKeyPrefix  = "compaction-checkpoint:"
	modelContinuationEventType          = "coordinator_model_continuation_admitted"
	compactionCheckpointAttestationMode = "snapshot"
	compactionLegacyAttestationMode     = "legacy_generation"
)

// ConversationCompactionState is versioned so a future schema can reject an
// unknown state instead of silently rebuilding a different conversation.
type ConversationCompactionState struct {
	Version     int                                         `json:"version"`
	Branches    map[string]ConversationCompactionCheckpoint `json:"branches"`
	Generations map[string]CompactionGeneration             `json:"generations"`
	// Checkpoints are immutable, event-bound snapshots. Branches is retained as
	// the latest snapshot index for compatibility with the original P2 schema.
	Checkpoints map[string][]ConversationCompactionCheckpoint `json:"checkpoints,omitempty"`
}

// ConversationCompactionCheckpoint is the complete durable active-branch
// conversation projection. The message and summary contents live here, never
// in the event store.
type ConversationCompactionCheckpoint struct {
	BranchID        string              `json:"branch_id"`
	EventID         string              `json:"event_id,omitempty"`
	GenerationID    string              `json:"generation_id"`
	AttestationMode string              `json:"attestation_mode,omitempty"`
	History         []fantasy.Message   `json:"history"`
	SourceOffset    int                 `json:"source_offset"`
	SourceCounts    []int               `json:"source_counts"`
	SourceRanges    [][]CompactionRange `json:"source_ranges,omitempty"`
	NextSourceIndex int                 `json:"next_source_index"`
	HistoryDigest   string              `json:"history_digest"`
}

// CompactionCheckpointReference is the reference-only event payload for an
// immutable checkpoint. The checkpoint digest commits the complete snapshot;
// the event ID is derived from that digest and is therefore not part of it.
type CompactionCheckpointReference struct {
	BranchID           string `json:"branch_id"`
	GenerationID       string `json:"generation_id"`
	GenerationChecksum string `json:"generation_checksum"`
	CheckpointDigest   string `json:"checkpoint_digest"`
}

// CompactionGeneration records immutable lineage and replacement metadata.
// Summary and replacement contents are retained in canonical state so a
// restart can restore exactly the same projection without replaying a model.
type CompactionGeneration struct {
	ID                string            `json:"id"`
	BranchID          string            `json:"branch_id"`
	PredecessorID     string            `json:"predecessor_id,omitempty"`
	ModelID           string            `json:"model_id"`
	CreatedAt         time.Time         `json:"created_at"`
	TokensBefore      int               `json:"tokens_before"`
	TokensAfter       int               `json:"tokens_after"`
	SourceRanges      []CompactionRange `json:"source_ranges"`
	Summary           StructuredSummary `json:"summary"`
	Replacement       []fantasy.Message `json:"replacement"`
	SummaryDigest     string            `json:"summary_digest"`
	ReplacementDigest string            `json:"replacement_digest"`
	Checksum          string            `json:"checksum"`
}

// CompactionReference is safe to place in the event store: it contains no
// transcript or summary text.
type CompactionReference struct {
	GenerationID string `json:"generation_id"`
	BranchID     string `json:"branch_id"`
	Checksum     string `json:"checksum"`
}

var errNoCanonicalCompactionState = errors.New("canonical compaction state does not exist")
var errCompactionEventMissing = errors.New("deterministic compaction attestation is missing")

func compactionStatePath(workspace string) string {
	return filepath.Join(workspace, conversationCompactionStateFile)
}

func newCompactionState() *ConversationCompactionState {
	return &ConversationCompactionState{
		Version:     conversationCompactionStateVersion,
		Branches:    make(map[string]ConversationCompactionCheckpoint),
		Generations: make(map[string]CompactionGeneration),
		Checkpoints: make(map[string][]ConversationCompactionCheckpoint),
	}
}

func cloneCompactionState(state *ConversationCompactionState) *ConversationCompactionState {
	if state == nil {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil
	}
	var clone ConversationCompactionState
	if json.Unmarshal(data, &clone) != nil {
		return nil
	}
	return &clone
}

// LoadConversationCompactionState returns exists=false only for a missing
// canonical file. A present but invalid file is recovery-required.
func LoadConversationCompactionState(workspace string) (*ConversationCompactionState, bool, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(compactionStatePath(workspace))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("read canonical compaction state: %w", err)
	}
	var state ConversationCompactionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, true, fmt.Errorf("decode canonical compaction state: %w", err)
	}
	if err := validateCompactionState(&state); err != nil {
		return nil, true, fmt.Errorf("validate canonical compaction state: %w", err)
	}
	return &state, true, nil
}

func SaveConversationCompactionState(workspace string, state *ConversationCompactionState) error {
	if strings.TrimSpace(workspace) == "" {
		return errNoCanonicalCompactionState
	}
	state, err := redactedCompactionState(state)
	if err != nil {
		return fmt.Errorf("redact canonical compaction state: %w", err)
	}
	state = retainReachableCompactionState(workspace, state)
	if err := validateCompactionState(state); err != nil {
		return fmt.Errorf("validate canonical compaction state before save: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal canonical compaction state: %w", err)
	}
	return AtomicWriteFile(compactionStatePath(workspace), data, 0o600)
}

// retainReachableCompactionState bounds immutable checkpoint growth without
// invalidating time travel. The latest snapshot of every canonical branch is
// retained, as is the parent snapshot at every extant fork point. Everything
// else is unreachable from the current session tree and may be removed. If
// lineage evidence is unavailable, the safe choice is to retain the state and
// let validation preserve the existing recovery behavior.
func retainReachableCompactionState(workspace string, state *ConversationCompactionState) *ConversationCompactionState {
	if state == nil || strings.TrimSpace(workspace) == "" {
		return state
	}
	tree, err := LoadSessionTree(workspace)
	if err != nil || tree == nil {
		return state
	}
	if _, err := os.Stat(filepath.Join(workspace, sessionTreeFile)); err != nil {
		return state
	}
	if _, err := os.Stat(filepath.Join(workspace, logsDir, eventStoreFile)); err != nil {
		return state
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		return state
	}
	defer func() { _ = es.Close() }()
	events, err := es.ReadEvents()
	if err != nil {
		return state
	}

	keep := make(map[string]map[string]struct{}, len(state.Branches))
	keepCheckpoint := func(checkpoint ConversationCompactionCheckpoint) {
		if checkpoint.BranchID == "" || checkpoint.EventID == "" {
			return
		}
		if keep[checkpoint.BranchID] == nil {
			keep[checkpoint.BranchID] = make(map[string]struct{})
		}
		keep[checkpoint.BranchID][checkpoint.EventID] = struct{}{}
	}
	for branchID, head := range state.Branches {
		checkpoints := state.Checkpoints[branchID]
		if len(checkpoints) > 0 {
			keepCheckpoint(checkpoints[len(checkpoints)-1])
		} else {
			keepCheckpoint(head)
		}
	}

	for _, branch := range tree.Branches {
		if branch == nil || branch.ParentID == "" || branch.ForkEventID == "" {
			continue
		}
		lineage := FilterEventsForBranch(events, tree, branch.ParentID)
		forkIndex := -1
		for index, event := range lineage {
			if event.ID == branch.ForkEventID {
				forkIndex = index
				break
			}
		}
		if forkIndex < 0 {
			// A partially written tree/event projection must not cause a
			// checkpoint to be deleted. The next successful save can retry.
			return state
		}
		eventIndex := make(map[string]int, len(lineage))
		for index, event := range lineage {
			eventIndex[event.ID] = index
		}
		var selected ConversationCompactionCheckpoint
		selectedIndex := -1
		for _, checkpoint := range state.Checkpoints[branch.ParentID] {
			index, exists := eventIndex[checkpoint.EventID]
			if exists && index <= forkIndex && index >= selectedIndex {
				selected = checkpoint
				selectedIndex = index
			}
		}
		if selectedIndex >= 0 {
			keepCheckpoint(selected)
		}
	}

	retainedGenerations := make(map[string]struct{}, len(state.Generations))
	for branchID, checkpoints := range state.Checkpoints {
		filtered := make([]ConversationCompactionCheckpoint, 0, len(checkpoints))
		for _, checkpoint := range checkpoints {
			if _, ok := keep[branchID][checkpoint.EventID]; !ok {
				continue
			}
			filtered = append(filtered, checkpoint)
			for generationID := checkpoint.GenerationID; generationID != ""; {
				if _, seen := retainedGenerations[generationID]; seen {
					break
				}
				retainedGenerations[generationID] = struct{}{}
				generation, exists := state.Generations[generationID]
				if !exists {
					break
				}
				generationID = generation.PredecessorID
			}
		}
		if len(filtered) == 0 {
			// Every canonical branch must remain loadable. Keep its current
			// head when no event-bound checkpoint was selected above.
			filtered = []ConversationCompactionCheckpoint{state.Branches[branchID]}
		}
		state.Checkpoints[branchID] = filtered
	}
	for generationID := range state.Generations {
		if _, ok := retainedGenerations[generationID]; !ok {
			delete(state.Generations, generationID)
		}
	}
	return state
}

func redactedCompactionState(state *ConversationCompactionState) (*ConversationCompactionState, error) {
	clone := cloneCompactionState(state)
	if clone == nil {
		return nil, errors.New("clone canonical compaction state")
	}
	data, err := json.Marshal(clone)
	if err != nil {
		return nil, err
	}
	data, err = utils.RedactJSON(data)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, clone); err != nil {
		return nil, err
	}
	// The checkpoints field is omitempty, so the JSON round-trip above drops
	// an empty map; restore it so callers can append without re-initializing
	// and validation still sees the field present.
	if clone.Checkpoints == nil {
		clone.Checkpoints = make(map[string][]ConversationCompactionCheckpoint)
	}
	for id, generation := range clone.Generations {
		generation.SummaryDigest = digestStructuredSummary(&generation.Summary)
		generation.ReplacementDigest = digestMessages(generation.Replacement)
		generation.Checksum = digestGeneration(generation)
		clone.Generations[id] = generation
	}
	for id, checkpoint := range clone.Branches {
		checkpoint.HistoryDigest = digestMessages(checkpoint.History)
		clone.Branches[id] = checkpoint
	}
	for branchID, checkpoints := range clone.Checkpoints {
		for i, checkpoint := range checkpoints {
			checkpoint.HistoryDigest = digestMessages(checkpoint.History)
			checkpoints[i] = checkpoint
		}
		clone.Checkpoints[branchID] = checkpoints
	}
	return clone, nil
}

// MigrateLegacyCompactionState promotes the old compaction history plus its
// session-history projection exactly once. It is intentionally explicit so a
// corrupt legacy file is recovery-required rather than silently discarded.
func MigrateLegacyCompactionState(workspace, branchID string) error {
	if strings.TrimSpace(workspace) == "" {
		return nil
	}
	if _, exists, err := LoadConversationCompactionState(workspace); exists {
		return err
	}
	legacy, err := loadLegacyCompactionHistory(workspace)
	if err != nil || len(legacy) == 0 {
		return err
	}
	if branchID == "" {
		branchID = "main"
	}
	history := LoadConversationHistory(workspace)
	if len(history) == 0 {
		return errors.New("cannot migrate legacy compaction without conversation history")
	}
	sourceOffset := 0
	var sourceCounts []int
	if session := LoadSession(workspace); session != nil {
		sourceOffset = session.ConversationHistorySourceOffset
		if len(session.ConversationHistorySourceCounts) > 0 {
			if len(session.ConversationHistorySourceCounts) != len(history) {
				return fmt.Errorf("legacy conversation source counts do not match history: %d counts for %d messages", len(session.ConversationHistorySourceCounts), len(history))
			}
			sourceCounts = append([]int(nil), session.ConversationHistorySourceCounts...)
		}
	}
	if sourceOffset < 0 {
		return errors.New("legacy conversation source offset is invalid")
	}
	// Only the latest legacy record maps onto the current conversation
	// history: every earlier record's replacement was itself consumed by a
	// later compaction, so no immutable checkpoint can be reconstructed for
	// it. Migrate the latest record; the legacy file retains the full lineage.
	record := legacy[len(legacy)-1]
	if record.BranchID != "" && record.BranchID != branchID {
		return fmt.Errorf("legacy compaction belongs to branch %q, cannot migrate to %q", record.BranchID, branchID)
	}
	// Legacy records carry no replacement messages, so a predecessor
	// generation can never be reconstructed; the migrated generation becomes
	// the lineage root. Validation would otherwise reject the dangling
	// predecessor reference.
	record.PredecessorID = ""
	historyRanges, sourceOffset, err := legacyHistorySourceRanges(history, sourceOffset, sourceCounts, record)
	if err != nil {
		return err
	}
	sourceCounts = sourceCountsForRanges(historyRanges)
	generation, err := legacyMigrationGeneration(record, branchID, history)
	if err != nil {
		return err
	}
	eventID, attested, err := findLegacyCompactionAttestation(workspace, generation)
	if err != nil {
		return err
	}
	// A legacy record has no authoritative event identity by itself. Keep the
	// workspace in compatibility mode until an existing event-store reference
	// attests the exact migrated generation; a synthetic event would make the
	// canonical checkpoint appear durable when it is not.
	if !attested || strings.TrimSpace(eventID) == "" {
		return nil
	}
	state := newCompactionState()
	state.Generations[generation.ID] = generation
	checkpoint := ConversationCompactionCheckpoint{
		BranchID: branchID, GenerationID: generation.ID, History: cloneMessages(history),
		AttestationMode: compactionCheckpointAttestationMode,
		SourceOffset:    sourceOffset, SourceCounts: append([]int(nil), sourceCounts...), SourceRanges: cloneSourceRanges(historyRanges),
		NextSourceIndex: maxSourceIndex(historyRanges), HistoryDigest: digestMessages(history),
	}
	state.Branches[branchID] = checkpoint
	state.Checkpoints[branchID] = []ConversationCompactionCheckpoint{checkpoint}
	state, err = redactedCompactionState(state)
	if err != nil {
		return fmt.Errorf("redact migrated canonical compaction state: %w", err)
	}
	checkpoint = state.Branches[branchID]
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Branches[branchID] = checkpoint
	state.Checkpoints[branchID] = []ConversationCompactionCheckpoint{checkpoint}
	if err := SaveConversationCompactionState(workspace, state); err != nil {
		return err
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		return fmt.Errorf("open event store for migrated compaction attestations: %w", err)
	}
	defer func() { _ = es.Close() }()
	if err := appendCompactionGenerationAttestation(es, generation); err != nil {
		return fmt.Errorf("attest migrated compaction generation: %w", err)
	}
	if err := appendCompactionCheckpointAttestation(es, checkpoint, generation); err != nil {
		return fmt.Errorf("attest migrated compaction checkpoint: %w", err)
	}
	return nil
}

func legacyMigrationGeneration(record CompactionRecord, branchID string, history []fantasy.Message) (CompactionGeneration, error) {
	ranges := append([]CompactionRange(nil), record.SourceRanges...)
	if len(ranges) == 0 && record.SourceRange.MsgCount > 0 {
		ranges = []CompactionRange{record.SourceRange}
	}
	if err := validateCompactionRanges(ranges); err != nil {
		return CompactionGeneration{}, fmt.Errorf("legacy compaction provenance: %w", err)
	}
	if len(ranges) == 0 {
		return CompactionGeneration{}, errors.New("legacy compaction has no source provenance")
	}
	replacement := make([]fantasy.Message, 0, len(history))
	for _, message := range history {
		if isVerifiedHistoryMessage(message) {
			replacement = append(replacement, message)
		}
	}
	if len(replacement) == 0 {
		return CompactionGeneration{}, errors.New("legacy compaction has no verified replacement history")
	}
	generationID := record.GenerationID
	if generationID == "" {
		generationID = record.ID
	}
	if generationID == "" {
		generationID = "legacy-compaction-1"
	}
	modelID := record.ModelID
	if modelID == "" {
		modelID = "legacy"
	}
	createdAt := record.Timestamp
	if createdAt.IsZero() {
		createdAt = time.Unix(0, 1).UTC()
	}
	generation := CompactionGeneration{
		ID: generationID, BranchID: branchID, ModelID: modelID, CreatedAt: createdAt,
		TokensBefore: record.TokensBefore, TokensAfter: record.TokensAfter, SourceRanges: ranges,
		Summary: *cloneStructuredSummary(&record.Summary), Replacement: cloneMessages(replacement),
	}
	generation.SummaryDigest = digestStructuredSummary(&generation.Summary)
	generation.ReplacementDigest = digestMessages(generation.Replacement)
	generation.Checksum = digestGeneration(generation)
	return generation, nil
}

func legacyHistorySourceRanges(history []fantasy.Message, sourceOffset int, sourceCounts []int, record CompactionRecord) ([][]CompactionRange, int, error) {
	ranges := append([]CompactionRange(nil), record.SourceRanges...)
	if len(ranges) == 0 && record.SourceRange.MsgCount > 0 {
		ranges = []CompactionRange{record.SourceRange}
	}
	if err := validateCompactionRanges(ranges); err != nil {
		return nil, 0, fmt.Errorf("legacy compaction provenance: %w", err)
	}
	if len(ranges) == 0 {
		return nil, 0, errors.New("legacy compaction has no source provenance")
	}
	if len(history) == 0 || !isVerifiedHistoryMessage(history[0]) {
		return nil, 0, errors.New("legacy compaction replacement position is not provable")
	}
	for _, message := range history[1:] {
		if isVerifiedHistoryMessage(message) {
			return nil, 0, errors.New("legacy compaction has multiple unscoped replacement messages")
		}
	}
	compactedCount := sumCompactionRanges(ranges)
	if len(sourceCounts) != 0 {
		if len(sourceCounts) != len(history) {
			return nil, 0, fmt.Errorf("legacy conversation source counts do not match history: %d counts for %d messages", len(sourceCounts), len(history))
		}
		if sourceCounts[0] != compactedCount {
			return nil, 0, fmt.Errorf("legacy replacement source count %d does not match compacted range count %d", sourceCounts[0], compactedCount)
		}
		for _, count := range sourceCounts {
			if count <= 0 {
				return nil, 0, errors.New("legacy conversation source counts contain an invalid value")
			}
		}
	}
	if sourceOffset > 0 && sourceOffset != ranges[0].StartIndex {
		return nil, 0, fmt.Errorf("legacy conversation source offset %d does not match compacted range start %d", sourceOffset, ranges[0].StartIndex)
	}
	sourceOffset = ranges[0].StartIndex
	result := make([][]CompactionRange, len(history))
	result[0] = append([]CompactionRange(nil), ranges...)
	next := maxSourceIndex(result[:1])
	for i := 1; i < len(history); i++ {
		count := 1
		if len(sourceCounts) != 0 {
			count = sourceCounts[i]
		}
		result[i] = []CompactionRange{{StartIndex: next, EndIndex: next + count - 1, MsgCount: count}}
		next += count
	}
	if err := validateHistorySourceRanges(result); err != nil {
		return nil, 0, fmt.Errorf("legacy conversation provenance: %w", err)
	}
	return result, sourceOffset, nil
}

// findLegacyCompactionAttestation accepts an existing event only when the
// reference identifies the exact migrated generation and checksum. A legacy
// record ID is not treated as an event ID because the old format did not
// durably record that identity.
func findLegacyCompactionAttestation(workspace string, generation CompactionGeneration) (string, bool, error) {
	es, err := OpenEventStore(workspace)
	if err != nil {
		return "", false, fmt.Errorf("open event store for legacy compaction migration: %w", err)
	}
	defer func() { _ = es.Close() }()
	events, err := es.ReadEvents()
	if err != nil {
		return "", false, fmt.Errorf("read event store for legacy compaction migration: %w", err)
	}
	for _, event := range events {
		if event.Type != compactionGenerationEventType {
			continue
		}
		var reference CompactionReference
		if err := json.Unmarshal(event.Payload, &reference); err != nil || reference.GenerationID == "" || reference.BranchID == "" || reference.Checksum == "" {
			return "", false, errors.New("legacy migration found malformed compaction attestation")
		}
		if reference.GenerationID != generation.ID {
			continue
		}
		if strings.TrimSpace(event.ID) == "" {
			return "", false, errors.New("legacy migration found attestation without event identity")
		}
		if effectiveEventBranchID(event) != generation.BranchID || reference.BranchID != generation.BranchID {
			return "", false, errors.New("legacy migration found cross-branch compaction attestation")
		}
		if reference.Checksum != generation.Checksum {
			return "", false, fmt.Errorf("legacy migration found checksum-mismatched attestation for generation %q", generation.ID)
		}
		return event.ID, true, nil
	}
	return "", false, nil
}

func validateCompactionState(state *ConversationCompactionState) error {
	if state == nil {
		return errors.New("state is nil")
	}
	if state.Version != conversationCompactionStateVersion {
		return fmt.Errorf("unsupported version %d", state.Version)
	}
	if state.Branches == nil || state.Generations == nil || state.Checkpoints == nil {
		return errors.New("branches, generations, and immutable checkpoints are required")
	}
	if len(state.Branches) == 0 || len(state.Generations) == 0 {
		return errors.New("canonical compaction state has no checkpoint generations")
	}
	seen := make(map[string]bool, len(state.Generations))
	for id, generation := range state.Generations {
		if id == "" || generation.ID == "" || id != generation.ID {
			return fmt.Errorf("generation identity is invalid")
		}
		if seen[id] {
			return fmt.Errorf("duplicate generation %q", id)
		}
		seen[id] = true
		if strings.TrimSpace(generation.BranchID) == "" || strings.TrimSpace(generation.ModelID) == "" {
			return fmt.Errorf("generation %q has incomplete branch/model identity", id)
		}
		if generation.PredecessorID != "" {
			predecessor, ok := state.Generations[generation.PredecessorID]
			if !ok || predecessor.BranchID != generation.BranchID {
				return fmt.Errorf("generation %q has invalid predecessor", id)
			}
		}
		if err := validateCompactionRanges(generation.SourceRanges); err != nil {
			return fmt.Errorf("generation %q: %w", id, err)
		}
		if len(generation.SourceRanges) == 0 {
			return fmt.Errorf("generation %q has no source provenance", id)
		}
		if !toolPairsIntact(generation.Replacement) {
			return fmt.Errorf("generation %q replacement splits tool pairs", id)
		}
		if generation.SummaryDigest != digestStructuredSummary(&generation.Summary) {
			return fmt.Errorf("generation %q summary checksum mismatch", id)
		}
		if generation.ReplacementDigest != digestMessages(generation.Replacement) {
			return fmt.Errorf("generation %q replacement checksum mismatch", id)
		}
		if generation.Checksum != digestGeneration(generation) {
			return fmt.Errorf("generation %q checksum mismatch", id)
		}
	}
	if err := validateCompactionLineageRanges(state); err != nil {
		return err
	}
	for branchID, checkpoint := range state.Branches {
		if err := validateCompactionCheckpoint(state, branchID, checkpoint); err != nil {
			return err
		}
		checkpoints, ok := state.Checkpoints[branchID]
		if !ok || len(checkpoints) == 0 {
			return fmt.Errorf("branch %q has no immutable checkpoints", branchID)
		}
		if !reflect.DeepEqual(checkpoints[len(checkpoints)-1], checkpoint) {
			return fmt.Errorf("branch %q latest checkpoint is not its canonical branch snapshot", branchID)
		}
		previousNextSourceIndex := -1
		seenEvents := make(map[string]bool, len(checkpoints))
		for _, immutable := range checkpoints {
			if err := validateCompactionCheckpoint(state, branchID, immutable); err != nil {
				return err
			}
			if immutable.NextSourceIndex < previousNextSourceIndex {
				return fmt.Errorf("branch %q checkpoint source index regressed", branchID)
			}
			if seenEvents[immutable.EventID] {
				return fmt.Errorf("branch %q has duplicate immutable checkpoint event %q", branchID, immutable.EventID)
			}
			seenEvents[immutable.EventID] = true
			previousNextSourceIndex = immutable.NextSourceIndex
		}
	}
	for branchID, checkpoints := range state.Checkpoints {
		if _, ok := state.Branches[branchID]; !ok {
			return fmt.Errorf("immutable checkpoints reference unknown branch %q", branchID)
		}
		for _, checkpoint := range checkpoints {
			if err := validateCompactionCheckpoint(state, branchID, checkpoint); err != nil {
				return err
			}
			if checkpoint.EventID == "" {
				return fmt.Errorf("checkpoint %q is not event-bound", branchID)
			}
		}
	}
	return nil
}

func validateCompactionCheckpoint(state *ConversationCompactionState, branchID string, checkpoint ConversationCompactionCheckpoint) error {
	if branchID == "" || checkpoint.BranchID != branchID {
		return fmt.Errorf("checkpoint branch identity is invalid")
	}
	generation, ok := state.Generations[checkpoint.GenerationID]
	if !ok || generation.BranchID != branchID {
		return fmt.Errorf("checkpoint %q references an invalid generation", branchID)
	}
	if checkpoint.EventID == "" {
		return fmt.Errorf("checkpoint %q is not event-bound", branchID)
	}
	mode := checkpointAttestationMode(checkpoint)
	if mode != compactionCheckpointAttestationMode {
		return fmt.Errorf("checkpoint %q has missing or invalid attestation mode %q", branchID, mode)
	}
	if checkpoint.EventID != compactionCheckpointEventID(checkpoint) {
		return fmt.Errorf("checkpoint %q has an identity-mismatched attestation event", branchID)
	}
	if len(checkpoint.History) == 0 || !toolPairsIntact(checkpoint.History) {
		return fmt.Errorf("checkpoint %q splits tool pairs", branchID)
	}
	if checkpoint.HistoryDigest != digestMessages(checkpoint.History) {
		return fmt.Errorf("checkpoint %q history checksum mismatch", branchID)
	}
	if len(checkpoint.SourceCounts) != len(checkpoint.History) {
		return fmt.Errorf("checkpoint %q source counts do not match history", branchID)
	}
	if len(checkpoint.SourceRanges) != len(checkpoint.History) {
		return fmt.Errorf("checkpoint %q source ranges do not match history", branchID)
	}
	if err := validateHistorySourceRanges(checkpoint.SourceRanges); err != nil {
		return fmt.Errorf("checkpoint %q: %w", branchID, err)
	}
	for i, ranges := range checkpoint.SourceRanges {
		if sumCompactionRanges(ranges) != checkpoint.SourceCounts[i] {
			return fmt.Errorf("checkpoint %q source count does not match provenance at message %d", branchID, i)
		}
	}
	for _, count := range checkpoint.SourceCounts {
		if count <= 0 {
			return fmt.Errorf("checkpoint %q has invalid source count", branchID)
		}
	}
	if checkpoint.SourceOffset < 0 || checkpoint.NextSourceIndex < 0 {
		return fmt.Errorf("checkpoint %q has invalid source index", branchID)
	}
	if checkpoint.NextSourceIndex < maxSourceIndex(checkpoint.SourceRanges) {
		return fmt.Errorf("checkpoint %q next source index precedes provenance", branchID)
	}
	return nil
}

func validateCompactionRanges(ranges []CompactionRange) error {
	previousEnd := -1
	for _, r := range ranges {
		if r.StartIndex < 0 || r.EndIndex < r.StartIndex || r.MsgCount != r.EndIndex-r.StartIndex+1 {
			return fmt.Errorf("invalid source range %+v", r)
		}
		if r.StartIndex <= previousEnd {
			return errors.New("source ranges overlap or are unordered")
		}
		previousEnd = r.EndIndex
	}
	return nil
}

type compactionLineageValidationStats struct {
	RangeQueries int
}

// compactionRangeIndex is an active-ancestor interval index. Generations are
// walked as a graph, so each generation is checked against its active
// predecessor path once instead of walking the complete predecessor chain
// again. Coordinate compression keeps the index bounded by the source ranges
// present in the state and makes validation O(R log R).
type compactionRangeIndex struct {
	coordinates []int
	maximum     []int
	lazy        []int
}

func newCompactionRangeIndex(coordinates []int) *compactionRangeIndex {
	if len(coordinates) == 0 {
		return &compactionRangeIndex{}
	}
	sorted := append([]int(nil), coordinates...)
	sort.Ints(sorted)
	sorted = slices.Compact(sorted)
	size := len(sorted) * 4
	return &compactionRangeIndex{coordinates: sorted, maximum: make([]int, size), lazy: make([]int, size)}
}

func (index *compactionRangeIndex) update(sourceRange CompactionRange, delta int) {
	if index == nil || len(index.coordinates) == 0 {
		return
	}
	left := sort.SearchInts(index.coordinates, sourceRange.StartIndex)
	right := sort.SearchInts(index.coordinates, sourceRange.EndIndex)
	if left > right {
		return
	}
	index.updateRange(1, 0, len(index.coordinates)-1, left, right, delta)
}

func (index *compactionRangeIndex) updateRange(node, start, end, left, right, delta int) {
	if left > end || right < start {
		return
	}
	if left <= start && end <= right {
		index.maximum[node] += delta
		index.lazy[node] += delta
		return
	}
	middle := start + (end-start)/2
	index.updateRange(node*2, start, middle, left, right, delta)
	index.updateRange(node*2+1, middle+1, end, left, right, delta)
	index.maximum[node] = index.lazy[node] + max(index.maximum[node*2], index.maximum[node*2+1])
}

func (index *compactionRangeIndex) overlaps(sourceRange CompactionRange) bool {
	if index == nil || len(index.coordinates) == 0 {
		return false
	}
	left := sort.SearchInts(index.coordinates, sourceRange.StartIndex)
	right := sort.SearchInts(index.coordinates, sourceRange.EndIndex)
	if left > right {
		return false
	}
	return index.queryRange(1, 0, len(index.coordinates)-1, left, right, 0) > 0
}

func (index *compactionRangeIndex) queryRange(node, start, end, left, right, inherited int) int {
	if left > end || right < start {
		return 0
	}
	inherited += index.lazy[node]
	if left <= start && end <= right {
		return inherited + index.maximum[node]
	}
	middle := start + (end-start)/2
	return max(
		index.queryRange(node*2, start, middle, left, right, inherited),
		index.queryRange(node*2+1, middle+1, end, left, right, inherited),
	)
}

// validateCompactionLineageRanges prevents a later generation from claiming
// source messages already claimed by one of its predecessors. The active-path
// index preserves the same invariant as the old predecessor walk while making
// repeated linear compactions incremental rather than O(K^2).
func validateCompactionLineageRanges(state *ConversationCompactionState) error {
	_, err := validateCompactionLineageRangesWithStats(state)
	return err
}

func validateCompactionLineageRangesWithStats(state *ConversationCompactionState) (compactionLineageValidationStats, error) {
	var stats compactionLineageValidationStats
	if state == nil {
		return stats, errors.New("state is nil")
	}
	coordinates := make([]int, 0)
	children := make(map[string][]string, len(state.Generations))
	roots := make([]string, 0)
	for id, generation := range state.Generations {
		for _, sourceRange := range generation.SourceRanges {
			coordinates = append(coordinates, sourceRange.StartIndex, sourceRange.EndIndex)
		}
		if generation.PredecessorID == "" {
			roots = append(roots, id)
		} else {
			children[generation.PredecessorID] = append(children[generation.PredecessorID], id)
		}
	}
	sort.Strings(roots)
	for predecessorID := range children {
		sort.Strings(children[predecessorID])
	}
	index := newCompactionRangeIndex(coordinates)
	colors := make(map[string]uint8, len(state.Generations))
	var walk func(string) error
	walk = func(id string) error {
		switch colors[id] {
		case 1:
			return errors.New("generation predecessor lineage contains a cycle")
		case 2:
			return nil
		}
		generation, ok := state.Generations[id]
		if !ok {
			return fmt.Errorf("generation predecessor %q is missing", id)
		}
		colors[id] = 1
		for _, sourceRange := range generation.SourceRanges {
			stats.RangeQueries++
			if index.overlaps(sourceRange) {
				return fmt.Errorf("source range %+v overlaps predecessor lineage of generation %q", sourceRange, generation.ID)
			}
		}
		for _, sourceRange := range generation.SourceRanges {
			index.update(sourceRange, 1)
		}
		for _, childID := range children[id] {
			if err := walk(childID); err != nil {
				return err
			}
		}
		for _, sourceRange := range generation.SourceRanges {
			index.update(sourceRange, -1)
		}
		colors[id] = 2
		return nil
	}
	for _, rootID := range roots {
		if err := walk(rootID); err != nil {
			return stats, err
		}
	}
	for id := range state.Generations {
		if colors[id] == 0 {
			if err := walk(id); err != nil {
				return stats, err
			}
		}
	}
	return stats, nil
}

func validateHistorySourceRanges(sourceRanges [][]CompactionRange) error {
	var flattened []CompactionRange
	previousEnd := -1
	for _, ranges := range sourceRanges {
		if len(ranges) == 0 {
			return errors.New("history message has no source provenance")
		}
		if err := validateCompactionRanges(ranges); err != nil {
			return err
		}
		if ranges[0].StartIndex <= previousEnd {
			return errors.New("history source ranges are not monotonic")
		}
		flattened = append(flattened, ranges...)
		previousEnd = ranges[len(ranges)-1].EndIndex
	}
	return validateCompactionRanges(flattened)
}

func cloneSourceRanges(sourceRanges [][]CompactionRange) [][]CompactionRange {
	if sourceRanges == nil {
		return nil
	}
	clone := make([][]CompactionRange, len(sourceRanges))
	for i, ranges := range sourceRanges {
		clone[i] = append([]CompactionRange(nil), ranges...)
	}
	return clone
}

func historySourceRanges(length, offset int, counts []int, sourceRanges [][]CompactionRange) [][]CompactionRange {
	if len(sourceRanges) == length {
		clone := cloneSourceRanges(sourceRanges)
		if validateHistorySourceRanges(clone) == nil {
			return clone
		}
	}
	counts = normalizeSourceCounts(length, counts)
	result := make([][]CompactionRange, length)
	index := offset
	for i, count := range counts {
		result[i] = []CompactionRange{{StartIndex: index, EndIndex: index + count - 1, MsgCount: count}}
		index += count
	}
	return result
}

func sourceCountsForRanges(sourceRanges [][]CompactionRange) []int {
	counts := make([]int, len(sourceRanges))
	for i, ranges := range sourceRanges {
		counts[i] = sumCompactionRanges(ranges)
	}
	return counts
}

func flattenSourceRanges(sourceRanges [][]CompactionRange) []CompactionRange {
	var flattened []CompactionRange
	for _, ranges := range sourceRanges {
		flattened = append(flattened, ranges...)
	}
	return mergeCompactionRanges(flattened)
}

func mergeCompactionRanges(ranges []CompactionRange) []CompactionRange {
	if len(ranges) == 0 {
		return nil
	}
	sorted := append([]CompactionRange(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StartIndex < sorted[j].StartIndex })
	merged := make([]CompactionRange, 0, len(sorted))
	for _, current := range sorted {
		if len(merged) == 0 || current.StartIndex > merged[len(merged)-1].EndIndex+1 {
			merged = append(merged, current)
			continue
		}
		if current.EndIndex > merged[len(merged)-1].EndIndex {
			merged[len(merged)-1].EndIndex = current.EndIndex
			merged[len(merged)-1].MsgCount = merged[len(merged)-1].EndIndex - merged[len(merged)-1].StartIndex + 1
		}
	}
	return merged
}

func subtractCompactionRanges(ranges, claimed []CompactionRange) []CompactionRange {
	remaining := mergeCompactionRanges(ranges)
	for _, used := range mergeCompactionRanges(claimed) {
		var next []CompactionRange
		for _, current := range remaining {
			if used.EndIndex < current.StartIndex || used.StartIndex > current.EndIndex {
				next = append(next, current)
				continue
			}
			if current.StartIndex < used.StartIndex {
				next = append(next, CompactionRange{StartIndex: current.StartIndex, EndIndex: used.StartIndex - 1, MsgCount: used.StartIndex - current.StartIndex})
			}
			if current.EndIndex > used.EndIndex {
				next = append(next, CompactionRange{StartIndex: used.EndIndex + 1, EndIndex: current.EndIndex, MsgCount: current.EndIndex - used.EndIndex})
			}
		}
		remaining = next
	}
	return remaining
}

func maxSourceIndex(sourceRanges [][]CompactionRange) int {
	max := 0
	for _, ranges := range sourceRanges {
		for _, sourceRange := range ranges {
			if sourceRange.EndIndex+1 > max {
				max = sourceRange.EndIndex + 1
			}
		}
	}
	return max
}

func sourceOffsetForRanges(sourceRanges [][]CompactionRange, fallback int) int {
	if len(sourceRanges) == 0 {
		return fallback
	}
	minimum := sourceRanges[0][0].StartIndex
	for _, ranges := range sourceRanges {
		for _, sourceRange := range ranges {
			if sourceRange.StartIndex < minimum {
				minimum = sourceRange.StartIndex
			}
		}
	}
	return minimum
}

func normalizedCompactionRanges(offset int, counts []int) []CompactionRange {
	if offset < 0 {
		offset = 0
	}
	counts = normalizeSourceCounts(len(counts), counts)
	if len(counts) == 0 {
		return nil
	}
	ranges := make([]CompactionRange, 0, len(counts))
	start := offset
	for _, count := range counts {
		ranges = append(ranges, CompactionRange{StartIndex: start, EndIndex: start + count - 1, MsgCount: count})
		start += count
	}
	return ranges
}

func digestStructuredSummary(summary *StructuredSummary) string {
	if summary == nil {
		return digestBytes(nil)
	}
	data, _ := json.Marshal(summary)
	return digestBytes(data)
}

func digestMessages(messages []fantasy.Message) string {
	data, _ := json.Marshal(messages)
	return digestBytes(data)
}

func digestBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func digestGeneration(generation CompactionGeneration) string {
	copyGeneration := generation
	copyGeneration.Checksum = ""
	data, _ := json.Marshal(copyGeneration)
	return digestBytes(data)
}

func digestCompactionCheckpoint(checkpoint ConversationCompactionCheckpoint) string {
	checkpoint.EventID = ""
	data, _ := json.Marshal(checkpoint)
	return digestBytes(data)
}

func compactionCheckpointEventID(checkpoint ConversationCompactionCheckpoint) string {
	return "compaction-checkpoint-event-" + digestCompactionCheckpoint(checkpoint)
}

func checkpointAttestationMode(checkpoint ConversationCompactionCheckpoint) string {
	// An absent mode is not evidence of legacy provenance. In particular, do not
	// infer generation-only attestation from an EventID: a damaged current
	// snapshot could otherwise be restored without its snapshot digest check.
	return checkpoint.AttestationMode
}

func nextCompactionGenerationID(branchID string) string {
	return fmt.Sprintf("compact-gen-%s-%d", strings.ReplaceAll(branchID, "/", "-"), time.Now().UTC().UnixNano())
}

func compactionGenerationEventID(generationID string) string {
	return "compaction-event-" + generationID
}

func compactionRecordFromGeneration(generation CompactionGeneration) CompactionRecord {
	return CompactionRecord{
		ID: generation.ID, Timestamp: generation.CreatedAt, TokensBefore: generation.TokensBefore,
		TokensAfter: generation.TokensAfter, SourceRange: firstCompactionRange(generation.SourceRanges),
		SourceRanges: append([]CompactionRange(nil), generation.SourceRanges...), Summary: *cloneStructuredSummary(&generation.Summary),
		GenerationID: generation.ID, PredecessorID: generation.PredecessorID, BranchID: generation.BranchID,
		ModelID: generation.ModelID, SummaryDigest: generation.SummaryDigest, ReplacementDigest: generation.ReplacementDigest,
	}
}

func firstCompactionRange(ranges []CompactionRange) CompactionRange {
	if len(ranges) == 0 {
		return CompactionRange{}
	}
	// SourceRange is the legacy compatibility field. Keep the exact
	// normalized ranges in SourceRanges, while exposing their complete
	// absolute span through the legacy field so callers do not lose source
	// lineage when a generation contains multiple contiguous ranges.
	result := CompactionRange{StartIndex: ranges[0].StartIndex}
	for _, sourceRange := range ranges {
		if sourceRange.EndIndex > result.EndIndex {
			result.EndIndex = sourceRange.EndIndex
		}
		result.MsgCount += sourceRange.MsgCount
	}
	return result
}

func sortedGenerationIDs(state *ConversationCompactionState, branchID string) []string {
	ids := make([]string, 0)
	for id, generation := range state.Generations {
		if generation.BranchID == branchID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return state.Generations[ids[i]].CreatedAt.Before(state.Generations[ids[j]].CreatedAt)
	})
	return ids
}

// commitCompactionCheckpoint performs the durable half of a compaction. A
// non-empty record means canonical state committed even when a later
// compatibility projection or attestation reports a recoverable gap.
func (c *Coordinator) commitCompactionCheckpoint(ctx context.Context, history []fantasy.Message, sourceOffset int, checkpointCounts, compactedCounts []int, projection compactionProjection) (CompactionRecord, error) {
	historyRanges := historySourceRanges(len(history), sourceOffset, checkpointCounts, nil)
	compactedRanges := normalizedCompactionRanges(sourceOffset, compactedCounts)
	return c.commitCompactionCheckpointWithProvenance(ctx, history, sourceOffset, checkpointCounts, compactedRanges, historyRanges, maxSourceIndex(historyRanges), projection)
}

func (c *Coordinator) commitCompactionCheckpointWithProvenance(ctx context.Context, history []fantasy.Message, sourceOffset int, checkpointCounts []int, compactedRanges []CompactionRange, historyRanges [][]CompactionRange, nextSourceIndex int, projection compactionProjection) (CompactionRecord, error) {
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return CompactionRecord{}, errors.New("compaction workspace is unavailable")
	}
	if projection.summary == nil || len(projection.messages) == 0 {
		return CompactionRecord{}, errors.New("compaction projection is incomplete")
	}
	if !toolPairsIntact(history) || !toolPairsIntact(projection.messages) {
		return CompactionRecord{}, errors.New("compaction projection splits tool pairs")
	}
	branchID := c.compactionBranch()
	if branchID == "" {
		branchID = "main"
	}
	state, exists, err := LoadConversationCompactionState(c.session.Workspace)
	if err != nil {
		return CompactionRecord{}, err
	}
	if !exists {
		state = newCompactionState()
	}
	state = cloneCompactionState(state)
	if state == nil {
		return CompactionRecord{}, errors.New("clone canonical compaction state")
	}
	if state.Checkpoints == nil {
		state.Checkpoints = make(map[string][]ConversationCompactionCheckpoint)
	}
	predecessorID := ""
	if checkpoint, ok := state.Branches[branchID]; ok {
		predecessorID = checkpoint.GenerationID
	}
	provenance := historySourceRanges(len(history), sourceOffset, checkpointCounts, historyRanges)
	claimed := compactionRangesClaimedByLineage(state, predecessorID)
	newSourceRanges := subtractCompactionRanges(compactedRanges, claimed)
	generation := CompactionGeneration{
		ID: nextCompactionGenerationID(branchID), BranchID: branchID, PredecessorID: predecessorID,
		ModelID: c.coordinatorModelID(), CreatedAt: time.Now().UTC(), TokensBefore: projection.tokensBefore,
		TokensAfter: projection.tokensAfter, SourceRanges: newSourceRanges,
		Summary: *cloneStructuredSummary(projection.summary), Replacement: cloneMessages(projection.messages),
	}
	generation.SummaryDigest = digestStructuredSummary(&generation.Summary)
	generation.ReplacementDigest = digestMessages(generation.Replacement)
	generation.Checksum = digestGeneration(generation)
	state.Generations[generation.ID] = generation
	checkpoint := ConversationCompactionCheckpoint{
		BranchID: branchID, GenerationID: generation.ID, AttestationMode: compactionCheckpointAttestationMode, History: cloneMessages(history),
		SourceOffset: sourceOffsetForRanges(provenance, sourceOffset), SourceCounts: sourceCountsForRanges(provenance), SourceRanges: cloneSourceRanges(provenance),
		NextSourceIndex: trimMaxInt(nextSourceIndex, maxSourceIndex(provenance)), HistoryDigest: digestMessages(history),
	}
	state.Branches[branchID] = checkpoint
	// This is the ownership boundary: durable canonical state precedes every
	// in-memory conversation, metric, legacy projection, or event mutation.
	// Redaction is part of the persisted payload, so the immutable checkpoint
	// identity is derived only after the canonical redaction pass — deriving
	// it before redaction would make every redacted save fail validation.
	state, err = redactedCompactionState(state)
	if err != nil {
		return CompactionRecord{}, fmt.Errorf("redact canonical compaction state: %w", err)
	}
	generation = state.Generations[generation.ID]
	checkpoint = state.Branches[branchID]
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Checkpoints[branchID] = append(state.Checkpoints[branchID], checkpoint)
	state.Branches[branchID] = checkpoint
	if err := SaveConversationCompactionState(c.session.Workspace, state); err != nil {
		return CompactionRecord{}, err
	}
	// Reload the file that was just atomically committed. The reloaded state is
	// the canonical object that must win if a compatibility projection or
	// attestation fails below.
	committedState, committedExists, loadErr := LoadConversationCompactionState(c.session.Workspace)
	if loadErr != nil || !committedExists {
		c.publishCommittedCompaction(state, branchID)
		if loadErr == nil {
			loadErr = errors.New("canonical compaction state disappeared after commit")
		}
		c.markCompactionRecovery(loadErr)
		return compactionRecordFromGeneration(generation), fmt.Errorf("reload committed compaction state: %w", loadErr)
	}
	state = committedState
	generation = state.Generations[generation.ID]
	checkpoint = state.Branches[branchID]
	record := compactionRecordFromGeneration(generation)
	c.publishCommittedCompaction(state, branchID)
	if err := c.saveLegacyCompactionProjection(c.session.Workspace, record); err != nil {
		c.markCompactionRecovery(fmt.Errorf("save compaction compatibility projection: %w", err))
		return record, fmt.Errorf("save compaction compatibility projection: %w", err)
	}
	if err := c.attestCompactionGeneration(ctx, generation); err != nil {
		c.markCompactionRecovery(fmt.Errorf("attest compaction generation: %w", err))
		return record, fmt.Errorf("attest compaction generation: %w", err)
	}
	if err := c.attestCompactionCheckpoint(ctx, checkpoint, generation); err != nil {
		c.markCompactionRecovery(fmt.Errorf("attest compaction checkpoint: %w", err))
		return record, fmt.Errorf("attest compaction checkpoint: %w", err)
	}
	// Canonical state, the compatibility projection, and both provenance
	// attestations have succeeded. Compaction telemetry is deliberately the
	// final commit step so a P3 failure cannot mask a required P2 failure.
	compactionSpec := globalRegistry.GetSpec(record.ModelID)
	compactionEvent := c.newContextWindowTelemetry(EventContextWindowCompactionCommitted, ContextWindowRequest{
		ModelID: record.ModelID, ReservedOutputTokens: compactionSpec.MaxOutputTokens, SafetyMarginTokens: compactionSpec.SafetyMarginTokens, Window: compactionSpec.ContextWindow, StepNumber: 0,
	}, ContextWindowAdmission{Decision: ContextWindowCompactMidTurn, RequestTokens: record.TokensBefore, Budget: CalculateContextBudget(compactionSpec, 0, 0)}, "canonical_commit", CoordTodoID, 0)
	compactionEvent.Decision = "committed"
	compactionEvent.CompactionCount = c.Metrics().Compactions + 1
	if err := c.recordContextWindowTelemetry(EventContextWindowCompactionCommitted, compactionEvent, CoordTodoID); err != nil {
		c.markCompactionRecovery(fmt.Errorf("persist compaction telemetry: %w", err))
		return record, err
	}
	return record, nil
}

// publishCommittedCompaction advances only in-memory canonical state. It is
// deliberately called immediately after the canonical file is reloaded and
// before compatibility projections or event-store attestation.
func (c *Coordinator) publishCommittedCompaction(state *ConversationCompactionState, branchID string) {
	if c == nil || state == nil {
		return
	}
	checkpoint, ok := state.Branches[branchID]
	if !ok {
		return
	}
	generation, ok := state.Generations[checkpoint.GenerationID]
	if !ok {
		return
	}
	c.compactionMu.Lock()
	c.compactionState = state
	c.compactionBranchID = branchID
	c.lastCompactionSummary = cloneStructuredSummary(&generation.Summary)
	c.compactionMu.Unlock()
}

func (c *Coordinator) persistConversationCheckpointWithProvenance(history []fantasy.Message, sourceOffset int, sourceCounts []int, provenance [][]CompactionRange, nextSourceIndex int) error {
	workspace := c.sessionWorkspace()
	if workspace == "" {
		return nil
	}
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		return err
	}
	branchID := c.compactionBranch()
	checkpoint, ok := state.Branches[branchID]
	if !ok {
		return nil
	}
	state = cloneCompactionState(state)
	if state == nil {
		return errors.New("clone canonical compaction state")
	}
	provenance = historySourceRanges(len(history), sourceOffset, sourceCounts, provenance)
	checkpoint.History = cloneMessages(history)
	checkpoint.EventID = ""
	checkpoint.AttestationMode = compactionCheckpointAttestationMode
	checkpoint.SourceOffset = sourceOffsetForRanges(provenance, sourceOffset)
	checkpoint.SourceCounts = sourceCountsForRanges(provenance)
	checkpoint.SourceRanges = cloneSourceRanges(provenance)
	checkpoint.NextSourceIndex = trimMaxInt(nextSourceIndex, maxSourceIndex(provenance))
	checkpoint.HistoryDigest = digestMessages(history)
	state.Branches[branchID] = checkpoint
	if state.Checkpoints == nil {
		state.Checkpoints = make(map[string][]ConversationCompactionCheckpoint)
	}
	// Redaction is part of the persisted payload, so derive the immutable
	// checkpoint identity only after applying the same canonical redaction pass
	// used by SaveConversationCompactionState.
	state, err = redactedCompactionState(state)
	if err != nil {
		return fmt.Errorf("redact canonical compaction state: %w", err)
	}
	checkpoint = state.Branches[branchID]
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	checkpoints := state.Checkpoints[branchID]
	if len(checkpoints) == 0 || checkpoints[len(checkpoints)-1].EventID != checkpoint.EventID || digestCompactionCheckpoint(checkpoints[len(checkpoints)-1]) != digestCompactionCheckpoint(checkpoint) {
		state.Checkpoints[branchID] = append(checkpoints, checkpoint)
	}
	state.Branches[branchID] = checkpoint
	if err := SaveConversationCompactionState(workspace, state); err != nil {
		return err
	}
	committedState, committedExists, loadErr := LoadConversationCompactionState(workspace)
	if loadErr != nil || !committedExists {
		if loadErr == nil {
			loadErr = errors.New("canonical compaction state disappeared after checkpoint commit")
		}
		c.markCompactionRecovery(loadErr)
		return fmt.Errorf("reload committed compaction checkpoint: %w", loadErr)
	}
	state = committedState
	checkpoint = state.Branches[branchID]
	c.publishCommittedCompaction(state, branchID)
	generation := state.Generations[checkpoint.GenerationID]
	if err := c.attestCompactionGeneration(context.Background(), generation); err != nil {
		c.markCompactionRecovery(fmt.Errorf("attest compaction generation for checkpoint: %w", err))
		return fmt.Errorf("attest compaction generation for checkpoint: %w", err)
	}
	if err := c.attestCompactionCheckpoint(context.Background(), checkpoint, generation); err != nil {
		c.markCompactionRecovery(fmt.Errorf("attest compaction checkpoint: %w", err))
		return fmt.Errorf("attest compaction checkpoint: %w", err)
	}
	return nil
}

func (c *Coordinator) compactionBranch() string {
	c.compactionMu.Lock()
	branch := c.compactionBranchID
	c.compactionMu.Unlock()
	if branch != "" {
		return branch
	}
	if c != nil && c.session != nil {
		if tree, err := LoadSessionTree(c.session.Workspace); err == nil && tree.ActiveBranch != "" {
			return tree.ActiveBranch
		}
	}
	return "main"
}

func (c *Coordinator) sessionWorkspace() string {
	if c != nil && c.session != nil {
		return c.session.Workspace
	}
	return ""
}

func (c *Coordinator) restoreCanonicalCompactionForBranch(branchID string) error {
	if c == nil || c.session == nil {
		return nil
	}
	if c.eventStore == nil {
		return errors.New("cannot restore canonical compaction without an event store")
	}
	state, exists, err := LoadConversationCompactionState(c.session.Workspace)
	if err != nil {
		c.compactionMu.Lock()
		c.compactionRecoveryErr = err
		c.compactionMu.Unlock()
		return err
	}
	if !exists {
		return nil
	}
	if branchID == "" {
		branchID = "main"
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return fmt.Errorf("read event store before compaction restore: %w", err)
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return fmt.Errorf("load session tree before compaction restore: %w", err)
	}
	if _, err := validateCompactionBranchContext(state, branchID, events, tree, ""); err != nil {
		return fmt.Errorf("validate compaction provenance before restore: %w", err)
	}
	checkpoint, ok := state.Branches[branchID]
	if !ok {
		// A fresh root branch may legitimately have no compaction yet while
		// another branch in the same event store does.
		c.conversationHistoryMu.Lock()
		c.conversationHistory = nil
		c.conversationHistorySourceCounts = nil
		c.conversationHistorySourceRanges = nil
		c.conversationHistorySourceOffset = 0
		c.conversationHistoryNextSourceIndex = 0
		c.conversationHistoryMu.Unlock()
		c.compactionMu.Lock()
		c.compactionState = state
		c.compactionBranchID = branchID
		c.lastCompactionSummary = nil
		c.compactionMu.Unlock()
		return nil
	}
	generation := state.Generations[checkpoint.GenerationID]
	c.conversationHistoryMu.Lock()
	c.conversationHistory = cloneMessages(checkpoint.History)
	c.conversationHistorySourceCounts = append([]int(nil), checkpoint.SourceCounts...)
	c.conversationHistorySourceRanges = cloneSourceRanges(checkpoint.SourceRanges)
	c.conversationHistorySourceOffset = checkpoint.SourceOffset
	c.conversationHistoryNextSourceIndex = checkpoint.NextSourceIndex
	c.conversationHistoryMu.Unlock()
	c.compactionMu.Lock()
	c.compactionState = state
	c.compactionBranchID = branchID
	c.lastCompactionSummary = cloneStructuredSummary(&generation.Summary)
	c.compactionMu.Unlock()
	return nil
}

func (c *Coordinator) markCompactionRecovery(err error) {
	if c == nil || err == nil {
		return
	}
	redacted := utils.RedactSecrets(err.Error())
	c.compactionMu.Lock()
	c.compactionRecoveryErr = fmt.Errorf("coordinator compaction recovery required: %s", redacted)
	c.compactionMu.Unlock()
	c.markSessionRecovery(redacted)
}

func (c *Coordinator) compactionRecoveryError() error {
	if c == nil {
		return nil
	}
	c.compactionMu.Lock()
	defer c.compactionMu.Unlock()
	return c.compactionRecoveryErr
}

func (c *Coordinator) attestCompactionGeneration(ctx context.Context, generation CompactionGeneration) error {
	if c == nil || c.eventStore == nil {
		return nil
	}
	expected, err := compactionGenerationAttestationEvent(generation)
	if err != nil {
		return err
	}
	if err := ensureCompactionEventIdentity(c.eventStore, expected, func(event RunEvent) bool {
		return compactionAttestationMatches(event, generation)
	}); err != nil && !errors.Is(err, errCompactionEventMissing) {
		return err
	}
	_, err = c.emitEventOnce(compactionGenerationEventKeyPrefix+generation.ID, expected)
	if err != nil {
		return err
	}
	return ensureCompactionEventIdentity(c.eventStore, expected, func(event RunEvent) bool {
		return compactionAttestationMatches(event, generation)
	})
}

func (c *Coordinator) attestCompactionCheckpoint(ctx context.Context, checkpoint ConversationCompactionCheckpoint, generation CompactionGeneration) error {
	if c == nil || c.eventStore == nil {
		return nil
	}
	expected, err := compactionCheckpointAttestationEvent(checkpoint, generation)
	if err != nil {
		return err
	}
	if err := ensureCompactionEventIdentity(c.eventStore, expected, func(event RunEvent) bool {
		return compactionCheckpointAttestationMatches(event, checkpoint, generation)
	}); err != nil && !errors.Is(err, errCompactionEventMissing) {
		return err
	}
	_, err = c.emitEventOnce(compactionCheckpointEventKeyPrefix+digestCompactionCheckpoint(checkpoint), expected)
	if err != nil {
		return err
	}
	return ensureCompactionEventIdentity(c.eventStore, expected, func(event RunEvent) bool {
		return compactionCheckpointAttestationMatches(event, checkpoint, generation)
	})
}

func compactionGenerationAttestationEvent(generation CompactionGeneration) (RunEvent, error) {
	payload, err := json.Marshal(CompactionReference{GenerationID: generation.ID, BranchID: generation.BranchID, Checksum: generation.Checksum})
	if err != nil {
		return RunEvent{}, err
	}
	return RunEvent{
		ID: compactionGenerationEventID(generation.ID), Type: compactionGenerationEventType, Actor: "coordinator", BranchID: generation.BranchID,
		Payload: payload,
	}, nil
}

func compactionCheckpointAttestationEvent(checkpoint ConversationCompactionCheckpoint, generation CompactionGeneration) (RunEvent, error) {
	if checkpointAttestationMode(checkpoint) != compactionCheckpointAttestationMode {
		return RunEvent{}, fmt.Errorf("checkpoint has invalid attestation mode %q", checkpoint.AttestationMode)
	}
	if checkpoint.EventID != compactionCheckpointEventID(checkpoint) {
		return RunEvent{}, errors.New("checkpoint attestation identity does not match checkpoint payload")
	}
	payload, err := json.Marshal(CompactionCheckpointReference{
		BranchID: checkpoint.BranchID, GenerationID: generation.ID,
		GenerationChecksum: generation.Checksum, CheckpointDigest: digestCompactionCheckpoint(checkpoint),
	})
	if err != nil {
		return RunEvent{}, err
	}
	return RunEvent{
		ID: checkpoint.EventID, Type: compactionCheckpointEventType, Actor: "coordinator", BranchID: checkpoint.BranchID,
		Payload: payload,
	}, nil
}

func appendCompactionGenerationAttestation(es *EventStore, generation CompactionGeneration) error {
	expected, err := compactionGenerationAttestationEvent(generation)
	if err != nil {
		return err
	}
	if err := ensureCompactionEventIdentity(es, expected, func(event RunEvent) bool {
		return compactionAttestationMatches(event, generation)
	}); err != nil {
		if !errors.Is(err, errCompactionEventMissing) {
			return err
		}
		expected.IdempotencyKey = compactionGenerationEventKeyPrefix + generation.ID
		if _, err := es.AppendPersisted(expected); err != nil {
			return err
		}
	}
	return ensureCompactionEventIdentity(es, expected, func(event RunEvent) bool {
		return compactionAttestationMatches(event, generation)
	})
}

func appendCompactionCheckpointAttestation(es *EventStore, checkpoint ConversationCompactionCheckpoint, generation CompactionGeneration) error {
	expected, err := compactionCheckpointAttestationEvent(checkpoint, generation)
	if err != nil {
		return err
	}
	if err := ensureCompactionEventIdentity(es, expected, func(event RunEvent) bool {
		return compactionCheckpointAttestationMatches(event, checkpoint, generation)
	}); err != nil {
		if !errors.Is(err, errCompactionEventMissing) {
			return err
		}
		expected.IdempotencyKey = compactionCheckpointEventKeyPrefix + digestCompactionCheckpoint(checkpoint)
		if _, err := es.AppendPersisted(expected); err != nil {
			return err
		}
	}
	return ensureCompactionEventIdentity(es, expected, func(event RunEvent) bool {
		return compactionCheckpointAttestationMatches(event, checkpoint, generation)
	})
}

func ensureCompactionEventIdentity(es *EventStore, expected RunEvent, matches func(RunEvent) bool) error {
	events, err := es.ReadEvents()
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.ID != expected.ID {
			continue
		}
		if !matches(event) {
			return fmt.Errorf("event %q does not match its deterministic compaction attestation", expected.ID)
		}
		return nil
	}
	return fmt.Errorf("%w: %q was not persisted", errCompactionEventMissing, expected.ID)
}

func compactionRangesClaimedByLineage(state *ConversationCompactionState, predecessorID string) []CompactionRange {
	var claimed []CompactionRange
	seen := make(map[string]bool)
	for predecessorID != "" && !seen[predecessorID] {
		seen[predecessorID] = true
		predecessor, ok := state.Generations[predecessorID]
		if !ok {
			break
		}
		claimed = append(claimed, predecessor.SourceRanges...)
		predecessorID = predecessor.PredecessorID
	}
	return mergeCompactionRanges(claimed)
}

// reconcileCompactionState repairs a missing reference-only attestation and
// rejects all ambiguity before the next provider call.
func (c *Coordinator) reconcileCompactionState(ctx context.Context, branchID string) error {
	if c == nil || c.session == nil {
		return nil
	}
	state, exists, err := LoadConversationCompactionState(c.session.Workspace)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, ok := state.Branches[branchID]; !ok {
		return nil
	}
	if c.eventStore == nil {
		return errors.New("cannot reconcile canonical compaction without an event store")
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return err
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return fmt.Errorf("load session tree for compaction reconciliation: %w", err)
	}
	missing, err := validateCompactionBranchContext(state, branchID, events, tree, "")
	if err != nil {
		return err
	}
	for _, gap := range missing {
		if gap.generationMissing {
			if err := c.attestCompactionGeneration(ctx, gap.generation); err != nil {
				return fmt.Errorf("attest missing compaction generation %q: %w", gap.generation.ID, err)
			}
		}
		if gap.checkpointMissing {
			if err := c.attestCompactionCheckpoint(ctx, gap.checkpoint, gap.generation); err != nil {
				return fmt.Errorf("attest missing compaction checkpoint %q: %w", gap.checkpoint.EventID, err)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// The append is only repair evidence. Re-read the durable lineage and run
	// the same contextual validator before any history can be restored.
	events, err = c.eventStore.ReadEvents()
	if err != nil {
		return fmt.Errorf("re-read event store after compaction reconciliation: %w", err)
	}
	if _, err := validateCompactionBranchContext(state, branchID, events, tree, ""); err != nil {
		return fmt.Errorf("verify compaction reconciliation: %w", err)
	}
	return nil
}

type compactionAttestationGap struct {
	generation        CompactionGeneration
	checkpoint        ConversationCompactionCheckpoint
	generationMissing bool
	checkpointMissing bool
}

type compactionIndexedEvent struct {
	event RunEvent
	index int
}

// validateCompactionBranchContext binds immutable checkpoints to the durable
// event lineage. A missing snapshot event is returned as a repairable gap;
// every other provenance gap is recovery-required.
func validateCompactionBranchContext(state *ConversationCompactionState, branchID string, events []RunEvent, tree *SessionTree, throughEventID string) ([]compactionAttestationGap, error) {
	if state == nil {
		return nil, errors.New("canonical compaction state is nil")
	}
	branchID, lineage, lineageIndexes, cutoff, err := compactionValidationLineage(events, tree, branchID, throughEventID)
	if err != nil {
		return nil, err
	}

	allEventsByID := make(map[string]RunEvent, len(events))
	for _, event := range events {
		allEventsByID[event.ID] = event
	}
	generationEvents, checkpointEvents, err := indexCompactionAttestations(state, lineage, cutoff)
	if err != nil {
		return nil, err
	}

	checkpoints := state.Checkpoints[branchID]
	missing := make([]compactionAttestationGap, 0)
	for _, checkpoint := range checkpoints {
		gap, include, err := validateCompactionCheckpointContext(state, branchID, checkpoint, lineageIndexes, allEventsByID, generationEvents, checkpointEvents, throughEventID, cutoff)
		if err != nil {
			return nil, err
		}
		if include {
			missing = append(missing, gap)
		}
	}
	sort.SliceStable(missing, func(i, j int) bool {
		return missing[i].generation.CreatedAt.Before(missing[j].generation.CreatedAt)
	})
	return missing, nil
}

func compactionValidationLineage(events []RunEvent, tree *SessionTree, branchID, throughEventID string) (string, []RunEvent, map[string]int, int, error) {
	if branchID == "" {
		branchID = "main"
	}
	lineage := FilterEventsForBranch(events, tree, branchID)
	lineageIndexes := make(map[string]int, len(lineage))
	for index, event := range lineage {
		lineageIndexes[event.ID] = index
	}
	cutoff := len(lineage) - 1
	if throughEventID != "" {
		index, ok := lineageIndexes[throughEventID]
		if !ok {
			return "", nil, nil, 0, fmt.Errorf("event %q is not in branch %q lineage", throughEventID, branchID)
		}
		cutoff = index
	}
	return branchID, lineage, lineageIndexes, cutoff, nil
}

func indexCompactionAttestations(state *ConversationCompactionState, lineage []RunEvent, cutoff int) (map[string][]compactionIndexedEvent, map[string]RunEvent, error) {
	generationEvents := make(map[string][]compactionIndexedEvent)
	checkpointEvents := make(map[string]RunEvent)
	limit := cutoff + 1
	if limit < 0 {
		limit = 0
	}
	for index, event := range lineage[:limit] {
		if event.Type != compactionGenerationEventType {
			if event.Type != compactionCheckpointEventType {
				continue
			}
			ref, err := compactionCheckpointReferenceFromEvent(event)
			if err != nil {
				return nil, nil, err
			}
			if effectiveEventBranchID(event) != ref.BranchID {
				return nil, nil, errors.New("compaction checkpoint attestation has cross-branch event identity")
			}
			generation, ok := state.Generations[ref.GenerationID]
			if !ok || generation.BranchID != ref.BranchID || ref.GenerationChecksum != generation.Checksum {
				return nil, nil, fmt.Errorf("compaction checkpoint attestation for generation %q does not match canonical generation", ref.GenerationID)
			}
			checkpoint, ok := findCompactionCheckpoint(state, ref)
			if !ok || checkpointAttestationMode(checkpoint) != compactionCheckpointAttestationMode {
				return nil, nil, fmt.Errorf("compaction checkpoint attestation %q does not match canonical checkpoint", event.ID)
			}
			if event.ID != compactionCheckpointEventID(checkpoint) {
				return nil, nil, fmt.Errorf("compaction checkpoint attestation %q has the wrong deterministic identity", event.ID)
			}
			checkpointEvents[event.ID] = event
			continue
		}
		ref, err := compactionReferenceFromEvent(event)
		if err != nil {
			return nil, nil, err
		}
		generation, ok := state.Generations[ref.GenerationID]
		if !ok {
			return nil, nil, fmt.Errorf("compaction generation %q attestation references an unknown generation", ref.GenerationID)
		}
		if effectiveEventBranchID(event) != ref.BranchID {
			return nil, nil, fmt.Errorf("compaction generation %q event has cross-branch attestation", ref.GenerationID)
		}
		if generation.BranchID != ref.BranchID {
			return nil, nil, fmt.Errorf("compaction generation %q has cross-branch attestation", ref.GenerationID)
		}
		if ref.Checksum != generation.Checksum {
			return nil, nil, fmt.Errorf("compaction generation %q has checksum-mismatched attestation", ref.GenerationID)
		}
		generationEvents[ref.GenerationID] = append(generationEvents[ref.GenerationID], compactionIndexedEvent{event: event, index: index})
	}
	return generationEvents, checkpointEvents, nil
}

func validateCompactionCheckpointContext(state *ConversationCompactionState, branchID string, checkpoint ConversationCompactionCheckpoint, lineageIndexes map[string]int, allEventsByID map[string]RunEvent, generationEvents map[string][]compactionIndexedEvent, checkpointEvents map[string]RunEvent, throughEventID string, cutoff int) (compactionAttestationGap, bool, error) {
	generation, generationExists := state.Generations[checkpoint.GenerationID]
	if !generationExists || generation.BranchID != branchID {
		return compactionAttestationGap{}, false, fmt.Errorf("checkpoint %q references an invalid generation", branchID)
	}
	checkpointIndex, exists := lineageIndexes[checkpoint.EventID]
	if throughEventID != "" && exists && checkpointIndex > cutoff {
		return compactionAttestationGap{}, false, nil
	}
	if throughEventID != "" && !exists {
		// A checkpoint after the requested time-travel point is not part of
		// this contextual validation. Reconciliation of the active branch
		// still validates every checkpoint without this cutoff.
		return compactionAttestationGap{}, false, nil
	}
	mode := checkpointAttestationMode(checkpoint)
	if mode != compactionCheckpointAttestationMode {
		return compactionAttestationGap{}, false, fmt.Errorf("checkpoint %q has missing or invalid attestation mode %q", branchID, mode)
	}
	expectedEventID := compactionCheckpointEventID(checkpoint)
	gap := compactionAttestationGap{generation: generation, checkpoint: checkpoint}
	if checkpoint.EventID != expectedEventID {
		return compactionAttestationGap{}, false, fmt.Errorf("checkpoint %q has an identity-mismatched attestation event", branchID)
	}
	if !exists {
		if _, outside := allEventsByID[expectedEventID]; outside {
			return compactionAttestationGap{}, false, fmt.Errorf("checkpoint %q deterministic attestation exists outside branch lineage", branchID)
		}
		gap.checkpointMissing = true
	} else if _, ok := checkpointEvents[checkpoint.EventID]; !ok {
		return compactionAttestationGap{}, false, fmt.Errorf("checkpoint %q deterministic attestation event %q does not exactly match checkpoint", branchID, checkpoint.EventID)
	}

	matchedGeneration := false
	for _, candidate := range generationEvents[generation.ID] {
		if candidate.index > checkpointIndex || (candidate.event.BranchID != checkpoint.BranchID && effectiveEventBranchID(candidate.event) != checkpoint.BranchID) {
			continue
		}
		if candidate.event.ID != compactionGenerationEventID(generation.ID) {
			continue
		}
		matchedGeneration = true
		break
	}
	if !matchedGeneration {
		if !exists {
			if _, outside := allEventsByID[compactionGenerationEventID(generation.ID)]; outside {
				return compactionAttestationGap{}, false, fmt.Errorf("compaction generation %q deterministic attestation exists outside branch lineage", generation.ID)
			}
			gap.generationMissing = true
			gap.checkpointMissing = true
		} else {
			return compactionAttestationGap{}, false, fmt.Errorf("checkpoint %q generation %q has no matching attestation at or before event %q", branchID, generation.ID, checkpoint.EventID)
		}
	}
	return gap, gap.generationMissing || gap.checkpointMissing, nil
}

func findCompactionCheckpoint(state *ConversationCompactionState, reference CompactionCheckpointReference) (ConversationCompactionCheckpoint, bool) {
	for _, checkpoint := range state.Checkpoints[reference.BranchID] {
		if checkpoint.GenerationID == reference.GenerationID && digestCompactionCheckpoint(checkpoint) == reference.CheckpointDigest {
			return checkpoint, true
		}
	}
	return ConversationCompactionCheckpoint{}, false
}

func compactionReferenceFromEvent(event RunEvent) (CompactionReference, error) {
	var ref CompactionReference
	if err := json.Unmarshal(event.Payload, &ref); err != nil || ref.GenerationID == "" || ref.BranchID == "" || ref.Checksum == "" {
		return CompactionReference{}, errors.New("compaction generation attestation is malformed")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &fields); err != nil || len(fields) != 3 {
		return CompactionReference{}, errors.New("compaction generation attestation is malformed")
	}
	return ref, nil
}

func compactionAttestationMatches(event RunEvent, generation CompactionGeneration) bool {
	if event.ID != compactionGenerationEventID(generation.ID) || event.Type != compactionGenerationEventType {
		return false
	}
	ref, err := compactionReferenceFromEvent(event)
	return err == nil && ref == (CompactionReference{GenerationID: generation.ID, BranchID: generation.BranchID, Checksum: generation.Checksum}) && effectiveEventBranchID(event) == generation.BranchID
}

func compactionCheckpointReferenceFromEvent(event RunEvent) (CompactionCheckpointReference, error) {
	var ref CompactionCheckpointReference
	if event.Type != compactionCheckpointEventType || json.Unmarshal(event.Payload, &ref) != nil || ref.BranchID == "" || ref.GenerationID == "" || ref.GenerationChecksum == "" || ref.CheckpointDigest == "" {
		return CompactionCheckpointReference{}, errors.New("compaction checkpoint attestation is malformed")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(event.Payload, &fields) != nil || len(fields) != 4 {
		return CompactionCheckpointReference{}, errors.New("compaction checkpoint attestation is malformed")
	}
	return ref, nil
}

func compactionCheckpointAttestationMatches(event RunEvent, checkpoint ConversationCompactionCheckpoint, generation CompactionGeneration) bool {
	if event.ID != compactionCheckpointEventID(checkpoint) || event.Type != compactionCheckpointEventType || effectiveEventBranchID(event) != checkpoint.BranchID {
		return false
	}
	ref, err := compactionCheckpointReferenceFromEvent(event)
	return err == nil && ref == (CompactionCheckpointReference{
		BranchID: checkpoint.BranchID, GenerationID: generation.ID, GenerationChecksum: generation.Checksum,
		CheckpointDigest: digestCompactionCheckpoint(checkpoint),
	})
}

// MaterializeCompactionBranch gives a fork its own checkpoint and generation
// lineage. When forkEventID is supplied, only canonical compaction state
// attested in the parent lineage through that event is materialized. No
// mutable history slice is shared with the parent branch.
func MaterializeCompactionBranch(workspace, parentBranchID, childBranchID string, forkEventIDs ...string) error {
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		return err
	}
	forkEventID := ""
	if len(forkEventIDs) > 0 {
		forkEventID = strings.TrimSpace(forkEventIDs[0])
	}
	state = cloneCompactionState(state)
	if state == nil {
		return errors.New("clone canonical compaction state")
	}
	if state.Checkpoints == nil {
		state.Checkpoints = make(map[string][]ConversationCompactionCheckpoint)
	}
	parent, ok, err := latestCompactionCheckpointThroughEvent(workspace, parentBranchID, forkEventID, state)
	if err != nil {
		return err
	}
	if !ok {
		// A fork before the first compaction must not inherit the parent's
		// current compaction projection through BranchState or a mutable head.
		delete(state.Branches, childBranchID)
		delete(state.Checkpoints, childBranchID)
		return SaveConversationCompactionState(workspace, state)
	}

	ids := compactionGenerationAncestry(state, parent.GenerationID)
	remap := make(map[string]string, len(ids))
	for _, id := range ids {
		generation := state.Generations[id]
		newID := nextCompactionGenerationID(childBranchID)
		remap[id] = newID
		generation.ID = newID
		generation.BranchID = childBranchID
		generation.PredecessorID = remap[generation.PredecessorID]
		generation.Checksum = digestGeneration(generation)
		state.Generations[newID] = generation
	}
	childGenerationID := remap[parent.GenerationID]
	attestationMode := checkpointAttestationMode(parent)
	if attestationMode != compactionCheckpointAttestationMode {
		return fmt.Errorf("checkpoint %q has invalid attestation mode %q", parentBranchID, attestationMode)
	}
	parent.GenerationID = childGenerationID
	parent.BranchID = childBranchID
	parent.AttestationMode = attestationMode
	parent.EventID = ""
	// The checkpoint is bound to the child generation's deterministic event. Its
	// contents are the complete post-compaction snapshot, including retained
	// tail messages and per-message provenance; reconciliation appends the
	// matching child attestation after the child branch is durably registered.
	parent.History = cloneMessages(parent.History)
	parent.SourceCounts = append([]int(nil), parent.SourceCounts...)
	parent.SourceRanges = cloneSourceRanges(parent.SourceRanges)
	parent.HistoryDigest = digestMessages(parent.History)
	parent.EventID = compactionCheckpointEventID(parent)
	state.Branches[childBranchID] = parent
	state.Checkpoints[childBranchID] = []ConversationCompactionCheckpoint{parent}
	return SaveConversationCompactionState(workspace, state)
}

func compactionGenerationAncestry(state *ConversationCompactionState, generationID string) []string {
	var reverse []string
	seen := make(map[string]bool)
	for generationID != "" && !seen[generationID] {
		seen[generationID] = true
		generation, ok := state.Generations[generationID]
		if !ok {
			break
		}
		reverse = append(reverse, generationID)
		generationID = generation.PredecessorID
	}
	for left, right := 0, len(reverse)-1; left < right; left, right = left+1, right-1 {
		reverse[left], reverse[right] = reverse[right], reverse[left]
	}
	return reverse
}

func latestCompactionCheckpointThroughEvent(workspace, branchID, forkEventID string, state *ConversationCompactionState) (ConversationCompactionCheckpoint, bool, error) {
	checkpoints := state.Checkpoints[branchID]
	if forkEventID == "" {
		if len(checkpoints) > 0 {
			return checkpoints[len(checkpoints)-1], true, nil
		}
		checkpoint, ok := state.Branches[branchID]
		return checkpoint, ok, nil
	}

	es, err := OpenEventStore(workspace)
	if err != nil {
		return ConversationCompactionCheckpoint{}, false, fmt.Errorf("open event store for compaction fork: %w", err)
	}
	defer func() { _ = es.Close() }()
	events, err := es.ReadEvents()
	if err != nil {
		return ConversationCompactionCheckpoint{}, false, fmt.Errorf("read event store for compaction fork: %w", err)
	}
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return ConversationCompactionCheckpoint{}, false, fmt.Errorf("load session tree for compaction fork: %w", err)
	}
	lineage := FilterEventsForBranch(events, tree, branchID)
	forkIndex := -1
	for index, event := range lineage {
		if event.ID == forkEventID {
			forkIndex = index
			break
		}
	}
	if forkIndex < 0 {
		return ConversationCompactionCheckpoint{}, false, fmt.Errorf("fork event %q is not in branch %q lineage", forkEventID, branchID)
	}
	if _, err := validateCompactionBranchContext(state, branchID, events, tree, forkEventID); err != nil {
		return ConversationCompactionCheckpoint{}, false, fmt.Errorf("validate compaction state through fork event: %w", err)
	}
	eventIndex := make(map[string]int, len(lineage))
	for index, event := range lineage {
		eventIndex[event.ID] = index
	}
	var selected ConversationCompactionCheckpoint
	selectedIndex := -1
	for _, checkpoint := range checkpoints {
		if checkpointAttestationMode(checkpoint) == compactionCheckpointAttestationMode && checkpoint.EventID != compactionCheckpointEventID(checkpoint) {
			return ConversationCompactionCheckpoint{}, false, fmt.Errorf("checkpoint %q has an identity-mismatched attestation event", branchID)
		}
		index, exists := eventIndex[checkpoint.EventID]
		if !exists || index > forkIndex || index < selectedIndex {
			continue
		}
		selected = checkpoint
		selectedIndex = index
	}
	return selected, selectedIndex >= 0, nil
}

func sumCompactionRanges(ranges []CompactionRange) int {
	total := 0
	for _, sourceRange := range ranges {
		total += sourceRange.MsgCount
	}
	return trimMaxInt(1, total)
}
