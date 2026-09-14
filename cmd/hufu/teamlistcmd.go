package main

import "github.com/spf13/cobra"

type teamListOptions struct {
	output, searchPath string
}

func newTeamListCommand() *cobra.Command {
	options := &teamListOptions{output: "text"}
	command := &cobra.Command{
		Use:               "list [team]",
		Aliases:           []string{"ls"},
		Short:             "List discoverable teams and their agents",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			previousOutput, previousOpts := listOutput, opts
			defer func() { listOutput, opts = previousOutput, previousOpts }()
			listOutput = options.output
			opts.agentTeamSearchPath = options.searchPath
			return runList(command, args)
		},
	}
	command.Flags().StringVar(&options.output, "output", "text", "Output format: text, table, or json")
	command.Flags().StringVar(&options.searchPath, "agent-team-search-path", "", "Comma-separated team search paths")
	registerStaticFlagCompletion(command, "output", []string{"text", "table", "json"})
	return command
}
