package main

import "github.com/spf13/pflag"

// registerTeamSelectionFlags binds both spellings to the value used by the command.
func registerTeamSelectionFlags(flags *pflag.FlagSet, target *string, usage string) {
	flags.StringVar(target, "team", "", usage)
	flags.StringVar(target, "agent-team", "", "Alias for --team")
}
