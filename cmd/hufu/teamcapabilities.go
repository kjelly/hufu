package main

import (
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

func newTeamCapabilitiesCommand() *cobra.Command {
	var output string
	command := &cobra.Command{
		Use: "capabilities <team>", Short: "List static capability declarations without starting a team",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if output != "text" && output != "json" {
				return fmt.Errorf("output must be text or json")
			}
			teamDir, err := resolveTeamDirArg(nil, args[0])
			if err != nil {
				return fmt.Errorf("team capability target could not be resolved")
			}
			inventory, err := team.InspectCapabilities(teamDir)
			if err != nil {
				return err
			}
			if output == "json" {
				return json.MarshalWrite(command.OutOrStdout(), inventory)
			}
			if _, err := fmt.Fprintln(command.OutOrStdout(), "Static declarations only; availability is not runtime authorization or remote health."); err != nil {
				return err
			}
			for _, entry := range inventory.Capabilities {
				digest := "unknown"
				if entry.ContentDigest != nil {
					digest = *entry.ContentDigest
				}
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s %s %s: %s effect=%s digest=%s\n",
					entry.Origin, entry.Owner, entry.Name, entry.Availability, entry.EffectClass, digest); err != nil {
					return err
				}
			}
			for _, diagnostic := range inventory.Diagnostics {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s: %s\n", diagnostic.Severity, diagnostic.Code); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return command
}

func init() {
	teamCmd.AddCommand(newTeamCapabilitiesCommand())
}
