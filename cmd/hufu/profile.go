package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/kjelly/hufu/internal/config"
)

// applyProfile applies a named flag bundle from hufu.yaml `profiles:` to the
// current command's flags. Precedence is: explicit CLI flag > profile > default,
// achieved by skipping any flag the user already set (flag.Changed). Flag values
// are stored as strings the flag itself parses, so type handling is automatic.
// A flag named in the profile that this command does not define is reported as
// an error rather than silently ignored, so typos surface early.
func applyProfile(cmd *cobra.Command) error {
	if opts.profileName == "" {
		return nil
	}
	cfg := config.LoadConfig()
	profile, ok := cfg.Profiles[opts.profileName]
	if !ok {
		var names []string
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return fmt.Errorf("profile %q not found: no profiles defined in hufu.yaml", opts.profileName)
		}
		return fmt.Errorf("profile %q not found. Available: %s", opts.profileName, strings.Join(names, ", "))
	}

	// Resolve a flag by name against this command, then its root (for flags
	// bound on the root command while a subcommand is executing).
	lookup := func(name string) *pflag.FlagSet {
		if cmd.Flags().Lookup(name) != nil {
			return cmd.Flags()
		}
		if root := cmd.Root(); root != nil && root.Flags().Lookup(name) != nil {
			return root.Flags()
		}
		return nil
	}

	// Apply in sorted key order for deterministic behavior.
	keys := make([]string, 0, len(profile))
	for k := range profile {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		fs := lookup(name)
		if fs == nil {
			return fmt.Errorf("profile %q sets unknown flag %q", opts.profileName, name)
		}
		if fs.Changed(name) {
			continue // explicit CLI flag wins
		}
		if err := fs.Set(name, profile[name]); err != nil {
			return fmt.Errorf("profile %q: invalid value for --%s: %w", opts.profileName, name, err)
		}
	}
	fmt.Fprintf(os.Stderr, "%s Applied profile %s\n", dimStyle.Render("·"), boldStyle.Render(opts.profileName))
	return nil
}
