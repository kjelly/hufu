package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	internalteam "github.com/kjelly/hufu/internal/team"
)

var (
	teamExplainName   string
	teamExplainFormat string
)

var teamExplainCmd = &cobra.Command{
	Use:   "explain [team-directory]",
	Short: "Show the compiled effective team configuration and where each value came from",
	Long: `Compile a team and show its resolved configuration with provenance: which
values are explicit, which came from a preset, and which are built-in
defaults. Use a directory argument, or --team to resolve a discoverable
team name. Performs no model call.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTeamExplain,
}

func init() {
	teamCmd.AddCommand(teamExplainCmd)
	teamExplainCmd.Flags().StringVar(&teamExplainName, "team", "", "Discoverable team name to explain")
	teamExplainCmd.Flags().StringVar(&teamExplainFormat, "format", "text", "Output format: text, yaml, or json")
}

func runTeamExplain(_ *cobra.Command, args []string) error {
	teamDir, err := resolveTeamDirArg(args, teamExplainName)
	if err != nil {
		return err
	}
	spec, err := internalteam.CompileTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry)
	if err != nil {
		return err
	}

	switch strings.ToLower(strings.TrimSpace(teamExplainFormat)) {
	case "", "text":
		_, err = fmt.Fprint(os.Stdout, renderTeamExplainText(spec))
		return err
	case "json":
		out, err := json.MarshalIndent(buildExplainOutput(spec), "", "  ")
		if err != nil {
			return fmt.Errorf("marshal explain output: %w", err)
		}
		_, err = fmt.Fprintln(os.Stdout, string(out))
		return err
	case "yaml":
		out, err := yaml.Marshal(buildExplainOutput(spec))
		if err != nil {
			return fmt.Errorf("marshal explain output: %w", err)
		}
		_, err = fmt.Fprint(os.Stdout, string(out))
		return err
	default:
		return fmt.Errorf("unknown --format %q: supported formats are text, yaml, json", teamExplainFormat)
	}
}

// explainAgent is one deduplicated agent entry for --format yaml/json.
// internalteam.EffectiveTeamSpec.Agents is keyed by both an agent's
// file-alias and its (possibly different) explicit name, both pointing at
// equivalent data; explain output shows each real agent exactly once.
type explainAgent struct {
	Key        string                             `json:"key" yaml:"key"`
	Name       internalteam.ResolvedValue[string] `json:"name" yaml:"name"`
	Role       internalteam.ResolvedValue[string] `json:"role" yaml:"role"`
	Tools      internalteam.ResolvedValue[string] `json:"tools" yaml:"tools"`
	SideEffect internalteam.ResolvedValue[string] `json:"side_effect,omitempty" yaml:"side_effect,omitempty"`
}

// explainOutput is the --format yaml/json shape. It intentionally excludes
// internalteam.EffectiveTeamSpec's unexported runtime session — explain
// output is meant for a human or a tool inspecting resolved configuration,
// not for reconstructing runtime state (that projection point is
// EffectiveTeamSpec.RuntimeSession(), reserved for Go callers).
type explainOutput struct {
	Name        internalteam.ResolvedValue[string] `json:"name" yaml:"name"`
	Description internalteam.ResolvedValue[string] `json:"description,omitempty" yaml:"description,omitempty"`
	Model       internalteam.ResolvedValue[string] `json:"model,omitempty" yaml:"model,omitempty"`
	MaxRounds   internalteam.ResolvedValue[int]    `json:"max_rounds" yaml:"max_rounds"`
	Timeout     internalteam.ResolvedValue[int64]  `json:"timeout" yaml:"timeout"`
	MaxRetries  internalteam.ResolvedValue[int]    `json:"max_retries" yaml:"max_retries"`
	Agents      []explainAgent                     `json:"agents" yaml:"agents"`
	Diagnostics []internalteam.ContractFinding     `json:"diagnostics,omitempty" yaml:"diagnostics,omitempty"`
}

// dedupedExplainAgents collapses EffectiveTeamSpec.Agents' dual file-alias/
// name keys into one entry per real agent (grouped by resolved Name.Value),
// sorted by key for deterministic output.
func dedupedExplainAgents(spec *internalteam.EffectiveTeamSpec) []explainAgent {
	seen := make(map[string]bool, len(spec.Agents))
	keys := make([]string, 0, len(spec.Agents))
	for key := range spec.Agents {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	agents := make([]explainAgent, 0, len(spec.Agents))
	for _, key := range keys {
		a := spec.Agents[key]
		if seen[a.Name.Value] {
			continue
		}
		seen[a.Name.Value] = true
		agents = append(agents, explainAgent{
			Key:        key,
			Name:       a.Name,
			Role:       a.Role,
			Tools:      a.Tools,
			SideEffect: a.SideEffect,
		})
	}
	return agents
}

func buildExplainOutput(spec *internalteam.EffectiveTeamSpec) explainOutput {
	return explainOutput{
		Name:        spec.Name,
		Description: spec.Description,
		Model:       spec.Model,
		MaxRounds:   spec.MaxRounds,
		Timeout:     spec.Timeout,
		MaxRetries:  spec.MaxRetries,
		Agents:      dedupedExplainAgents(spec),
		Diagnostics: internalteam.ValidateEffectiveTeam(spec),
	}
}

func renderTeamExplainText(spec *internalteam.EffectiveTeamSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Team: %s\n\n", spec.Name.Value)

	fmt.Fprintln(&b, "Resolution")
	writeResolvedLine(&b, "  ", "name", spec.Name.Value, spec.Name.Source, spec.Name.Detail)
	if spec.Description.Value != "" {
		writeResolvedLine(&b, "  ", "description", spec.Description.Value, spec.Description.Source, spec.Description.Detail)
	}
	if spec.Model.Value != "" {
		writeResolvedLine(&b, "  ", "model", spec.Model.Value, spec.Model.Source, spec.Model.Detail)
	}
	writeResolvedLine(&b, "  ", "max-rounds", spec.MaxRounds.Value, spec.MaxRounds.Source, spec.MaxRounds.Detail)
	writeResolvedLine(&b, "  ", "timeout", spec.Timeout.Value, spec.Timeout.Source, spec.Timeout.Detail)
	writeResolvedLine(&b, "  ", "max-retries", spec.MaxRetries.Value, spec.MaxRetries.Source, spec.MaxRetries.Detail)

	session := spec.RuntimeSession()
	if session != nil {
		cfg := session.Config
		var restrictions []string
		if cfg.NoNet {
			restrictions = append(restrictions, "no-net")
		}
		if cfg.ForceMCP {
			restrictions = append(restrictions, "force-mcp")
		}
		if cfg.Unattended {
			restrictions = append(restrictions, "unattended")
		}
		if len(cfg.ToolsDenied) > 0 {
			restrictions = append(restrictions, "tools.denied: "+strings.Join(cfg.ToolsDenied, ","))
		}
		if len(restrictions) > 0 {
			fmt.Fprintf(&b, "\nPolicy restrictions: %s\n", strings.Join(restrictions, ", "))
		}
		if len(cfg.Workflow.Phases) > 0 {
			fmt.Fprintf(&b, "\nWorkflow: %d phase(s) (%s), %d static task(s)\n", len(cfg.Workflow.Phases), strings.Join(cfg.Workflow.Phases, ", "), len(session.ContractTasks))
		}
		if cfg.Unattended || cfg.MaxWallClock > 0 || cfg.MaxTotalTokens > 0 {
			fmt.Fprintln(&b, "\nReliability:")
			if cfg.Unattended {
				fmt.Fprintf(&b, "  unattended: true\n")
			}
			if cfg.MaxWallClock > 0 {
				fmt.Fprintf(&b, "  max-duration: %ds\n", cfg.MaxWallClock)
			}
			if cfg.MaxTotalTokens > 0 {
				fmt.Fprintf(&b, "  max-total-tokens: %d\n", cfg.MaxTotalTokens)
			}
		}
	}

	fmt.Fprintln(&b, "\nAgents")
	for _, a := range dedupedExplainAgents(spec) {
		fmt.Fprintf(&b, "\n  %s\n", a.Name.Value)
		writeResolvedLine(&b, "    ", "name", a.Name.Value, a.Name.Source, a.Name.Detail)
		writeResolvedLine(&b, "    ", "role", a.Role.Value, a.Role.Source, a.Role.Detail)
		fmt.Fprintln(&b, "    effective tools:")
		tools := strings.Split(a.Tools.Value, ",")
		for _, tool := range tools {
			if tool = strings.TrimSpace(tool); tool != "" {
				fmt.Fprintf(&b, "      %s\n", tool)
			}
		}
		fmt.Fprintf(&b, "      source: %s\n", explainSourceDetail(a.Tools))
		if a.SideEffect.Value != "" {
			writeResolvedLine(&b, "    ", "side effect", a.SideEffect.Value, a.SideEffect.Source, a.SideEffect.Detail)
		}
	}

	if findings := internalteam.ValidateEffectiveTeam(spec); len(findings) > 0 {
		fmt.Fprintln(&b, "\nDiagnostics")
		for _, f := range findings {
			fmt.Fprintf(&b, "  [%s] %s: %s\n", f.Severity, f.Field, f.Message)
		}
	}

	return b.String()
}

// writeResolvedLine prints one "field: value" line followed by an indented
// "source: ..." line, matching spec.md Specification 03 §7's example
// explain layout.
func writeResolvedLine[T any](b *strings.Builder, indent, field string, value T, source internalteam.ValueSource, detail string) {
	fmt.Fprintf(b, "%s%s: %v\n", indent, field, value)
	fmt.Fprintf(b, "%s  source: %s\n", indent, explainSourceDetailFor(source, detail))
}

func explainSourceDetail(v internalteam.ResolvedValue[string]) string {
	return explainSourceDetailFor(v.Source, v.Detail)
}

func explainSourceDetailFor(source internalteam.ValueSource, detail string) string {
	if detail == "" {
		return string(source)
	}
	return fmt.Sprintf("%s (%s)", source, detail)
}
