package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

var sessionMetadataOnly bool

// sessionForkOutput is the --json output of `session fork`.
type sessionForkOutput struct {
	BranchID              string `json:"branch_id"`
	ParentBranchID        string `json:"parent_branch_id"`
	ForkEventID           string `json:"fork_event_id,omitempty"`
	WorkspaceSnapshotID   string `json:"workspace_snapshot_id,omitempty"`
	WorkspaceRootTreeHash string `json:"workspace_root_tree_hash,omitempty"`
	WorkspaceState        string `json:"workspace_state,omitempty"`
	Materialized          bool   `json:"materialized,omitempty"`
	MetadataOnly          bool   `json:"metadata_only"`
}

// sessionCheckoutOutput is the --json output of `session checkout`.
type sessionCheckoutOutput struct {
	BranchID               string `json:"branch_id"`
	PreviousBranchID       string `json:"previous_branch_id"`
	CheckoutSaveSnapshotID string `json:"checkout_save_snapshot_id,omitempty"`
	WorkspaceSnapshotID    string `json:"workspace_snapshot_id,omitempty"`
	WorkspaceRootTreeHash  string `json:"workspace_root_tree_hash,omitempty"`
	FilesWritten           int    `json:"files_written,omitempty"`
	FilesDeleted           int    `json:"files_deleted,omitempty"`
	MetadataOnly           bool   `json:"metadata_only"`
}

func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func writeJSON(value any) error {
	return json.NewEncoder(os.Stdout).Encode(value)
}

// openSessionStores loads the session tree and a writable event store. The
// event store is required (E3): a fork or checkout without it would record
// no lineage and silently skip the session rebuild.
func openSessionStores(workspace string) (*team.SessionTree, *team.EventStore, error) {
	st, err := team.LoadSessionTree(workspace)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load session tree: %w", err)
	}
	es, err := team.OpenEventStore(workspace)
	if err != nil {
		return nil, nil, fmt.Errorf("open event store: %w", err)
	}
	return st, es, nil
}

// workspaceOps opens the version store and runs startup recovery before any
// workspace-aware session operation (§19.2).
func workspaceOps(ctx context.Context, vs *versionedSession, st *team.SessionTree, es *team.EventStore) (*team.WorkspaceSessionOps, func(), error) {
	store, err := vs.version.OpenStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	ops := &team.WorkspaceSessionOps{Version: vs.version, Workspace: vs.workspace, Tree: st, Events: es, Store: store}
	report, err := team.RecoverWorkspaceOperations(ctx, ops)
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("workspace recovery: %w", err)
	}
	if !report.Empty() {
		stderrLog("workspace recovery: %d snapshot(s) settled, %d operation(s) completed, %d failed, %d incomplete fork branch(es) removed\n",
			len(report.Snapshots), len(report.Completed), len(report.Failed), len(report.Removed))
	}
	return ops, func() { _ = store.Close() }, nil
}

func defaultForkName(target string) string {
	if sessionForkName != "" {
		return sessionForkName
	}
	if target != "" {
		return fmt.Sprintf("fork-%s", target)
	}
	return "fork-branch"
}

func runSessionFork(cmd *cobra.Command, args []string) error {
	ctx := commandContext(cmd)
	vs, err := openVersionedSession(ctx, true, "fork")
	if err != nil {
		return err
	}
	defer func() { _ = vs.Close() }()
	st, es, err := openSessionStores(vs.workspace)
	if err != nil {
		return err
	}
	defer func() { _ = es.Close() }()
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	name := defaultForkName(target)
	if vs.version.Required() && !sessionMetadataOnly {
		ops, closeStore, opsErr := workspaceOps(ctx, vs, st, es)
		if opsErr != nil {
			return opsErr
		}
		defer closeStore()
		result, forkErr := ops.Fork(ctx, name, target)
		if forkErr != nil {
			return forkErr
		}
		output := sessionForkOutput{
			BranchID: result.Branch.ID, ParentBranchID: result.Branch.ParentID, ForkEventID: result.Branch.ForkEventID,
			WorkspaceSnapshotID: string(result.Snapshot.ID), WorkspaceRootTreeHash: result.Snapshot.RootTreeHash,
			WorkspaceState: "available", Materialized: result.Materialized,
		}
		if sessionJSON {
			return writeJSON(output)
		}
		fmt.Printf("✓ Forked new branch %q (ID: %s) from %q and checked out.\n", result.Branch.Name, result.Branch.ID, result.Branch.ParentID)
		fmt.Printf("  workspace: %s (tree %s)\n", result.Snapshot.ID, shortHash(result.Snapshot.RootTreeHash))
		return nil
	}
	return forkMetadataOnly(vs, st, es, name, target)
}

// forkMetadataOnly is the pre-versioning fork: session lineage only.
func forkMetadataOnly(vs *versionedSession, st *team.SessionTree, es *team.EventStore, name, target string) error {
	ws := vs.workspace
	team.SnapshotBranchState(ws, st, st.ActiveBranch)
	b, err := st.CreateBranch(name, target, es)
	if err != nil {
		return fmt.Errorf("failed to fork branch: %w", err)
	}
	if err := team.MaterializeCompactionBranch(ws, b.ParentID, b.ID, b.ForkEventID); err != nil {
		return fmt.Errorf("failed to materialize compaction state for branch %q: %w", b.ID, err)
	}
	// Snapshot the live session into the new branch so the fork starts from
	// the current state rather than a potentially stale parent snapshot.
	team.SnapshotBranchState(ws, st, b.ID)
	// Rebuild session.json for the new branch (its lineage = parent's
	// up-to-fork + live state), then activate it.
	if err := team.RebuildSessionForBranch(ws, st, es, b.ID); err != nil {
		return fmt.Errorf("failed to rebuild session for new branch: %w", err)
	}
	st.ActiveBranch = b.ID
	if err := team.SaveSessionTree(ws, st); err != nil {
		return fmt.Errorf("failed to save session tree: %w", err)
	}
	output := sessionForkOutput{BranchID: b.ID, ParentBranchID: b.ParentID, ForkEventID: b.ForkEventID, MetadataOnly: true}
	if vs.version.Active() {
		output.WorkspaceState = "unavailable"
	}
	if sessionJSON {
		return writeJSON(output)
	}
	fmt.Printf("✓ Forked new branch %q (ID: %s) from %q and checked out.\n", b.Name, b.ID, b.ParentID)
	if vs.version.Active() {
		fmt.Println("  workspace_state: unavailable (metadata-only fork)")
	}
	return nil
}

func runSessionCheckout(cmd *cobra.Command, args []string) error {
	ctx := commandContext(cmd)
	vs, err := openVersionedSession(ctx, true, "checkout")
	if err != nil {
		return err
	}
	defer func() { _ = vs.Close() }()
	st, es, err := openSessionStores(vs.workspace)
	if err != nil {
		return err
	}
	defer func() { _ = es.Close() }()
	if !vs.version.Required() {
		return checkoutMetadataOnly(vs, st, es, args[0])
	}
	ops, closeStore, err := workspaceOps(ctx, vs, st, es)
	if err != nil {
		return err
	}
	defer closeStore()
	if sessionMetadataOnly {
		branch, resolveErr := st.ResolveCheckoutBranch(args[0], es)
		if resolveErr != nil {
			return resolveErr
		}
		if _, _, hasHead, headErr := ops.Store.GetBranchHead(ctx, vs.version.WorkspaceID, branch.ID); headErr != nil {
			return headErr
		} else if hasHead {
			return fmt.Errorf("branch %q has a workspace snapshot; --metadata-only is only for legacy branches", branch.ID)
		}
		return checkoutMetadataOnly(vs, st, es, args[0])
	}
	result, err := ops.Checkout(ctx, args[0])
	if err != nil {
		return err
	}
	output := sessionCheckoutOutput{
		BranchID: result.Branch.ID, PreviousBranchID: result.PreviousID,
		WorkspaceSnapshotID: string(result.Head.ID), WorkspaceRootTreeHash: result.Head.RootTreeHash,
		FilesWritten: result.Materialized.FilesWritten, FilesDeleted: result.Materialized.FilesDeleted,
	}
	if result.Saved != nil {
		output.CheckoutSaveSnapshotID = string(result.Saved.ID)
	}
	if sessionJSON {
		return writeJSON(output)
	}
	fmt.Printf("✓ Checked out branch %q (%s).\n", result.Branch.Name, result.Branch.ID)
	fmt.Printf("  workspace: %s (%d written, %d deleted)\n", result.Head.ID, result.Materialized.FilesWritten, result.Materialized.FilesDeleted)
	return nil
}

// checkoutMetadataOnly is the pre-versioning checkout: session projection
// only, the filesystem is untouched.
func checkoutMetadataOnly(vs *versionedSession, st *team.SessionTree, es *team.EventStore, target string) error {
	ws := vs.workspace
	previous := st.ActiveBranch
	// Snapshot current branch's live state before switching.
	team.SnapshotBranchState(ws, st, st.ActiveBranch)
	b, err := st.CheckoutBranch(target, es)
	if err != nil {
		return err
	}
	// Rebuild session.json for the target branch so the next run resumes it.
	if err := team.RebuildSessionForBranch(ws, st, es, b.ID); err != nil {
		return fmt.Errorf("failed to rebuild session for branch %q: %w", b.ID, err)
	}
	if err := team.SaveSessionTree(ws, st); err != nil {
		return fmt.Errorf("failed to save session tree: %w", err)
	}
	if sessionJSON {
		return writeJSON(sessionCheckoutOutput{BranchID: b.ID, PreviousBranchID: previous, MetadataOnly: true})
	}
	fmt.Printf("✓ Checked out branch %q (%s).\n", b.Name, b.ID)
	if vs.version.Active() {
		fmt.Println("  workspace_state: unavailable (metadata-only checkout; files were not changed)")
	}
	return nil
}

// sessionListBranch adds workspace fields to a branch in `session list --json`.
type sessionListBranch struct {
	*team.SessionBranch
	WorkspaceSnapshotID string `json:"workspace_snapshot_id,omitempty"`
	WorkspaceState      string `json:"workspace_state,omitempty"`
}

// branchWorkspaceStates reads each branch's head when versioning is active.
func branchWorkspaceStates(ctx context.Context, vs *versionedSession, branches []*team.SessionBranch) (map[string]sessionListBranch, error) {
	states := make(map[string]sessionListBranch, len(branches))
	for _, b := range branches {
		states[b.ID] = sessionListBranch{SessionBranch: b}
	}
	if !vs.version.Active() {
		return states, nil
	}
	store, err := versionstore.Open(ctx, versionstore.Options{StateDir: vs.version.StateDir, ReadOnly: true})
	if errors.Is(err, versionstore.ErrStoreNotFound) {
		for id, entry := range states {
			entry.WorkspaceState = "unavailable_legacy"
			states[id] = entry
		}
		return states, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	for id, entry := range states {
		head, _, ok, headErr := store.GetBranchHead(ctx, vs.version.WorkspaceID, id)
		if headErr != nil {
			return nil, headErr
		}
		entry.WorkspaceState = "unavailable_legacy"
		if ok {
			entry.WorkspaceSnapshotID, entry.WorkspaceState = string(head.ID), "available"
		}
		states[id] = entry
	}
	return states, nil
}

func workspaceColumn(entry sessionListBranch) string {
	switch entry.WorkspaceState {
	case "available":
		return "  workspace: " + entry.WorkspaceSnapshotID
	case "":
		return ""
	default:
		return "  workspace: unavailable (legacy branch)"
	}
}

// attachSessionWorkspaceDiff adds the workspace section to `session diff`.
func attachSessionWorkspaceDiff(ctx context.Context, vs *versionedSession, st *team.SessionTree, diff *team.SessionDiff, branchA, branchB string) error {
	if !vs.version.Active() {
		return nil
	}
	store, err := versionstore.Open(ctx, versionstore.Options{StateDir: vs.version.StateDir, ReadOnly: true})
	if errors.Is(err, versionstore.ErrStoreNotFound) {
		diff.WorkspaceDiffUnavailable = "no workspace snapshots have been recorded yet"
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	a, b := st.GetBranch(branchA), st.GetBranch(branchB)
	if a == nil || b == nil {
		return nil
	}
	return team.AttachWorkspaceDiff(ctx, diff, store, vs.version.WorkspaceID, a.ID, b.ID)
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
