package team

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
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
		Priority: contextstore.PriorityNormal, Confidence: 1.0,
		Source: contextstore.SourceRef{Type: "legacy-shadow", Ref: source},
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

// rebuildLegacyContextProjections regenerates compatibility Markdown from the
// two canonical lifetimes. It is deliberately called after the SQLite write
// commits: a projection failure never erases canonical knowledge and can be
// repaired by rebuilding projections later.
func (c *Coordinator) rebuildLegacyContextProjections(ctx context.Context) error {
	if c == nil || c.contextRepo == nil || c.session == nil {
		return fmt.Errorf("canonical context repository is unavailable")
	}
	scope := c.contextScope()
	if err := c.contextRepo.RebuildProjection(ctx, scope); err != nil {
		return err
	}
	stmItems, err := c.sharedSessionPromptItems(ctx, scope)
	if err != nil {
		return err
	}
	ltmItems, err := c.contextRepo.QuerySharedPersistentProjection(ctx, scope)
	if err != nil {
		return err
	}
	if err := NewSTMWriter(c.session.Workspace).Update(func(string) string {
		return contextstore.RenderLegacySTMMarkdown(stmItems)
	}); err != nil {
		return err
	}
	c.ltmWriteMu.Lock()
	err = SaveLTM(c.session.Workspace, c.session.Config.Name, contextstore.RenderLegacyLTMMarkdown(ltmItems))
	c.ltmWriteMu.Unlock()
	return err
}

// canonicalPromptMemory returns compatibility-formatted text directly from
// the canonical repository. The existing compiler accepts Markdown-shaped
// text, but it must never reread stm.md or ltm-TEAM.md when SQLite is active:
// those files are projections only.
func (c *Coordinator) canonicalPromptMemory(ctx context.Context) (stm, ltm string, canonical bool, err error) {
	if c == nil || c.contextRepo == nil {
		return "", "", false, nil
	}
	if c.session == nil {
		return "", "", true, fmt.Errorf("canonical context requires a team session")
	}
	scope := c.contextScope()
	stmItems, err := c.sharedSessionPromptItems(ctx, scope)
	if err != nil {
		return "", "", true, err
	}
	ltmItems, err := c.contextRepo.QuerySharedPersistentProjection(ctx, scope)
	if err != nil {
		return "", "", true, err
	}
	return contextstore.RenderLegacySTMMarkdown(stmItems), contextstore.RenderLegacyLTMMarkdown(ltmItems), true, nil
}

func (c *Coordinator) canonicalContextBundle(ctx context.Context) (*CanonicalContextBundle, bool, error) {
	return c.canonicalContextBundleForQuery(ctx, "")
}

func (c *Coordinator) canonicalContextBundleForQuery(ctx context.Context, query string) (*CanonicalContextBundle, bool, error) {
	if c == nil || c.contextRepo == nil {
		return nil, false, nil
	}
	if c.session == nil {
		return nil, true, fmt.Errorf("canonical context requires a team session")
	}
	scope := c.contextScope()
	stm, err := c.sharedSessionPromptItems(ctx, scope)
	if err != nil {
		return nil, true, err
	}
	baseLTM, err := c.contextRepo.QuerySharedPersistentProjection(ctx, scope)
	if err != nil {
		return nil, true, err
	}
	ltm, scores, finalScores, rankErr := c.rankSharedPersistentMemory(ctx, query, baseLTM)
	if rankErr != nil {
		mode := c.session.Config.MemoryLearning.Mode
		if mode == agent.MemoryLearningShadow || mode == agent.MemoryLearningActive {
			redactedErr := contextstore.RedactSecrets(rankErr.Error())
			log.Printf("warning: %s memory ranker degraded to base ordering: %s", mode, redactedErr)
			c.persistMemoryRankingTrace(MemoryRankingTrace{CreatedAt: time.Now().UTC(), Mode: mode, PolicyVersion: c.session.Config.MemoryLearning.PolicyVersion, QueryHash: hashContentKey(query), Error: redactedErr})
			_ = c.emitEvent("observability_degraded", "memory_ranker", "", map[string]any{"component": "memory_learning", "mode": mode, "policy_version": c.session.Config.MemoryLearning.PolicyVersion, "error": redactedErr})
		}
		ltm, scores, finalScores = baseLTM, nil, nil
	}
	return &CanonicalContextBundle{SharedSession: stm, SharedPersistent: ltm, SharedPersistentScores: scores, SharedPersistentFinalScores: finalScores}, true, nil
}

// appendCanonicalContext is the unified memory ingestion path. It appends the
// canonical record first, then regenerates the legacy prompt files solely as
// projections. Callers must not write STM/LTM directly after this returns.
//
// Run-produced shared context is written as a candidate bound to the run, not
// as confirmed knowledge. It is visible to the current run's own prompts (see
// sharedSessionPromptItems) but is promoted to confirmed, prompt-visible
// knowledge only by the accepted finalizer; a failed run's records are rejected
// and never become prompt-visible. Items written outside a run (no active
// executionRunID) keep the legacy confirmed lifecycle.
func (c *Coordinator) appendCanonicalContext(ctx context.Context, kind contextstore.ContextKind, content, source string, metadata map[string]string) error {
	if c == nil || c.contextRepo == nil || c.session == nil {
		return fmt.Errorf("canonical context repository is unavailable")
	}
	sessionID := filepath.Base(c.session.Workspace)
	if c.sessionData != nil && c.sessionData.CreatedAt != "" {
		sessionID = c.sessionData.CreatedAt
	}
	// Stamp the originating run so run-scoped extraction (autoExtractCanonicalLTM)
	// never re-proposes knowledge from a previous failed run under a later
	// accepted run. Items written outside a run keep no run_id and remain
	// eligible for extraction.
	if metadata == nil {
		metadata = map[string]string{}
	}
	lifecycle := contextstore.LifecycleConfirmed
	sourceType := "memory"
	if c.executionRunID != "" {
		metadata["run_id"] = c.executionRunID
		lifecycle = contextstore.LifecycleCandidate
		sourceType = "run_shared_context"
	}
	item := contextstore.ContextItem{
		Kind: kind, Content: content,
		Scope:     contextstore.Scope{ProjectID: c.projectDir, TeamID: c.session.Config.Name, SessionID: sessionID},
		Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal,
		Priority: contextstore.PriorityNormal, Confidence: 1.0,
		Source:    contextstore.SourceRef{Type: sourceType, Ref: source},
		Metadata:  metadata,
		Lifecycle: lifecycle,
	}
	if err := c.contextRepo.Append(ctx, item); err != nil {
		return err
	}
	return c.rebuildLegacyContextProjections(ctx)
}

// sharedSessionPromptItems returns prompt-eligible shared session knowledge:
// confirmed items plus candidates produced by the current run. Candidates from
// other runs — including a previously failed run — are never prompt-visible.
// The caller must already hold a non-nil contextRepo.
func (c *Coordinator) sharedSessionPromptItems(ctx context.Context, scope contextstore.Scope) ([]contextstore.ContextItem, error) {
	items, err := c.contextRepo.QuerySharedSessionProjection(ctx, scope)
	if err != nil {
		return nil, err
	}
	if c.executionRunID == "" {
		return items, nil
	}
	candidates, err := c.contextRepo.Query(ctx, contextstore.RepositoryQuery{
		Scope:             scope,
		Visibility:        contextstore.VisibilityExact,
		IncludeCandidates: true,
		Limit:             100000,
	})
	if err != nil {
		return nil, err
	}
	for _, item := range candidates {
		if item.Lifecycle == contextstore.LifecycleCandidate && item.Metadata["run_id"] == c.executionRunID {
			items = append(items, item)
		}
	}
	return items, nil
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
