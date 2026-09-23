package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// newWorkspaceVersionCommand is `hufu workspace version`: inspection and
// explicit operations on the workspace version store. Unlike `hufu
// workspace restore <trash-id>` (which restores a deleted team workspace),
// `hufu workspace version restore <snapshot>` restores files.
func newWorkspaceVersionCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "version",
		Short: "Inspect and operate on workspace file versions (snapshots of the project files)",
		Long: `Inspect and operate on workspace versions: content-addressed snapshots of
the project's files that session fork and checkout switch between when
workspace-versioning.mode is required.

Note: "hufu workspace restore <trash-id>" restores a deleted team workspace;
"hufu workspace version restore <snapshot>" restores project files.`,
		Args: cobra.NoArgs,
	}
	command.PersistentFlags().StringVarP(&sessionWorkspace, "workspace", "w", "", "Control workspace directory (default: active managed workspace)")
	command.PersistentFlags().BoolVar(&sessionJSON, "json", false, "Write output as JSON")
	command.AddCommand(
		newWorkspaceVersionListCommand(),
		newWorkspaceVersionShowCommand(),
		newWorkspaceVersionDiffCommand(),
		newWorkspaceVersionSnapshotCommand(),
		newWorkspaceVersionRestoreCommand(),
		newWorkspaceVersionAdoptCommand(),
		newWorkspaceVersionStatusCommand(),
		newWorkspaceVersionDoctorCommand(),
		newWorkspaceVersionGCCommand(),
		newWorkspaceVersionDowngradeCommand(),
	)
	return command
}

// openVersionStoreForCommand resolves an active versioned workspace and opens
// its store read-only (inspection) or writable (mutating commands).
func openVersionStoreForCommand(ctx context.Context, mutating bool, operation string) (*versionedSession, *versionstore.Store, error) {
	vs, err := openVersionedSession(ctx, mutating, operation)
	if err != nil {
		return nil, nil, err
	}
	if !vs.version.Active() {
		_ = vs.Close()
		return nil, nil, fmt.Errorf("%w: workspace versioning is off for %s (set workspace-versioning.mode)", versionstore.ErrVersionedWorkspaceUnresolved, vs.workspace)
	}
	store, err := versionstore.Open(ctx, versionstore.Options{StateDir: vs.version.StateDir, ReadOnly: !mutating})
	if err != nil {
		_ = vs.Close()
		return nil, nil, fmt.Errorf("open workspace version store: %w", err)
	}
	return vs, store, nil
}

// snapshotView is the JSON shape of one snapshot.
type snapshotView struct {
	ID             string `json:"id"`
	BranchID       string `json:"branch_id"`
	Parent         string `json:"parent,omitempty"`
	Reason         string `json:"reason"`
	State          string `json:"state"`
	FileCount      int    `json:"file_count"`
	LogicalBytes   int64  `json:"logical_bytes"`
	CreatedAt      string `json:"created_at"`
	RootTreeHash   string `json:"root_tree_hash,omitempty"`
	CommitEventID  string `json:"commit_event_id,omitempty"`
	Materializable *bool  `json:"materializable,omitempty"`
}

func viewSnapshot(snapshot versionstore.Snapshot, detailed bool) snapshotView {
	view := snapshotView{
		ID: string(snapshot.ID), BranchID: snapshot.BranchID, Parent: string(snapshot.Parent),
		Reason: string(snapshot.Reason), State: string(snapshot.State), FileCount: snapshot.FileCount,
		LogicalBytes: snapshot.LogicalBytes, CreatedAt: snapshot.CreatedAt.UTC().Format(time.RFC3339),
	}
	if detailed {
		materializable := snapshot.Materializable
		view.RootTreeHash, view.CommitEventID, view.Materializable = snapshot.RootTreeHash, snapshot.CommitEventID, &materializable
	}
	return view
}

func newWorkspaceVersionListCommand() *cobra.Command {
	var branch string
	command := &cobra.Command{
		Use:   "list",
		Short: "List workspace snapshots of this workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := commandContext(cmd)
			vs, store, err := openVersionStoreForCommand(ctx, false, "list")
			if err != nil {
				return err
			}
			defer func() { _ = vs.Close(); _ = store.Close() }()
			snapshots, err := store.ListSnapshots(ctx, versionstore.SnapshotFilter{WorkspaceID: vs.version.WorkspaceID, BranchID: branch})
			if err != nil {
				return err
			}
			views := make([]snapshotView, 0, len(snapshots))
			for _, snapshot := range snapshots {
				views = append(views, viewSnapshot(snapshot, false))
			}
			if sessionJSON {
				return writeJSON(views)
			}
			for _, view := range views {
				fmt.Printf("%s  %-16s %-15s %-9s %6d files  %s\n", view.ID, view.BranchID, view.Reason, view.State, view.FileCount, view.CreatedAt)
			}
			return nil
		},
	}
	command.Flags().StringVar(&branch, "branch", "", "Only list snapshots of this session branch ID")
	return command
}

func newWorkspaceVersionShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show <snapshot>",
		Short: "Show one workspace snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := commandContext(cmd)
			vs, store, err := openVersionStoreForCommand(ctx, false, "show")
			if err != nil {
				return err
			}
			defer func() { _ = vs.Close(); _ = store.Close() }()
			snapshot, err := store.GetSnapshot(ctx, versionstore.SnapshotID(args[0]))
			if err != nil {
				return err
			}
			view := viewSnapshot(snapshot, true)
			if sessionJSON {
				return writeJSON(view)
			}
			fmt.Printf("id:             %s\nbranch:         %s\nparent:         %s\nreason:         %s\nstate:          %s\nfiles:          %d (%d bytes)\nroot tree:      %s\ncommit event:   %s\nmaterializable: %v\ncreated:        %s\n",
				view.ID, view.BranchID, view.Parent, view.Reason, view.State, view.FileCount, view.LogicalBytes,
				view.RootTreeHash, view.CommitEventID, *view.Materializable, view.CreatedAt)
			return nil
		},
	}
}

func newWorkspaceVersionDiffCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <snapshot-a> <snapshot-b>",
		Short: "Show the path-level difference between two workspace snapshots",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := commandContext(cmd)
			vs, store, err := openVersionStoreForCommand(ctx, false, "diff")
			if err != nil {
				return err
			}
			defer func() { _ = vs.Close(); _ = store.Close() }()
			delta, err := store.Diff(ctx, versionstore.SnapshotID(args[0]), versionstore.SnapshotID(args[1]))
			if err != nil {
				return err
			}
			summary := team.WorkspaceDiffSummary{
				Added: changeList(delta.Added), Modified: changeList(delta.Modified),
				Deleted: changeList(delta.Deleted), TypeChanged: changeList(delta.TypeChanged),
			}
			if sessionJSON {
				return writeJSON(summary)
			}
			printPaths := func(marker string, paths []string) {
				for _, path := range paths {
					fmt.Printf("%s %s\n", marker, path)
				}
			}
			printPaths("+", summary.Added)
			printPaths("~", summary.Modified)
			printPaths("-", summary.Deleted)
			printPaths("!", summary.TypeChanged)
			return nil
		},
	}
}

func changeList(changes []versionstore.PathChange) []string {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	return paths
}

// withWorkspaceOps runs fn with recovered workspace session operations.
func withWorkspaceOps(ctx context.Context, operation string, needRequired bool, fn func(*team.WorkspaceSessionOps) error) error {
	vs, err := openVersionedSession(ctx, true, operation)
	if err != nil {
		return err
	}
	defer func() { _ = vs.Close() }()
	if needRequired && !vs.version.Required() {
		return fmt.Errorf("%s changes project files and requires workspace-versioning.mode: required", operation)
	}
	if !vs.version.Active() {
		return fmt.Errorf("%w: workspace versioning is off for %s", versionstore.ErrVersionedWorkspaceUnresolved, vs.workspace)
	}
	st, es, err := openSessionStores(vs.workspace)
	if err != nil {
		return err
	}
	defer func() { _ = es.Close() }()
	ops, closeStore, err := workspaceOps(ctx, vs, st, es)
	if err != nil {
		return err
	}
	defer closeStore()
	return fn(ops)
}

func newWorkspaceVersionSnapshotCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshot",
		Short: "Record the current project files as a snapshot of the active session branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withWorkspaceOps(commandContext(cmd), "snapshot", false, func(ops *team.WorkspaceSessionOps) error {
				head, created, err := ops.SnapshotNow(commandContext(cmd))
				if err != nil {
					return err
				}
				if sessionJSON {
					return writeJSON(map[string]any{"snapshot": viewSnapshot(head, true), "created": created})
				}
				if created {
					fmt.Printf("✓ Recorded snapshot %s on branch %s (%d files).\n", head.ID, head.BranchID, head.FileCount)
				} else {
					fmt.Printf("No changes since %s on branch %s.\n", head.ID, head.BranchID)
				}
				return nil
			})
		},
	}
}

func newWorkspaceVersionRestoreCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <snapshot>",
		Short: "Restore the project files of the active branch to an earlier snapshot",
		Long: `Restore the project files of the active session branch to an earlier
snapshot of the same workspace. History is never rewritten: a new restore
snapshot is appended on top of the current head. Unmanaged and ignored paths
are left alone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withWorkspaceOps(commandContext(cmd), "restore", true, func(ops *team.WorkspaceSessionOps) error {
				node, result, err := ops.Restore(commandContext(cmd), versionstore.SnapshotID(args[0]))
				if err != nil {
					return err
				}
				if sessionJSON {
					return writeJSON(map[string]any{"snapshot": viewSnapshot(node, true), "files_written": result.FilesWritten, "files_deleted": result.FilesDeleted})
				}
				fmt.Printf("✓ Restored %s as %s (%d written, %d deleted).\n", args[0], node.ID, result.FilesWritten, result.FilesDeleted)
				return nil
			})
		},
	}
}

func newWorkspaceVersionAdoptCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "adopt",
		Short: "Accept the current project files after a recovery-required stop",
		Long: `After an external provider changed files outside its authorized roots,
runs stop with a recovery-required marker. "adopt" records the current files
as the active branch's new head and clears the marker; "restore <head>"
undoes the change instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withWorkspaceOps(commandContext(cmd), "adopt", false, func(ops *team.WorkspaceSessionOps) error {
				snapshot, err := ops.Adopt(commandContext(cmd))
				if err != nil {
					return err
				}
				if sessionJSON {
					return writeJSON(viewSnapshot(snapshot, true))
				}
				fmt.Printf("✓ Adopted the current files as %s on branch %s.\n", snapshot.ID, snapshot.BranchID)
				return nil
			})
		},
	}
}
