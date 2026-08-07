package team

import (
	"context"
	"fmt"
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
		return
	}
	// Keep the human-readable projections derived from the canonical store.
	// This is best-effort so a projection filesystem failure never loses the
	// already-committed canonical item.
	if err := c.contextRepo.RebuildProjection(context.Background(), item.Scope); err != nil {
		log.Printf("warning: context projection rebuild failed (%s): %s", source, contextstore.RedactSecrets(err.Error()))
	}
}

// appendCanonicalContext is the unified memory ingestion path. It appends the
// canonical record first, then regenerates the legacy prompt files solely as
// projections. Callers must not write STM/LTM directly after this returns.
func (c *Coordinator) appendCanonicalContext(ctx context.Context, kind contextstore.ContextKind, content, source string, metadata map[string]string) error {
	if c == nil || c.contextRepo == nil || c.session == nil {
		return fmt.Errorf("canonical context repository is unavailable")
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
		Source:   contextstore.SourceRef{Type: "memory", Ref: source},
		Metadata: metadata,
	}
	if err := c.contextRepo.Append(ctx, item); err != nil {
		return err
	}
	if err := c.contextRepo.RebuildProjection(ctx, item.Scope); err != nil {
		return err
	}
	items, err := c.contextRepo.QuerySharedProjection(ctx, item.Scope)
	if err != nil {
		return err
	}
	if err := NewSTMWriter(c.session.Workspace).Update(func(string) string {
		return contextstore.RenderLegacySTMMarkdown(items)
	}); err != nil {
		return err
	}
	c.ltmWriteMu.Lock()
	err = SaveLTM(c.session.Workspace, c.session.Config.Name, contextstore.RenderLegacyLTMMarkdown(items))
	c.ltmWriteMu.Unlock()
	return err
}

func (c *Coordinator) contextScope() contextstore.Scope {
	sessionID := filepath.Base(c.session.Workspace)
	if c.sessionData != nil && c.sessionData.CreatedAt != "" {
		sessionID = c.sessionData.CreatedAt
	}
	// Shared canonical context deliberately remains branch-neutral. Branch
	// isolation is applied only to private worker memory via resolveWorkerScope.
	return contextstore.Scope{ProjectID: c.projectDir, TeamID: c.session.Config.Name, SessionID: sessionID}
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
