package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/manifoldco/promptui"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/hooks"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/notify"
	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
)

type teamContext struct {
	teamName    string
	session     *team.TeamSession
	coordinator *team.Coordinator
	sessionData *team.SessionData
	notifier    *notify.Notifier
}

// applyUnattendedAndBudget configures the coordinator's unattended mode,
// run budgets, and acceptance check from the CLI flags and team config.
// CLI flags take precedence; team.yaml values are the fallback.
func applyUnattendedAndBudget(coordinator *team.Coordinator, session *team.TeamSession) error {
	prof := coordinator.ExecutionProfile()
	coordinator.SetUnattended(opts.unattended || session.Config.Unattended || prof.IsUnattended())
	coordinator.SetAutoApprove(opts.autoApprove || session.Config.AutoApprove)
	coordinator.SetNoJournal(opts.noJournal)

	budgetSeconds := opts.maxDuration
	if budgetSeconds == 0 {
		budgetSeconds = session.Config.MaxWallClock
	}
	budgetTokens := opts.maxTotalTokens
	if budgetTokens == 0 {
		budgetTokens = session.Config.MaxTotalTokens
	}
	coordinator.SetBudget(budgetSeconds, budgetTokens)
	coordinator.SetAcceptance(session.Config.Acceptance)
	coordinator.SetRollback(session.Config.Rollback)

	goalMode := opts.goalMode
	if goalMode == "" {
		goalMode = session.Config.GoalMode
	}
	if goalMode != "" {
		gm, err := team.ParseGoalMode(goalMode)
		if err != nil {
			return fmt.Errorf("invalid goal mode: %w", err)
		}
		if err := coordinator.SetGoalMode(gm); err != nil {
			return err
		}
	}
	return nil
}

// loadTeamCommon is the shared post-load setup for both named and default
// teams. It handles workspace creation, session lifecycle, model/provider
// resolution, coordinator construction, and notification setup.
// The session must already be loaded (via team.LoadTeam or team.LoadDefaultTeam)
// and have its Workspace set.
func loadTeamCommon(ctx context.Context, teamName string, session *team.TeamSession, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, registry *team.TeamRegistry, forcedSkills []string, planMode bool, autoSkillsMode bool, buildMCP bool) (*teamContext, error) {
	// Apply CLI model overrides as the highest-priority model config layer.
	applyCLIModelOverrides(&session.Config, currentModelOverrides())
	applyCLITimeoutOverrides(session, currentTimeoutOverrides())
	applyCLIVerifyTimeoutOverrides(session, currentVerifyTimeoutOverrides())
	applyCLITuningOverrides(session, currentTuningOverrides())
	propagateTeamGenerationToAgents(session)

	execProfile, err := team.ResolveExecutionProfile(opts.executionProfile, session.Config.ExecutionProfile)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve execution profile: %w", err)
	}

	// Validate workspace isolation early BEFORE any workspace directory creation,
	// file initialization (InitLTM/InitSTM), or session lifecycle I/O so rejected
	// strict workspaces leave no side effects on disk.
	projectDir, _ := os.Getwd()
	if err := team.ValidateWorkspaceIsolationPaths(session.Workspace, projectDir, session.Dir, session.Config.Name, execProfile); err != nil {
		return nil, err
	}

	resolvedProviderURL := config.ResolveProviderURL(defaultProviderURL, session.Config.ProviderURL, "")
	resolvedProviderAPIKey := config.ResolveProviderAPIKey(defaultProviderAPIKey, session.Config.ProviderAPIKey)

	displayTeamHeader(session)

	cfg := config.LoadConfig()
	var mcpManager *mcp.MCPToolManager
	if buildMCP {
		mcpManager = buildMCPManager(ctx, session, cfg)
	}
	memStore := buildMemoryStore(resolvedProviderURL)

	resolvedModelList := cfg.ResolveModelList(session.Config.ModelList)
	resolvedSidecarModel := cfg.ResolveSidecarModel(session.Config.SidecarModel)
	resolvedGuardModel := cfg.ResolveGuardModel(session.Config.GuardModel, session.Config.SidecarModel)
	resolvedJudgeModel := cfg.ResolveJudgeModel(session.Config.JudgeModel, session.Config.SidecarModel)
	resolvedPlanReviewerModel := cfg.ResolvePlanReviewerModel(session.Config.PlanReviewerModel, session.Config.Generation.Model)
	resolvedMaxConcurrent := cfg.ResolveMaxConcurrent(session.Config.MaxConcurrent)
	if resolvedMaxConcurrent <= 0 {
		resolvedMaxConcurrent = 8
	}
	if err := resolveAndCheckModel(session, cfg); err != nil {
		return nil, err
	}

	allowedPaths := buildAllowedPaths(session, registry, cfg)

	if err := team.EnsureWorkspaceDirs(session.Workspace); err != nil {
		stderrLog("%s Failed to ensure workspace dirs: %v\n", errStyle.Render("⚠"), err)
	}
	if err := team.InitLTM(session.Workspace, session.Config.Name); err != nil {
		stderrLog("%s Failed to init ltm.md: %v\n", errStyle.Render("⚠"), err)
	}
	if err := team.InitSTM(session.Workspace); err != nil {
		stderrLog("%s Failed to init stm.md: %v\n", errStyle.Render("⚠"), err)
	}
	if opts.newSession {
		team.ExtractLTMFromHistory(session.Workspace, session.Config.Name)
		team.PruneSessionHistory(session.Workspace, team.MaxSessionHistoryFiles)
	}

	sessionData, oldSessionEntries, err := prepareSessionLifecycle(session)
	if err != nil {
		return nil, err
	}

	hookRegistry := registerHooks(cfg)
	if hookRegistry != nil {
		hookRegistry.SetFailureMode(hooks.PolicyFailureMode(execProfile.HookFailureMode))
	}

	resolvedRestrictedPath := resolveRestrictedPath(session, cfg)
	resolvedNoNet := opts.noNet || cfg.NoNet || session.Config.NoNet
	resolvedForceMCP := opts.forceMCP || cfg.ForceMCP || session.Config.ForceMCP

	// Each team receives its own consent store. The store persists explicit
	// "always" decisions in the team directory and prevents one team's policy
	// from granting access to another team in a multi-team prompt.
	teamPathConsent := pathConsent
	if pathConsent != nil {
		teamPathConsent, err = tools.NewTeamPathConsent(session.Dir)
		if err != nil {
			return nil, fmt.Errorf("failed to load path consent policy: %w", err)
		}
	}

	roleModels := team.RoleModels{Sidecar: resolvedSidecarModel, Guard: resolvedGuardModel, Judge: resolvedJudgeModel, PlanReviewer: resolvedPlanReviewerModel}
	coordinator, err := team.NewCoordinator(session, resolvedProviderURL, resolvedProviderAPIKey, mcpManager, memStore, resolvedModelList, roleModels, resolvedMaxConcurrent, opts.verbose, opts.think, opts.direnv, allowedPaths, teamPathConsent, hookRegistry, opts.rbashMode, resolvedRestrictedPath, resolvedNoNet, resolvedForceMCP, forcedSkills, planMode, autoSkillsMode)
	if err != nil {
		return nil, fmt.Errorf("failed to create coordinator: %w", err)
	}

	coordinator.SetExecutionProfile(execProfile)
	coordinator.SetSessionData(sessionData)
	if err := applyUnattendedAndBudget(coordinator, session); err != nil {
		return nil, err
	}
	if err := coordinator.SetPTYTerminalEnabled(opts.enablePTYTerminal); err != nil {
		return nil, err
	}
	archiveToMemory(ctx, memStore, coordinator, session, oldSessionEntries)
	displayResolvedConfig(session, resolvedModelList, resolvedSidecarModel, resolvedGuardModel, resolvedJudgeModel, resolvedPlanReviewerModel, resolvedMaxConcurrent, execProfile)
	notifierInst := buildNotifier(cfg, session)

	return &teamContext{
		teamName:    teamName,
		session:     session,
		coordinator: coordinator,
		sessionData: sessionData,
		notifier:    notifierInst,
	}, nil
}

func loadTeamByName(ctx context.Context, teamName string, registry *team.TeamRegistry, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, vars map[string]string, forcedSkills []string, planMode bool, autoSkillsMode bool) (*teamContext, error) {
	teamDir, err := registry.Resolve(teamName)
	if err != nil {
		return nil, err
	}

	session, err := team.LoadTeam(teamDir, vars, forcedSkills)
	if err != nil {
		return nil, err
	}

	if err := resolveTeamWorkspacePath(teamName, session); err != nil {
		return nil, err
	}

	return loadTeamCommon(ctx, teamName, session, defaultProviderURL, defaultProviderAPIKey, pathConsent, registry, forcedSkills, planMode, autoSkillsMode, true)
}

// loadDefaultTeam builds an in-memory default team (coordinator + Helper)
// without consulting the .agent-teams/ registry.
func loadDefaultTeam(ctx context.Context, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, vars map[string]string, forcedSkills []string, planMode bool, autoSkillsMode bool) (*teamContext, error) {
	teamName := "default"

	dummySession := &team.TeamSession{}
	if err := resolveTeamWorkspacePath(teamName, dummySession); err != nil {
		return nil, err
	}
	teamWorkspace := dummySession.Workspace

	session, err := team.LoadDefaultTeam(teamWorkspace, forcedSkills, opts.helperTools)
	if err != nil {
		return nil, err
	}
	session.Workspace = teamWorkspace
	session.Config.WorkspaceDir = teamWorkspace

	return loadTeamCommon(ctx, teamName, session, defaultProviderURL, defaultProviderAPIKey, pathConsent, nil, forcedSkills, planMode, autoSkillsMode, false)
}

func buildAllowedPaths(session *team.TeamSession, registry *team.TeamRegistry, cfg *config.Config) []string {
	seen := make(map[string]bool)
	var paths []string

	if projectDir := currentWorkingDir(); projectDir != "" && !seen[projectDir] {
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

	if registry != nil {
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
	}

	skillDirs := []string{
		filepath.Join(session.Dir, "skills"),
		filepath.Join(session.Dir, ".agents", "skills"),
		filepath.Join(os.Getenv("HOME"), ".agents", "skills"),
	}
	if registry != nil {
		for _, teamDir := range registry.TeamDirs() {
			skillDirs = append(skillDirs, filepath.Join(teamDir, "skills"))
		}
	}
	migrateLegacyDrafts(skillDirs)
	for _, dir := range skillDirs {
		abs, err := filepath.Abs(dir)
		if err == nil && !seen[abs] {
			seen[abs] = true
			paths = append(paths, abs)
		}
	}

	for _, p := range normalizeAllowedPaths(cfg.AllowedPaths) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	for _, p := range normalizeAllowedPaths(session.Config.AllowedPaths) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	for _, p := range normalizeAllowedPaths(opts.allowPaths) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	return paths
}

func normalizeAllowedPaths(paths []string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, p := range paths {
		expanded := os.ExpandEnv(strings.TrimSpace(p))
		if expanded == "" {
			continue
		}
		if strings.HasPrefix(expanded, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				expanded = filepath.Join(home, expanded[1:])
			}
		}
		abs, err := filepath.Abs(expanded)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		result = append(result, abs)
	}
	return result
}

func currentWorkingDir() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return ""
	}
	if eval, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = eval
	}
	return filepath.Clean(cwd)
}

// migrateLegacyDrafts moves any skills/draft-* to skills/drafts/draft-*
// to support the new draft directory layout. Idempotent.
func migrateLegacyDrafts(skillDirs []string) {
	for _, dir := range skillDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "draft-") {
				continue
			}
			src := filepath.Join(dir, entry.Name())
			dst := filepath.Join(dir, "drafts", entry.Name())
			// Skip if dst already exists
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := os.MkdirAll(filepath.Join(dir, "drafts"), 0o755); err != nil {
				log.Printf("[WARN] failed to create drafts/ in %s: %v", dir, err)
				continue
			}
			if err := os.Rename(src, dst); err != nil {
				log.Printf("[WARN] failed to migrate %s -> %s: %v", src, dst, err)
				continue
			}
			log.Printf("[INFO] migrated legacy draft: %s -> %s", src, dst)
		}
	}
}

func newPathConsent() *tools.PathConsent {
	return tools.NewPathConsent()
}

// promptForMissingTemplateVars detects required template variables
// (e.g. {{PROJECT_NAME}}) in the team's config files and prompts the
// user to fill them in when --unattended is not set. The updated vars
// map is returned (or unchanged if there are no missing vars).
func promptForMissingTemplateVars(ctx context.Context, segName string, registry *team.TeamRegistry, pr *readline.PromptReader, vars map[string]string) (map[string]string, error) {
	if opts.unattended {
		return vars, nil
	}
	teamDir, err := registry.Resolve(segName)
	if err != nil {
		return vars, nil // unknown team — loadTeamByName will surface the error
	}
	missingVars, err := team.FindMissingVars(teamDir, vars)
	if err != nil || len(missingVars) == 0 {
		return vars, nil
	}
	fmt.Fprintf(os.Stderr, "\n%s Some template variables are required for team %s:\n", boldStyle.Render("📋"), teamStyle.Render(segName))
	for _, mv := range missingVars {
		var val string
		if pr != nil {
			promptStr := fmt.Sprintf("  Enter value for %s: ", teamStyle.Render(mv))
			val, err = pr.ReadLine(boldStyle.Render(promptStr))
		} else {
			prompt := promptui.Prompt{
				Label: fmt.Sprintf("Enter value for %s", mv),
			}
			val, err = prompt.Run()
		}
		if err != nil {
			return vars, fmt.Errorf("failed to read variable input: %w", err)
		}
		if vars == nil {
			vars = make(map[string]string)
		}
		vars[mv] = val
	}
	return vars, nil
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
