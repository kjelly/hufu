package main

import (
	"fmt"
	"strings"
)

// WorkerModelOverride is one operator-supplied `agent=target` entry from
// --worker-model or a profile's worker-model value. Target is an execution
// target in the same syntax as --model (for example codex/gpt-6-sol); it is
// validated later by the canonical execution-selector preflight, never here.
type WorkerModelOverride struct {
	Agent  string
	Target string
}

// String renders the override in the flag syntax it was parsed from.
func (o WorkerModelOverride) String() string {
	return o.Agent + "=" + o.Target
}

// workerModelKey is the case-insensitive lookup key for an operator-supplied
// worker name. It matches the normalization the team loader uses for agent
// identities, so no separate alias system is introduced.
func workerModelKey(agentName string) string {
	return strings.ToLower(strings.TrimSpace(agentName))
}

// parseWorkerModelOverrides parses --worker-model values into keyed entries.
// Each value may itself hold comma-separated entries, which covers both the
// repeated CLI form and the single-string profile form. Within the input the
// last occurrence of an agent wins, while the entry keeps the position of its
// first occurrence so diagnostics stay in operator order.
func parseWorkerModelOverrides(values []string) ([]WorkerModelOverride, error) {
	var overrides []WorkerModelOverride
	positions := make(map[string]int)
	for _, value := range values {
		for _, entry := range strings.Split(value, ",") {
			override, err := parseWorkerModelEntry(entry)
			if err != nil {
				return nil, err
			}
			key := workerModelKey(override.Agent)
			if index, seen := positions[key]; seen {
				overrides[index] = override
				continue
			}
			positions[key] = len(overrides)
			overrides = append(overrides, override)
		}
	}
	return overrides, nil
}

func parseWorkerModelEntry(entry string) (WorkerModelOverride, error) {
	agentName, target, ok := strings.Cut(entry, "=")
	if !ok {
		return WorkerModelOverride{}, fmt.Errorf("invalid --worker-model entry %q: expected agent=target", entry)
	}
	agentName = strings.TrimSpace(agentName)
	target = strings.TrimSpace(target)
	if agentName == "" {
		return WorkerModelOverride{}, fmt.Errorf("invalid --worker-model entry %q: missing agent name", entry)
	}
	if target == "" {
		return WorkerModelOverride{}, fmt.Errorf("invalid --worker-model entry %q: missing execution target", entry)
	}
	return WorkerModelOverride{Agent: agentName, Target: target}, nil
}

// mergeWorkerModelOverrides overlays higher-precedence entries onto base by
// agent key. Base order is preserved and new agents are appended.
func mergeWorkerModelOverrides(base, overlay []WorkerModelOverride) []WorkerModelOverride {
	merged := make([]WorkerModelOverride, 0, len(base)+len(overlay))
	positions := make(map[string]int, len(base)+len(overlay))
	for _, layer := range [][]WorkerModelOverride{base, overlay} {
		for _, override := range layer {
			key := workerModelKey(override.Agent)
			if index, seen := positions[key]; seen {
				merged[index] = override
				continue
			}
			positions[key] = len(merged)
			merged = append(merged, override)
		}
	}
	return merged
}

// formatWorkerModelOverrides renders entries back into flag values.
func formatWorkerModelOverrides(overrides []WorkerModelOverride) []string {
	values := make([]string, 0, len(overrides))
	for _, override := range overrides {
		values = append(values, override.String())
	}
	return values
}
