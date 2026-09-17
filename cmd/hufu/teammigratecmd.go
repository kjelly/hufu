package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	internalteam "github.com/kjelly/hufu/internal/team"
)

var (
	teamMigrateName               string
	teamMigrateTo                 string
	teamMigrateDryRun             bool
	teamMigrateCanonicalAuthoring bool
)

var teamMigrateCmd = &cobra.Command{
	Use:   "migrate [team-directory]",
	Short: "Preview migrating a team.yaml to a versioned schema",
	Long: `Read a team's team.yml/team.yaml, whatever schema it currently declares, and
print the stable, semantically equivalent hufu.io/v1alpha1 manifest.

Use a directory argument, or --team to resolve a discoverable team name.
This version only supports --dry-run: it never writes anything, it only
prints the migrated manifest to stdout for review (docs/architecture/
team-schema-versioning.md §9).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTeamMigrate,
}

func init() {
	teamCmd.AddCommand(teamMigrateCmd)
	teamMigrateCmd.Flags().StringVar(&teamMigrateName, "team", "", "Discoverable team name to migrate")
	teamMigrateCmd.Flags().StringVar(&teamMigrateTo, "to", internalteam.SchemaVersionV1Alpha1, "Target schema version")
	teamMigrateCmd.Flags().BoolVar(&teamMigrateDryRun, "dry-run", false, "Print the migrated manifest without writing it (required in this version)")
	teamMigrateCmd.Flags().BoolVar(&teamMigrateCanonicalAuthoring, "canonical-authoring", false, "Rewrite supported legacy decision/request fields to canonical authoring")
}

func runTeamMigrate(_ *cobra.Command, args []string) error {
	teamDir, err := resolveTeamDirArg(args, teamMigrateName)
	if err != nil {
		return err
	}
	if teamMigrateTo != internalteam.SchemaVersionV1Alpha1 {
		return fmt.Errorf("unsupported --to %q (supported: %q)", teamMigrateTo, internalteam.SchemaVersionV1Alpha1)
	}
	if !teamMigrateDryRun {
		return fmt.Errorf("team migrate only supports --dry-run in this version; writing the migrated manifest directly is not yet implemented")
	}

	var out []byte
	var already bool
	var warnings []string
	if teamMigrateCanonicalAuthoring {
		out, already, warnings, err = internalteam.MigrateTeamManifestToV1Alpha1CanonicalAuthoringDetailed(teamDir)
	} else {
		out, already, err = internalteam.MigrateTeamManifestToV1Alpha1(teamDir)
	}
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(os.Stderr, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	if already {
		if _, err := fmt.Fprintf(os.Stderr, "team at %s already declares apiVersion %s; showing its canonical re-serialization\n", teamDir, internalteam.SchemaVersionV1Alpha1); err != nil {
			return err
		}
	}
	_, err = os.Stdout.Write(out)
	return err
}
