package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var teamRemoveForce bool

var teamRemoveCmd = &cobra.Command{
	Use:   "remove NAME",
	Short: "Delete a team's directory",
	Long: `Delete .agent-teams/<name>/ and everything in it. Requires --force; without
it, this only reports what would be removed.`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamRemove,
}

func init() {
	teamCmd.AddCommand(teamRemoveCmd)
	teamRemoveCmd.Flags().BoolVar(&teamRemoveForce, "force", false, "Actually delete the team directory")
}

func runTeamRemove(_ *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return fmt.Errorf("team name required: hufu team remove <name>")
	}
	teamDir := filepath.Join(".agent-teams", name)
	info, err := os.Stat(teamDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("team directory %s does not exist", teamDir)
		}
		return fmt.Errorf("inspect team directory %s: %w", teamDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", teamDir)
	}

	if !teamRemoveForce {
		entries, _ := os.ReadDir(teamDir)
		_, err := fmt.Fprintf(os.Stdout, "Would remove %s (%d file(s)). Pass --force to actually delete it.\n", teamDir, len(entries))
		return err
	}

	if err := os.RemoveAll(teamDir); err != nil {
		return fmt.Errorf("remove %s: %w", teamDir, err)
	}
	_, err = fmt.Fprintf(os.Stdout, "%s removed %s\n", doneStyle.Render("✓"), teamDir)
	return err
}
