package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/utils"
)

var (
	skillGraphFormat       string
	skillGraphAgent        string
	skillGraphMinFrequency int64

	skillGraphCmd = &cobra.Command{
		Use:   "graph",
		Short: "Visualize detected skill patterns",
		Long: `Render the latest persisted skill-pattern snapshot.

The command performs no model calls and does not re-run pattern detection.
Use --format text for a human-readable view, json for automation, or mermaid
for raw Mermaid graph source.`,
		Args: cobra.NoArgs,
		RunE: runSkillGraph,
	}
)

func runSkillGraph(cmd *cobra.Command, _ []string) error {
	if skillGraphMinFrequency < 0 {
		return errors.New("--min-frequency must not be negative")
	}
	if skillGraphFormat != "text" && skillGraphFormat != "json" && skillGraphFormat != "mermaid" {
		return fmt.Errorf("unsupported --format %q (allowed: text, json, mermaid)", skillGraphFormat)
	}

	snapshot, available, err := skill.LoadSkillPatternSnapshot(
		skill.SkillPatternSnapshotPath(getWorkspace()),
	)
	if err != nil {
		return fmt.Errorf("load skill-pattern snapshot: %s", utils.RedactSecrets(err.Error()))
	}

	graph := skill.NewUnavailableSkillPatternGraph()
	if available {
		graph, err = skill.BuildSkillPatternGraph(snapshot, skill.SkillPatternGraphFilter{
			Agent:        skillGraphAgent,
			MinFrequency: skillGraphMinFrequency,
		})
		if err != nil {
			return fmt.Errorf("build skill-pattern graph: %s", utils.RedactSecrets(err.Error()))
		}
	}

	if skillGraphFormat == "json" {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(graph); err != nil {
			return fmt.Errorf("write skill-pattern graph JSON: %w", err)
		}
		return nil
	}
	if skillGraphFormat == "mermaid" {
		if _, err := fmt.Fprint(cmd.OutOrStdout(), skill.RenderSkillPatternGraphMermaid(graph)); err != nil {
			return fmt.Errorf("write skill-pattern graph Mermaid: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprint(cmd.OutOrStdout(), skill.RenderSkillPatternGraphText(graph)); err != nil {
		return fmt.Errorf("write skill-pattern graph text: %w", err)
	}
	return nil
}
