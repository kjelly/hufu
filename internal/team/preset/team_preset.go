package preset

import (
	"fmt"
	"sort"
	"strings"
)

// TeamPresetAgent is one agent file a TeamPreset generates. Team presets
// compile down to normal authoring-level agent files (an AgentPreset name
// in frontmatter) rather than a distinct runtime concept: a caller writes
// each Agents entry as "<FileName>" with a `preset: <AgentPreset>`
// frontmatter field and the given System prompt, then loads the result
// through the ordinary CompileTeam/LoadTeam pipeline like any other team
// (spec.md Specification 05 Phase 7 "Team preset compilation should
// produce normal intermediate agent/team definitions and then pass through
// the same compiler").
type TeamPresetAgent struct {
	FileName    string
	Role        string
	Description string
	AgentPreset string
	System      string
}

// TeamPreset is a deterministic, named team-level composition (spec.md
// Specification 01 §8).
type TeamPreset struct {
	Name   string
	Agents []TeamPresetAgent
}

// teamRegistry holds the initial built-in team presets. Each AgentPreset
// referenced here must exist in registry (agent presets); a test in
// team_preset_test.go enforces this so the two registries can't drift.
var teamRegistry = map[string]TeamPreset{
	"coding-single": {
		Name: "coding-single",
		Agents: []TeamPresetAgent{
			{
				FileName:    "worker.md",
				Role:        "worker",
				Description: "General-purpose worker",
				AgentPreset: "coding",
				System:      "You are a capable, careful worker. Implement the requested change end to end. Prefer reading existing files before editing them, keep changes minimal and idiomatic, and report concisely what you did.",
			},
		},
	},
	"coding-reviewed": {
		Name: "coding-reviewed",
		Agents: []TeamPresetAgent{
			{
				FileName:    "developer.md",
				Role:        "worker",
				Description: "Implements production code changes",
				AgentPreset: "coding",
				System:      "Implement the requested change carefully. Read the code and tests first, then verify the result.",
			},
			{
				FileName:    "reviewer.md",
				Role:        "worker",
				Description: "Reviews changes for correctness and regressions",
				AgentPreset: "review",
				System:      "Review the implementation for correctness, edge cases, and missing tests. Report actionable findings.",
			},
		},
	},
	"research": {
		Name: "research",
		Agents: []TeamPresetAgent{
			{
				FileName:    "researcher.md",
				Role:        "worker",
				Description: "Investigates sources and technical context",
				AgentPreset: "research",
				System:      "Research the question, distinguish facts from inferences, and provide concise source-backed findings.",
			},
			{
				FileName:    "writer.md",
				Role:        "worker",
				Description: "Produces clear, accurate deliverables",
				AgentPreset: "writer",
				System:      "Turn the available research into a clear, accurate deliverable for the requested audience.",
			},
		},
	},
	"safe-ops": {
		Name: "safe-ops",
		Agents: []TeamPresetAgent{
			{
				FileName:    "operator.md",
				Role:        "worker",
				Description: "Performs carefully scoped operational tasks",
				AgentPreset: "ops",
				System:      "Inspect first, make the smallest safe operational change, and verify the resulting system state.",
			},
			{
				FileName:    "monitor.md",
				Role:        "worker",
				Description: "Verifies system health and operational outcomes",
				AgentPreset: "review",
				System:      "Check health signals, identify deviations, and provide an evidence-based operational summary.",
			},
		},
	},
}

// Render produces the on-disk file contents for this team preset: a map
// from filename (e.g. "developer.md") to its full Markdown content
// (frontmatter + system prompt). `role` is only emitted when it is not
// "worker" — Specification 05 Phase 2's filename inference already
// defaults every non-coordinator.md file to "worker", so omitting it here
// follows Phase 1's "don't emit a value equal to its default" rule; team
// presets currently never name a file coordinator.md, so this never fires
// today, but stays correct if a future preset does. Callers own writing
// these files to disk with whatever overwrite-safety policy they need —
// this package performs no I/O.
func (p TeamPreset) Render() map[string]string {
	files := make(map[string]string, len(p.Agents))
	for _, a := range p.Agents {
		var b strings.Builder
		b.WriteString("---\n")
		fmt.Fprintf(&b, "description: %s\n", a.Description)
		if a.Role != "" && a.Role != "worker" {
			fmt.Fprintf(&b, "role: %s\n", a.Role)
		}
		fmt.Fprintf(&b, "preset: %s\n", a.AgentPreset)
		b.WriteString("---\n")
		b.WriteString(a.System)
		b.WriteString("\n")
		files[a.FileName] = b.String()
	}
	return files
}

// LookupTeam returns the named built-in team preset. Matching is
// case-insensitive, mirroring Lookup for agent presets.
func LookupTeam(name string) (TeamPreset, bool) {
	p, ok := teamRegistry[normalizePresetName(name)]
	return p, ok
}

// TeamNames returns every built-in team preset name, sorted.
func TeamNames() []string {
	names := make([]string, 0, len(teamRegistry))
	for name := range teamRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
