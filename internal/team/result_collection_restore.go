package team

import (
	"context"
	"path/filepath"

	"github.com/kjelly/hufu/internal/tools"
)

type resultCollectionEvidenceKey struct{}

// restoreCollectionContext reopens only the submission lease already persisted
// by dispatch or an explicit operator grant. Original work identity is retained.
func (c *Coordinator) restoreCollectionContext(ctx context.Context, expected *ResultCollectionCheckpoint) (context.Context, error) {
	if expected == nil {
		return nil, errResultCollectionStale
	}
	item := c.todoItemByID(expected.TaskID)
	receipt := collectionReceipt(item, expected)
	if receipt == nil || item.OccurrenceRevision != expected.SubmissionRevision || item.DispatchID != expected.SubmissionDispatchID {
		return nil, errResultCollectionStale
	}
	pinned, err := collectionDigest(expected)
	if err != nil {
		return nil, err
	}
	current, err := collectionDigest(receipt.ResultCollection)
	if err != nil || current != pinned {
		return nil, errResultCollectionStale
	}
	work, err := c.loadCollectionWork(ctx, item, expected)
	if err != nil {
		return nil, err
	}
	workspace, err := filepath.Abs(c.session.Workspace)
	if err != nil || workspace != work.Workspace {
		return nil, errResultCollectionStale
	}
	identity := submitResultRuntimeIdentity{RunID: expected.SourceRunID, TaskID: expected.TaskID, Attempt: expected.Attempt, Agent: expected.Agent,
		OccurrenceRevision: expected.SubmissionRevision, DispatchID: expected.SubmissionDispatchID}
	if active, ok := c.activeTaskResultOccurrence(item.ID); ok {
		if !sameTaskResultOccurrence(active, identity) {
			return nil, errResultCollectionStale
		}
	} else {
		resuming, _ := ctx.Value(sessionResumeKey{}).(bool)
		if !resuming {
			return nil, errResultCollectionStale
		}
		if err := c.activateTaskDispatch(identity); err != nil {
			return nil, err
		}
	}
	evidence, err := c.restoreTranscriptEvidence(ctx, &work.Receipt, work.Receipt.RunID)
	if err != nil {
		return nil, err
	}
	ctx = withSubmitResultRuntimeIdentity(ctx, identity)
	ctx = context.WithValue(ctx, todoIDKey{}, expected.TaskID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, expected.Attempt)
	ctx = context.WithValue(ctx, tools.AgentNameKey, expected.Agent)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: expected.SourceRunID, TaskID: expected.TaskID, Attempt: expected.Attempt,
		AgentName: expected.Agent, AgentRole: "worker", ModelExecutionID: expected.ModelExecutionID})
	ctx = context.WithValue(ctx, taskTranscriptKey{}, (*taskTranscript)(nil))
	ctx = context.WithValue(ctx, resultCollectionEvidenceKey{}, evidence)
	if work.Receipt.ArtifactScope != nil {
		ctx = context.WithValue(ctx, artifactAccessScopeKey, cloneArtifactAccessScope(work.Receipt.ArtifactScope))
	}
	if work.Receipt.ContextManifest != nil {
		manifest, err := authoritativeTaskContextManifest(item, expected.Attempt, expected.ModelExecutionID)
		if err != nil {
			return nil, err
		}
		if manifest.Fingerprint != work.Receipt.ContextManifest.Fingerprint {
			return nil, errResultCollectionStale
		}
		ctx = withInvocationMetadata(ctx, invocationMetadataFromManifest(manifest))
	}
	return ctx, nil
}
