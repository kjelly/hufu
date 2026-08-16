package team

import "context"

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
}

type contextInvocationKey struct{}

func withInvocationMetadata(ctx context.Context, metadata InvocationMetadata) context.Context {
	if metadata.Attempt < 1 {
		metadata.Attempt = 1
	}
	return context.WithValue(ctx, contextInvocationKey{}, metadata)
}

func invocationMetadataFromContext(ctx context.Context) (InvocationMetadata, bool) {
	metadata, ok := ctx.Value(contextInvocationKey{}).(InvocationMetadata)
	return metadata, ok
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
	}
}
