package main

import (
	"fmt"
	"slices"
	"strings"
)

// resolveOutputAlias is the shared, side-effect-free selector for domain
// format flags and their common --output/--json aliases. Equivalent explicit
// values are accepted; conflicting explicit values fail before the command's
// operation begins.
func resolveOutputAlias(primaryName, primary string, primaryChanged bool, aliasName, alias string, aliasChanged bool, fallback string, allowed []string) (string, error) {
	primary = strings.ToLower(strings.TrimSpace(primary))
	alias = strings.ToLower(strings.TrimSpace(alias))
	selected := primary
	if selected == "" {
		selected = fallback
	}
	if aliasChanged {
		if primaryChanged && selected != alias {
			return "", fmt.Errorf("--%s %q conflicts with --%s %q", primaryName, primary, aliasName, alias)
		}
		selected = alias
	}
	if !slices.Contains(allowed, selected) {
		return "", fmt.Errorf("invalid --%s %q: use %s", primaryName, selected, strings.Join(allowed, " or "))
	}
	return selected, nil
}
