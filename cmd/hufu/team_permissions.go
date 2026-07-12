package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	internalteam "github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
)

var teamPermissionsSearchPath string

var teamPermissionsCmd = &cobra.Command{
	Use:   "permissions <team> [allow|deny|remove <path>]",
	Short: "View and maintain a team's persistent path-consent policy",
	Long: `Path permissions are stored in .hufu-path-consent.yaml beside the team.

Choosing "Always allow" or "Always deny" during a run updates this file automatically.
Use this command to review or change the same policy without editing YAML.`,
	Example: `  hufu team permissions dev-team
  hufu team permissions dev-team allow /srv/project
  hufu team permissions dev-team deny /etc
  hufu team permissions dev-team remove /srv/project`,
	Args: cobra.RangeArgs(1, 3),
	RunE: runTeamPermissions,
}

func init() {
	teamCmd.AddCommand(teamPermissionsCmd)
	teamPermissionsCmd.Flags().StringVar(&teamPermissionsSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams")
}

func runTeamPermissions(_ *cobra.Command, args []string) error {
	teamDir, err := resolvePermissionsTeam(args[0])
	if err != nil {
		return err
	}
	if len(args) == 1 {
		policy, err := tools.LoadPathConsentPolicy(teamDir)
		if err != nil {
			return err
		}
		fmt.Printf("Team: %s\nPolicy: %s\n", args[0], tools.PathConsentPolicyPath(teamDir))
		printPolicyPaths("Allowed", policy.Allowed)
		printPolicyPaths("Denied", policy.Denied)
		return nil
	}
	if len(args) != 3 {
		return fmt.Errorf("provide both an action and a path, for example: hufu team permissions %s allow /srv/project", args[0])
	}
	action := strings.ToLower(strings.TrimSpace(args[1]))
	if action != "allow" && action != "deny" && action != "remove" {
		return fmt.Errorf("action must be allow, deny, or remove; got %q", args[1])
	}
	policy, err := tools.UpdatePathConsentPolicy(teamDir, action, args[2])
	if err != nil {
		return err
	}
	fmt.Printf("Updated %s path policy for team %s.\n", action, args[0])
	printPolicyPaths("Allowed", policy.Allowed)
	printPolicyPaths("Denied", policy.Denied)
	return nil
}

func resolvePermissionsTeam(name string) (string, error) {
	searchPaths := internalteam.DefaultSearchPaths()
	if teamPermissionsSearchPath != "" {
		searchPaths = strings.Split(teamPermissionsSearchPath, ",")
	}
	registry := internalteam.NewTeamRegistry(searchPaths)
	if err := registry.Discover(); err != nil {
		return "", err
	}
	return registry.Resolve(name)
}

func printPolicyPaths(label string, paths []string) {
	if len(paths) == 0 {
		fmt.Printf("%s: (none)\n", label)
		return
	}
	fmt.Printf("%s:\n", label)
	for _, path := range paths {
		fmt.Printf("  - %s\n", path)
	}
}
