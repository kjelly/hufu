package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

var listOutput string

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "teams"},
	Short:   "List discoverable teams and their agents",
	Long: `Show every team hufu can find, and for each team its agents with their
role, model, tools and skills — so you know what you can call before you call it.

Includes Team Preset, inline worker and Markdown members, without injected helpers.
Malformed manifests or invalid composed rosters return an error, without partial JSON.

Usage:
  hufu list
  hufu list <team-name>`,
	Args: cobra.MaximumNArgs(1),
	RunE: runList,
}

// agentFrontmatter is the subset of an agent .md frontmatter we display.
// tools/skills accept either a string or a YAML list, hence interface{}.
type agentFrontmatter struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Role        string      `yaml:"role"`
	Model       string      `yaml:"model"`
	Tools       interface{} `yaml:"tools"`
	Skills      interface{} `yaml:"skills"`
	MemoryID    string      `yaml:"memory-id"`
	Memory      struct {
		Mode string `yaml:"mode"`
	} `yaml:"memory"`
}

type listedTeam struct {
	Name   string           `json:"name"`
	Dir    string           `json:"dir"`
	Agents []listedAgent    `json:"agents"`
	Inputs []listedRunInput `json:"inputs,omitempty"`
}

type listedRunInput struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Required bool            `json:"required,omitzero"`
	Default  json.RawMessage `json:"default,omitempty"`
}

type listedAgent struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Role        string   `json:"role"`
	Model       string   `json:"model,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	MemoryID    string   `json:"memory_id,omitempty"`
	MemoryMode  string   `json:"memory_mode,omitempty"`
}

func init() {
	listCmd.Flags().StringVar(&listOutput, "output", "text", "Output format: text, table, or json")
}

func runList(cmd *cobra.Command, args []string) error {
	if listOutput != "text" && listOutput != "table" && listOutput != "json" {
		return fmt.Errorf("invalid --output %q: use 'text', 'table', or 'json'", listOutput)
	}
	searchPaths := resolveSearchPaths()
	registry := team.NewTeamRegistry(searchPaths)
	if err := registry.Discover(); err != nil {
		return fmt.Errorf("failed to discover teams: %w", err)
	}
	if registry.TeamCount() == 0 {
		fmt.Fprintf(os.Stderr, "%s No teams found in %s.\n", errStyle.Render("✗"), strings.Join(searchPaths, ", "))
		fmt.Fprintf(os.Stderr, "  Use --default for the built-in team, or scaffold one: hufu init <team>\n")
		return nil
	}

	want := ""
	if len(args) == 1 {
		want = strings.ToLower(strings.TrimPrefix(args[0], "@"))
	}

	teams := registry.ListTeams()
	slices.Sort(teams)
	records := make([]listedTeam, 0, len(teams))
	composed := make(map[string]bool)
	for _, name := range teams {
		if want != "" && strings.ToLower(name) != want {
			continue
		}
		dir, err := registry.Resolve(name)
		if err != nil {
			return err
		}
		record, isComposed, err := collectListedTeamChecked(name, dir)
		if err != nil {
			return fmt.Errorf("team %s: %w", name, err)
		}
		records = append(records, record)
		composed[name] = isComposed
	}
	if want != "" && len(records) == 0 {
		return fmt.Errorf("team %q not found. Available: %s", want, strings.Join(teams, ", "))
	}
	if listOutput == "json" {
		return json.NewEncoder(os.Stdout).Encode(records)
	}
	for _, record := range records {
		if composed[record.Name] {
			printComposedTeam(record)
		} else {
			printTeam(record.Name, record.Dir)
		}
	}
	return nil
}

// New authoring uses the runtime compiler; legacy agent field display stays best-effort.
func collectListedTeamChecked(name, dir string) (listedTeam, bool, error) {
	composed, err := team.HasTeamComposition(dir, nil)
	if err != nil {
		return listedTeam{}, false, err
	}
	if !composed {
		return collectListedTeam(name, dir), false, nil
	}
	spec, err := team.CompileTeam(dir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		return listedTeam{}, true, err
	}
	session := spec.RuntimeSession()
	record := listedTeam{Name: name, Dir: dir, Agents: make([]listedAgent, 0)}
	seen := make(map[*agent.AgentDef]bool)
	for _, def := range session.Agents {
		if seen[def] || strings.EqualFold(def.FileAlias, "helper") {
			continue
		}
		seen[def] = true
		record.Agents = append(record.Agents, listedAgent{
			Name: def.Name, Description: def.Description, Role: def.Role,
			Model: cmp.Or(def.Generation.Model, session.Config.Generation.Model),
			Tools: splitNormalizedList(def.Tools), Skills: splitNormalizedList(def.Skills),
			MemoryID: def.MemoryID, MemoryMode: string(def.Memory.Mode),
		})
	}
	slices.SortFunc(record.Agents, func(a, b listedAgent) int { return strings.Compare(a.Name, b.Name) })
	for _, definition := range session.RunInputDefinitions {
		record.Inputs = append(record.Inputs, listedRunInput{Name: definition.Name, Type: definition.Schema.Type, Required: definition.Required, Default: definition.Default})
	}
	return record, true, nil
}

func printComposedTeam(record listedTeam) {
	fmt.Printf("%s %s  \n", teamStyle.Render("@"+record.Name), dimStyle.Render(record.Dir))
	for _, def := range record.Agents {
		fmt.Printf("  %s %s", agentStyle.Render("@"+def.Name), dimStyle.Render("["+def.Role+"]"))
		if def.Model != "" {
			fmt.Printf(" %s", dimStyle.Render(def.Model))
		}
		fmt.Println()
		if def.Description != "" {
			fmt.Printf("      %s\n", def.Description)
		}
		if len(def.Tools) > 0 {
			fmt.Printf("      tools:  %s\n", strings.Join(def.Tools, ","))
		}
		if len(def.Skills) > 0 {
			fmt.Printf("      skills: %s\n", strings.Join(def.Skills, ","))
		}
		if def.MemoryMode != "" && def.MemoryMode != "off" {
			fmt.Printf("      memory: %s\n", def.MemoryMode)
		}
		if def.MemoryID != "" {
			fmt.Printf("      memory-id: %s\n", def.MemoryID)
		}
	}
	if len(record.Inputs) > 0 {
		fmt.Println("  typed run inputs:")
		for _, input := range record.Inputs {
			value := "(none)"
			if len(input.Default) > 0 {
				value = string(input.Default)
			}
			fmt.Printf("      %s (%s, required=%t, default=%s)\n", input.Name, input.Type, input.Required, value)
		}
	}
	fmt.Println()
}

func collectListedTeam(name, dir string) listedTeam {
	record := listedTeam{Name: name, Dir: dir}
	if definitions, err := team.LoadRunInputDefinitions(dir, nil); err == nil {
		for _, definition := range definitions {
			record.Inputs = append(record.Inputs, listedRunInput{Name: definition.Name, Type: definition.Schema.Type, Required: definition.Required, Default: definition.Default})
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return record
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		fm := readAgentFrontmatter(filepath.Join(dir, entry.Name()))
		agentName := fm.Name
		if agentName == "" {
			agentName = strings.TrimSuffix(entry.Name(), ".md")
		}
		role := fm.Role
		if role == "" {
			role = "worker"
		}
		record.Agents = append(record.Agents, listedAgent{
			Name: agentName, Description: fm.Description, Role: role, Model: fm.Model,
			Tools:      splitNormalizedList(agent.ExpandImpliedTools(normalizeList(fm.Tools))),
			Skills:     splitNormalizedList(normalizeList(fm.Skills)),
			MemoryID:   fm.MemoryID,
			MemoryMode: fm.Memory.Mode,
		})
	}
	sort.Slice(record.Agents, func(i, j int) bool { return record.Agents[i].Name < record.Agents[j].Name })
	return record
}

func splitNormalizedList(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func printTeam(name, dir string) {
	fmt.Printf("%s %s  %s\n", teamStyle.Render("@"+name), dimStyle.Render(dir), "")

	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Printf("  %s could not read team dir: %v\n", errStyle.Render("✗"), err)
		return
	}
	var agentFiles []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		agentFiles = append(agentFiles, e.Name())
	}
	sort.Strings(agentFiles)
	if len(agentFiles) == 0 {
		fmt.Printf("  %s\n", dimStyle.Render("(no agent .md files)"))
		fmt.Println()
		return
	}

	for _, fn := range agentFiles {
		fm := readAgentFrontmatter(filepath.Join(dir, fn))
		display := fm.Name
		if display == "" {
			display = strings.TrimSuffix(fn, ".md")
		}
		role := fm.Role
		if role == "" {
			role = "worker"
		}
		fmt.Printf("  %s %s", agentStyle.Render("@"+display), dimStyle.Render("["+role+"]"))
		if fm.Model != "" {
			fmt.Printf(" %s", dimStyle.Render(fm.Model))
		}
		fmt.Println()
		if fm.Description != "" {
			fmt.Printf("      %s\n", fm.Description)
		}
		if tools := agent.ExpandImpliedTools(normalizeList(fm.Tools)); tools != "" {
			fmt.Printf("      tools:  %s\n", tools)
		}
		if skills := normalizeList(fm.Skills); skills != "" {
			fmt.Printf("      skills: %s\n", skills)
		}
		if fm.Memory.Mode != "" && fm.Memory.Mode != "off" {
			fmt.Printf("      memory: %s\n", fm.Memory.Mode)
		}
		if fm.MemoryID != "" {
			fmt.Printf("      memory-id: %s\n", fm.MemoryID)
		}
	}
	if definitions, err := team.LoadRunInputDefinitions(dir, nil); err == nil && len(definitions) > 0 {
		fmt.Println("  typed run inputs:")
		for _, definition := range definitions {
			defaultValue := "(none)"
			if len(definition.Default) > 0 {
				defaultValue = string(definition.Default)
			}
			fmt.Printf("      %s (%s, required=%t, default=%s)\n", definition.Name, definition.Schema.Type, definition.Required, defaultValue)
		}
	}
	fmt.Println()
}

// readAgentFrontmatter extracts the YAML frontmatter (between the first two
// `---` fences) from an agent .md file. Returns a zero value on any error so
// listing is best-effort and never fails the whole command.
func readAgentFrontmatter(path string) agentFrontmatter {
	var fm agentFrontmatter
	data, err := os.ReadFile(path)
	if err != nil {
		return fm
	}
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return fm
	}
	rest := strings.TrimPrefix(content, "---")
	// Find the closing fence.
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm
	}
	_ = yaml.Unmarshal([]byte(rest[:end]), &fm)
	return fm
}

// normalizeList renders a tools/skills frontmatter value (string or list) as a
// comma-separated string.
func normalizeList(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, strings.TrimSpace(fmt.Sprintf("%v", e)))
		}
		return strings.Join(parts, ", ")
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}
