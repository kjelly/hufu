package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var examplesCmd = &cobra.Command{Use: "examples", Short: "Show common hufu commands", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
	_, err := fmt.Fprint(os.Stdout, `Quick start:
  hufu doctor
  hufu init dev-team --template dev
  hufu @dev-team "review this codebase"

Interactive:
  hufu chat --agent-team dev-team
  hufu --tui @dev-team "implement a feature"

Automation:
  hufu --quiet --output json --event-format jsonl @dev-team "run checks"
  hufu --unattended --max-duration 600 @ops-team "check service health"
`)
	return err
}}

var helpFlagsCmd = &cobra.Command{Use: "help-flags <group>", Short: "Show flags in a named group", Args: cobra.ExactArgs(1), RunE: runHelpFlags}

var flagGroups = map[string][]string{
	"core":       {"model", "agent-team", "default", "workspace"},
	"execution":  {"plan", "auto-skills", "steps", "dry-run", "timeout", "max-rounds"},
	"output":     {"verbose", "quiet", "output", "event-format", "report", "think", "tui", "no-color"},
	"security":   {"rbash", "no-net", "force-mcp", "allow-path", "direnv"},
	"unattended": {"unattended", "max-duration", "max-total-tokens", "auto-approve"},
	"advanced":   {"provider-url", "provider-api-key", "profile", "var", "var-file", "memory", "template"},
}

func runHelpFlags(cmd *cobra.Command, args []string) error {
	group := strings.ToLower(args[0])
	flags, ok := flagGroups[group]
	if !ok {
		names := make([]string, 0, len(flagGroups))
		for name := range flagGroups {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown flag group %q; available: %s", group, strings.Join(names, ", "))
	}
	root := cmd.Root()
	for _, name := range flags {
		flag := root.Flags().Lookup(name)
		if flag == nil {
			flag = root.PersistentFlags().Lookup(name)
		}
		if flag == nil {
			continue
		}
		if _, err := fmt.Fprintf(os.Stdout, "--%-20s %s\n", name, flag.Usage); err != nil {
			return err
		}
	}
	return nil
}
