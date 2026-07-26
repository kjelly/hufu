package team

import (
	"context"
	"log"
	"path/filepath"

	contextstore "github.com/anomalyco/hufu/internal/context"
)

// shadowContextAppend records legacy memory writes in the canonical store.
// It intentionally has no return value: Phase 1 must preserve the legacy
// operation and prompt even when SQLite is unavailable. Failures are emitted
// as events so a future repair command can identify/replay them.
func (c *Coordinator) shadowContextAppend(kind contextstore.ContextKind, content, source string) {
	if c == nil || c.contextRepo == nil || c.session == nil {
		return
	}
	sessionID := filepath.Base(c.session.Workspace)
	if c.sessionData != nil && c.sessionData.CreatedAt != "" {
		sessionID = c.sessionData.CreatedAt
	}
	item := contextstore.ContextItem{
		Kind: kind, Content: content,
		Scope:     contextstore.Scope{ProjectID: c.projectDir, TeamID: c.session.Config.Name, SessionID: sessionID},
		Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal,
		Priority: contextstore.PriorityNormal,
		Source:   contextstore.SourceRef{Type: "legacy-shadow", Ref: source},
	}
	if err := c.contextRepo.Append(context.Background(), item); err != nil {
		// A repository/driver error can echo back the rejected value
		// verbatim, so it is never logged or persisted unredacted, the same
		// as item content.
		redactedErr := contextstore.RedactSecrets(err.Error())
		log.Printf("warning: context shadow write failed (%s): %s", source, redactedErr)
		c.emitEvent("context_shadow_write_error", "coordinator", "", map[string]interface{}{"source": source, "error": redactedErr})
		if perr := contextstore.AppendPendingWrite(c.contextPendingPath(), item, err); perr != nil {
			log.Printf("warning: could not persist pending context write for repair (%s): %s", source, contextstore.RedactSecrets(perr.Error()))
		}
	}
}

// contextPendingPath is where failed shadow writes are durably queued so
// `hufu context repair` (or RepairContextShadowWrites) can replay them later
// without depending on the store that just failed.
func (c *Coordinator) contextPendingPath() string {
	return filepath.Join(c.session.Workspace, "context-pending.jsonl")
}

// RepairContextShadowWrites replays any shadow writes that failed earlier
// (see shadowContextAppend) into the canonical store, draining the pending
// queue of everything that now succeeds. It is safe to call repeatedly.
func (c *Coordinator) RepairContextShadowWrites(ctx context.Context) (recovered, remaining int, err error) {
	if c == nil || c.contextRepo == nil || c.session == nil {
		return 0, 0, nil
	}
	return contextstore.RepairPendingWrites(ctx, c.contextRepo, c.contextPendingPath())
}
