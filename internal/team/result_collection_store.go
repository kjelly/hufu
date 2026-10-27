package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/utils"
)

func collectionBindingDigest(checkpoint *ResultCollectionCheckpoint) (string, error) {
	return collectionDigest(struct {
		Version         int                       `json:"version"`
		Branch          string                    `json:"branch"`
		Run             string                    `json:"run"`
		Task            string                    `json:"task"`
		Agent           string                    `json:"agent"`
		Attempt         int                       `json:"attempt"`
		ModelExecution  string                    `json:"model_execution"`
		FanoutLeaf      bool                      `json:"fanout_leaf"`
		Target          execution.ExecutionTarget `json:"target"`
		Revision        int                       `json:"revision"`
		Dispatch        string                    `json:"dispatch"`
		Policy          string                    `json:"policy"`
		Contract        string                    `json:"contract"`
		InputsID        string                    `json:"inputs_id"`
		InputsHash      string                    `json:"inputs_hash"`
		Evidence        string                    `json:"evidence"`
		Workspace       string                    `json:"workspace"`
		CollectorSchema int                       `json:"collector_schema"`
	}{checkpoint.Version, checkpoint.BranchID, checkpoint.SourceRunID, checkpoint.TaskID, checkpoint.Agent,
		checkpoint.Attempt, checkpoint.ModelExecutionID, checkpoint.FanoutLeaf, checkpoint.ExecutionTarget, checkpoint.WorkOccurrenceRevision,
		checkpoint.WorkDispatchID, checkpoint.PolicySHA256, checkpoint.TaskContractSHA256, checkpoint.InputsID,
		checkpoint.InputsSHA256, checkpoint.EvidenceSHA256, checkpoint.WorkspaceIdentitySHA256, checkpoint.CollectorSchemaRevision})
}

func (c *Coordinator) collectionWorkspaceIdentity(receipt *ExecutionReceipt, workspace string) (string, error) {
	root, err := filepath.Abs(c.artifactStoreRootPath())
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(realRoot, realWorkspace)
	if err != nil || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("result collection workspace escapes its control root")
	}
	// This hashes logical workspace identity, not filesystem contents. Isolated
	// worlds retain their existing before/after snapshots and apply conditions.
	return collectionDigest(struct {
		ControlRoot   string               `json:"control_root"`
		RealRoot      string               `json:"real_control_root"`
		Workspace     string               `json:"workspace"`
		RealWorkspace string               `json:"real_workspace"`
		World         string               `json:"world"`
		Before        string               `json:"before"`
		After         string               `json:"after"`
		Scope         *ArtifactAccessScope `json:"scope"`
	}{root, realRoot, workspace, realWorkspace, receipt.ExecutionWorldID, receipt.WorkspaceBefore, receipt.WorkspaceAfter, receipt.ArtifactScope})
}

func (c *Coordinator) collectionPolicy(target execution.ExecutionTarget) (*ExecutionPolicySnapshot, error) {
	policy, err := c.canonicalExecutionPolicySnapshot()
	if err != nil || policy == nil {
		return nil, fmt.Errorf("result collection requires a durable execution policy: %w", errors.Join(err, errResultCollectionStale))
	}
	if current := c.ExecutionPolicySnapshot(); current != nil && current.ConfigurationHash != policy.ConfigurationHash {
		return nil, fmt.Errorf("result collection execution policy drift: %w", errResultCollectionStale)
	}
	if !slices.ContainsFunc(policy.Backends, func(backend ExecutionBackendPolicySnapshot) bool {
		return backend.Backend == target.Backend && backend.Kind == string(execution.BackendKindLLM)
	}) || !slices.ContainsFunc(policy.ModelRoutes, func(route ExecutionModelRouteSnapshot) bool {
		selector, parseErr := execution.ParseExecutionSelector(route.Model)
		return parseErr == nil && route.Backend == target.Backend && selector.Model == target.Model
	}) {
		return nil, fmt.Errorf("result collection target is not an admitted local LLM: %w", errResultCollectionStale)
	}
	return policy, nil
}

func (c *Coordinator) putCollectionBytes(ctx context.Context, checkpoint *ResultCollectionCheckpoint, kind string, body []byte, limit int) (ResultCollectionRef, error) {
	if len(body) == 0 || len(body) > limit {
		return ResultCollectionRef{}, fmt.Errorf("result collection %s exceeds its byte limit", kind)
	}
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		return ResultCollectionRef{}, err
	}
	put, err := store.Put(ctx, PutArtifactRequest{Content: body, Path: "result-collection-" + kind + ".json", Kind: "result_collection", MediaType: "application/json",
		RunID: checkpoint.SourceRunID, TaskID: checkpoint.TaskID, Attempt: checkpoint.Attempt, Agent: checkpoint.Agent})
	if err != nil {
		return ResultCollectionRef{}, err
	}
	return ResultCollectionRef{ArtifactID: put.ID, SHA256: put.SHA256, Bytes: int64(len(body))}, nil
}

func (c *Coordinator) readCollectionBytes(ctx context.Context, ref ResultCollectionRef, limit int) ([]byte, error) {
	if !validCollectionRef(&ref) || ref.Bytes > int64(limit) {
		return nil, fmt.Errorf("invalid result collection CAS reference")
	}
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		return nil, err
	}
	reader, err := store.Open(ctx, ref.ArtifactID)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	if int64(len(body)) != ref.Bytes || len(body) > limit || hex.EncodeToString(digest[:]) != ref.SHA256 {
		return nil, fmt.Errorf("result collection CAS content mismatch")
	}
	return body, nil
}

func (c *Coordinator) putCollectionBlob(ctx context.Context, checkpoint *ResultCollectionCheckpoint, kind string, data []byte) (*ResultCollectionRef, error) {
	limit := resultCollectionResponseMaxBytes
	if kind == "request" {
		limit = resultCollectionWorkMaxBytes
	}
	if len(data) > limit {
		return nil, fmt.Errorf("result collection %s exceeds %d bytes", kind, limit)
	}
	// Responses can be invalid JSON. Preserve their complete redacted bytes as
	// data rather than requiring successful parsing to save the diagnostic.
	var redacted []byte
	switch kind {
	case "response", "claims":
		redacted = []byte(utils.RedactSecrets(string(data)))
	case "accepted", "request":
		// Full canonical data already passed redaction and binding. Rewriting
		// it here could invalidate immutable runtime-owned payload hashes.
		redacted = data
	default:
		return nil, fmt.Errorf("unknown result collection blob kind %q", kind)
	}
	body, err := json.Marshal(resultCollectionBlob{Version: resultCollectionVersion, WorkSHA256: checkpoint.Work.SHA256, InvocationID: checkpoint.InvocationID, InvocationRunID: checkpoint.InvocationRunID, Kind: kind, Data: redacted}, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	ref, err := c.putCollectionBytes(ctx, checkpoint, kind, body, limit*2)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (c *Coordinator) readCollectionBlob(ctx context.Context, checkpoint *ResultCollectionCheckpoint, kind string, ref *ResultCollectionRef) ([]byte, error) {
	if ref == nil {
		return nil, fmt.Errorf("result collection %s evidence is missing", kind)
	}
	limit := resultCollectionResponseMaxBytes
	if kind == "request" {
		limit = resultCollectionWorkMaxBytes
	}
	body, err := c.readCollectionBytes(ctx, *ref, limit*2)
	if err != nil {
		return nil, err
	}
	var blob resultCollectionBlob
	if err := json.Unmarshal(body, &blob, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if blob.Version != resultCollectionVersion || blob.WorkSHA256 != checkpoint.Work.SHA256 || blob.InvocationID != checkpoint.InvocationID || blob.InvocationRunID != checkpoint.InvocationRunID || blob.Kind != kind || len(blob.Data) > limit {
		return nil, fmt.Errorf("result collection %s binding mismatch: %w", kind, errResultCollectionStale)
	}
	return blob.Data, nil
}
