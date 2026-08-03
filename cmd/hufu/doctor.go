package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/team"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that hufu is ready to call agents",
	Long: `Run preflight checks before invoking an agent team:

  - the LLM provider is reachable and which models it exposes
  - the resolved default / sidecar / guard models
  - the workspace directory is writable
  - how many agent teams are discoverable
  - static task and acceptance verifier contracts are asserting and resolvable

Most "the agent did nothing" failures are a provider/model misconfiguration.
Run this first to find them in seconds instead of waiting for a timeout.`,
	RunE: runDoctor,
}

// modelsResponse matches the OpenAI-compatible GET /models payload that both
// OpenAI and Ollama (under /v1) return.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func runDoctor(cmd *cobra.Command, args []string) error {
	ok := true
	pass := doneStyle.Render("✓")
	warn := errStyle.Render("⚠")
	fail := errStyle.Render("✗")

	fmt.Fprintf(os.Stderr, "%s\n\n", boldStyle.Render("─── hufu doctor ───"))

	// 1. Provider connectivity + model list.
	cfg := config.LoadConfig()
	providerURLResolved := config.ResolveProviderURL(opts.providerURL, "", "")
	apiKey := config.ResolveProviderAPIKey(opts.providerAPIKey, "")

	fmt.Fprintf(os.Stderr, "%s %s\n", boldStyle.Render("Provider:"), providerURLResolved)
	models, err := fetchModels(providerURLResolved, apiKey)
	if err != nil {
		ok = false
		fmt.Fprintf(os.Stderr, "  %s unreachable: %v\n", fail, err)
		fmt.Fprintf(os.Stderr, "    %s\n", dimStyle.Render("Is the server running? For Ollama: `ollama serve`. Check --provider-url / hufu.yaml."))
	} else if len(models) == 0 {
		fmt.Fprintf(os.Stderr, "  %s reachable, but it reports no models\n", warn)
		fmt.Fprintf(os.Stderr, "    %s\n", dimStyle.Render("For Ollama, pull one: `ollama pull qwen3:8b`."))
	} else {
		fmt.Fprintf(os.Stderr, "  %s reachable — %d model(s) available:\n", pass, len(models))
		sort.Strings(models)
		for _, m := range models {
			fmt.Fprintf(os.Stderr, "      %s\n", m)
		}
	}

	// 2. Resolved roles. CLI flag > hufu.yaml. (Team/agent overrides apply later
	// at run time and can't be resolved without a target team.)
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("Resolved models (hufu.yaml + flags; team/agent may override):"))
	defModel := firstNonEmpty(opts.modelOverride, cfg.Model)
	sidecar := firstNonEmpty(opts.sidecarModelOverride, cfg.SidecarModel, defModel)
	guard := firstNonEmpty(opts.guardModelOverride, cfg.GuardModel, sidecar)
	printRole(os.Stderr, "default", defModel, models, &ok)
	printRole(os.Stderr, "sidecar", sidecar, models, nil)
	printRole(os.Stderr, "guard", guard, models, nil)

	// 3. Workspace writability.
	ws := getWorkspace()
	fmt.Fprintf(os.Stderr, "\n%s %s\n", boldStyle.Render("Workspace:"), ws)
	if err := checkWritable(ws); err != nil {
		ok = false
		fmt.Fprintf(os.Stderr, "  %s not writable: %v\n", fail, err)
	} else {
		fmt.Fprintf(os.Stderr, "  %s writable\n", pass)
	}

	// 4. Team discovery.
	searchPaths := resolveSearchPaths()
	registry := team.NewTeamRegistry(searchPaths)
	fmt.Fprintf(os.Stderr, "\n%s %s\n", boldStyle.Render("Teams:"), strings.Join(searchPaths, ", "))
	if err := registry.Discover(); err != nil {
		fmt.Fprintf(os.Stderr, "  %s discovery failed: %v\n", warn, err)
	} else if registry.TeamCount() == 0 {
		fmt.Fprintf(os.Stderr, "  %s none found — use --default, or scaffold one with `hufu init <team>`\n", warn)
	} else {
		fmt.Fprintf(os.Stderr, "  %s %d team(s): %s\n", pass, registry.TeamCount(), strings.Join(registry.ListTeams(), ", "))
	}

	// 5. Verifier contract linting.
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("Contract & Verifier Linting:"))
	contractWarnings := 0
	contractErrors := 0

	if registry != nil && registry.TeamCount() > 0 {
		for _, teamName := range registry.ListTeams() {
			teamDir, err := registry.Resolve(teamName)
			if err != nil {
				continue
			}
			session, err := team.LoadTeam(teamDir, nil, nil)
			if err != nil {
				contractErrors++
				ok = false
				fmt.Fprintf(os.Stderr, "  %s team %s: contract load failed: %v\n", fail, teamName, err)
				continue
			}
			projectDir, err := os.Getwd()
			if err != nil {
				contractErrors++
				ok = false
				fmt.Fprintf(os.Stderr, "  %s team %s: resolve runtime project directory: %v\n", fail, teamName, err)
				continue
			}
			for _, finding := range collectDoctorContractFindings(session, projectDir) {
				f := finding.Finding
				location := finding.Location
				if location == "" {
					location = "contract"
				}
				if f.Severity == team.FindingSeverityError {
					contractErrors++
					ok = false
					fmt.Fprintf(os.Stderr, "  %s team %s %s: %s (%s)\n", fail, teamName, location, f.Message, f.Code)
				} else {
					contractWarnings++
					fmt.Fprintf(os.Stderr, "  %s team %s %s: %s (%s)\n", warn, teamName, location, f.Message, f.Code)
				}
			}
		}
	}

	if contractErrors == 0 && contractWarnings == 0 {
		fmt.Fprintf(os.Stderr, "  %s all team verifier contracts valid and asserting\n", pass)
	}

	fmt.Fprintln(os.Stderr)
	if ok {
		fmt.Fprintf(os.Stderr, "%s Ready to call agents.\n", pass)
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s Some checks failed — fix the items above before running a task.\n", fail)
	return fmt.Errorf("doctor: preflight checks failed")
}

type doctorContractFinding struct {
	Location string
	Finding  team.ContractFinding
}

// collectDoctorContractFindings combines the pure WP-01 verifier lint and
// WP-04 executable resolver for a loaded team without executing contracts.
// projectDir is the same process CWD that NewCoordinator uses at runtime.
func collectDoctorContractFindings(session *team.TeamSession, projectDir string) []doctorContractFinding {
	findings := append(team.LintTeamContracts(session), team.ResolveTeamContractExecutables(session, projectDir)...)
	out := make([]doctorContractFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, doctorContractFinding{Location: finding.Field, Finding: finding})
	}
	return out
}

func fetchModels(providerURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(providerURL, "/") + "/models"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	var mr modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("could not parse model list: %w", err)
	}
	out := make([]string, 0, len(mr.Data))
	for _, d := range mr.Data {
		out = append(out, d.ID)
	}
	return out, nil
}

// printRole prints a resolved model role and, when the model list is known,
// warns if the model is not among the available ones. failPtr (when non-nil)
// is set to false to mark the overall run as failed.
func printRole(w *os.File, role, model string, available []string, failPtr *bool) {
	if model == "" {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", role+":", dimStyle.Render("(not set — must be supplied via --model, team.yaml, or agent .md)"))
		return
	}
	bare := strings.TrimPrefix(model, "ollama/")
	if len(available) > 0 && !modelAvailable(bare, available) {
		_, _ = fmt.Fprintf(w, "  %-9s %s  %s\n", role+":", model, errStyle.Render("⚠ not in provider's model list"))
		if failPtr != nil {
			*failPtr = false
		}
		return
	}
	_, _ = fmt.Fprintf(w, "  %-9s %s\n", role+":", model)
}

func modelAvailable(bare string, available []string) bool {
	for _, a := range available {
		if a == bare || strings.TrimPrefix(a, "ollama/") == bare {
			return true
		}
	}
	return false
}

func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".hufu-doctor-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return err
	}
	return os.Remove(probe)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveSearchPaths returns the team search paths from the --agent-team-search-path
// flag or the built-in defaults.
func resolveSearchPaths() []string {
	if opts.agentTeamSearchPath != "" {
		return strings.Split(opts.agentTeamSearchPath, ",")
	}
	return team.DefaultSearchPaths()
}
