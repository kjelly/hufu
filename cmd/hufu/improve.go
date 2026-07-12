package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/improve"
	"github.com/anomalyco/hufu/internal/team"

	"github.com/spf13/cobra"
)

var (
	improveWorkspace  string
	improveTeam       string
	improveSearchPath string
	improveOutput     string
	improveFormat     string
)

var improveCmd = &cobra.Command{
	Use:   "improve",
	Short: "Analyze the latest execution run and suggest deterministic improvements",
	Long: `Analyze the newest structured execution run in a workspace and write a
deterministic report. The report contains no prompts, task output, tool input,
or tool-result text, so it is suitable for sharing by default.`,
	Args: cobra.NoArgs,
	RunE: runImprove,
}

func runImprove(cmd *cobra.Command, args []string) error {
	format := strings.ToLower(strings.TrimSpace(improveFormat))
	if format != "markdown" && format != "json" {
		return fmt.Errorf("unsupported format %q (want markdown or json)", improveFormat)
	}
	ws, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	teamName := strings.TrimSpace(improveTeam)
	if teamName == "" {
		teamName, err = improve.LatestTeam(ws)
		if errors.Is(err, improve.ErrNoExecutionData) {
			fmt.Fprintf(os.Stderr, "No execution data found in %s. Run a team first, then retry.\n", ws)
			return nil
		}
		if err != nil {
			return err
		}
	}
	paths := team.DefaultSearchPaths()
	if strings.TrimSpace(improveSearchPath) != "" {
		paths = strings.Split(improveSearchPath, ",")
	}
	registry := team.NewTeamRegistry(paths)
	if err := registry.Discover(); err != nil {
		return fmt.Errorf("discover teams: %w", err)
	}
	teamDir, err := registry.Resolve(teamName)
	if err != nil {
		return fmt.Errorf("team %q not found. Available: %s", teamName, strings.Join(registry.ListTeams(), ", "))
	}
	report, err := improve.Analyze(ws, teamName, teamDir)
	if errors.Is(err, improve.ErrNoExecutionData) {
		fmt.Fprintf(os.Stderr, "No execution data found in %s. Run a team first, then retry.\n", ws)
		return nil
	}
	if err != nil {
		return err
	}
	if format == "json" {
		data, err := improve.JSON(report)
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	output := improveOutput
	if output == "" {
		output = filepath.Join(ws, "reports", fmt.Sprintf("improve-%s-%s.md", teamName, time.Now().UTC().Format("20060102T150405Z")))
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	if err := os.WriteFile(output, []byte(improve.Markdown(report)), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Println(output)
	return nil
}

func resolveImproveWorkspace(value string) (string, error) {
	if strings.TrimSpace(value) != "" {
		return filepath.Abs(value)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return filepath.Join(cwd, "workspace"), nil
}
