package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var teamShowName string

var teamShowCmd = &cobra.Command{
	Use:   "show [team-directory]",
	Short: "Show what you actually authored for a team, without expanding defaults",
	Long: `Show the authored files and their raw, unresolved fields for a team — what
you configured, not the fully resolved effective configuration (use
"hufu team explain" for that). Use a directory argument, or --team to
resolve a discoverable team name.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTeamShow,
}

func init() {
	teamCmd.AddCommand(teamShowCmd)
	teamShowCmd.Flags().StringVar(&teamShowName, "team", "", "Discoverable team name to show")
}

func runTeamShow(_ *cobra.Command, args []string) error {
	teamDir, err := resolveTeamDirArg(args, teamShowName)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(teamDir)
	if err != nil {
		return fmt.Errorf("read team directory %s: %w", teamDir, err)
	}

	var teamYAMLRaw map[string]interface{}
	hasTeamYAML := false
	var agentFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch {
		case entry.Name() == "team.yaml" || entry.Name() == "team.yml":
			hasTeamYAML = true
			if data, readErr := os.ReadFile(filepath.Join(teamDir, entry.Name())); readErr == nil {
				_ = yaml.Unmarshal(data, &teamYAMLRaw)
			}
		case strings.HasSuffix(entry.Name(), ".md"):
			agentFiles = append(agentFiles, entry.Name())
		}
	}
	sort.Strings(agentFiles)

	var b strings.Builder
	fmt.Fprintf(&b, "Team: %s\n\n", filepath.Base(teamDir))

	fmt.Fprintln(&b, "Authored files:")
	if hasTeamYAML {
		fmt.Fprintln(&b, "  team.yaml")
	}
	for _, f := range agentFiles {
		fmt.Fprintf(&b, "  %s\n", f)
	}

	if len(teamYAMLRaw) > 0 {
		fmt.Fprintln(&b, "\nTeam overrides:")
		writeSortedRawFields(&b, "  ", teamYAMLRaw)
	}

	fmt.Fprintln(&b, "\nAgents:")
	for _, f := range agentFiles {
		fm, ok := readAgentFrontmatterRaw(filepath.Join(teamDir, f))
		fmt.Fprintf(&b, "\n  %s\n", strings.TrimSuffix(f, ".md"))
		if ok {
			writeSortedRawFields(&b, "    ", fm)
		}
	}

	_, err = fmt.Fprint(os.Stdout, b.String())
	return err
}

func writeSortedRawFields(b *strings.Builder, indent string, fields map[string]interface{}) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s%s: %v\n", indent, k, fields[k])
	}
}

// readAgentFrontmatterRaw reads one agent file's raw frontmatter block (if
// any) as a generic map, for display purposes only. It intentionally
// mirrors the authored YAML rather than the resolved AgentDef.
func readAgentFrontmatterRaw(path string) (map[string]interface{}, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return nil, false
	}
	rest := text[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		return nil, false
	}
	var fm map[string]interface{}
	if err := yaml.Unmarshal([]byte(rest[:idx]), &fm); err != nil {
		return nil, false
	}
	return fm, true
}
