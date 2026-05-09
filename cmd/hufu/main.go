package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	ergoreadline "github.com/ergochat/readline"
	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/hooks"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/memory"
	"github.com/anomalyco/hufu/internal/notify"
	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
	tuipkg "github.com/anomalyco/hufu/internal/tui"
)

var (
	providerURL         string
	providerAPIKey      string
	verbose             bool
	workspace           string
	newSession          bool
	tempWorkspace       bool
	agentTeamName       string
	agentTeamSearchPath string
	memoryEnabled       bool
	memoryModel         string
	archiveMemory       bool
	showHistory         bool
	stepsMode           bool
	dryRun              bool
	tuiMode             bool
	rbashMode           bool
	noNet               bool
	think               bool
	direnv              bool
	varFlags            []string
	varFiles            []string
	globalPromptReader  atomic.Pointer[readline.PromptReader]
)

type errInterrupted struct{}

func (errInterrupted) Error() string { return "interrupted" }

type errForced struct{}

func (errForced) Error() string { return "force quit" }

var version = "dev"

func main() {
	exitCode := 0
	rootCmd := &cobra.Command{
		Use:   "hufu [prompt]",
		Short: "Run an agent team to accomplish a task",
		Long:  "hufu discovers and runs agent teams by name. Use --agent-team or @team-name in the prompt to select a team.",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runTeam,
		Version: version,
	}

	rootCmd.Flags().StringVar(&providerURL, "provider-url", "", "Ollama API base URL (default: from hufu.yaml or http://localhost:11434/v1)")
	rootCmd.Flags().StringVar(&providerAPIKey, "provider-api-key", "", "Provider API key (default: from HUFU_PROVIDER_API_KEY env or team.yaml)")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show full agent text output in real-time")
	rootCmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	rootCmd.Flags().BoolVarP(&newSession, "new", "n", false, "Archive old session and start fresh")
	rootCmd.Flags().BoolVarP(&tempWorkspace, "temp", "t", false, "Use a temporary directory for workspace")
	rootCmd.Flags().StringVar(&agentTeamName, "agent-team", "", "Agent team name to load")
	rootCmd.Flags().StringVar(&agentTeamSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams (default: .agent-teams/,~/.agent-teams/)")
	rootCmd.Flags().BoolVar(&memoryEnabled, "memory", true, "Enable long-term memory (RAG with vector search)")
	rootCmd.Flags().StringVar(&memoryModel, "memory-model", "", "Embedding model for memory (default: qwen3-embedding:4b, overrides hufu.yaml)")
	rootCmd.Flags().BoolVar(&archiveMemory, "archive-memory", false, "Archive session summary to memory and exit")
	rootCmd.Flags().BoolVar(&showHistory, "show-history", false, "Show previous session history on resume")
	rootCmd.Flags().BoolVarP(&stepsMode, "steps", "s", false, "Pause for user confirmation before executing each batch of worker tasks")
	rootCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview skill matching and task delegation without executing agents")
	rootCmd.Flags().BoolVar(&tuiMode, "tui", false, "Show a Bubble Tea TUI for real-time task tracking")
	rootCmd.Flags().BoolVar(&rbashMode, "rbash", false, "Use restricted bash (rbash) for the bash tool")
	rootCmd.Flags().BoolVar(&noNet, "no-net", false, "Block all network access for agent subprocesses")
	rootCmd.Flags().BoolVar(&direnv, "direnv", false, "Load .envrc/.env environment for bash tool; parses .env (key=value) and/or uses direnv for full shell support")
	rootCmd.Flags().BoolVar(&think, "think", false, "Show coordinator decision reasoning (skills, agents, tasks, system prompt)")
	rootCmd.Flags().StringArrayVar(&varFlags, "var", nil, "Set template variable (key=value). Can be specified multiple times; later values override earlier ones")
	rootCmd.Flags().StringArrayVar(&varFiles, "var-file", nil, "Read template variables from a file (.yaml/.yml or KEY=VALUE format). Can be specified multiple times; later files override earlier ones")

	if err := rootCmd.Execute(); err != nil {
		var interrupted errInterrupted
		if errors.Is(err, interrupted) {
			exitCode = 130
		} else {
			exitCode = 1
		}
	}
	if pr := globalPromptReader.Load(); pr != nil {
		pr.Close()
	}
	os.Exit(exitCode)
}

type teamContext struct {
	session     *team.TeamSession
	coordinator *team.Coordinator
	sessionData *team.SessionData
	notifier    *notify.Notifier
}

func runTeam(cmd *cobra.Command, args []string) error {
	// Refuse --steps + --tui combination: step confirmation requires terminal
	// access that conflicts with the Bubble Tea altscreen.
	if stepsMode && tuiMode {
		return fmt.Errorf("cannot use --steps (step confirmation) with --tui (TUI mode); remove one flag")
	}

	pr, err := readline.NewPromptReader(defaultHistoryPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s Readline initialization failed, falling back to basic input: %v\n", errStyle.Render("⚠"), err)
		pr = nil
	}
	globalPromptReader.Store(pr)
	if tempWorkspace {
		tmpDir, err := os.MkdirTemp("", "hufu-*")
		if err != nil {
			return fmt.Errorf("failed to create temp directory: %w", err)
		}
		defer os.RemoveAll(tmpDir)
		workspace = filepath.Join(tmpDir, "workspace")
		fmt.Fprintf(os.Stderr, "%s Temp workspace: %s\n", stepStyle.Render("⟳"), workspace)
	}

	vars, err := team.ResolveVars(varFiles, varFlags)
	if err != nil {
		return fmt.Errorf("failed to resolve template variables: %w", err)
	}
	cfgVars := config.LoadConfig().GetVars()
	if len(cfgVars) > 0 || len(vars) > 0 {
		merged := team.MergeVars(cfgVars, vars)
		vars = merged
	}

	prompt := ""
	if len(args) > 0 {
		prompt = args[0]
	}
	if prompt == "" {
		prompt = readStdin()
	}

	if archiveMemory && prompt == "" {
		var searchPaths []string
		if agentTeamSearchPath != "" {
			searchPaths = strings.Split(agentTeamSearchPath, ",")
		} else {
			searchPaths = team.DefaultSearchPaths()
		}
		registry := team.NewTeamRegistry(searchPaths)
		if err := registry.Discover(); err != nil {
			return fmt.Errorf("failed to discover teams: %w", err)
		}
		return runArchiveMemory(context.Background(), registry, vars)
	}

	if prompt == "" {
		if pr != nil {
			p, err := askUserForPrompt(pr)
			if err != nil {
				return err
			}
			prompt = p
		} else {
			p, err := askUserForPromptFallback()
			if err != nil {
				return err
			}
			prompt = p
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	injector := newPromptInjector(pr)
	activeCoord := &activeCoordinator{}
	defer setupPromptSignals(injector)()

	sigIntCh := make(chan os.Signal, 1)
	signal.Notify(sigIntCh, os.Interrupt)
	sigIntDone := make(chan struct{})
	go func() {
		defer close(sigIntDone)
		first := true
		for range sigIntCh {
			if first {
				if p := activeTUIProgram.Load(); p != nil {
					// TUI path: send WrapUpMsg and let the TUI handle SetWrapUp/injectWrapUp
					// to avoid double-calling (the TUI's wrapUpCh goroutine calls them).
					p.Send(tuipkg.WrapUpMsg{})
				} else {
					// Non-TUI path: need to call SetWrapUp and injectWrapUp here.
					fmt.Fprintf(os.Stderr, "\n%s Wrapping up... (press Ctrl+C again to force quit)\n", boldStyle.Render("⏹"))
					if c := activeCoord.Get(); c != nil {
						c.SetWrapUp()
					}
					injector.injectWrapUp()
				}
				first = false
			} else {
				if activeTUIProgram.Load() == nil {
					fmt.Fprintf(os.Stderr, "\n%s Force quit\n", errStyle.Render("✗"))
				}
				cancel()
			}
		}
	}()

	defer func() {
		signal.Stop(sigIntCh)
		close(sigIntCh)
		<-sigIntDone
	}()

	var searchPaths []string
	if agentTeamSearchPath != "" {
		searchPaths = strings.Split(agentTeamSearchPath, ",")
	} else {
		searchPaths = team.DefaultSearchPaths()
	}

	registry := team.NewTeamRegistry(searchPaths)
	if err := registry.Discover(); err != nil {
		return fmt.Errorf("failed to discover teams: %w", err)
	}

	if registry.TeamCount() == 0 {
		return fmt.Errorf("no agent teams found in search paths: %s", strings.Join(searchPaths, ", "))
	}

	fmt.Fprintf(os.Stderr, "%s Available teams: %s\n", boldStyle.Render("Teams:"), strings.Join(registry.ListTeams(), ", "))

	if archiveMemory && prompt == "" && !newSession {
		return runArchiveMemory(context.Background(), registry, vars)
	}

	initialTeam := strings.ToLower(agentTeamName)

	initialSegments, err := team.ParsePromptWithLazyAgents(prompt, registry, initialTeam)
	if err != nil {
		teamNotFound := false
		if initialTeam == "" && !team.HasAtName(prompt) {
			teamNotFound = true
		} else {
			for _, prefix := range []string{"no team found", "no team specified"} {
				if strings.HasPrefix(err.Error(), prefix) {
					teamNotFound = true
					break
				}
			}
		}
		if teamNotFound {
			var chosen string
			if pr != nil {
				c, err := askUserForTeam(registry.ListTeams(), pr)
				if err != nil {
					return err
				}
				chosen = c
			} else {
				chosen = askUserForTeamFallback(registry.ListTeams())
			}
			if chosen == "" {
				fmt.Fprintf(os.Stderr, "%s No team selected.\n", errStyle.Render("✗"))
				return fmt.Errorf("no team selected")
			}
			initialTeam = chosen
			initialSegments, err = team.ParsePromptWithLazyAgents(prompt, registry, initialTeam)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	pathConsent := newPathConsent()

	loadedTeams := map[string]*teamContext{}
	for _, seg := range initialSegments {
		if seg.Type == team.SegmentSwitchTeam {
			if _, ok := loadedTeams[seg.Name]; !ok {
				tc, err := loadTeamByName(ctx, seg.Name, registry, providerURL, providerAPIKey, pathConsent, vars)
				if err != nil {
					return fmt.Errorf("failed to load team %q: %w", seg.Name, err)
				}
				if stepsMode {
					tc.coordinator.SetStepConfirmFn(makeStepConfirmFn())
				}
				if dryRun {
					tc.coordinator.SetDryRun(true)
				}
				loadedTeams[seg.Name] = tc
			}
		}
	}

	var segments []team.PromptSegment
	for _, seg := range initialSegments {
		if seg.Type == team.SegmentSwitchTeam {
			tc := loadedTeams[seg.Name]
			if tc != nil && seg.Content != "" {
				subSegs, err := team.SplitSegmentByAgents(seg, registry, agentNamesFromSession(tc.session))
				if err != nil {
					return err
				}
				segments = append(segments, subSegs...)
			} else {
				segments = append(segments, seg)
			}
		} else {
			segments = append(segments, seg)
		}
	}

	if dryRun {
		var dryRunTeamName string
		for _, seg := range segments {
			if seg.Type == team.SegmentSwitchTeam {
				dryRunTeamName = seg.Name
				break
			}
		}
		if dryRunTeamName == "" {
			dryRunTeamName = strings.ToLower(agentTeamName)
		}
		if dryRunTeamName == "" {
			return fmt.Errorf("--dry-run requires a team (use --agent-team or @team-name in the prompt)")
		}
		tc, ok := loadedTeams[dryRunTeamName]
		if !ok {
			return fmt.Errorf("failed to load team %q for dry-run", dryRunTeamName)
		}
		dryRunPrompt := ""
		for _, seg := range segments {
			if seg.Type == team.SegmentSwitchTeam && seg.Name == dryRunTeamName {
				dryRunPrompt = seg.Content
			} else if seg.Type == team.SegmentText && dryRunPrompt == "" && dryRunTeamName != "" {
				dryRunPrompt = seg.Content
			}
		}
		if dryRunPrompt == "" {
			dryRunPrompt = prompt
		}
		if dryRunPrompt == "" {
			return fmt.Errorf("--dry-run requires a prompt")
		}

		fmt.Fprintf(os.Stderr, "\n%s Running dry-run for team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(dryRunTeamName))

		dryDisp := newCoordDisplay(tc)
		result, err := tc.coordinator.DryRun(ctx, dryRunPrompt)
		dryDisp.stopTimer()

		if err != nil {
			return fmt.Errorf("dry-run failed: %w", err)
		}
		renderDryRun(result)
		return nil
	}

	var result string
	var runErr error
	if tuiMode {
		var teamInfo tuipkg.TeamInfo
		teamInfo.AvailableTeams = registry.ListTeams()
		for _, tc := range loadedTeams {
			if tc != nil && tc.session != nil {
				teamInfo.TeamName = tc.session.Config.Name
				for _, ag := range sortedAgents(tc.session.Agents) {
					teamInfo.Agents = append(teamInfo.Agents, tuipkg.AgentInfoEntry{
						Name: ag.Name,
						Role: ag.Role,
					})
				}
				for _, s := range tc.session.Skills {
					teamInfo.Skills = append(teamInfo.Skills, s.Name)
				}
				if sc := tc.session.Config.SidecarModel; sc != "" {
					teamInfo.SidecarModel = sc
				}
				if gm := tc.session.Config.GuardModel; gm != "" {
					teamInfo.GuardModel = gm
				}
				teamInfo.MemoryEnabled = memoryEnabled && !tempWorkspace
				if teamInfo.MemoryEnabled {
					teamInfo.MemoryModel = config.ResolveEmbeddingModel(memoryModel)
				}
				break
			}
		}
		result, runErr = runWithTUI(ctx, cancel, prompt, segments, registry, loadedTeams, injector, activeCoord, pathConsent, vars, teamInfo)
	} else {
		result, runErr = executeSegments(ctx, segments, registry, providerURL, loadedTeams, injector, activeCoord, pathConsent, vars)
	}
	if runErr != nil {
		return runErr
	}
	fmt.Println(result)

	var allSkillUsage []team.SkillUsageEntry
	seenSkill := map[string]int{}
	for teamName, tc := range loadedTeams {
		for _, entry := range tc.coordinator.SkillUsage() {
			key := strings.ToLower(entry.Name)
			if idx, ok := seenSkill[key]; ok {
				allSkillUsage[idx].Count += entry.Count
				for _, a := range entry.Agents {
					prefixed := teamName + "/" + a
					if !slices.Contains(allSkillUsage[idx].Agents, prefixed) {
						allSkillUsage[idx].Agents = append(allSkillUsage[idx].Agents, prefixed)
					}
				}
			} else {
				seenSkill[key] = len(allSkillUsage)
				prefixed := make([]string, len(entry.Agents))
				for i, a := range entry.Agents {
					prefixed[i] = teamName + "/" + a
				}
				allSkillUsage = append(allSkillUsage, team.SkillUsageEntry{
					Name:   entry.Name,
					Count:  entry.Count,
					Agents: prefixed,
				})
			}
		}
	}
	renderSkillSummary(allSkillUsage)

	if archiveMemory && !newSession {
		for _, tc := range loadedTeams {
			archiveCurrentSessionToMemory(ctx, tc)
		}
	}

	if tempWorkspace {
		absWS, _ := filepath.Abs(workspace)
		fmt.Fprintf(os.Stderr, "\n%s\n  Path: %s\n  Reuse: hufu -w %s [prompt]\n",
			boldStyle.Render("─── Temporary Workspace ───"),
			absWS, absWS)
	}

	return nil
}

func loadTeamByName(ctx context.Context, teamName string, registry *team.TeamRegistry, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, vars map[string]string) (*teamContext, error) {
	teamDir, err := registry.Resolve(teamName)
	if err != nil {
		return nil, err
	}

	session, err := team.LoadTeam(teamDir, vars)
	if err != nil {
		return nil, err
	}

	resolvedProviderURL := config.ResolveProviderURL(defaultProviderURL, session.Config.ProviderURL, "")
	resolvedProviderAPIKey := config.ResolveProviderAPIKey(defaultProviderAPIKey, session.Config.ProviderAPIKey)

	if workspace != "" {
		absWorkspace, err := filepath.Abs(workspace)
		if err != nil {
			return nil, fmt.Errorf("invalid workspace path: %w", err)
		}
		teamWorkspace := filepath.Join(absWorkspace, teamName)
		if err := os.MkdirAll(teamWorkspace, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create workspace: %w", err)
		}
		session.Workspace = teamWorkspace
		session.Config.WorkspaceDir = teamWorkspace
	}

	team.EnsureWorkspaceDirs(session.Workspace)

	if err := team.InitLTM(session.Dir); err != nil {
		stderrLog("%s Failed to init ltm.md: %v\n", errStyle.Render("⚠"), err)
	}
	if err := team.InitSTM(session.Workspace); err != nil {
		stderrLog("%s Failed to init stm.md: %v\n", errStyle.Render("⚠"), err)
	}

	var sessionData *team.SessionData
	var oldSessionEntries []memory.SessionSummaryEntry
	if newSession {
		oldSession := team.LoadSession(session.Workspace)
		if oldSession != nil {
			for _, e := range oldSession.Entries {
				oldSessionEntries = append(oldSessionEntries, memory.SessionSummaryEntry{
					Role:      e.Role,
					Content:   e.Content,
					Timestamp: e.Timestamp,
				})
			}
		}

		existingMD := team.LoadSessionMD(session.Workspace)
		if existingMD != "" {
			stderrLog("%s Archiving previous session...\n", stepStyle.Render("⟳"))
			archivedPath, err := team.ArchiveSessionMD(session.Workspace)
			if err != nil {
				stderrLog("%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
			} else {
				stderrLog("%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
			}
		} else if team.HasSession(session.Workspace) {
			oldSession := team.LoadSession(session.Workspace)
			if oldSession != nil && len(oldSession.Entries) > 0 {
				stderrLog("%s Archiving previous session...\n", stepStyle.Render("⟳"))
				md := team.GenerateSessionMD(oldSession, session.Config.Name)
				team.SaveSessionMD(session.Workspace, md)
				archivedPath, err := team.ArchiveSessionMD(session.Workspace)
				if err != nil {
					stderrLog("%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
				} else {
					stderrLog("%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
				}
				// ArchiveSessionMD already removed session.json; no further cleanup needed.
			} else {
				if err := os.Remove(filepath.Join(session.Workspace, "session.json")); err != nil && !os.IsNotExist(err) {
					stderrLog("%s Failed to remove session file: %v\n", errStyle.Render("⚠"), err)
				}
			}
			team.DeleteConversationHistory(session.Workspace)
		}
		if err := team.CleanRunDirs(session.Workspace); err != nil {
			stderrLog("%s Failed to clean workspace: %v\n", errStyle.Render("⚠"), err)
		}
		team.EnsureWorkspaceDirs(session.Workspace)
		sessionData = team.NewSession()
		team.SaveSession(session.Workspace, sessionData)
		team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name))
		stderrLog("%s Started new session\n", boldStyle.Render("→"))
	} else {
		sessionData = team.LoadSession(session.Workspace)
		existingMD := team.LoadSessionMD(session.Workspace)
		if existingMD != "" {
			stderrLog("%s Resuming session\n", boldStyle.Render("→"))
		} else if sessionData != nil && len(sessionData.Entries) > 0 {
			md := team.GenerateSessionMD(sessionData, session.Config.Name)
			team.SaveSessionMD(session.Workspace, md)
			existingMD = md
			stderrLog("%s Resuming session (%d exchanges, since %s)\n",
				boldStyle.Render("→"),
				len(sessionData.Entries),
				sessionData.CreatedAt,
			)
		} else {
			if sessionData == nil {
				sessionData = team.NewSession()
			}
			team.SaveSession(session.Workspace, sessionData)
			team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name))
			stderrLog("%s Starting new session\n", boldStyle.Render("→"))
		}

		if showHistory && existingMD != "" {
			lines := strings.SplitN(existingMD, "\n", 30)
			preview := strings.Join(lines, "\n")
			stderrLog("\n%s\n%s\n\n",
				boldStyle.Render("─── Previous Session ───"),
				dimStyle.Render(preview),
			)
		}
	}

	stderrLog("%s %s\n", boldStyle.Render("Team:"), session.Config.Name)
	stderrLog("%s ", boldStyle.Render("Agents:"))
	var agentDisplayNames []string
	for _, def := range sortedAgents(session.Agents) {
		roleLabel := def.Role
		agentDisplayNames = append(agentDisplayNames, fmt.Sprintf("%s (%s)", agentStyle.Render(def.Name), dimStyle.Render(roleLabel)))
	}
	stderrLog("%s\n", strings.Join(agentDisplayNames, ", "))

	var mcpManager *mcp.MCPToolManager
	if len(session.MCPServers) > 0 {
		mcpManager = mcp.NewMCPToolManager()
		stderrLog("%s Loading MCP servers...\n", stepStyle.Render("⟳"))
		if err := mcpManager.LoadTools(ctx, session.MCPServers); err != nil {
			stderrLog("%s MCP loading failed: %v\n", errStyle.Render("⚠"), err)
		} else {
			stderrLog("%s MCP tools: %d loaded\n", doneStyle.Render("✓"), len(mcpManager.GetTools()))
		}
	}

	var memStore *memory.MemoryStore
	if memoryEnabled && !tempWorkspace {
		ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedProviderURL)
		embedModel := config.ResolveEmbeddingModel(memoryModel)
		projectDir, _ := os.Getwd()
		var err error
		memStore, err = memory.NewMemoryStore(projectDir, ollamaAPIURL, embedModel)
		if err != nil {
			stderrLog("%s Memory store directory creation failed: %v\n", errStyle.Render("⚠"), err)
		}
		stderrLog("%s Memory: enabled (model: %s)\n", doneStyle.Render("✓"), embedModel)
	} else if !memoryEnabled {
		stderrLog("%s Memory: disabled\n", dimStyle.Render("○"))
	}

	cfg := config.LoadConfig()
	resolvedModelList := cfg.ResolveModelList(session.Config.ModelList)
	resolvedSidecarModel := cfg.ResolveSidecarModel(session.Config.SidecarModel)
	resolvedGuardModel := cfg.ResolveGuardModel(session.Config.GuardModel, session.Config.SidecarModel)
	resolvedMaxConcurrent := cfg.ResolveMaxConcurrent(session.Config.MaxConcurrent)
	if resolvedMaxConcurrent <= 0 {
		resolvedMaxConcurrent = 8
	}
	session.Config.Generation.Model = cfg.ResolveModel(session.Config.Generation.Model)

	allowedPaths := buildAllowedPaths(session, registry, cfg)

	hookRegistry := hooks.NewHookRegistry()
	if configHooks := cfg.GetHooks(); len(configHooks) > 0 {
		if err := hooks.RegisterShellHooks(hookRegistry, configHooks); err != nil {
			stderrLog("%s Invalid hooks config: %v\n", errStyle.Render("⚠"), err)
		} else {
			for k := range configHooks {
				stderrLog("%s Hook: %s\n", dimStyle.Render("◆"), k)
			}
		}
	}

	resolvedRestrictedPath := cfg.RestrictedPath
	if session.Config.RestrictedPath != "" {
		resolvedRestrictedPath = session.Config.RestrictedPath
	}
	if rbashMode && resolvedRestrictedPath == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			rbashBin := filepath.Join(home, ".rbash-bin")
			if fi, err := os.Stat(rbashBin); err == nil && fi.IsDir() {
				resolvedRestrictedPath = rbashBin
			}
		}
	}

	resolvedNoNet := noNet || cfg.NoNet || session.Config.NoNet

	coordinator, err := team.NewCoordinator(session, resolvedProviderURL, resolvedProviderAPIKey, mcpManager, memStore, resolvedModelList, resolvedSidecarModel, resolvedGuardModel, resolvedMaxConcurrent, verbose, think, direnv, allowedPaths, pathConsent, hookRegistry, rbashMode, resolvedRestrictedPath, resolvedNoNet)
	if err != nil {
		return nil, fmt.Errorf("failed to create coordinator: %w", err)
	}
	coordinator.SetSessionData(sessionData)

	if memStore != nil && len(oldSessionEntries) > 0 {
		var summarizeFn memory.SummarizeFunc
		if s := coordinator.Sidecar(); s != nil {
			summarizeFn = s.Summarize
		}
		if err := memory.ArchiveSessionSummary(ctx, memStore, oldSessionEntries, session.Config.Name, summarizeFn); err != nil {
			stderrLog("%s Failed to archive session to memory: %v\n", errStyle.Render("⚠"), err)
		}
	}

	if len(session.Skills) > 0 {
		var skillNames []string
		for _, s := range session.Skills {
			skillNames = append(skillNames, s.Name)
		}
		stderrLog("%s %s\n", boldStyle.Render("Skills:"), strings.Join(skillNames, ", "))
	}

	if len(resolvedModelList) > 0 {
		var modelIDs []string
		for _, m := range resolvedModelList {
			modelIDs = append(modelIDs, m.ID)
		}
		stderrLog("%s %s\n", boldStyle.Render("Models:"), strings.Join(modelIDs, ", "))
	}

	if resolvedSidecarModel != "" {
		stderrLog("%s %s\n", boldStyle.Render("Sidecar:"), resolvedSidecarModel)
	}
	if resolvedGuardModel != "" {
		stderrLog("%s %s\n", boldStyle.Render("Guard:"), resolvedGuardModel)
	}
	if resolvedMaxConcurrent != 8 {
		stderrLog("%s %d\n", boldStyle.Render("Max concurrent:"), resolvedMaxConcurrent)
	}

	resolvedNotify := cfg.ResolveNotify(session.Config.Notify)
	var notifierInst *notify.Notifier
	if resolvedNotify.Enabled() {
		notifierInst = notify.NewNotifier(resolvedNotify, os.Stderr)
		if resolvedNotify.OSC {
			stderrLog("%s %s\n", dimStyle.Render("◆"), "Notify: OSC enabled")
		}
		if resolvedNotify.Command != "" {
			stderrLog("%s %s %s\n", dimStyle.Render("◆"), "Notify:", resolvedNotify.Command)
		}
	}

	return &teamContext{
		session:     session,
		coordinator: coordinator,
		sessionData: sessionData,
		notifier:    notifierInst,
	}, nil
}

func buildAllowedPaths(session *team.TeamSession, registry *team.TeamRegistry, cfg *config.Config) []string {
	seen := make(map[string]bool)
	var paths []string

	projectDir, _ := os.Getwd()
	if projectDir != "" && !seen[projectDir] {
		seen[projectDir] = true
		paths = append(paths, projectDir)
	}

	if session.Dir != "" && !seen[session.Dir] {
		seen[session.Dir] = true
		paths = append(paths, session.Dir)
	}

	if session.Workspace != "" && !seen[session.Workspace] {
		seen[session.Workspace] = true
		paths = append(paths, session.Workspace)
	}

	for _, searchPath := range registry.SearchPaths() {
		if !seen[searchPath] {
			seen[searchPath] = true
			paths = append(paths, searchPath)
		}
	}

	for _, teamDir := range registry.TeamDirs() {
		if !seen[teamDir] {
			seen[teamDir] = true
			paths = append(paths, teamDir)
		}
	}

	skillDirs := []string{
		filepath.Join(session.Dir, ".agents", "skills"),
		filepath.Join(os.Getenv("HOME"), ".agents", "skills"),
	}
	for _, dir := range skillDirs {
		abs, err := filepath.Abs(dir)
		if err == nil && !seen[abs] {
			seen[abs] = true
			paths = append(paths, abs)
		}
	}

	for _, p := range cfg.AllowedPaths {
		expanded := os.ExpandEnv(p)
		if strings.HasPrefix(expanded, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				expanded = filepath.Join(home, expanded[1:])
			}
		}
		abs, err := filepath.Abs(expanded)
		if err == nil && !seen[abs] {
			seen[abs] = true
			paths = append(paths, abs)
		}
	}

	for _, p := range session.Config.AllowedPaths {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	return paths
}

func newPathConsent() *tools.PathConsent {
	return tools.NewPathConsent()
}

func makeStepConfirmFn() func(context.Context, []team.TaskDef) (bool, error) {
	return func(ctx context.Context, tasks []team.TaskDef) (bool, error) {
		tools.StdinMu.Lock()
		defer tools.StdinMu.Unlock()

		tools.SetAskUserActive(true)
		defer tools.SetAskUserActive(false)

		fmt.Fprintf(os.Stderr, "\n%s %d worker task(s) ready to execute:\n",
			boldStyle.Render("─── STEPS ───"), len(tasks))
		for i, t := range tasks {
			fmt.Fprintf(os.Stderr, "  %s %s: %s\n",
				dimStyle.Render(fmt.Sprintf("%d.", i+1)),
				agentStyle.Render(strings.ToLower(t.Agent)),
				t.Goal)
		}
		fmt.Fprintf(os.Stderr, "\n")

		pr := globalPromptReader.Load()
		var answer string
		if pr != nil {
			line, err := pr.ReadLine(boldStyle.Render("Execute? [Y/n]: "))
			if err != nil {
				if errors.Is(err, io.EOF) {
					return false, nil
				}
				return false, err
			}
			answer = strings.TrimSpace(line)
		} else {
			fmt.Fprintf(os.Stderr, "%s", boldStyle.Render("Execute? [Y/n]: "))
			if _, err := fmt.Scanln(&answer); err != nil {
				if err == io.EOF {
					return false, nil
				}
				return false, err
			}
		}

		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "" || answer == "y" || answer == "yes", nil
	}
}

func runWithInjection(ctx context.Context, tc *teamContext, initialResult string, injector *promptInjector) (string, error) {
	result := initialResult
	for {
		select {
		case <-injector.wrapUpCh:
			stderrLog("\n%s Wrapping up — coordinator will summarize and finish.\n\n", boldStyle.Render("⏹"))
			contResult, err := tc.coordinator.ContinueWithPrompt(ctx, "")
			if err != nil {
				if ctx.Err() == context.Canceled {
					return result, nil
				}
				return result, err
			}
			result += "\n\n---\n\n" + contResult
			return result, nil
		case prompt, ok := <-injector.ch:
			if !ok {
				return result, nil
			}
			stderrLog("\n%s Injecting additional prompt...\n\n", boldStyle.Render("↩"))

			contResult, err := tc.coordinator.ContinueWithPrompt(ctx, prompt)
			if err != nil {
				return result, err
			}
			result += "\n\n---\n\n" + contResult
		default:
			return result, nil
		}
	}
}

func executeSegments(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry, defaultProviderURL string, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string) (string, error) {
	var results []string
	currentTeamName := ""

	for i, seg := range segments {
		switch seg.Type {
		case team.SegmentSwitchTeam:
			teamName := seg.Name
			tc, ok := loadedTeams[teamName]
			if !ok {
				loaded, err := loadTeamByName(ctx, teamName, registry, providerURL, providerAPIKey, pathConsent, vars)
				if err != nil {
					return strings.Join(results, "\n\n"), fmt.Errorf("failed to load team %q: %w", teamName, err)
				}
				if stepsMode {
					loaded.coordinator.SetStepConfirmFn(makeStepConfirmFn())
				}
				tc = loaded
				loadedTeams[teamName] = tc
			}

			if currentTeamName != "" && currentTeamName != teamName {
				prevTC := loadedTeams[currentTeamName]
				if prevTC != nil {
					team.SaveSessionMD(prevTC.session.Workspace, team.GenerateSessionMD(prevTC.sessionData, prevTC.session.Config.Name))
				}
				stderrLog("\n%s Switching team: %s → %s\n\n", boldStyle.Render("⇒"), teamStyle.Render(currentTeamName), teamStyle.Render(teamName))
			}

			currentTeamName = teamName

			if seg.Content == "" {
				continue
			}

			disp := newCoordDisplay(tc)

			stderrLog("\n%s Starting team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(teamName))

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, seg.Content)
			activeCoord.Clear()
			disp.stopTimer()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				team.SaveSession(tc.session.Workspace, tc.sessionData)
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q failed: %w", teamName, err)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				team.SaveSession(tc.session.Workspace, tc.sessionData)
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q continuation failed: %w", teamName, err)
			}

			disp.finalizeTasks()
			stderrLog("\n%s Team %s coordination complete.\n", doneStyle.Render("✓"), teamStyle.Render(teamName))
			results = append(results, fmt.Sprintf("## Team: %s\n%s", teamName, result))

		case team.SegmentInvokeAgent:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("@%s — no active team. Specify a team with --agent-team or @team-name first", seg.Name)
			}

			tc := loadedTeams[currentTeamName]
			if tc == nil {
				return strings.Join(results, "\n\n"), fmt.Errorf("@%s — team %q not loaded", seg.Name, currentTeamName)
			}

			disp2 := newCoordDisplay(tc)

			stderrLog("\n%s Direct invocation: @%s (team: %s)\n\n", boldStyle.Render("→"), agentStyle.Render(seg.Name), teamStyle.Render(currentTeamName))

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			directResult, err := tc.coordinator.RunDirectAgent(ctx, seg.Name, seg.Content)
			activeCoord.Clear()
			disp2.stopTimer()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				return strings.Join(results, "\n\n"), fmt.Errorf("direct agent @%s failed: %w", seg.Name, err)
			}

			if directResult.Error != nil {
				stderrLog("\n%s %s failed: %s\n", errStyle.Render("✗"), agentStyle.Render(seg.Name), errStyle.Render(directResult.Error.Error()))
				results = append(results, fmt.Sprintf("## Agent: @%s\n**ERROR**: %s", seg.Name, directResult.Error))
				continue
			}

			stderrLog("\n%s %s completed, synthesizing...\n\n", doneStyle.Render("✓"), agentStyle.Render(seg.Name))

			orchDef := tc.coordinator.GetOrchestratorDef()
			if orchDef == nil {
				disp2.finalizeTasks()
				results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, directResult.Output))
			} else {
				synthesisPrompt := fmt.Sprintf("A user directly asked @%s to do the following task:\n\n%s\n\nHere is what %s produced:\n\n---\n%s\n---\n\nPlease synthesize this into a final, well-organized answer for the user.",
					seg.Name, seg.Content, seg.Name, directResult.Output)
				activeCoord.Set(tc.coordinator)
				if injector.IsWrapUpRequested() {
					tc.coordinator.SetWrapUp()
				}
				synthResult, err := tc.coordinator.Run(ctx, synthesisPrompt)
				activeCoord.Clear()
				if err != nil {
					if ctx.Err() == context.Canceled {
						team.SaveSession(tc.session.Workspace, tc.sessionData)
						stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
						return "", errInterrupted{}
					}
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
					return strings.Join(results, "\n\n"), fmt.Errorf("synthesis for @%s failed: %w", seg.Name, err)
				}

				synthResult, err = runWithInjection(ctx, tc, synthResult, injector)
				if err != nil {
					if ctx.Err() == context.Canceled {
						team.SaveSession(tc.session.Workspace, tc.sessionData)
						stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
						return "", errInterrupted{}
					}
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
					return strings.Join(results, "\n\n"), fmt.Errorf("synthesis continuation for @%s failed: %w", seg.Name, err)
				}

				disp2.finalizeTasks()
				results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, synthResult))
			}

		case team.SegmentText:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("text segment with no active team — specify a team with --agent-team or @team-name first")
			}

			tc := loadedTeams[currentTeamName]

			disp3 := newCoordDisplay(tc)

			stderrLog("\n%s Team %s processing...\n\n", boldStyle.Render("→"), teamStyle.Render(currentTeamName))

			activeCoord.Set(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, seg.Content)
			activeCoord.Clear()
			disp3.stopTimer()

			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				team.SaveSession(tc.session.Workspace, tc.sessionData)
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q failed: %w", currentTeamName, err)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				if ctx.Err() == context.Canceled {
					team.SaveSession(tc.session.Workspace, tc.sessionData)
					stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
					return "", errInterrupted{}
				}
				team.SaveSession(tc.session.Workspace, tc.sessionData)
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
				return strings.Join(results, "\n\n"), fmt.Errorf("team %q continuation failed: %w", currentTeamName, err)
			}

			disp3.finalizeTasks()
			results = append(results, fmt.Sprintf("## Team: %s\n%s", currentTeamName, result))
		}

		if i == len(segments)-1 {
			if tc, ok := loadedTeams[currentTeamName]; ok {
				team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
			}
		}
	}

	if len(results) == 0 {
		return "", nil
	}
	if len(results) == 1 {
		return results[0], nil
	}
	return strings.Join(results, "\n\n---\n\n"), nil
}

func agentNamesFromSession(session *team.TeamSession) []*agent.AgentDef {
	seen := make(map[string]bool)
	var agents []*agent.AgentDef
	for _, def := range session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		agents = append(agents, def)
	}
	return agents
}

func sortedAgents(agents map[string]*agent.AgentDef) []*agent.AgentDef {
	seen := make(map[string]bool)
	var result []*agent.AgentDef
	for _, def := range agents {
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		result = append(result, def)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func defaultHistoryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".hufu")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return filepath.Join(dir, "prompt_history")
}

func archiveCurrentSessionToMemory(ctx context.Context, tc *teamContext) {
	if tc == nil || tc.sessionData == nil || len(tc.sessionData.Entries) == 0 {
		fmt.Fprintf(os.Stderr, "%s No session data to archive.\n", dimStyle.Render("○"))
		return
	}

	resolvedURL := config.ResolveProviderURL(providerURL, "", "")
	ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedURL)
	embedModel := config.ResolveEmbeddingModel(memoryModel)
	projectDir, _ := os.Getwd()

	memStore, err := memory.NewMemoryStore(projectDir, ollamaAPIURL, embedModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s Memory unavailable for archive: %v\n", errStyle.Render("⚠"), err)
		return
	}
	defer memStore.Close()

	var entries []memory.SessionSummaryEntry
	for _, e := range tc.sessionData.Entries {
		entries = append(entries, memory.SessionSummaryEntry{
			Role:      e.Role,
			Content:   e.Content,
			Timestamp: e.Timestamp,
		})
	}

	var summarizeFn memory.SummarizeFunc
	if s := tc.coordinator.Sidecar(); s != nil {
		summarizeFn = s.Summarize
	}
	if err := memory.ArchiveSessionSummary(ctx, memStore, entries, tc.session.Config.Name, summarizeFn); err != nil {
		fmt.Fprintf(os.Stderr, "%s Failed to archive session to memory: %v\n", errStyle.Render("⚠"), err)
		return
	}
	fmt.Fprintf(os.Stderr, "%s Session archived to memory.\n", doneStyle.Render("✓"))
}

func runArchiveMemory(ctx context.Context, registry *team.TeamRegistry, vars map[string]string) error {
	teams := registry.ListTeams()
	if len(teams) == 0 {
		return fmt.Errorf("no teams available")
	}

	var teamNames []string
	if agentTeamName != "" {
		teamNames = []string{strings.ToLower(agentTeamName)}
	} else {
		teamNames = teams
	}

	archived := 0
	for _, name := range teamNames {
		tc, err := loadTeamByName(ctx, name, registry, providerURL, providerAPIKey, newPathConsent(), vars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s Failed to load team %q: %v\n", errStyle.Render("⚠"), name, err)
			continue
		}
		if tc.sessionData != nil && len(tc.sessionData.Entries) > 0 {
			archiveCurrentSessionToMemory(ctx, tc)
			archived++
		} else {
			fmt.Fprintf(os.Stderr, "%s No session data for team %q\n", dimStyle.Render("○"), name)
		}
	}

	if archived == 0 {
		fmt.Fprintf(os.Stderr, "%s No session data found to archive.\n", dimStyle.Render("○"))
	}
	return nil
}

func askUserForPromptFallback() (string, error) {
	var input string
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	fmt.Fprintf(os.Stderr, "%s ", boldStyle.Render(">"))
	fmt.Scanln(&input)
	prompt := strings.TrimSpace(input)
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		return "", fmt.Errorf("no prompt provided")
	}
	return prompt, nil
}

func askUserForTeamFallback(teams []string) string {
	if len(teams) == 0 {
		return ""
	}
	sort.Strings(teams)
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Select Team ───"))
	for i, t := range teams {
		fmt.Fprintf(os.Stderr, "  %s %s\n", dimStyle.Render(fmt.Sprintf("%d.", i+1)), teamStyle.Render(t))
	}
	fmt.Fprintf(os.Stderr, "\n%s ", boldStyle.Render("Team name or number:>"))
	var input string
	fmt.Scanln(&input)
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	var num int
	if n, err := fmt.Sscanf(input, "%d", &num); err == nil && n == 1 && num >= 1 && num <= len(teams) {
		return teams[num-1]
	}
	lower := strings.ToLower(input)
	for _, t := range teams {
		if strings.ToLower(t) == lower {
			return t
		}
	}
	return input
}

func readStdin() string {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return ""
	}
	data, _ := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(data))
}

func askUserForPrompt(pr *readline.PromptReader) (string, error) {
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Enter Prompt ───"))
	fmt.Fprintf(os.Stderr, "Describe the task (use @team-name or @agent-name in the prompt):\n")
	prompt, err := pr.ReadLine(boldStyle.Render("> "))
	if err != nil {
		if err == ergoreadline.ErrInterrupt || err == io.EOF {
			fmt.Fprintf(os.Stderr, "\n")
			return "", errInterrupted{}
		}
		fmt.Fprintf(os.Stderr, "%s Input error: %v\n", errStyle.Render("✗"), err)
		return "", fmt.Errorf("input error: %w", err)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "%s No prompt provided.\n", errStyle.Render("✗"))
		return "", fmt.Errorf("no prompt provided")
	}
	return prompt, nil
}

func askUserForTeam(teams []string, pr *readline.PromptReader) (string, error) {
	if len(teams) == 0 {
		return "", nil
	}
	sort.Strings(teams)
	fmt.Fprintf(os.Stderr, "\n%s\n", boldStyle.Render("─── Select Team ───"))
	for i, t := range teams {
		fmt.Fprintf(os.Stderr, "  %s %s\n", dimStyle.Render(fmt.Sprintf("%d.", i+1)), teamStyle.Render(t))
	}
	input, err := pr.ReadLine(boldStyle.Render("Team name or number:>"))
	if err != nil {
		if err == ergoreadline.ErrInterrupt || err == io.EOF {
			fmt.Fprintf(os.Stderr, "\n")
			return "", errInterrupted{}
		}
		return "", fmt.Errorf("input error: %w", err)
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nil
	}
	var num int
	if n, err := fmt.Sscanf(input, "%d", &num); err == nil && n == 1 && num >= 1 && num <= len(teams) {
		return teams[num-1], nil
	}
	lower := strings.ToLower(input)
	for _, t := range teams {
		if strings.ToLower(t) == lower {
			return t, nil
		}
	}
	return input, nil
}
