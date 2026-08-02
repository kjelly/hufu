package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/team"
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

var completionHelperCmd = &cobra.Command{
	Use:    "completion-helper [teams|agents]",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		registry := team.NewTeamRegistry(resolveSearchPaths())
		if err := registry.Discover(); err != nil {
			return err
		}

		switch args[0] {
		case "teams":
			var teams []string
			for _, name := range registry.ListTeams() {
				teams = append(teams, "@"+name)
			}
			for _, t := range teams {
				fmt.Println(t)
			}
		case "agents":
			var agents []string
			for _, dir := range registry.TeamDirs() {
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
						continue
					}
					agents = append(agents, "@"+strings.TrimSuffix(entry.Name(), ".md"))
				}
			}
			unique := make(map[string]bool)
			for _, a := range agents {
				if !unique[a] {
					unique[a] = true
					fmt.Println(a)
				}
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
`
	_, err := fmt.Fprint(w, script)
	return err
}
