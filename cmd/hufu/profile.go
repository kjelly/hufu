package main

import (
	"fmt"
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
//
// worker-model is the one keyed flag: see applyProfileWorkerModels.
func applyProfile(cmd *cobra.Command) error {
	return applyNamedProfile(cmd, opts.profileName)
}

func applyNamedProfile(cmd *cobra.Command, profileName string) error {
	if profileName == "" {
		return nil
	}
	cfg := config.LoadConfig()
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		var names []string
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return fmt.Errorf("profile %q not found: no profiles defined in hufu.yaml", profileName)
		}
		return fmt.Errorf("profile %q not found. Available: %s", profileName, strings.Join(names, ", "))
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

	// Capture this before any profile value is applied: fs.Set marks a flag
	// changed, and a profile `model` sorts before `worker-model`.
	explicitModel := false
	if fs := lookup("model"); fs != nil {
		explicitModel = fs.Changed("model")
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
			return fmt.Errorf("profile %q sets unknown flag %q", profileName, name)
		}
		if name == workerModelFlag {
			if err := applyProfileWorkerModels(fs, profileName, profile[name], explicitModel); err != nil {
				return err
			}
			continue
		}
		if fs.Changed(name) {
			continue // explicit CLI flag wins
		}
		if err := fs.Set(name, profile[name]); err != nil {
			return fmt.Errorf("profile %q: invalid value for --%s: %w", profileName, name, err)
		}
	}
	// JSONL is a framing contract on diagnostics as well as status events.
	// A profile may itself select JSONL, so inspect the now-resolved command
	// flag instead of only the package-level runtime snapshot.
	if flag := cmd.Flags().Lookup("event-format"); flag == nil || flag.Value.String() != "jsonl" {
		_, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s Applied profile %s\n", dimStyle.Render("·"), boldStyle.Render(profileName))
		return err
	}
	return nil
}

const workerModelFlag = "worker-model"

// applyProfileWorkerModels merges a profile's worker-model entries into the
// --worker-model flag by agent instead of skipping the whole flag when the
// CLI set it: explicit CLI entries replace profile entries for the same agent
// and every other profile entry is kept. An explicit CLI --model outranks all
// profile worker entries, so they are ignored (unparsed) in that case; this
// keeps `-m` meaning "use this worker target for this run".
func applyProfileWorkerModels(fs *pflag.FlagSet, profileName, value string, explicitModel bool) error {
	if explicitModel {
		return nil
	}
	profileEntries, err := parseWorkerModelOverrides([]string{value})
	if err != nil {
		return fmt.Errorf("profile %q: invalid value for --%s: %w", profileName, workerModelFlag, err)
	}
	if !fs.Changed(workerModelFlag) {
		if err := fs.Set(workerModelFlag, value); err != nil {
			return fmt.Errorf("profile %q: invalid value for --%s: %w", profileName, workerModelFlag, err)
		}
		return nil
	}
	cliValues, err := fs.GetStringSlice(workerModelFlag)
	if err != nil {
		return fmt.Errorf("read --%s: %w", workerModelFlag, err)
	}
	cliEntries, err := parseWorkerModelOverrides(cliValues)
	if err != nil {
		return err
	}
	list, ok := fs.Lookup(workerModelFlag).Value.(pflag.SliceValue)
	if !ok {
		return fmt.Errorf("--%s is not a list flag", workerModelFlag)
	}
	merged := mergeWorkerModelOverrides(profileEntries, cliEntries)
	if err := list.Replace(formatWorkerModelOverrides(merged)); err != nil {
		return fmt.Errorf("merge profile %q into --%s: %w", profileName, workerModelFlag, err)
	}
	return nil
}
