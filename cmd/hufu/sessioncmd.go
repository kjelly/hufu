package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/team"
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
  diff <branch-a> <branch-b>    Compare tasks, artifacts, and verification results between branches`,
	Args: cobra.NoArgs,
}

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all session branches and labels",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ws := getSessionWorkspace()
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}
		es, _ := team.OpenEventStore(ws)
		if es != nil {
			defer func() { _ = es.Close() }()
		}

		if sessionJSON {
			data := map[string]any{
				"active_branch": st.ActiveBranch,
				"branches":      st.ListBranches(),
				"labels":        st.Labels,
			}
			return json.NewEncoder(os.Stdout).Encode(data)
		}

		fmt.Printf("Workspace: %s\n", ws)
		fmt.Printf("Active Branch: %s\n\n", st.ActiveBranch)
		fmt.Println("Branches:")
		for _, b := range st.ListBranches() {
			marker := " "
			if b.ID == st.ActiveBranch {
				marker = "*"
			}
			parentStr := ""
			if b.ParentID != "" {
				parentStr = fmt.Sprintf(" (forked from %s)", b.ParentID)
			}
			fmt.Printf(" %s %-20s %s%s\n", marker, b.Name, b.CreatedAt, parentStr)
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
		ws := getSessionWorkspace()
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}
		es, _ := team.OpenEventStore(ws)
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
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws := getSessionWorkspace()
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}
		es, _ := team.OpenEventStore(ws)
		if es != nil {
			defer func() { _ = es.Close() }()
		}

		target := ""
		if len(args) > 0 {
			target = args[0]
		}

		// Snapshot the current branch's live state before forking.
		team.SnapshotBranchState(ws, st, st.ActiveBranch)

		name := sessionForkName
		if name == "" {
			if target != "" {
				name = fmt.Sprintf("fork-%s", target)
			} else {
				name = "fork-branch"
			}
		}

		b, err := st.CreateBranch(name, target, es)
		if err != nil {
			return fmt.Errorf("failed to fork branch: %w", err)
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

		fmt.Printf("✓ Forked new branch %q (ID: %s) from %q and checked out.\n", b.Name, b.ID, b.ParentID)
		return nil
	},
}

var sessionCheckoutCmd = &cobra.Command{
	Use:   "checkout <target>",
	Short: "Switch active branch to target branch, checkpoint label, or event ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws := getSessionWorkspace()
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}
		es, _ := team.OpenEventStore(ws)
		if es != nil {
			defer func() { _ = es.Close() }()
		}

		// Snapshot current branch's live state before switching.
		team.SnapshotBranchState(ws, st, st.ActiveBranch)

		target := args[0]
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

		fmt.Printf("✓ Checked out branch %q (%s).\n", b.Name, b.ID)
		return nil
	},
}

var sessionLabelCmd = &cobra.Command{
	Use:   "label <target> <name>",
	Short: "Add a label to a checkpoint, event ID, or branch",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws := getSessionWorkspace()
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
	Short: "Compare tasks, artifacts, and verification results between two branches",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ws := getSessionWorkspace()
		st, err := team.LoadSessionTree(ws)
		if err != nil {
			return fmt.Errorf("failed to load session tree: %w", err)
		}
		es, _ := team.OpenEventStore(ws)
		if es != nil {
			defer func() { _ = es.Close() }()
		}

		branchA := args[0]
		branchB := args[1]

		diff, err := team.DiffBranches(ws, st, es, branchA, branchB)
		if err != nil {
			return fmt.Errorf("diff failed: %w", err)
		}

		if sessionJSON {
			return json.NewEncoder(os.Stdout).Encode(diff)
		}

		fmt.Print(diff.RenderText())
		return nil
	},
}

func getSessionWorkspace() string {
	if sessionWorkspace != "" {
		return sessionWorkspace
	}
	return getWorkspace()
}

func init() {
	sessionCmd.PersistentFlags().StringVarP(&sessionWorkspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	sessionCmd.PersistentFlags().BoolVar(&sessionJSON, "json", false, "Write output as JSON")

	sessionForkCmd.Flags().StringVar(&sessionForkName, "name", "", "Name of the new branch")

	sessionCmd.AddCommand(sessionListCmd)
	sessionCmd.AddCommand(sessionTreeCmd)
	sessionCmd.AddCommand(sessionForkCmd)
	sessionCmd.AddCommand(sessionCheckoutCmd)
	sessionCmd.AddCommand(sessionLabelCmd)
	sessionCmd.AddCommand(sessionDiffCmd)
}
