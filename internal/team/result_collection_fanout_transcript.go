package team

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

func isCodexFanoutTranscript(id string) bool {
	return strings.HasPrefix(id, "provider-transcript-")
}

func codexFanoutTranscriptKey(modelExecutionID string, attempt int) string {
	return fmt.Sprintf("external-fanout-transcript:%s:%d", modelExecutionID, attempt)
}

func stageCodexFanoutTranscript(workspace, provider string, request AttemptRequest, content string) (string, error) {
	if request.ArtifactScope == nil || workspace != request.ArtifactScope.StoreRoot ||
		provider != request.ExecutionTarget.Backend || request.modelExecutionID == "" {
		return "", rejectLocalResult(RepairFailureSource, errResultCollectionStale)
	}
	key := codexFanoutTranscriptKey(request.modelExecutionID, request.Attempt)
	if err := stageFanoutBytes(context.Background(), workspace, key, []byte(content), codexDefaultMaxTranscript); err != nil {
		return "", rejectLocalResultPersistence(err)
	}
	return codexTranscriptArtifactID(provider, request.RunID, request.TaskID, request.Attempt, content), nil
}

func readCodexFanoutTranscript(ctx context.Context, workspace string, receipt *ExecutionReceipt) ([]byte, error) {
	body, err := readFanoutStage(ctx, workspace, codexFanoutTranscriptKey(receipt.ModelExecutionID, receipt.Attempt), codexDefaultMaxTranscript)
	if err != nil {
		return nil, err
	}
	return body, verifyCodexFanoutTranscriptIdentity(receipt, body)
}

func verifyCodexFanoutTranscriptIdentity(receipt *ExecutionReceipt, body []byte) error {
	id := codexTranscriptArtifactID(receipt.ExecutionTarget.Backend, receipt.RunID, receipt.TaskID, receipt.Attempt, string(body))
	if receipt.ProviderTranscriptRef != id {
		return fmt.Errorf("codex transcript does not match the accepted attempt: %w", errResultCollectionStale)
	}
	return nil
}

func verifyPublishedCodexFanoutTranscript(ctx context.Context, workspace string, receipt *ExecutionReceipt) error {
	store, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		return err
	}
	ref, err := codexTranscriptArtifactRef(ctx, store, receipt.ProviderTranscriptRef, receipt.ExecutionTarget.Backend, receipt.RunID, receipt.TaskID, receipt.Attempt)
	if err != nil {
		return err
	}
	if ref.Bytes > codexDefaultMaxTranscript {
		return ErrReadLimit
	}
	if !ref.CreatedAt.Equal(receipt.FinishedAt) {
		return fmt.Errorf("codex transcript timestamp differs from the accepted attempt: %w", errResultCollectionStale)
	}
	file, err := openBoundedArtifact(filepath.Join(store.root, "data", ref.ID))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	body, err := readBoundedRegularFile(ctx, file, int(ref.Bytes))
	if err != nil {
		return err
	}
	if int64(len(body)) != ref.Bytes || sha256HexBytes(body) != ref.SHA256 {
		return fmt.Errorf("codex transcript data conflicts with its public CAS metadata: %w", errResultCollectionStale)
	}
	return verifyCodexFanoutTranscriptIdentity(receipt, body)
}

func (c *Coordinator) validateFanoutProviderTranscript(ctx context.Context, receipt *ExecutionReceipt, pending bool) error {
	if !isCodexFanoutTranscript(receipt.ProviderTranscriptRef) {
		backend, err := c.ExecutionRegistry().ResolveBackend(receipt.ExecutionTarget.Backend)
		if err != nil {
			return err
		}
		if adapter, ok := backend.(*AgentExecutionBackend); ok {
			if _, isCodex := adapter.provider.(*CodexSubagentProvider); isCodex {
				return fmt.Errorf("codex fanout receipt lacks its provider transcript: %w", errResultCollectionStale)
			}
		}
		return nil
	}
	root := c.artifactStoreRootPath()
	if pending {
		_, err := readCodexFanoutTranscript(ctx, root, receipt)
		return err
	}
	return verifyPublishedCodexFanoutTranscript(ctx, root, receipt)
}
