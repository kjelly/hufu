package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init <team-name>",
	Short: "Scaffold a minimal runnable agent team",
	Long: `Create a minimal team under .agent-teams/<team-name>/ so you can start
calling agents immediately. Generates a team.yaml and a single worker agent.

Existing files are never overwritten.

Examples:
  hufu init dev-team
  hufu init dev-team --model local-model`,
	Args: cobra.ExactArgs(1),
	RunE: runInit,
}

const teamYAMLTemplate = `name: %[1]s
description: Scaffolded by hufu init
max-rounds: 10
timeout: 600
max-retries: 2
workspace: workspace
%[2]s`

const agentMDTemplate = `---
name: worker
description: General-purpose worker
role: worker
tools: view,write,edit,multiedit,grep,glob,ls,bash
---
You are a capable, careful worker on the %[1]s team.

Complete the task you are given end to end. Prefer reading existing files before
editing them, keep changes minimal and idiomatic, and report concisely what you
did. If a task is ambiguous, make the most reasonable assumption and state it.
`

type scaffoldTemplate struct {
	agents map[string]string
}

var scaffoldTemplates = map[string]scaffoldTemplate{
	"default": {agents: map[string]string{"worker.md": agentMDTemplate}},
	"dev": {agents: map[string]string{
		"developer.md": `---
name: developer
description: Implements production code changes
role: worker
tools: view,write,edit,multiedit,grep,glob,ls,bash
---
Implement the requested change carefully. Read the code and tests first, then verify the result.
`,
		"reviewer.md": `---
name: reviewer
description: Reviews changes for correctness and regressions
role: worker
tools: view,grep,glob,ls,bash
---
Review the implementation for correctness, edge cases, and missing tests. Report actionable findings.
`,
		"tester.md": `---
name: tester
description: Validates behavior with focused tests
role: worker
tools: view,write,edit,grep,glob,ls,bash
---
Create or run focused tests that demonstrate the requested behavior and report the result.
`,
	}},
	"research": {agents: map[string]string{
		"researcher.md": `---
name: researcher
description: Investigates sources and technical context
role: worker
tools: view,grep,glob,ls,fetch,agentic_fetch
---
Research the question, distinguish facts from inferences, and provide concise source-backed findings.
`,
		"writer.md": `---
name: writer
description: Produces clear, accurate deliverables
role: worker
tools: view,write,edit,grep,glob,ls
---
Turn the available research into a clear, accurate deliverable for the requested audience.
`,
	}},
	"ops": {agents: map[string]string{
		"operator.md": `---
name: operator
description: Performs carefully scoped operational tasks
role: worker
tools: view,grep,glob,ls,bash,sudo,ssh
---
Inspect first, make the smallest safe operational change, and verify the resulting system state.
`,
		"monitor.md": `---
name: monitor
description: Verifies system health and operational outcomes
role: worker
tools: view,grep,glob,ls,bash,ssh
---
Check health signals, identify deviations, and provide an evidence-based operational summary.
`,
	}},
	"minimal": {agents: map[string]string{}},
}

func runInit(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return fmt.Errorf("team name required: hufu init <team-name>")
	}

	templateName := strings.ToLower(strings.TrimSpace(opts.initTemplateName))
	if templateName == "" {
		templateName = "default"
	}
	template, ok := scaffoldTemplates[templateName]
	if !ok {
		names := make([]string, 0, len(scaffoldTemplates))
		for candidate := range scaffoldTemplates {
			names = append(names, candidate)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown --template %q: supported templates: %s", opts.initTemplateName, strings.Join(names, ", "))
	}

	teamDir := filepath.Join(".agent-teams", name)
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		return fmt.Errorf("failed to create %s: %w", teamDir, err)
	}

	modelLine := ""
	if opts.modelOverride != "" {
		modelLine = "model: " + opts.modelOverride + "\n"
	}

	created, err := writeIfAbsent(
		filepath.Join(teamDir, "team.yaml"),
		fmt.Sprintf(teamYAMLTemplate, name, modelLine),
	)
	if err != nil {
		return err
	}
	reportScaffold(filepath.Join(teamDir, "team.yaml"), created)

	agentFiles := make([]string, 0, len(template.agents))
	for filename := range template.agents {
		agentFiles = append(agentFiles, filename)
	}
	sort.Strings(agentFiles)
	for _, filename := range agentFiles {
		created, err = writeIfAbsent(
			filepath.Join(teamDir, filename),
			fmt.Sprintf(template.agents[filename], name),
		)
		if err != nil {
			return err
		}
		reportScaffold(filepath.Join(teamDir, filename), created)
	}

	fmt.Fprintf(os.Stderr, "\n%s Team %s is ready.\n", doneStyle.Render("✓"), teamStyle.Render(name))
	fmt.Fprintf(os.Stderr, "  Try it:   hufu @%s \"say hello\"\n", name)
	fmt.Fprintf(os.Stderr, "  Inspect:  hufu list %s\n", name)
	fmt.Fprintf(os.Stderr, "  Preflight: hufu doctor\n")
	return nil
}

// writeIfAbsent writes content to path only if it does not already exist.
// Returns true if the file was created, false if it was left untouched.
func writeIfAbsent(path, content string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, fmt.Errorf("failed to write %s: %w", path, err)
	}
	return true, nil
}

func reportScaffold(path string, created bool) {
	if created {
		fmt.Fprintf(os.Stderr, "  %s created %s\n", doneStyle.Render("+"), path)
	} else {
		fmt.Fprintf(os.Stderr, "  %s %s already exists, left unchanged\n", dimStyle.Render("·"), path)
	}
}
