package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	internalteam "github.com/kjelly/hufu/internal/team"
)

// teamCmd contains commands that create and maintain on-disk team definitions.
var teamCmd = &cobra.Command{
	Use:   "team",
	Short: "Create and manage agent-team definitions",
}

var (
	teamGeneratePrompt    string
	teamGenerateWrite     bool
	teamGenerateDryRun    bool
	teamGenerateOutputDir string
	teamGenerateModel     string
	teamValidateName      string
)

var teamValidateCmd = &cobra.Command{
	Use:   "validate [team-directory]",
	Short: "Validate team contracts before dispatch",
	Long: `Load and validate a team without calling a model or creating a workspace.

Use a directory argument, or --team to resolve a discoverable team name. This
checks delegation references, tool-policy conflicts, machine-readable
requirements, and bound task execution/output contracts before they can consume
an execution retry. Environment-dependent requirements are checked by doctor and
again after runtime CLI/profile policy is resolved.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTeamValidate,
}

var teamGenerateCmd = &cobra.Command{
	Use:   "generate <team-name>",
	Short: "Generate a task-specific agent team",
	Long: `Generate a candidate team from a task description.

By default this previews the generated files and validates that hufu can load
them. Pass --write to save the validated team. The initial generator uses
deterministic task categories, so generation does not make an LLM call.`,
	Example: `  hufu team generate oauth-bugfix --from-prompt "Fix the OAuth callback bug and add regression tests"
  hufu team generate docs-research --from-prompt "Compare API authentication options and write documentation" --write`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamGenerate,
}

func init() {
	teamCmd.AddCommand(teamGenerateCmd)
	teamCmd.AddCommand(teamValidateCmd)
	teamGenerateCmd.Flags().StringVar(&teamGeneratePrompt, "from-prompt", "", "Task description used to design the team (required)")
	teamGenerateCmd.Flags().BoolVar(&teamGenerateWrite, "write", false, "Write the validated team under --output-dir")
	teamGenerateCmd.Flags().BoolVar(&teamGenerateDryRun, "dry-run", false, "Validate without writing files (the default preview also validates)")
	teamGenerateCmd.Flags().StringVar(&teamGenerateOutputDir, "output-dir", ".agent-teams", "Directory in which to create the team")
	teamGenerateCmd.Flags().StringVar(&teamGenerateModel, "model", "", "Optional model to set in the generated team.yaml")
	_ = teamGenerateCmd.MarkFlagRequired("from-prompt")
	teamValidateCmd.Flags().StringVar(&teamValidateName, "team", "", "Discoverable team name to validate")
}

func runTeamValidate(_ *cobra.Command, args []string) error {
	if len(args) == 1 && strings.TrimSpace(teamValidateName) != "" {
		return fmt.Errorf("provide either a team directory or --team, not both")
	}
	teamDir := ""
	if len(args) == 1 {
		teamDir = args[0]
	} else if strings.TrimSpace(teamValidateName) != "" {
		registry := internalteam.NewTeamRegistry(resolveSearchPaths())
		if err := registry.Discover(); err != nil {
			return fmt.Errorf("discover teams: %w", err)
		}
		var err error
		teamDir, err = registry.Resolve(teamValidateName)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("team directory or --team is required")
	}
	session, err := internalteam.LoadTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry)
	if err != nil {
		return err
	}
	findings := internalteam.LintTeamContracts(session)
	for _, finding := range findings {
		if finding.Severity == internalteam.FindingSeverityError {
			return fmt.Errorf("%s: %s (%s)", finding.Field, finding.Message, finding.Code)
		}
		if finding.Severity == internalteam.FindingSeverityWarning {
			_, _ = fmt.Fprintf(os.Stderr, "warning: %s: %s (%s)\n", finding.Field, finding.Message, finding.Code)
		}
	}
	_, err = fmt.Fprintf(os.Stdout, "team %s: contracts valid\n", session.Config.Name)
	return err
}

func runTeamGenerate(_ *cobra.Command, args []string) error {
	name, err := normalizeGeneratedTeamName(args[0])
	if err != nil {
		return err
	}
	if teamGenerateWrite && teamGenerateDryRun {
		return fmt.Errorf("--write and --dry-run cannot be used together")
	}

	generated := buildGeneratedTeam(name, teamGeneratePrompt, teamGenerateModel, "workspace")
	if err := validateGeneratedTeam(generated); err != nil {
		return fmt.Errorf("generated team is invalid: %w", err)
	}

	if !teamGenerateWrite {
		_, _ = fmt.Fprint(os.Stdout, generated.preview())
		fmt.Fprintln(os.Stderr, "\n✓ Validation passed. This is a preview; add --write to create the team.")
		return nil
	}

	targetDir := filepath.Join(teamGenerateOutputDir, name)
	if err := writeGeneratedTeam(targetDir, generated); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ Created task-specific team at %s\n", targetDir)
	fmt.Fprintf(os.Stderr, "  Try it: hufu @%s %q\n", name, strings.TrimSpace(teamGeneratePrompt))
	return nil
}

type generatedTeam struct {
	Name     string
	Category string
	Prompt   string
	Model    string
	Files    map[string]string
}

func (g generatedTeam) preview() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Candidate team: %s (%s)\n\n", g.Name, g.Category)
	for _, name := range sortedGeneratedFileNames(g.Files) {
		fmt.Fprintf(&b, "## %s\n\n%s\n", name, g.Files[name])
	}
	return b.String()
}

func sortedGeneratedFileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func normalizeGeneratedTeamName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", fmt.Errorf("team name is required")
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", fmt.Errorf("invalid team name %q: use lowercase letters, numbers, and hyphens", name)
		}
	}
	return name, nil
}

func buildGeneratedTeam(name, prompt, model, workspaceDir string) generatedTeam {
	category := classifyGeneratedTeamTask(prompt)
	description := fmt.Sprintf("Task-specific %s team generated for: %s", category, strings.TrimSpace(prompt))
	modelLine := ""
	if strings.TrimSpace(model) != "" {
		modelLine = fmt.Sprintf("model: %q\n", strings.TrimSpace(model))
	}
	files := map[string]string{
		"team.yaml": fmt.Sprintf(`name: %q
description: %q
acceptance: 'true'
max-rounds: 10
max-steps: 30
timeout: 600
max-retries: 2
max-concurrent: 4
workspace: %q
%sauto-skills: false
`, name, description, workspaceDir, modelLine),
		"coordinator.md": generatedAgentMarkdown("coordinator", "Task coordinator", "coordinator", "ask_user", `You coordinate this task-specific team.

Analyze the request, delegate independent work to the appropriate workers, and synthesize a concise final answer. Do not implement work yourself when a worker can perform it.`),
	}

	switch category {
	case "bugfix":
		files["reproducer.md"] = generatedAgentMarkdown("reproducer", "Reproduces failures and isolates root causes", "worker", "view,grep,glob,ls,bash", "Reproduce the reported issue with the smallest reliable case. Identify the root cause, affected files, and a concrete regression test plan. Do not modify production code.")
		files["fixer.md"] = generatedAgentMarkdown("fixer", "Implements minimal corrective changes", "worker", "view,write,edit,multiedit,grep,glob,ls,bash", "Implement the smallest safe fix after inspecting the existing code and tests. Run relevant tests and report the exact changes and results.")
		files["reviewer.md"] = generatedAgentMarkdown("reviewer", "Reviews correctness and regression coverage", "worker", "view,grep,glob,ls,bash", "Review the proposed fix for correctness, compatibility, and regression coverage. Report findings by severity and run focused verification where possible.")
	case "research":
		files["researcher.md"] = generatedAgentMarkdown("researcher", "Investigates sources and collects evidence", "worker", "view,grep,glob,ls,fetch,agentic_fetch", "Research the question systematically. Separate facts from assumptions, retain source links, and summarize trade-offs.")
		files["writer.md"] = generatedAgentMarkdown("writer", "Produces clear decision-ready documentation", "worker", "view,write,edit,grep,glob,ls", "Turn the gathered evidence into concise, audience-appropriate documentation. Preserve uncertainty and do not invent facts.")
		files["fact-checker.md"] = generatedAgentMarkdown("fact-checker", "Checks evidence, claims, and omissions", "worker", "view,grep,glob,ls,fetch,agentic_fetch", "Verify material claims against available evidence. Flag unsupported statements, missing alternatives, and stale assumptions.")
	case "release":
		files["release-engineer.md"] = generatedAgentMarkdown("release-engineer", "Prepares release and deployment changes", "worker", "view,write,edit,multiedit,grep,glob,ls,bash", "Inspect release configuration, implement necessary release changes, and run relevant checks. Prefer reversible, well-documented actions.")
		files["risk-reviewer.md"] = generatedAgentMarkdown("risk-reviewer", "Assesses operational and rollback risk", "worker", "view,grep,glob,ls,bash", "Review the release plan for operational risks, observability gaps, rollout sequencing, and rollback readiness.")
	default:
		files["architect.md"] = generatedAgentMarkdown("architect", "Designs implementation-ready changes", "worker", "view,grep,glob,ls", "Inspect the codebase and propose a minimal implementation plan, including affected interfaces, files, and verification steps. Do not modify code.")
		files["developer.md"] = generatedAgentMarkdown("developer", "Implements production changes", "worker", "view,write,edit,multiedit,grep,glob,ls,bash", "Implement the approved change with minimal, idiomatic edits. Run focused tests and clearly report what changed.")
		files["test-reviewer.md"] = generatedAgentMarkdown("test-reviewer", "Reviews tests and implementation quality", "worker", "view,grep,glob,ls,bash", "Review the implementation for correctness, maintainability, and test coverage. Run relevant tests and report concrete findings.")
	}
	return generatedTeam{Name: name, Category: category, Prompt: prompt, Model: model, Files: files}
}

func generatedAgentMarkdown(name, description, role, tools, system string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\nrole: %s\ntools: %s\n---\n%s\n", name, description, role, tools, system)
}

func classifyGeneratedTeamTask(prompt string) string {
	p := strings.ToLower(prompt)
	if containsGeneratedTaskKeyword(p, "bug", "fix", "regression", "error", "failure", "issue", "修正", "修復", "錯誤", "問題") {
		return "bugfix"
	}
	if containsGeneratedTaskKeyword(p, "research", "investigate", "compare", "documentation", "document", "docs", "研究", "調查", "比較", "文件") {
		return "research"
	}
	if containsGeneratedTaskKeyword(p, "release", "deploy", "deployment", "incident", "operations", "infrastructure", "發佈", "發布", "部署", "維運", "事故") {
		return "release"
	}
	return "development"
}

func containsGeneratedTaskKeyword(prompt string, keywords ...string) bool {
	for _, keyword := range keywords {
		if strings.Contains(prompt, keyword) {
			return true
		}
	}
	return false
}

// validateGeneratedTeam loads an isolated copy through the normal team parser.
// This catches invalid YAML/frontmatter without creating a workspace in the
// caller's project directory.
func validateGeneratedTeam(g generatedTeam) error {
	if _, err := normalizeGeneratedTeamName(g.Name); err != nil {
		return err
	}
	if len(strings.TrimSpace(g.Files["team.yaml"])) == 0 || len(strings.TrimSpace(g.Files["coordinator.md"])) == 0 {
		return fmt.Errorf("team.yaml and coordinator.md are required")
	}
	root, err := os.MkdirTemp("", "hufu-team-generate-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(root) }()

	validation := g
	validation.Files = make(map[string]string, len(g.Files))
	for name, content := range g.Files {
		validation.Files[name] = content
	}
	var replaced bool
	validation.Files["team.yaml"], replaced = replaceGeneratedWorkspace(validation.Files["team.yaml"], filepath.Join(root, "workspace"))
	if !replaced {
		return fmt.Errorf("team.yaml must declare a workspace")
	}
	teamDir := filepath.Join(root, g.Name)
	if err := writeGeneratedTeam(teamDir, validation); err != nil {
		return err
	}
	if _, err := internalteam.LoadTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry); err != nil {
		return err
	}
	return nil
}

func replaceGeneratedWorkspace(teamYAML, workspace string) (string, bool) {
	lines := strings.Split(teamYAML, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "workspace:") {
			lines[i] = fmt.Sprintf("workspace: %q", workspace)
			return strings.Join(lines, "\n"), true
		}
	}
	return teamYAML, false
}

func writeGeneratedTeam(targetDir string, g generatedTeam) error {
	if _, err := os.Stat(targetDir); err == nil {
		return fmt.Errorf("team directory %s already exists; refusing to overwrite it", targetDir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect team directory %s: %w", targetDir, err)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("create team directory: %w", err)
	}
	for _, name := range sortedGeneratedFileNames(g.Files) {
		if err := os.WriteFile(filepath.Join(targetDir, name), []byte(g.Files[name]), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}
