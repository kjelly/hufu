package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	fanoutExternalArtifactMaxBytes  = 8 << 20
	fanoutExternalArtifactsMaxBytes = 32 << 20
	fanoutExternalArtifactsMaxCount = 200
)

func validateFanoutExternalArtifactSizes(refs []ArtifactRef) error {
	if len(refs) > fanoutExternalArtifactsMaxCount {
		return fmt.Errorf("too many external fanout artifacts: %w", ErrReadLimit)
	}
	var total int64
	for _, ref := range refs {
		if ref.Bytes < 0 || ref.Bytes > fanoutExternalArtifactMaxBytes {
			return fmt.Errorf("external fanout artifact %q exceeds %d bytes: %w", ref.Path, fanoutExternalArtifactMaxBytes, ErrReadLimit)
		}
		total += ref.Bytes
	}
	if total > fanoutExternalArtifactsMaxBytes {
		return fmt.Errorf("external fanout artifacts exceed %d bytes in total: %w", fanoutExternalArtifactsMaxBytes, ErrReadLimit)
	}
	return nil
}

func externalFanoutArtifactKey(modelExecutionID, id string) string {
	return "external-fanout-artifact:" + modelExecutionID + ":" + id
}

func stageExternalResultArtifacts(ctx context.Context, request AttemptRequest, workspaceRoot string, refs []ArtifactRef) error {
	if err := validateFanoutExternalArtifactSizes(refs); err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	root, err := os.OpenRoot(workspaceRoot)
	if err != nil {
		return rejectNormalizationRecoveryRead(err)
	}
	defer func() { _ = root.Close() }()
	capturedAt := time.Now().UTC()
	for i, ref := range refs {
		body, err := readGroundedExternalResultFile(ctx, root, ref)
		if err != nil {
			return rejectNormalizationRecoveryRead(err)
		}
		ref.Agent = canonicalAttemptAgentName(request)
		digest, err := collectionDigest(ref)
		if err != nil {
			return rejectLocalResult(RepairFailureSource, err)
		}
		ref.ID, ref.ByteSize, ref.CreatedAt = "external-result-"+digest, ref.Bytes, capturedAt
		key := externalFanoutArtifactKey(request.modelExecutionID, ref.ID)
		if err := stageFanoutBytes(ctx, request.ArtifactScope.StoreRoot, key, body, fanoutExternalArtifactMaxBytes); err != nil {
			return rejectLocalResultPersistence(err)
		}
		refs[i] = ref
	}
	return nil
}

func readGroundedExternalResultFile(ctx context.Context, root *os.Root, ref ArtifactRef) ([]byte, error) {
	file, err := openGroundedArtifact(root, filepath.FromSlash(ref.Path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := readBoundedRegularFile(ctx, file, int(ref.Bytes))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != ref.Bytes || sha256HexBytes(body) != ref.SHA256 {
		return nil, fmt.Errorf("external result artifact %q changed after workspace observation", ref.Path)
	}
	return body, nil
}

func readExternalFanoutArtifact(ctx context.Context, workspace string, receipt *ExecutionReceipt, ref ArtifactRef) ([]byte, error) {
	if !strings.HasPrefix(ref.ID, "external-result-") || !validArtifactID(ref.ID) ||
		ref.RunID != receipt.RunID || ref.TaskID != receipt.TaskID || ref.Attempt != receipt.Attempt ||
		ref.Agent != receipt.ProducerID || ref.ByteSize != ref.Bytes || ref.CreatedAt.IsZero() {
		return nil, errResultCollectionStale
	}
	key := externalFanoutArtifactKey(receipt.ModelExecutionID, ref.ID)
	body, err := readFanoutStage(ctx, workspace, key, int(ref.Bytes))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != ref.Bytes || sha256HexBytes(body) != ref.SHA256 {
		return nil, fmt.Errorf("staged external artifact %q hash or size mismatch", ref.ID)
	}
	return body, nil
}

func publishExternalFanoutArtifact(store *FileArtifactStore, ref ArtifactRef, body []byte) error {
	saved, err := store.putFile(PutArtifactRequest{
		ID: ref.ID, Path: ref.Path, Description: ref.Description, Role: ref.Role, Content: body,
		RunID: ref.RunID, TaskID: ref.TaskID, Attempt: ref.Attempt, Agent: ref.Agent, Provider: ref.Provider, CreatedAt: ref.CreatedAt,
	})
	if err == nil && !sameArtifactRef(saved.ArtifactRef, ref) {
		err = errResultCollectionStale
	}
	return err
}
