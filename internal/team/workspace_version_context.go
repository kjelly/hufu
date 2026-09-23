package team

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// WorkspaceVersionContext binds one team session to workspace versioning
// (docs/archive/implementation-plans/workspace-versioning.md §25). It is
// built by the command layer from the managed workspace resolution; the
// coordinator never opens the registry itself.
type WorkspaceVersionContext struct {
	// Mode is the effective mode after the config, platform, and managed
	// checks; ModeOff (the zero value) disables every versioning path.
	Mode        versionstore.Mode
	WorkspaceID string
	ProjectID   string
	TeamName    string
	// SubjectRoot and ControlRoot are canonical absolute paths.
	SubjectRoot string
	ControlRoot string
	// StateDir is Project.StateDir; the store lives below it.
	StateDir string
	// HoldsProjectLock reports that the command layer holds the project
	// lock for this process (required mode, §27).
	HoldsProjectLock bool
	Limits           versionstore.CaptureLimits
}

// Active reports whether versioning is enabled for this session.
func (c WorkspaceVersionContext) Active() bool {
	return c.Mode != "" && c.Mode != versionstore.ModeOff && versionstore.PlatformSupported() &&
		c.WorkspaceID != "" && c.SubjectRoot != "" && c.StateDir != ""
}

// Required reports whether fork, checkout, and restore must be
// workspace-aware.
func (c WorkspaceVersionContext) Required() bool {
	return c.Active() && c.Mode == versionstore.ModeRequired
}

// ExcludeSubtrees returns the control-root subtree relative to the subject
// root when the control root lies inside it (§9.5). A control root equal to
// the subject root cannot be versioned.
func (c WorkspaceVersionContext) ExcludeSubtrees() ([]string, error) {
	subject := filepath.Clean(c.SubjectRoot)
	control := filepath.Clean(c.ControlRoot)
	if control == "" || control == "." {
		return nil, nil
	}
	if control == subject {
		return nil, fmt.Errorf("%w: control root %q is the subject root", versionstore.ErrVersionedWorkspaceUnresolved, control)
	}
	if !strings.HasPrefix(control, subject+string(filepath.Separator)) {
		return nil, nil
	}
	rel, err := filepath.Rel(subject, control)
	if err != nil {
		return nil, fmt.Errorf("derive control subtree: %w", err)
	}
	return []string{filepath.ToSlash(rel)}, nil
}

// OpenStore opens the project's version store.
func (c WorkspaceVersionContext) OpenStore(ctx context.Context) (*versionstore.Store, error) {
	if !c.Active() {
		return nil, fmt.Errorf("%w: workspace versioning is not enabled", versionstore.ErrVersionedWorkspaceUnresolved)
	}
	store, err := versionstore.Open(ctx, versionstore.Options{StateDir: c.StateDir})
	if err != nil {
		return nil, fmt.Errorf("open workspace version store: %w", err)
	}
	return store, nil
}

// captureRequest builds a capture of the subject root for branchID.
func (c WorkspaceVersionContext) captureRequest(ctx context.Context, branchID string, parent versionstore.SnapshotID, reason versionstore.SnapshotReason) (versionstore.CaptureRequest, error) {
	subtrees, err := c.ExcludeSubtrees()
	if err != nil {
		return versionstore.CaptureRequest{}, err
	}
	return versionstore.CaptureRequest{
		WorkspaceID:       c.WorkspaceID,
		BranchID:          branchID,
		Root:              c.SubjectRoot,
		ExcludeSubtrees:   subtrees,
		Parent:            parent,
		Reason:            reason,
		Limits:            c.Limits,
		RequireHufuignore: c.Required() && !versionstore.IsGitWorkTree(ctx, c.SubjectRoot),
	}, nil
}
