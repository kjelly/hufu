package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	internalteam "github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/team/preset"
)

var (
	teamAddPreset string
	teamAddForce  bool
)

var teamAddCmd = &cobra.Command{
	Use:   "add TEAM AGENT",
	Short: "Add an agent to an existing team",
	Long: `Add a new agent Markdown file to an existing team directory
(.agent-teams/<team>/<agent>.md), then compile and validate the full team.
--preset selects a deterministic agent preset. The new file is rolled back
(removed, or restored if it already existed and --force overwrote it) if
validation fails; existing files are never overwritten without --force.`,
	Args: cobra.ExactArgs(2),
	RunE: runTeamAdd,
}

func init() {
	teamCmd.AddCommand(teamAddCmd)
	teamAddCmd.Flags().StringVar(&teamAddPreset, "preset", "", "Agent preset name")
	teamAddCmd.Flags().BoolVar(&teamAddForce, "force", false, "Overwrite an existing agent file")
}

func runTeamAdd(_ *cobra.Command, args []string) error {
	teamName := strings.TrimSpace(args[0])
	agentName := strings.TrimSpace(args[1])
	if teamName == "" || agentName == "" {
		return fmt.Errorf("team name and agent name are required: hufu team add <team> <agent>")
	}

	teamDir := filepath.Join(".agent-teams", teamName)
	if info, err := os.Stat(teamDir); err != nil || !info.IsDir() {
		return fmt.Errorf("team directory %s does not exist; create it first with `hufu team create %s`", teamDir, teamName)
	}

	var content string
	if strings.TrimSpace(teamAddPreset) != "" {
		agentPreset, ok := preset.Lookup(teamAddPreset)
		if !ok {
			return fmt.Errorf("unknown --preset %q: available agent presets are %s", teamAddPreset, strings.Join(preset.Names(), ", "))
		}
		content = fmt.Sprintf("---\ndescription: %s\npreset: %s\n---\nPerform the %s role for this team.\n", agentName, agentPreset.Name, agentName)
	} else {
		content = fmt.Sprintf("---\ndescription: %s\n---\nPerform the %s role for this team.\n", agentName, agentName)
	}

	agentFile := filepath.Join(teamDir, agentName+".md")
	previous, hadPrevious, err := readIfExists(agentFile)
	if err != nil {
		return err
	}
	if hadPrevious && !teamAddForce {
		return fmt.Errorf("agent file %s already exists; pass --force to overwrite", agentFile)
	}

	if err := os.WriteFile(agentFile, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", agentFile, err)
	}

	if _, err := internalteam.CompileTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry); err != nil {
		if hadPrevious {
			_ = os.WriteFile(agentFile, []byte(previous), 0o644)
		} else {
			_ = os.Remove(agentFile)
		}
		return fmt.Errorf("adding %s made the team invalid, rolled back: %w", agentName, err)
	}

	_, err = fmt.Fprintf(os.Stdout, "%s added %s to %s\n", doneStyle.Render("✓"), agentName, teamDir)
	return err
}

// readIfExists returns a file's content and whether it existed. A
// not-exist error is not an error here; any other stat/read failure is.
func readIfExists(path string) (content string, existed bool, err error) {
	data, readErr := os.ReadFile(path)
	if readErr == nil {
		return string(data), true, nil
	}
	if os.IsNotExist(readErr) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("inspect %s: %w", path, readErr)
}
