package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var examplesCmd = &cobra.Command{Use: "examples", Short: "Show common hufu commands", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
	var output strings.Builder
	section := ""
	for _, example := range canonicalExamples {
		if example.Section != section {
			if section != "" {
				output.WriteByte('\n')
			}
			section = example.Section
			output.WriteString(section)
			output.WriteString(":\n")
		}
		output.WriteString("  ")
		output.WriteString(example.Argv)
		output.WriteByte('\n')
	}
	_, err := fmt.Fprint(os.Stdout, output.String())
	return err
}}

var helpFlagsCmd = &cobra.Command{Use: "help-flags <group>", Short: "Show flags in a named group", Args: cobra.ExactArgs(1), RunE: runHelpFlags}

func runHelpFlags(cmd *cobra.Command, args []string) error {
	group := strings.ToLower(args[0])
	flags, ok := cliFlagGroups[group]
	if !ok {
		names := make([]string, 0, len(cliFlagGroups))
		for name := range cliFlagGroups {
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
			if run, _, err := root.Find([]string{"run"}); err == nil {
				flag = run.Flags().Lookup(name)
			}
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
