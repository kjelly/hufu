package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

var completionCmd = &cobra.Command{
	Use:   "completion",
	Short: "Generate completion script for the specified shell",
	Long:  "Generate completion script for hufu.",
}

var completionBashCmd = &cobra.Command{
	Use:   "bash",
	Short: "Generate completion script for bash",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Root().GenBashCompletion(os.Stdout)
	},
}

var completionZshCmd = &cobra.Command{
	Use:   "zsh",
	Short: "Generate completion script for zsh",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Root().GenZshCompletion(os.Stdout)
	},
}

var completionFishCmd = &cobra.Command{
	Use:   "fish",
	Short: "Generate completion script for fish",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Root().GenFishCompletion(os.Stdout, true)
	},
}

var completionPowerShellCmd = &cobra.Command{
	Use:   "powershell",
	Short: "Generate completion script for powershell",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Root().GenPowerShellCompletion(os.Stdout)
	},
}

var completionNushellCmd = &cobra.Command{
	Use:   "nushell",
	Short: "Generate completion script for nushell",
	RunE: func(cmd *cobra.Command, args []string) error {
		return generateNushellCompletion(os.Stdout)
	},
}

var (
	completionHelperRun     string
	completionHelperBranch  string
	completionHelperProject string
	completionHelperTeam    string
)

func init() {
	completionHelperCmd.Flags().StringVar(&completionHelperRun, "run", "", "Run ID to scope task completion")
	completionHelperCmd.Flags().StringVar(&completionHelperBranch, "branch", "", "Branch ID to scope run/task completion")
	completionHelperCmd.Flags().StringVar(&completionHelperProject, "project", "", "Project ID to scope proposal completion")
	completionHelperCmd.Flags().StringVar(&completionHelperTeam, "team", "", "Team ID to scope proposal completion")
}

// completionHelperCmd is invoked as an external command by shells (Nushell)
// whose completion protocol cannot call back into Cobra's own
// ValidArgsFunction/RegisterFlagCompletionFunc machinery. It reuses the exact
// same bounded, read-only complete*IDs logic registered for Bash/Zsh/Fish/
// PowerShell in completion_dynamic.go, so results and scope rules stay in
// one place.
var completionHelperCmd = &cobra.Command{
	Use:    "completion-helper [teams|agents|runs|tasks|branches|proposals|projects]",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "teams", "agents":
			registry := team.NewTeamRegistry(resolveSearchPaths())
			if err := registry.Discover(); err != nil {
				return err
			}
			switch args[0] {
			case "teams":
				for _, name := range registry.ListTeams() {
					fmt.Println("@" + name)
				}
			case "agents":
				unique := make(map[string]bool)
				for _, dir := range registry.TeamDirs() {
					entries, err := os.ReadDir(dir)
					if err != nil {
						continue
					}
					for _, entry := range entries {
						if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
							continue
						}
						agent := "@" + strings.TrimSuffix(entry.Name(), ".md")
						if !unique[agent] {
							unique[agent] = true
							fmt.Println(agent)
						}
					}
				}
			}
		case "runs":
			for _, id := range completionHelperRunIDs(getWorkspace(), completionHelperBranch) {
				fmt.Println(id)
			}
		case "tasks":
			for _, id := range completionHelperTaskIDs(getWorkspace(), completionHelperRun, completionHelperBranch) {
				fmt.Println(id)
			}
		case "branches":
			for _, id := range completionHelperBranchIDs(getWorkspace()) {
				fmt.Println(id)
			}
		case "proposals":
			for _, id := range completionHelperProposalIDs(getWorkspace(), completionHelperProject, completionHelperTeam) {
				fmt.Println(id)
			}
		case "projects":
			stateRoot, err := workspacepkg.DefaultStateRoot()
			if err != nil {
				return err
			}
			registry, err := workspacepkg.OpenReadOnly(stateRoot)
			if errors.Is(err, workspacepkg.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			defer func() { _ = registry.Close() }()
			projects, err := registry.ListProjects(cmd.Context(), workspacepkg.ListOptions{})
			if err != nil {
				return err
			}
			unique := make(map[string]struct{})
			for _, project := range projects {
				for _, value := range []string{project.ID, project.Alias, project.Slug} {
					if value != "" {
						unique[value] = struct{}{}
					}
				}
			}
			values := make([]string, 0, len(unique))
			for value := range unique {
				values = append(values, value)
			}
			slices.Sort(values)
			for _, value := range values {
				fmt.Println(value)
			}
		}
		return nil
	},
}

func generateNushellCompletion(w io.Writer) error {
	script := `# Custom completion helper for hufu
def "nu-complete hufu teams" [] {
  ^hufu completion-helper teams | lines
}

def "nu-complete hufu atnames" [] {
  [
    (^hufu completion-helper teams | lines)
    (^hufu completion-helper agents | lines)
  ] | flatten
}

def "nu-complete hufu branches" [] {
  ^hufu completion-helper branches | lines
}

def "nu-complete hufu runs" [context: string] {
  let matches = ($context | parse --regex '--branch[ =]+(?<v>[^ ]+)')
  let branch = if ($matches | is-empty) { "" } else { $matches.0.v }
  mut helper_args = [runs]
  if $branch != "" { $helper_args = ($helper_args | append [--branch $branch]) }
  ^hufu completion-helper ...$helper_args | lines
}

def "nu-complete hufu tasks" [context: string] {
  let run_matches = ($context | parse --regex '--run[ =]+(?<v>[^ ]+)')
  let run = if ($run_matches | is-empty) { "" } else { $run_matches.0.v }
  let branch_matches = ($context | parse --regex '--branch[ =]+(?<v>[^ ]+)')
  let branch = if ($branch_matches | is-empty) { "" } else { $branch_matches.0.v }
  mut helper_args = [tasks]
  if $run != "" { $helper_args = ($helper_args | append [--run $run]) }
  if $branch != "" { $helper_args = ($helper_args | append [--branch $branch]) }
  ^hufu completion-helper ...$helper_args | lines
}

def "nu-complete hufu proposals" [context: string] {
  let project_matches = ($context | parse --regex '--project[ =]+(?<v>[^ ]+)')
  let project = if ($project_matches | is-empty) { "" } else { $project_matches.0.v }
  let team_matches = ($context | parse --regex '--team[ =]+(?<v>[^ ]+)')
  let team = if ($team_matches | is-empty) { "" } else { $team_matches.0.v }
  if $project == "" or $team == "" { return [] }
  ^hufu completion-helper proposals --project $project --team $team | lines
}

def "nu-complete hufu workspace projects" [] {
  ^hufu completion-helper projects | lines
}

# Run an agent team to accomplish a task
export extern "hufu" [
  prompt?: string@'nu-complete hufu atnames' # Prompt task
  --provider-url: string # Ollama API base URL
  --provider-api-key: string # Provider API key
  --verbose(-v) # Show full agent text output in real-time
  --workspace(-w): string # Workspace directory
  --new(-n) # Archive old session and start fresh
  --temp(-t) # Use a temporary directory for workspace
  --agent-team: string@'nu-complete hufu teams' # Agent team name to load
  --agent-team-search-path: string # Comma-separated paths to search for teams
  --memory # Enable long-term memory
  --memory-model: string # Embedding model for memory
  --archive-memory # Archive session summary to memory and exit
  --show-history # Show previous session history on resume
  --steps(-s) # Pause for user confirmation before executing each batch of worker tasks
  --dry-run # Preview skill matching and task delegation without executing agents
  --tui # Show a Bubble Tea TUI for real-time task tracking
  --tui-compact # Force the compact three-column TUI layout
  --display-mode: string # Status display mode: auto, terminal, or plain
  --theme: string # Display theme: auto, light, dark, or mono
  --display-preset: string # Display preset: default or epaper
  --no-color # Disable ANSI color output
  --no-spinner # Disable the TUI waiting spinner
  --no-summary # Suppress the operator execution summary
  --rbash # Use restricted bash (rbash) for the bash tool
  --no-net # Block all network access for agent subprocesses
  --force-mcp # Force MCP mode
  --direnv # Load .envrc/.env environment for bash tool
  --think # Show coordinator decision reasoning
  --var: string # Set template variable (key=value)
  --var-file: string # Read template variables from a file
  --skill: string # Force-load specific skills
  --plan # Force plan-first mode
  --auto-skills # Enable automatic skill detection
  --auto-team # Enable automatic team routing via sidecar
  --template: string # Load a prompt template
  --fix: string # Analyze previous execution data and suggest improvements
  --report # Generate a full execution report as a markdown file
  --default # Use the built-in default team
  --helper-tools: string # Comma-separated extra tools to enable for the Helper
  --allow-path: string # Additional filesystem paths to allow for the active team
  --auto-approve # Automatically choose clearly safe ask_user options
  --model: string # Override default model
  --temperature: string # Override sampling temperature
  --max-tokens: string # Override max output tokens
  --top-p: string # Override top-p
  --top-k: string # Override top-k
  --sidecar-model: string # Override sidecar model
  --guard-model: string # Override guard model
  --timeout: int # Override agent/coordinator timeout in seconds
  --unattended # Run with no human present
  --max-duration: int # Budget: max total wall-clock seconds
  --max-total-tokens: int # Budget: max cumulative LLM tokens
  --auto-team # Auto-select the team best suited to the prompt
  --profile: string # Apply a named flag bundle from hufu.yaml profiles:
  --quiet(-q) # Suppress status output; print only the final result
  --output: string # Output format for the result: text or json
  -h, --help # help for hufu
  -v, --version # version for hufu
]

export extern "hufu context promotion analyze" [
  --workspace(-w): string
  --project: string
  --team: string
  --team-search-path: string
  --policy-version: string
  --json
  --type: string
  --agent: string
  --model: string
  --dry-run
]

export extern "hufu context promotion list" [
  --workspace(-w): string
  --project: string
  --team: string
  --team-search-path: string
  --policy-version: string
  --json
]

export extern "hufu context promotion show" [
  proposal_id: string@'nu-complete hufu proposals'
  --workspace(-w): string
  --project: string
  --team: string
  --team-search-path: string
  --policy-version: string
  --json
  --show-content
]

export extern "hufu context promotion review" [
  proposal_id?: string@'nu-complete hufu proposals'
  --workspace(-w): string
  --project: string
  --team: string
  --team-search-path: string
  --policy-version: string
  --unattended
]

export extern "hufu context learning" [
  --workspace(-w): string
  --project: string
  --team: string
  --team-search-path: string
  --json
]

export extern "hufu context promotion edit" [
  proposal_id: string@'nu-complete hufu proposals'
  --workspace(-w): string
  --project: string
  --team: string
  --team-search-path: string
  --policy-version: string
  --json
  --draft-file: string
]

export extern "hufu context promotion approve" [proposal_id: string@'nu-complete hufu proposals' --workspace(-w): string --project: string --team: string --team-search-path: string --policy-version: string --json]
export extern "hufu context promotion reject" [proposal_id: string@'nu-complete hufu proposals' --workspace(-w): string --project: string --team: string --team-search-path: string --policy-version: string --json --reason: string]
export extern "hufu context promotion apply" [proposal_id: string@'nu-complete hufu proposals' --workspace(-w): string --project: string --team: string --team-search-path: string --policy-version: string --json]

export extern "hufu team create" [
  name: string
  --preset: string
  --from: string
  --model: string
  --force
  --expanded
  --wizard
]

export extern "hufu session status" [--workspace(-w): string --team: string --run: string@'nu-complete hufu runs' --branch: string@'nu-complete hufu branches' --output: string]
export extern "hufu session resume" [--workspace(-w): string --team: string --run: string@'nu-complete hufu runs' --branch: string@'nu-complete hufu branches']
export extern "hufu session retry" [--workspace(-w): string --team: string --run: string@'nu-complete hufu runs' --branch: string@'nu-complete hufu branches' --task: string@'nu-complete hufu tasks' --attempt: int --output: string]
export extern "hufu session reconcile" [--workspace(-w): string --team: string --run: string@'nu-complete hufu runs' --branch: string@'nu-complete hufu branches' --task: string@'nu-complete hufu tasks' --attempt: int --output: string]
export extern "hufu examples" [--format: string]

export extern "hufu workspace register" [path?: path --output: string]
export extern "hufu workspace list" [--all --output: string]
export extern "hufu workspace show" [selector?: string@'nu-complete hufu workspace projects' --output: string]
export extern "hufu workspace path" [selector?: string@'nu-complete hufu workspace projects' --team: string]
export extern "hufu workspace subject-path" [selector?: string@'nu-complete hufu workspace projects']
export extern "hufu workspace alias set" [selector: string@'nu-complete hufu workspace projects' alias: string --output: string]
export extern "hufu workspace alias clear" [selector: string@'nu-complete hufu workspace projects' --output: string]
export extern "hufu workspace rebind" [selector: string@'nu-complete hufu workspace projects' new_root: path --output: string]
export extern "hufu workspace migrate" [selector?: string@'nu-complete hufu workspace projects' --team: string --all-teams --legacy-root: path --output: string]
export extern "hufu workspace delete" [selector?: string@'nu-complete hufu workspace projects' --team: string --all-teams --yes --output: string]
export extern "hufu workspace restore" [trash_id: string --yes --output: string]
export extern "hufu workspace purge" [trash_id: string --yes --output: string]
export extern "hufu workspace gc" [--dry-run --apply --yes --trash-older-than: duration --output: string]
export extern "hufu workspace shell-init" [shell: string]
export extern "hufu workspace doctor" [selector?: string@'nu-complete hufu workspace projects' --repair --output: string]

def "nu-complete hufu inspect formats" [] {
  [text json]
}

export extern "hufu inspect" [
  --workspace(-w): string # Workspace directory
  --branch: string@'nu-complete hufu branches' # Exact branch ID, name, or label
  --session: string # Exact session ID filter
  --format: string@'nu-complete hufu inspect formats' # text or json
  --output: string@'nu-complete hufu inspect formats' # text or json
]

export extern "hufu inspect overview" [--run: string@'nu-complete hufu runs' --workspace(-w): string --branch: string@'nu-complete hufu branches' --session: string --format: string@'nu-complete hufu inspect formats' --output: string@'nu-complete hufu inspect formats']
export extern "hufu inspect run" [run_id: string@'nu-complete hufu runs' --workspace(-w): string --branch: string@'nu-complete hufu branches' --session: string --format: string@'nu-complete hufu inspect formats' --output: string@'nu-complete hufu inspect formats']
export extern "hufu inspect task" [task_id: string@'nu-complete hufu tasks' --run: string@'nu-complete hufu runs' --attempt: int --workspace(-w): string --branch: string@'nu-complete hufu branches' --session: string --format: string@'nu-complete hufu inspect formats' --output: string@'nu-complete hufu inspect formats']
export extern "hufu inspect evidence" [run_id: string@'nu-complete hufu runs' --workspace(-w): string --branch: string@'nu-complete hufu branches' --session: string --format: string@'nu-complete hufu inspect formats' --output: string@'nu-complete hufu inspect formats']
export extern "hufu inspect context" [task_id: string@'nu-complete hufu tasks' --run: string@'nu-complete hufu runs' --attempt: int --project: string --team: string --agent: string --all-agents --show-content --workspace(-w): string --branch: string@'nu-complete hufu branches' --session: string --format: string@'nu-complete hufu inspect formats' --output: string@'nu-complete hufu inspect formats']
export extern "hufu inspect trace" [run_id: string@'nu-complete hufu runs' --workspace(-w): string --branch: string@'nu-complete hufu branches' --session: string --format: string@'nu-complete hufu inspect formats' --output: string@'nu-complete hufu inspect formats']
export extern "hufu inspect replay" [run_id: string@'nu-complete hufu runs' --workspace(-w): string --branch: string@'nu-complete hufu branches' --session: string --format: string@'nu-complete hufu inspect formats' --output: string@'nu-complete hufu inspect formats']
export extern "hufu inspect storage" [--workspace(-w): string --format: string@'nu-complete hufu inspect formats' --output: string@'nu-complete hufu inspect formats']
`
	_, err := fmt.Fprint(w, script)
	return err
}
