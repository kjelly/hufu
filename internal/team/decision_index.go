package team

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	DecisionIndexSchemaVersion = 1
)

// DecisionIndexEntry is one decision's cross-run row.
type DecisionIndexEntry struct {
	SchemaVersion int    `json:"schema_version"`
	DecisionID    string `json:"decision_id"`
	RunID         string `json:"run_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	Profile       string `json:"profile,omitempty"`
	EvidenceHash  string `json:"evidence_hash,omitempty"`

	Question    string  `json:"question,omitempty"`
	FinalOption string  `json:"final_option,omitempty"`
	Probability float64 `json:"probability,omitempty"`

	// ForecastRequired and FalsificationConditions record what would have to be
	// observed for this decision to be judged wrong, so a later resolution has
	// something to resolve against (spec §40).
	ForecastRequired        bool     `json:"forecast_required,omitempty"`
	FalsificationConditions []string `json:"falsification_conditions,omitempty"`

	// RecordDigest addresses the durable DecisionRecord artifact.
	RecordDigest string `json:"record_digest,omitempty"`
	RecordPath   string `json:"record_path,omitempty"`

	Stale       bool      `json:"stale,omitempty"`
	StaleReason string    `json:"stale_reason,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`

	// Outcome is set by resolution. Its presence is what makes a decision
	// resolved; the decision record itself is never edited (spec §35).
	Outcome *DecisionOutcomeRecord `json:"outcome,omitempty"`

	// IndexedAt is when this row was written, used to project the latest row
	// per decision from the append-only file.
	IndexedAt time.Time `json:"indexed_at"`
}

// Resolved reports whether an outcome has been recorded for this decision.
func (e DecisionIndexEntry) Resolved() bool { return e.Outcome != nil }

// DecisionIndex is an append-only index file scoped to one workspace.
type DecisionIndex struct {
	path string
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
	return &DecisionIndex{path: path}, nil
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
	if entry.IndexedAt.IsZero() {
		entry.IndexedAt = time.Now().UTC()
	}

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
	entry, found, err := i.Get(decisionID)
	if err != nil {
		return DecisionIndexEntry{}, err
	}
	if !found {
		return DecisionIndexEntry{}, fmt.Errorf("decision %q is not in the index at %s", decisionID, i.path)
	}
	if entry.Resolved() {
		return entry, fmt.Errorf("decision %q was already resolved as %q at %s",
			decisionID, entry.Outcome.ResolvedOutcome, entry.Outcome.ResolvedAt.Format(time.RFC3339))
	}

	outcome.DecisionID = decisionID
	if outcome.Forecast == 0 {
		outcome.Forecast = entry.Probability
	}
	if err := ValidateOutcome(&outcome); err != nil {
		return DecisionIndexEntry{}, err
	}
	if outcome.ResolvedAt.IsZero() {
		outcome.ResolvedAt = time.Now().UTC()
	}

	updated := entry
	updated.Outcome = &outcome
	updated.IndexedAt = time.Time{}
	if err := i.Append(updated); err != nil {
		return DecisionIndexEntry{}, err
	}
	return updated, nil
}

// IndexEntryFor builds an index row from a finalized decision record.
func IndexEntryFor(record DecisionRecord, question string, forecastRequired bool, recordRef ArtifactRef) DecisionIndexEntry {
	return DecisionIndexEntry{
		SchemaVersion:           DecisionIndexSchemaVersion,
		DecisionID:              record.ID,
		RunID:                   record.RunID,
		TaskID:                  record.TaskID,
		Profile:                 record.Profile,
		EvidenceHash:            record.EvidenceHash,
		Question:                question,
		FinalOption:             record.FinalOption,
		Probability:             record.Probability,
		ForecastRequired:        forecastRequired,
		FalsificationConditions: record.FalsificationConditions,
		RecordDigest:            recordRef.SHA256,
		RecordPath:              recordRef.Path,
		Stale:                   record.Stale,
		StaleReason:             record.StaleReason,
		CreatedAt:               record.CreatedAt,
	}
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
