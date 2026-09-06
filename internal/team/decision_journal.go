package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// decisionControlPlane is the parent-owned authority used by an isolated
// worker leaf. Its members are immutable after pinning; sharing the handles is
// intentional, while the leaf's worker session remains isolated.
type decisionControlPlane struct {
	artifactStore ArtifactStore
	index         *DecisionIndex
	journal       EventJournal
}

// branchScopedDecisionJournal is the decision-owned view of the canonical
// event journal. The event store remains one global hash chain and keeps its
// existing deduplication semantics; this adapter is the ownership boundary
// that prevents decision reads and decision writes from crossing sibling
// branches.
type branchScopedDecisionJournal struct {
	raw      EventJournal
	tree     *SessionTree
	branchID string
}

// newBranchScopedDecisionJournal pins a journal to one validated active
// session branch. The tree is copied only at the structural level needed by
// FilterEventsForBranch; callers must not mutate it after construction.
func newBranchScopedDecisionJournal(raw EventJournal, tree *SessionTree) (*branchScopedDecisionJournal, error) {
	if raw == nil {
		return nil, fmt.Errorf("branch-scoped decision journal: canonical journal is unavailable")
	}
	branchID, err := validateActiveSessionBranch(tree)
	if err != nil {
		return nil, fmt.Errorf("branch-scoped decision journal: %w", err)
	}
	return &branchScopedDecisionJournal{raw: raw, tree: tree, branchID: branchID}, nil
}

func validateActiveSessionBranch(tree *SessionTree) (string, error) {
	if tree == nil || tree.Branches == nil {
		return "", fmt.Errorf("session tree has no branches")
	}
	branchID := strings.TrimSpace(tree.ActiveBranch)
	if branchID == "" {
		return "", fmt.Errorf("active session branch is empty")
	}

	seen := make(map[string]struct{})
	for current := branchID; current != ""; {
		if _, exists := seen[current]; exists {
			return "", fmt.Errorf("session tree branch lineage contains a cycle at %q", current)
		}
		seen[current] = struct{}{}
		branch, ok := tree.Branches[current]
		if !ok || branch == nil {
			return "", fmt.Errorf("active session branch %q is missing", current)
		}
		if strings.TrimSpace(branch.ID) != current {
			return "", fmt.Errorf("session tree branch key %q does not match branch ID %q", current, branch.ID)
		}
		parentID := strings.TrimSpace(branch.ParentID)
		if parentID != "" {
			parent, ok := tree.Branches[parentID]
			if !ok || parent == nil {
				return "", fmt.Errorf("session tree branch lineage references missing branch %q", parentID)
			}
		}
		current = parentID
	}
	return branchID, nil
}

func (j *branchScopedDecisionJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if j == nil || j.raw == nil {
		return RunEvent{}, fmt.Errorf("branch-scoped decision journal: canonical journal is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return RunEvent{}, err
	}
	if _, err := j.visibleEvents(ctx); err != nil {
		return RunEvent{}, fmt.Errorf("branch-scoped decision journal: validate lineage before append: %w", err)
	}
	if event.BranchID != "" && event.BranchID != j.branchID {
		return RunEvent{}, fmt.Errorf("decision event branch %q conflicts with pinned branch %q", event.BranchID, j.branchID)
	}
	event.BranchID = j.branchID
	if event.IdempotencyKey != "" {
		event.IdempotencyKey = branchDecisionIdempotencyKey(j.branchID, event.IdempotencyKey)
	}
	persisted, err := j.raw.Append(ctx, event)
	if err != nil {
		return RunEvent{}, err
	}
	if persisted.BranchID != j.branchID {
		return RunEvent{}, fmt.Errorf("decision journal append returned branch %q, want pinned branch %q", persisted.BranchID, j.branchID)
	}
	if event.IdempotencyKey != "" && persisted.IdempotencyKey != event.IdempotencyKey {
		return RunEvent{}, fmt.Errorf("decision journal append returned idempotency key %q, want %q", persisted.IdempotencyKey, event.IdempotencyKey)
	}
	return persisted, nil
}

func (j *branchScopedDecisionJournal) ReadEvents(ctx context.Context) ([]RunEvent, error) {
	if j == nil || j.raw == nil {
		return nil, fmt.Errorf("branch-scoped decision journal: canonical journal is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return j.visibleEvents(ctx)
}

func (j *branchScopedDecisionJournal) visibleEvents(ctx context.Context) ([]RunEvent, error) {
	events, err := j.raw.ReadEvents(ctx)
	if err != nil {
		return nil, err
	}
	lineage, err := projectEventsForBranch(events, j.tree, j.branchID)
	if err != nil {
		return nil, err
	}
	return lineage, nil
}

func (j *branchScopedDecisionJournal) VerifyHashChain(ctx context.Context) error {
	if j == nil || j.raw == nil {
		return fmt.Errorf("branch-scoped decision journal: canonical journal is unavailable")
	}
	return j.raw.VerifyHashChain(ctx)
}

func branchDecisionIdempotencyKey(branchID, key string) string {
	return "branch:" + branchID + ":" + key
}

func isCanonicalEventStoreJournal(journal EventJournal, store *EventStore) bool {
	if store == nil || journal == nil {
		return false
	}
	switch typed := journal.(type) {
	case eventStoreJournal:
		return typed.store == store
	case *eventStoreJournal:
		return typed != nil && typed.store == store
	default:
		return false
	}
}

// decisionJournalFor returns one pinned decision view for the coordinator's
// lifetime. A missing session-tree file is the legacy/unscoped main mode; an
// invalid persisted tree fails closed instead of silently projecting another
// branch. Custom injected journals are deliberately returned unchanged.
func (c *Coordinator) decisionJournalFor() (EventJournal, error) {
	if c == nil {
		return nil, nil
	}
	c.decisionControlPlaneMu.Lock()
	control := c.decisionControlPlane
	c.decisionControlPlaneMu.Unlock()
	if control != nil {
		return control.journal, nil
	}
	raw := c.EventJournal()
	if raw == nil {
		return nil, nil
	}
	if c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return raw, nil
	}

	c.decisionJournalMu.Lock()
	defer c.decisionJournalMu.Unlock()
	if c.scopedDecisionJournal != nil {
		return c.scopedDecisionJournal, nil
	}
	path := sessionTreePath(c.session.Workspace)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return raw, nil
		}
		return nil, fmt.Errorf("decision journal: stat session tree: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("decision journal: read session tree: %w", err)
	}
	var tree SessionTree
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("decision journal: decode session tree: %w", err)
	}
	if _, err := validateActiveSessionBranch(&tree); err != nil {
		return nil, fmt.Errorf("decision journal: %w", err)
	}
	if !isCanonicalEventStoreJournal(raw, c.eventStore) {
		return raw, nil
	}
	scoped, err := newBranchScopedDecisionJournal(raw, &tree)
	if err != nil {
		return nil, err
	}
	c.scopedDecisionJournal = scoped
	return scoped, nil
}

// pinDecisionControlPlane resolves all decision-owned resources from the
// parent coordinator before fanout. Leaves must not discover these resources
// from their scratch session because that would create a second CAS/index or
// silently fall back to an unscoped journal.
func (c *Coordinator) pinDecisionControlPlane() (*decisionControlPlane, error) {
	if c == nil {
		return nil, fmt.Errorf("decision control plane: coordinator is unavailable")
	}
	// Resolve the journal before taking the control-plane mutex because
	// decisionJournalFor also consults that mutex. The second check below makes
	// initialization and publication a single winner under the mutex.
	journal, err := c.decisionJournalFor()
	if err != nil {
		return nil, err
	}
	if journal == nil {
		return nil, fmt.Errorf("decision control plane: journal is unavailable")
	}
	c.decisionControlPlaneMu.Lock()
	defer c.decisionControlPlaneMu.Unlock()
	control := c.decisionControlPlane
	if control != nil {
		return control, nil
	}
	store := c.decisionStore
	if store == nil {
		if c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
			return nil, fmt.Errorf("decision control plane: parent workspace is unavailable")
		}
		store, err = NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
		if err != nil {
			return nil, fmt.Errorf("decision control plane: artifact store: %w", err)
		}
		c.decisionStore = store
	}
	index := c.decisionIndexProjection
	if index == nil {
		if c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
			return nil, fmt.Errorf("decision control plane: parent workspace is unavailable")
		}
		index, err = OpenDecisionIndex(c.session.Workspace)
		if err != nil {
			return nil, fmt.Errorf("decision control plane: decision index: %w", err)
		}
		c.decisionIndexProjection = index
	}
	index.SetEventJournal(journal)
	index.SetArtifactStore(store)
	control = &decisionControlPlane{artifactStore: store, index: index, journal: journal}
	c.decisionControlPlane = control
	return control, nil
}

func sessionTreePath(workspace string) string {
	return filepath.Join(workspace, sessionTreeFile)
}

var _ EventJournal = (*branchScopedDecisionJournal)(nil)
