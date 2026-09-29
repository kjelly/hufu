package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/teampkg"
)

func newTeamPackCommand() *cobra.Command {
	var version string
	var output string
	command := &cobra.Command{
		Use:   "pack <team-directory>",
		Short: "Build a deterministic portable team package",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			result, err := teampkg.Pack(teampkg.PackOptions{
				TeamDir: args[0], Version: version, Output: output,
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "packed team %s version %s to %s (%s, %d files)\n",
				result.Manifest.Name, result.Manifest.Version, result.Output, result.Lock.TeamDigest, result.FileCount)
			return err
		},
	}
	command.Flags().StringVar(&version, "version", "", "Package version label")
	command.Flags().StringVar(&output, "output", "", "Output .hufu archive path")
	_ = command.MarkFlagRequired("version")
	_ = command.MarkFlagRequired("output")
	return command
}

func init() {
	teamCmd.AddCommand(newTeamPackCommand())
}
