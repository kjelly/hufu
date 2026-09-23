package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// maintenanceOps opens a maintenance session with its store, session tree,
// and event store. writable is required for --repair, --apply, and
// downgrade; it takes the locks and a writable store and event store.
func maintenanceOps(ctx context.Context, writable bool, operation string) (*team.WorkspaceSessionOps, func(), error) {
	vs, err := openMaintenanceSession(ctx, writable, operation)
	if err != nil {
		return nil, nil, err
	}
	closers := []func(){func() { _ = vs.Close() }}
	cleanup := func() {
		for index := len(closers) - 1; index >= 0; index-- {
			closers[index]()
		}
	}
	store, err := versionstore.Open(ctx, versionstore.Options{StateDir: vs.version.StateDir, ReadOnly: !writable})
	if errors.Is(err, versionstore.ErrStoreNotFound) {
		cleanup()
		return nil, nil, fmt.Errorf("no workspace versions have been recorded for %s", vs.workspace)
	}
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("open workspace version store: %w", err)
	}
	closers = append(closers, func() { _ = store.Close() })
	tree, err := team.LoadSessionTree(vs.workspace)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("load session tree: %w", err)
	}
	var events *team.EventStore
	if writable {
		events, err = team.OpenEventStore(vs.workspace)
	} else {
		events, err = team.OpenEventStoreReadOnly(vs.workspace)
	}
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("open event store: %w", err)
	}
	closers = append(closers, func() { _ = events.Close() })
	return &team.WorkspaceSessionOps{Version: vs.version, Workspace: vs.workspace, Tree: tree, Events: events, Store: store}, cleanup, nil
}

func newWorkspaceVersionStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the workspace versioning state of this workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := commandContext(cmd)
			ops, cleanup, err := maintenanceOps(ctx, false, "status")
			if err != nil {
				return err
			}
			defer cleanup()
			status, err := ops.Status(ctx)
			if err != nil {
				return err
			}
			if sessionJSON {
				return writeJSON(status)
			}
			drift := "unknown"
			if status.LiveDrift != nil {
				drift = fmt.Sprintf("%v", *status.LiveDrift)
			}
			fmt.Printf("subject root:        %s\nmode:                %s (floor %s)\nactive branch:       %s (head %s)\nmaterialized:        %s %s\nlive drift:          %s\nrecovery required:   %v %s\ncheckpoint deferred: %v\nsnapshots:           %v\ncas:                 %d objects, %d bytes\ncommits:             %v (new %d / reused %d bytes)\n",
				status.SubjectRoot, status.EffectiveMode, status.ModeFloor, status.ActiveBranchID, status.ActiveHeadSnapshotID,
				status.MaterializedBranchID, status.MaterializedSnapshotID, drift, status.RecoveryRequired, status.RecoveryCode,
				status.CheckpointDeferred, status.SnapshotCounts, status.CASObjects, status.CASBytes, status.Commits,
				status.NewCASBytes, status.ReusedCASBytes)
			for _, op := range status.IncompleteOperations {
				fmt.Printf("incomplete operation: %s\n", op)
			}
			return nil
		},
	}
}

func newWorkspaceVersionDoctorCommand() *cobra.Command {
	var repair bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Check workspace version integrity (and repair deterministic problems with --repair)",
		Long: `Check the workspace version store and its link to this workspace's event log:
heads, commit events, parent chains, object integrity, head-to-lineage
consistency, legacy branches, live drift, markers, and stale temp files.

--repair only performs deterministic repairs: settling pending snapshots and
interrupted operations, resetting heads to the event lineage, and removing
stale temp files. It never invents missing content or picks a newer snapshot.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := commandContext(cmd)
			ops, cleanup, err := maintenanceOps(ctx, repair, "doctor")
			if err != nil {
				return err
			}
			defer cleanup()
			report, err := team.DoctorWorkspace(ctx, ops, repair)
			if err != nil {
				return err
			}
			if issue, stale := staleLockOwnerIssue(ops.Version.StateDir); stale && !repair {
				report.Issues = append(report.Issues, issue)
			}
			if sessionJSON {
				if err = writeJSON(report); err != nil {
					return err
				}
			} else {
				for _, line := range report.Repaired {
					fmt.Printf("repaired: %s\n", line)
				}
				for _, issue := range report.Issues {
					fmt.Printf("%-7s %-26s %s %s %s\n", issue.Severity, issue.Code, issue.BranchID, issue.SnapshotID, issue.Detail)
				}
				if len(report.Issues) == 0 {
					fmt.Println("✓ No workspace version problems found.")
				}
			}
			if report.HasErrors() {
				return errors.New("workspace version doctor found errors")
			}
			return nil
		},
	}
	command.Flags().BoolVar(&repair, "repair", false, "Perform deterministic repairs")
	return command
}

// staleLockOwnerIssue reports a lock owner record whose process is gone.
func staleLockOwnerIssue(stateDir string) (versionstore.DoctorIssue, bool) {
	owner, ok, err := versionstore.ReadLockOwner(stateDir)
	if err != nil || !ok || owner.PID <= 0 {
		return versionstore.DoctorIssue{}, false
	}
	process, err := os.FindProcess(owner.PID)
	if err == nil && process.Signal(syscall.Signal(0)) == nil {
		return versionstore.DoctorIssue{}, false
	}
	return versionstore.DoctorIssue{Code: "stale_lock_owner", Severity: "warning", WorkspaceID: owner.WorkspaceID,
		Detail: fmt.Sprintf("lock owner record for pid %d (%s) is stale; the lock itself is free", owner.PID, owner.Operation)}, true
}

func newWorkspaceVersionGCCommand() *cobra.Command {
	var apply bool
	command := &cobra.Command{
		Use:   "gc",
		Short: "Report (or with --apply, remove) unreferenced workspace versions",
		Long: `Mark every object reachable from branch heads, labels, incomplete operations,
and each branch's most recent snapshots (workspace-versioning.retention), then
report older snapshots and unreferenced objects. The default is a dry run.
--apply marks older snapshots as pruned (their history rows remain, but fork
or restore to them fails) and deletes unreferenced objects older than the
orphan grace period.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := commandContext(cmd)
			ops, cleanup, err := maintenanceOps(ctx, apply, "gc")
			if err != nil {
				return err
			}
			defer cleanup()
			retention := config.LoadConfig().WorkspaceVersioning.Retention
			grace, err := retention.Grace()
			if err != nil {
				return err
			}
			pinned, err := team.PinnedWorkspaceSnapshots(ops)
			if err != nil {
				return err
			}
			result, err := ops.Store.GC(ctx, versionstore.GCRequest{KeepRecent: retention.KeepRecent(), Grace: grace, Pinned: pinned, Apply: apply})
			if err != nil {
				return err
			}
			output := map[string]any{
				"snapshots_prunable": len(result.SnapshotsPrunable), "objects_prunable": result.ObjectsPrunable,
				"bytes_reclaimable": result.BytesReclaimable, "temp_files_prunable": result.TempFilesPrunable, "applied": result.Applied,
			}
			if sessionJSON {
				return writeJSON(output)
			}
			verb := "would be"
			if apply {
				verb = "were"
			}
			fmt.Printf("%d snapshot(s) %s pruned, %d object(s) (%d bytes) and %d temp file(s) %s removed.\n",
				len(result.SnapshotsPrunable), verb, result.ObjectsPrunable, result.BytesReclaimable, result.TempFilesPrunable, verb)
			return nil
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "Actually prune snapshots and delete objects")
	return command
}

func newWorkspaceVersionDowngradeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "downgrade <observe|off>",
		Short: "Explicitly lower the recorded workspace versioning mode floor",
		Long: `Once a project has run in required mode, a lower configured mode is refused
so versioning is never silently switched off. "downgrade" is the explicit way
to lower the recorded floor; set workspace-versioning.mode to match.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := commandContext(cmd)
			mode, err := versionstore.ParseMode(args[0])
			if err != nil {
				return err
			}
			ops, cleanup, err := maintenanceOps(ctx, true, "downgrade")
			if err != nil {
				return err
			}
			defer cleanup()
			if err = ops.Store.Downgrade(ctx, ops.Version.SubjectRoot, ops.Version.WorkspaceID, mode); err != nil {
				return err
			}
			fmt.Printf("✓ Workspace versioning floor for %s lowered to %s.\n", ops.Version.SubjectRoot, mode)
			return nil
		},
	}
}
