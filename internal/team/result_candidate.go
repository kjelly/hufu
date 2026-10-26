package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/kjelly/hufu/internal/utils"
)

const EventTaskResultCandidate EventType = "task_result_candidate"
const resultCandidateMaxBytes = 256 << 10

// ResultCandidate is rejected submission evidence, never a TaskResult. Its
// immutable bytes live in CAS; this descriptor is carried by canonical events.
type ResultCandidate struct {
	Version      int                     `json:"version"`
	BranchID     string                  `json:"branch_id"`
	SourceRunID  string                  `json:"source_run_id"`
	TaskID       string                  `json:"task_id"`
	Agent        string                  `json:"agent"`
	Attempt      int                     `json:"attempt"`
	ContractID   string                  `json:"contract_id"`
	ContractHash string                  `json:"contract_hash"`
	InputsID     string                  `json:"inputs_id"`
	InputsHash   string                  `json:"inputs_hash"`
	EvidenceHash string                  `json:"evidence_hash"`
	ParentSHA256 string                  `json:"parent_sha256,omitempty"`
	ArtifactID   string                  `json:"artifact_id"`
	SHA256       string                  `json:"sha256"`
	Issues       []ResultSubmissionIssue `json:"issues"`
	Truncated    bool                    `json:"truncated,omitzero"`
}

func cloneResultCandidate(c *ResultCandidate) *ResultCandidate {
	if c == nil {
		return nil
	}
	copy := *c
	copy.Issues = slices.Clone(c.Issues)
	for i := range copy.Issues {
		copy.Issues[i].Expected = slices.Clone(copy.Issues[i].Expected)
	}
	return &copy
}

func (c *Coordinator) resultCandidateEvidenceHash(item *TodoItem) (string, error) {
	// Hash the actual accepted results, including bindings/receipts, rather
	// than accepting a model-supplied list of source hashes.
	var sources []*TodoItem
	for _, id := range resultDependencyIDs(item) {
		source := c.todoItemByID(id)
		if source == nil || source.Status != TaskDone || source.TypedResult == nil {
			return "", fmt.Errorf("candidate source %s is unavailable", id)
		}
		if err := c.validateAcceptedDependencyPayload(source); err != nil {
			return "", err
		}
		sources = append(sources, source)
	}
	data, err := json.Marshal(sources, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Coordinator) saveResultCandidate(ctx context.Context, taskID, raw string, issues []ResultSubmissionIssue, truncated bool, base *ResultCandidate) error {
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, taskID)
	if err != nil {
		return err
	}
	c.taskProjectionMu.Lock()
	defer c.taskProjectionMu.Unlock()
	controller := c.occurrenceController(taskID)
	controller.projectionMu.Lock()
	defer controller.projectionMu.Unlock()
	snapshot := controller.snapshot()
	item := c.todoItemByID(taskID)
	if item == nil || item.ResultContract == nil || !snapshot.opened || snapshot.result != nil || snapshot.reserved || !sameTaskResultOccurrence(snapshot.identity, identity) || item.OccurrenceRevision != identity.OccurrenceRevision || item.DispatchID != identity.DispatchID {
		return errTaskResultStale
	}
	tx, err := c.beginTaskResultSubmission(identity)
	if err != nil {
		return err
	}
	defer func() { _ = tx.rollback() }()
	if base != nil && (item.ResultCandidate == nil || item.ResultCandidate.SHA256 != base.SHA256 || item.ResultCandidate.Version != base.Version) {
		return errTaskResultStale
	}
	if len(raw) > resultCandidateMaxBytes {
		return fmt.Errorf("result candidate exceeds %d bytes", resultCandidateMaxBytes)
	}
	// Redact JSON once before hashing and storing. Event payloads carry only
	// the reference; subsequent event redaction cannot rewrite these bytes.
	redacted, err := utils.RedactJSON([]byte(raw))
	if err != nil {
		return err
	}
	if _, err := decodeUniqueJSON(redacted); err != nil {
		return err
	}
	evidenceHash, err := c.resultCandidateEvidenceHash(item)
	if err != nil {
		return err
	}
	sourceRun := identity.RunID
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.RunID != "" {
		sourceRun = item.ExecutionReceipt.RunID
	}
	candidate := &ResultCandidate{Version: 1, BranchID: c.activeBranchID(), SourceRunID: sourceRun, TaskID: taskID, Agent: item.Agent, Attempt: identity.Attempt, ContractID: item.ResultContract.ID, ContractHash: item.ResultContract.SchemaSHA256, InputsID: item.RunInputSnapshotID, InputsHash: item.RunInputSnapshotHash, EvidenceHash: evidenceHash, Issues: issues, Truncated: truncated}
	if item.ResultCandidate != nil {
		candidate.Version = item.ResultCandidate.Version + 1
		candidate.ParentSHA256 = item.ResultCandidate.SHA256
	}
	body, err := json.Marshal(struct {
		Binding   *ResultCandidate `json:"binding"`
		Arguments jsontext.Value   `json:"arguments"`
	}{candidate, redacted}, json.Deterministic(true))
	if err != nil {
		return err
	}
	if len(body) > resultCandidateMaxBytes*2 {
		return fmt.Errorf("candidate envelope exceeds byte limit")
	}
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		return err
	}
	put, err := store.Put(ctx, PutArtifactRequest{Content: body, Path: "result-candidate.json", Kind: "result_candidate", MediaType: "application/json", RunID: sourceRun, TaskID: taskID, Agent: item.Agent, Attempt: identity.Attempt})
	if err != nil {
		return err
	}
	candidate.ArtifactID, candidate.SHA256 = put.ID, put.SHA256
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	if c.hasDurableEventJournal() {
		_, err = c.EventJournal().Append(ctx, RunEvent{Type: string(EventTaskResultCandidate), RunID: c.executionRunID, BranchID: c.activeBranchID(), Actor: item.Agent, TaskID: taskID, IdempotencyKey: "result-candidate:" + c.activeBranchID() + ":" + taskID + ":" + candidate.SHA256, Payload: data})
		if err != nil {
			return err
		}
	}
	item.ResultCandidate = candidate
	return c.taskTracker.TodoList().TryApplyProjectedItem(item)
}

func (c *Coordinator) loadResultCandidate(ctx context.Context, item *TodoItem, candidate *ResultCandidate) (map[string]any, error) {
	if candidate == nil || item == nil || item.ResultContract == nil || candidate.BranchID != c.activeBranchID() || candidate.TaskID != item.ID || candidate.Agent != item.Agent || candidate.ContractID != item.ResultContract.ID || candidate.ContractHash != item.ResultContract.SchemaSHA256 || candidate.InputsID != item.RunInputSnapshotID || candidate.InputsHash != item.RunInputSnapshotHash || candidate.Version < 1 {
		return nil, errTaskResultStale
	}
	if item.ResultCandidate == nil || item.ResultCandidate.SHA256 != candidate.SHA256 || item.ResultCandidate.Version != candidate.Version {
		return nil, errTaskResultStale
	}
	if _, _, err := c.boundResultContract(item.ID); err != nil {
		return nil, err
	}
	evidenceHash, err := c.resultCandidateEvidenceHash(item)
	if err != nil || evidenceHash != candidate.EvidenceHash {
		return nil, fmt.Errorf("candidate evidence changed: %w", errors.Join(err, errTaskResultStale))
	}
	if item.ExecutionReceipt != nil && (item.ExecutionReceipt.RunID != candidate.SourceRunID || item.ExecutionReceipt.Attempt != candidate.Attempt) {
		return nil, errTaskResultStale
	}
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		return nil, err
	}
	reader, err := store.Open(ctx, candidate.ArtifactID)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, resultCandidateMaxBytes*2+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if len(data) > resultCandidateMaxBytes*2 || hex.EncodeToString(sum[:]) != candidate.SHA256 || candidate.ArtifactID != "sha256-"+candidate.SHA256 {
		return nil, fmt.Errorf("candidate content hash mismatch")
	}
	var stored struct {
		Binding   *ResultCandidate `json:"binding"`
		Arguments jsontext.Value   `json:"arguments"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	expected := cloneResultCandidate(candidate)
	expected.ArtifactID = ""
	expected.SHA256 = ""
	a, _ := json.Marshal(expected, json.Deterministic(true))
	b, _ := json.Marshal(stored.Binding, json.Deterministic(true))
	if string(a) != string(b) {
		return nil, fmt.Errorf("candidate binding mismatch")
	}
	value, err := decodeResultContractJSON(stored.Arguments)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("candidate must be an object")
	}
	return object, nil
}

func validateResultCandidateEvent(event RunEvent) error {
	var candidate ResultCandidate
	if err := json.Unmarshal(event.Payload, &candidate); err != nil {
		return err
	}
	if event.TaskID == "" || candidate.TaskID != event.TaskID || candidate.Version < 1 || candidate.ArtifactID != "sha256-"+candidate.SHA256 || len(candidate.SHA256) != 64 {
		return fmt.Errorf("invalid result candidate identity")
	}
	return nil
}
