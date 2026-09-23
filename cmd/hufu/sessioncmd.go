package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

var (
	sessionWorkspace string
	sessionJSON      bool
	sessionForkName  string
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Manage session branches, fork points, checkpoints, and time travel",
	Long: `Manage session branches, fork points, checkpoints, and diffs.

Subcommands:
  list                          List all branches and checkpoints
  tree                          Display visual tree of session branches
  fork [target] [--name <name>] Fork a new branch from a branch, label, or event ID
  checkout <target>             Switch active session branch to a branch or label
  label <target> <name>         Create a human-readable label for a checkpoint or branch
  diff <branch-a> <branch-b>    Compare tasks, artifacts, verification results, and workspace files`,
	Args: cobra.NoArgs,
}

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all session branches and labels",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := commandContext(cmd)
		vs, err := openVersionedSession(ctx, false, "list")
		if err != nil {
			return err
		}
		defer func() { _ = vs.Close() }()
		ws := vs.workspace
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}
		branches := st.ListBranches()
		states, err := branchWorkspaceStates(ctx, vs, branches)
		if err != nil {
			return err
		}

		if sessionJSON {
			listed := make([]sessionListBranch, 0, len(branches))
			for _, b := range branches {
				listed = append(listed, states[b.ID])
			}
			data := map[string]any{
				"active_branch": st.ActiveBranch,
				"branches":      listed,
				"labels":        st.Labels,
			}
			return json.NewEncoder(os.Stdout).Encode(data)
		}

		fmt.Printf("Workspace: %s\n", ws)
		fmt.Printf("Active Branch: %s\n\n", st.ActiveBranch)
		fmt.Println("Branches:")
		for _, b := range branches {
			marker := " "
			if b.ID == st.ActiveBranch {
				marker = "*"
			}
			parentStr := ""
			if b.ParentID != "" {
				parentStr = fmt.Sprintf(" (forked from %s)", b.ParentID)
			}
			fmt.Printf(" %s %-20s %s%s%s\n", marker, b.Name, b.CreatedAt, parentStr, workspaceColumn(states[b.ID]))
		}

		if len(st.Labels) > 0 {
			fmt.Println("\nLabels:")
			for lbl, target := range st.Labels {
				fmt.Printf("   %-20s -> %s\n", lbl, target)
			}
		}
		return nil
	},
}

var sessionTreeCmd = &cobra.Command{
	Use:   "tree",
	Short: "Show visual ASCII session tree",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, err := requireSessionWorkspace()
		if err != nil {
			return err
		}
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}
		es, _ := team.OpenEventStoreReadOnly(ws)
		if es != nil {
			defer func() { _ = es.Close() }()
		}

		output := st.RenderTree(es)
		fmt.Print(output)
		return nil
	},
}

var sessionForkCmd = &cobra.Command{
	Use:   "fork [fork-target]",
	Short: "Fork a new branch from a branch, checkpoint label, or event ID",
	Long: `Fork a new branch from a branch, checkpoint label, or event ID.

With workspace versioning in required mode the fork also carries the
workspace: forking the active branch reuses its snapshot (no files change),
and forking an earlier branch or event first saves the live files and then
restores the snapshot recorded at that point. --metadata-only forks only the
session lineage (for legacy events without a workspace snapshot).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessionFork,
}

var sessionCheckoutCmd = &cobra.Command{
	Use:   "checkout <target>",
	Short: "Switch active branch to target branch, checkpoint label, or event ID",
	Long: `Switch the active session branch.

With workspace versioning in required mode the checkout also switches the
files: the live state of the current branch is saved first, then the target
branch's workspace snapshot is materialized. Paths the snapshots do not
manage (ignored or unmanaged files) are never deleted. --metadata-only
switches only the session projection, and is only accepted for legacy
branches that have no workspace snapshot.`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionCheckout,
}

var sessionLabelCmd = &cobra.Command{
	Use:   "label <target> <name>",
	Short: "Add a label to a checkpoint, event ID, or branch",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws, err := requireSessionWorkspace()
		if err != nil {
			return err
		}
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}

		target := args[0]
		name := args[1]

		if err := st.AddLabel(name, target); err != nil {
			return err
		}

		if err := team.SaveSessionTree(ws, st); err != nil {
			return fmt.Errorf("failed to save session tree: %w", err)
		}

		fmt.Printf("✓ Added label %q -> %s\n", name, target)
		return nil
	},
}

var sessionDiffCmd = &cobra.Command{
	Use:   "diff <branch-a> <branch-b>",
	Short: "Compare tasks, artifacts, verification results, and workspace files between two branches",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		vs, err := openVersionedSession(commandContext(cmd), false, "diff")
		if err != nil {
			return err
		}
		defer func() { _ = vs.Close() }()
		ws := vs.workspace
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}
		es, _ := team.OpenEventStoreReadOnly(ws)
		if es != nil {
			defer func() { _ = es.Close() }()
		}

		branchA := args[0]
		branchB := args[1]

		diff, err := team.DiffBranches(ws, st, es, branchA, branchB)
		if err != nil {
			return fmt.Errorf("diff failed: %w", err)
		}
		if err = attachSessionWorkspaceDiff(commandContext(cmd), vs, st, diff, branchA, branchB); err != nil {
			return fmt.Errorf("workspace diff failed: %w", err)
		}

		if sessionJSON {
			return json.NewEncoder(os.Stdout).Encode(diff)
		}

		fmt.Print(diff.RenderText())
		return nil
	},
}

func getSessionWorkspace() string {
	return getSessionWorkspaceForTeam("")
}

func getSessionWorkspaceForTeam(teamName string) string {
	if sessionWorkspace != "" {
		return sessionWorkspace
	}
	if strings.TrimSpace(teamName) != "" {
		return resolveWorkspaceForTeam(context.Background(), teamName)
	}
	return getWorkspace()
}

func requireSessionWorkspace() (string, error) {
	workspace := getSessionWorkspace()
	if workspace == "" {
		return "", fmt.Errorf("managed workspace not found; run a team first or pass --workspace")
	}
	return workspace, nil
}

func init() {
	sessionCmd.PersistentFlags().StringVarP(&sessionWorkspace, "workspace", "w", "", "Workspace directory (default: active managed workspace)")
	sessionCmd.PersistentFlags().BoolVar(&sessionJSON, "json", false, "Write output as JSON")

	sessionForkCmd.Flags().StringVar(&sessionForkName, "name", "", "Name of the new branch")
	sessionForkCmd.Flags().BoolVar(&sessionMetadataOnly, "metadata-only", false, "Fork only the session lineage; the workspace state is unavailable")
	sessionCheckoutCmd.Flags().BoolVar(&sessionMetadataOnly, "metadata-only", false, "Switch only the session projection (legacy branches without a workspace snapshot)")

	sessionCmd.AddCommand(sessionListCmd)
	sessionCmd.AddCommand(sessionTreeCmd)
	sessionCmd.AddCommand(sessionForkCmd)
	sessionCmd.AddCommand(sessionCheckoutCmd)
	sessionCmd.AddCommand(sessionLabelCmd)
	sessionCmd.AddCommand(sessionDiffCmd)
	sessionCmd.AddCommand(newSessionStatusCommand())
	sessionCmd.AddCommand(newSessionResumeCommand())
	sessionCmd.AddCommand(newSessionRecoveryCommand(team.TargetedRecoveryRetry))
	sessionCmd.AddCommand(newSessionRecoveryCommand(team.TargetedRecoveryReconcile))
}
