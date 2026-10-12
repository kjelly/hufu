package team

import (
	"context"
	"slices"

	"github.com/kjelly/hufu/internal/agent"
)

// InvocationMetadata is request-scoped execution identity carried only in the
// Go context. Tool calls use it to derive child context requests without
// weakening the phase, trigger, retry, role, or model-execution contract that
// admitted the current model stream.
//
// It intentionally contains no prompt, tool arguments, transcript, or other
// context content. Durable projections are carried by ContextInjectionManifest.
type InvocationMetadata struct {
	RunID                     string
	TaskID                    string
	AgentName                 string
	AgentRole                 string
	ModelExecutionID          string
	Attempt                   int
	Phase                     Phase
	Trigger                   ContextTrigger
	Purpose                   string
	ParentRequestID           string
	ParentManifestFingerprint string
	EnvironmentFingerprint    string
	TouchedPaths              []string
}

type contextInvocationKey struct{}
type contextDiagnosticInvocationKey struct{}
type contextAuxiliaryDiagnosticInvocationKey struct{}

func withInvocationMetadata(ctx context.Context, metadata InvocationMetadata) context.Context {
	if metadata.Attempt < 1 {
		metadata.Attempt = 1
	}
	if ctx.Value(contextAuxiliaryDiagnosticInvocationKey{}) != nil {
		ctx = context.WithValue(ctx, contextAuxiliaryDiagnosticInvocationKey{}, (*InvocationMetadata)(nil))
	}
	return context.WithValue(ctx, contextInvocationKey{}, metadata)
}

func invocationMetadataFromContext(ctx context.Context) (InvocationMetadata, bool) {
	metadata, ok := ctx.Value(contextInvocationKey{}).(InvocationMetadata)
	return metadata, ok
}

// InvocationMetadataSnapshot exposes copied, runtime-owned identity to
// request-bound diagnostics without giving them a mutable context slice.
func InvocationMetadataSnapshot(ctx context.Context) (InvocationMetadata, bool) {
	if diagnostic, ok := ctx.Value(contextAuxiliaryDiagnosticInvocationKey{}).(*InvocationMetadata); ok && diagnostic != nil {
		metadata := *diagnostic
		metadata.TouchedPaths = slices.Clone(metadata.TouchedPaths)
		return metadata, true
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	if !ok {
		metadata, ok = ctx.Value(contextDiagnosticInvocationKey{}).(InvocationMetadata)
	}
	metadata.TouchedPaths = slices.Clone(metadata.TouchedPaths)
	return metadata, ok
}

// This diagnostic identity never becomes the authority consumed by tools or
// accounting. Coordinator cost attempts retain their existing execution key.
func (c *Coordinator) withCoordinatorDiagnosticIdentity(ctx context.Context) context.Context {
	if !agent.ProviderTransportObserverEnabled(ctx) {
		return ctx
	}
	data := c.SessionData()
	if data == nil {
		return ctx
	}
	for index := len(data.CoordinatorContextManifests) - 1; index >= 0; index-- {
		manifest := data.CoordinatorContextManifests[index]
		if manifest.Agent != "coordinator" || manifest.RunID != c.contextRunID() || !manifest.ModelCalled || (manifest.Trigger != ContextTriggerCoordinatorStart && manifest.Trigger != ContextTriggerContinuation) {
			continue
		}
		return context.WithValue(ctx, contextDiagnosticInvocationKey{}, InvocationMetadata{
			RunID: manifest.RunID, TaskID: manifest.TaskID, Attempt: manifest.Attempt,
			AgentName: manifest.Agent, AgentRole: manifest.AgentRole, ModelExecutionID: manifest.ModelExecutionID,
			Phase: manifest.Phase, Trigger: manifest.Trigger, Purpose: manifest.Purpose,
			ParentRequestID: manifest.RequestID, ParentManifestFingerprint: manifest.Fingerprint,
			EnvironmentFingerprint: manifest.Environment,
		})
	}
	return ctx
}

func invocationMetadataFromRequest(request ContextRequest, manifest ContextInjectionManifest) InvocationMetadata {
	return InvocationMetadata{
		RunID:                     request.RunID,
		TaskID:                    request.TaskID,
		AgentName:                 request.AgentName,
		AgentRole:                 request.AgentRole,
		ModelExecutionID:          request.ModelExecutionID,
		Attempt:                   request.Attempt,
		Phase:                     request.Phase,
		Trigger:                   request.Trigger,
		Purpose:                   request.Purpose,
		ParentRequestID:           request.RequestID,
		ParentManifestFingerprint: manifest.Fingerprint,
		EnvironmentFingerprint:    request.EnvironmentFingerprint,
		TouchedPaths:              slices.Clone(request.TouchedPaths),
	}
}
