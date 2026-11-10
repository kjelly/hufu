package team

import (
	"context"
	"fmt"
)

// Every provider entry validates the event-first policy and immutable
// attempt capability before preflight, filesystem preparation or process I/O.
func (p *CodexSubagentProvider) validateAttemptEntry(ctx context.Context, request AttemptRequest) error {
	if p == nil || p.coordinator == nil {
		return fmt.Errorf("codex attempt requires a coordinator")
	}
	// RunAttempt is also a public provider entrypoint used by direct callers
	// and focused integrations. It must establish the same event-first policy
	// boundary as Coordinator.Run/ExecuteTasks before it validates a request,
	// runs preflight, or can start a child process.
	if err := p.coordinator.AdmitExecutionPolicy(); err != nil {
		return codexFail(CodexFailureUnavailable, fmt.Errorf("admit execution policy before Codex attempt: %w", err))
	}
	// ArtifactScope is the provider's immutable evidence capability for this
	// exact attempt. Reject an absent or mismatched capability before even
	// running preflight, resolving shared state, preparing an execution world,
	// or starting the app-server. Otherwise a malformed request could still
	// perform workspace/process side effects before canonicalization rejects
	// its result.
	if err := p.coordinator.validateCodexMediaAttempt(ctx, request); err != nil {
		return err
	}
	if err := validateAttemptArtifactScope(request); err != nil {
		return codexFail(CodexFailureUnavailable, fmt.Errorf("codex attempt artifact scope: %w", err))
	}
	return nil
}
