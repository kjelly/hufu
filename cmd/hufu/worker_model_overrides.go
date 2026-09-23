package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
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

// resolveWorkerModelTargets maps every override onto exactly one loaded worker
// definition. It fails closed on unknown workers, coordinator/orchestrator
// targets, names that match more than one definition, and two entries that
// reach the same definition through different identities (file alias and
// name), so a keyed override can never be silently dropped or reassigned.
func resolveWorkerModelTargets(session *team.TeamSession, overrides []WorkerModelOverride) (map[*agent.AgentDef]string, error) {
	if len(overrides) == 0 {
		return nil, nil
	}
	targets := make(map[*agent.AgentDef]string, len(overrides))
	owners := make(map[*agent.AgentDef]string, len(overrides))
	for _, override := range overrides {
		def, err := resolveWorkerModelAgent(session, override.Agent)
		if err != nil {
			return nil, err
		}
		if previous, exists := owners[def]; exists {
			return nil, fmt.Errorf("--worker-model entries %q and %q both target worker %q", previous, override.Agent, def.Name)
		}
		owners[def] = override.Agent
		targets[def] = override.Target
	}
	return targets, nil
}

// resolveWorkerModelAgent finds the single agent definition an operator name
// refers to. It matches case-insensitively against the identities the team
// loader registers (map key, name, and file alias) rather than introducing a
// new alias system.
func resolveWorkerModelAgent(session *team.TeamSession, name string) (*agent.AgentDef, error) {
	key := workerModelKey(name)
	if key == "coordinator" {
		return nil, fmt.Errorf("worker-model override targets coordinator %q; use --coordinator-model instead", name)
	}
	var matches []*agent.AgentDef
	seen := make(map[*agent.AgentDef]bool)
	for identity, def := range session.Agents {
		if def == nil || seen[def] {
			continue
		}
		if workerModelKey(identity) == key || workerModelKey(def.Name) == key || workerModelKey(def.FileAlias) == key {
			seen[def] = true
			matches = append(matches, def)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("unknown worker %q in --worker-model for team %q; available workers: %s", name, session.Config.Name, strings.Join(workerModelCandidateNames(session), ", "))
	case 1:
	default:
		names := make([]string, 0, len(matches))
		for _, def := range matches {
			names = append(names, def.Name)
		}
		sortNamesFold(names)
		return nil, fmt.Errorf("worker %q in --worker-model is ambiguous for team %q: matches %s", name, session.Config.Name, strings.Join(names, ", "))
	}
	def := matches[0]
	if isCoordinatorRole(def.Role) || workerModelKey(def.Name) == "coordinator" {
		return nil, fmt.Errorf("worker-model override targets coordinator %q; use --coordinator-model instead", name)
	}
	return def, nil
}

// workerModelCandidateNames lists the distinct worker display names a
// --worker-model entry may target.
func workerModelCandidateNames(session *team.TeamSession) []string {
	seen := make(map[*agent.AgentDef]bool)
	var names []string
	for _, def := range session.Agents {
		if def == nil || seen[def] || isCoordinatorRole(def.Role) || workerModelKey(def.Name) == "coordinator" {
			continue
		}
		seen[def] = true
		names = append(names, def.Name)
	}
	sortNamesFold(names)
	return names
}

// workerModelDisplayEntries renders validated overrides with each worker's
// canonical display name for the startup summary.
func workerModelDisplayEntries(session *team.TeamSession, overrides []WorkerModelOverride) []string {
	entries := make([]string, 0, len(overrides))
	for _, override := range overrides {
		name := override.Agent
		if def, err := resolveWorkerModelAgent(session, override.Agent); err == nil {
			name = def.Name
		}
		entries = append(entries, name+"="+override.Target)
	}
	return entries
}

func sortNamesFold(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		left, right := strings.ToLower(names[i]), strings.ToLower(names[j])
		if left != right {
			return left < right
		}
		return names[i] < names[j]
	})
}
