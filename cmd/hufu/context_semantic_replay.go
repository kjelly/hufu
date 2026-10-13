package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/config"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/semantic"
	"github.com/kjelly/hufu/internal/team"
)

// contextSemanticMode is --semantic-mode of explain-memory and
// ranking-replay; empty uses the configured mode.
var contextSemanticMode string

const contextSemanticModeUsage = "Semantic retrieval mode to replay: off, shadow, active, or auto (default: hufu.yaml with the team's semantic-retrieval block)"

// contextSemanticSettings resolves the semantic-retrieval settings a run of
// teamName would use: hufu.yaml merged with the team's own block when the
// team is discoverable. A team that cannot be read keeps hufu.yaml and is
// reported on warn.
func contextSemanticSettings(teamName string, warn io.Writer) (semantic.Settings, error) {
	settings, _, _, err := semanticSettingsForTeam(teamName, warn)
	return settings, err
}

// semanticSettingsForTeam is contextSemanticSettings that also reports
// whether the team was found in the search path and whether its block
// changed anything.
func semanticSettingsForTeam(teamName string, warn io.Writer) (settings semantic.Settings, found, overridden bool, err error) {
	cfg := config.LoadConfig().SemanticRetrieval
	registry := team.NewTeamRegistry(resolveSearchPaths())
	if discoverErr := registry.Discover(); discoverErr == nil && strings.TrimSpace(teamName) != "" {
		if dir, resolveErr := registry.Resolve(teamName); resolveErr == nil {
			found = true
			block, blockErr := team.TeamSemanticRetrievalConfig(dir)
			if blockErr != nil {
				_, _ = fmt.Fprintf(warn, "warning: %v; using hufu.yaml semantic-retrieval only\n", blockErr)
			} else if block != (semantic.Config{}) {
				cfg, overridden = semantic.Merge(cfg, block), true
			}
		}
	}
	settings, err = cfg.Resolve()
	if err != nil {
		return semantic.Settings{}, found, overridden, fmt.Errorf("resolve semantic-retrieval: %w", err)
	}
	return settings, found, overridden, nil
}

// parseContextSemanticMode validates a --semantic-mode style flag; empty
// keeps fallback.
func parseContextSemanticMode(flag, value string, fallback semantic.Mode) (semantic.Mode, error) {
	switch mode := semantic.Mode(strings.TrimSpace(value)); mode {
	case "":
		return fallback, nil
	case semantic.ModeOff, semantic.ModeShadow, semantic.ModeActive, semantic.ModeAuto:
		return mode, nil
	default:
		return "", fmt.Errorf("--%s must be off, shadow, active, or auto, got %q", flag, value)
	}
}

// openContextSemanticReplay opens the read-only semantic lane with the first
// enabled mode, or returns nil when every mode is off.
func openContextSemanticReplay(ctx context.Context, settings semantic.Settings, repo contextstore.Repository, projectID string, modes ...semantic.Mode) (*team.SemanticReplay, error) {
	for _, mode := range modes {
		if mode != semantic.ModeOff {
			settings.Mode = mode
			return team.OpenSemanticReplay(ctx, settings, repo, projectID)
		}
	}
	return nil, nil
}

// formatContextSemanticView is the text form of a replayed lane.
func formatContextSemanticView(view team.SemanticReplayView) string {
	line := fmt.Sprintf("mode=%s fusion=%s state=%s model=%s generation=%s", view.Mode, valueOrDash(string(view.Fusion)), view.State, view.Model, valueOrUnavailable(view.GenerationID))
	if view.Failure != "" {
		line += " failure=" + view.Failure
	}
	return line
}

// formatContextCounts renders key:count pairs in key order, or "-".
func formatContextCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(counts))
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s:%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

// replayRouteLabel names a replayed route; a side without the lane is "off".
func replayRouteLabel(route string) string {
	if route == "" {
		return "off"
	}
	return route
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
