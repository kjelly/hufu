package team

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// WorkspaceDiffSummary is the path-level workspace diff between two branch
// heads (§24). Only paths are reported, never content.
type WorkspaceDiffSummary struct {
	Added       []string `json:"added"`
	Modified    []string `json:"modified"`
	Deleted     []string `json:"deleted"`
	TypeChanged []string `json:"type_changed"`
}

func changePaths(changes []versionstore.PathChange) []string {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	return paths
}

// AttachWorkspaceDiff fills sd's workspace fields by diffing the heads of
// branchA and branchB. A branch without a head (legacy) or a head whose
// objects are gone leaves a reason in WorkspaceDiffUnavailable instead; the
// other diffs are unaffected.
func AttachWorkspaceDiff(ctx context.Context, sd *SessionDiff, store *versionstore.Store, workspaceID, branchA, branchB string) error {
	if sd == nil || store == nil {
		return nil
	}
	heads := make([]versionstore.Snapshot, 0, 2)
	for _, branchID := range []string{branchA, branchB} {
		head, _, ok, err := store.GetBranchHead(ctx, workspaceID, branchID)
		if err != nil {
			return fmt.Errorf("read workspace head of %s: %w", branchID, err)
		}
		if !ok {
			sd.WorkspaceDiffUnavailable = fmt.Sprintf("branch %s has no workspace snapshot (legacy branch)", branchID)
			return nil
		}
		heads = append(heads, head)
	}
	delta, err := store.Diff(ctx, heads[0].ID, heads[1].ID)
	if errors.Is(err, versionstore.ErrWorkspaceSnapshotUnavailable) || errors.Is(err, versionstore.ErrCASObjectMissing) {
		sd.WorkspaceDiffUnavailable = err.Error()
		return nil
	}
	if err != nil {
		return fmt.Errorf("diff workspace heads: %w", err)
	}
	sd.WorkspaceDiff = &WorkspaceDiffSummary{
		Added: changePaths(delta.Added), Modified: changePaths(delta.Modified),
		Deleted: changePaths(delta.Deleted), TypeChanged: changePaths(delta.TypeChanged),
	}
	return nil
}

func (sd *SessionDiff) workspaceDiffEmpty() bool {
	if sd.WorkspaceDiffUnavailable != "" {
		return false
	}
	w := sd.WorkspaceDiff
	return w == nil || len(w.Added)+len(w.Modified)+len(w.Deleted)+len(w.TypeChanged) == 0
}

func (sd *SessionDiff) renderWorkspaceDiff() string {
	if sd.WorkspaceDiffUnavailable != "" {
		return "Workspace:\n  unavailable: " + sd.WorkspaceDiffUnavailable + "\n\n"
	}
	if sd.workspaceDiffEmpty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Workspace:\n")
	for _, group := range []struct {
		marker string
		paths  []string
	}{{"+", sd.WorkspaceDiff.Added}, {"~", sd.WorkspaceDiff.Modified}, {"-", sd.WorkspaceDiff.Deleted}, {"!", sd.WorkspaceDiff.TypeChanged}} {
		for _, path := range group.paths {
			fmt.Fprintf(&b, "  %s %s\n", group.marker, path)
		}
	}
	b.WriteString("\n")
	return b.String()
}
