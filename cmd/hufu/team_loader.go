package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/hooks"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/notify"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
)

// resolveTeamWorkspacePath computes the per-team workspace directory path
// without creating it on disk. For named teams it nests under <workspace>/<team-name>;
// for the built-in default team it uses the workspace root itself.
// The computed directory path is assigned to session.Workspace and
// session.Config.WorkspaceDir before returning.
func resolveTeamWorkspacePath(teamName string, session *team.TeamSession) error {
	var baseWorkspace string
	if opts.workspace != "" {
		abs, err := filepath.Abs(opts.workspace)
		if err != nil {
			return fmt.Errorf("invalid workspace path: %w", err)
		}
		baseWorkspace = abs
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		baseWorkspace = filepath.Join(cwd, "workspace")
	}
	teamWorkspace := filepath.Join(baseWorkspace, teamName)
	session.Workspace = teamWorkspace
	session.Config.WorkspaceDir = teamWorkspace
	return nil
}

// prepareSessionLifecycle handles the newSession/resume branch:
//   - When newSession is set, the existing session.json (if any) is
//     archived, conversation history is cleared, and a fresh session
//     is started.
//   - When newSession is false, the existing session.json (if any) is
//     loaded and resumed.
//
// On success, it returns the SessionData to attach to the coordinator
// and the (possibly empty) list of old session entries to archive to
// memory.
func prepareSessionLifecycle(session *team.TeamSession) (*team.SessionData, []memory.SessionSummaryEntry, error) {
	if opts.newSession {
		oldSessionEntries := loadOldSessionEntries(session.Workspace)
		if err := archivePreviousSession(session); err != nil {
			// Archive failures are non-fatal — the user already gets a warning
			// via stderrLog. We continue with a fresh session below.
			_ = err
		}
		if err := team.CleanRunDirs(session.Workspace); err != nil {
			return nil, nil, fmt.Errorf("failed to clean workspace with durable evidence: %w", err)
		}
		if err := team.EnsureWorkspaceDirs(session.Workspace); err != nil {
			stderrLog("%s Failed to ensure workspace dirs: %v\n", errStyle.Render("⚠"), err)
		}
		sessionData := team.NewSession()
		if err := team.SaveSession(session.Workspace, sessionData); err != nil {
			stderrLog("%s Failed to save session: %v\n", errStyle.Render("⚠"), err)
		}
		if err := team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name)); err != nil {
			stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
		}
		stderrLog("%s Started new session\n", boldStyle.Render("→"))
		return sessionData, oldSessionEntries, nil
	}
	sessionData, err := resumeOrStartSession(session)
	return sessionData, nil, err
}

// loadOldSessionEntries reads the previous session.json (if any) and
// converts it into SessionSummaryEntry records for memory archiving.
func loadOldSessionEntries(workspacePath string) []memory.SessionSummaryEntry {
	oldSession := team.LoadSession(workspacePath)
	if oldSession == nil {
		return nil
	}
	entries := make([]memory.SessionSummaryEntry, 0, len(oldSession.Entries))
	for _, e := range oldSession.Entries {
		entries = append(entries, memory.SessionSummaryEntry{
			Role:      e.Role,
			Content:   e.Content,
			Timestamp: e.Timestamp,
		})
	}
	return entries
}

// archivePreviousSession implements the newSession archive flow:
//   - If a session.md exists, archive it directly.
//   - Otherwise, if session.json has entries, generate a session.md
//     from it, save, and then archive it.
//   - Remove the leftover session.json and clear conversation history.
func archivePreviousSession(session *team.TeamSession) error {
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
			if err := team.SaveSessionMD(session.Workspace, md); err != nil {
				stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
			}
			archivedPath, err := team.ArchiveSessionMD(session.Workspace)
			if err != nil {
				stderrLog("%s Failed to archive session: %v\n", errStyle.Render("⚠"), err)
			} else {
				stderrLog("%s Previous session archived to %s\n", doneStyle.Render("✓"), filepath.Base(archivedPath))
			}
		} else {
			if err := os.Remove(filepath.Join(session.Workspace, "session.json")); err != nil && !os.IsNotExist(err) {
				stderrLog("%s Failed to remove session file: %v\n", errStyle.Render("⚠"), err)
			}
		}
	}
	// session.md and chat_history.md are independent projections. The former
	// may have been archived through the first branch above, but the latter
	// still contains model messages and must never survive --new.
	if err := team.DeleteConversationHistory(session.Workspace); err != nil {
		stderrLog("%s Failed to delete conversation history: %v\n", errStyle.Render("⚠"), err)
	}
	return nil
}

// resumeOrStartSession implements the !newSession branch: load the
// existing session (if any), and print an appropriate status line. If
// no prior session is found, a fresh one is created.
func resumeOrStartSession(session *team.TeamSession) (*team.SessionData, error) {
	sessionData := team.LoadSession(session.Workspace)
	existingMD := team.LoadSessionMD(session.Workspace)
	if existingMD != "" {
		stderrLog("%s Resuming session\n", boldStyle.Render("→"))
	} else if sessionData != nil && len(sessionData.Entries) > 0 {
		md := team.GenerateSessionMD(sessionData, session.Config.Name)
		if err := team.SaveSessionMD(session.Workspace, md); err != nil {
			stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
		}
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
		if err := team.SaveSession(session.Workspace, sessionData); err != nil {
			stderrLog("%s Failed to save session: %v\n", errStyle.Render("⚠"), err)
		}
		if err := team.SaveSessionMD(session.Workspace, team.GenerateSessionMD(sessionData, session.Config.Name)); err != nil {
			stderrLog("%s Failed to save session md: %v\n", errStyle.Render("⚠"), err)
		}
		stderrLog("%s Starting new session\n", boldStyle.Render("→"))
	}

	if opts.showHistory && existingMD != "" {
		lines := strings.SplitN(existingMD, "\n", 30)
		preview := strings.Join(lines, "\n")
		stderrLog("\n%s\n%s\n\n",
			boldStyle.Render("─── Previous Session ───"),
			dimStyle.Render(preview),
		)
	}
	return sessionData, nil
}

// displayTeamHeader prints the team name and a comma-separated list of
// agents with their roles. Used by both loaders before continuing
// with MCP / memory / coordinator setup.
func displayTeamHeader(session *team.TeamSession) {
	stderrLog("%s %s\n", boldStyle.Render("Team:"), session.Config.Name)
	stderrLog("%s ", boldStyle.Render("Agents:"))
	var agentDisplayNames []string
	for _, def := range sortedAgents(session.Agents) {
		roleLabel := def.Role
		agentDisplayNames = append(agentDisplayNames, fmt.Sprintf("%s (%s)", agentStyle.Render(def.Name), dimStyle.Render(roleLabel)))
	}
	stderrLog("%s\n", strings.Join(agentDisplayNames, ", "))
}

// buildMCPManager creates an MCPToolManager if the session has MCP
// servers or any agent defines mcp-tools. Returns nil otherwise (and
// for the default team which never has MCP).
func buildMCPManager(ctx context.Context, session *team.TeamSession, cfg *config.Config) *mcp.MCPToolManager {
	hasMCPTools := false
	for _, def := range session.Agents {
		if len(def.MCPTools) > 0 {
			hasMCPTools = true
			break
		}
	}
	if len(session.MCPServers) == 0 && !hasMCPTools {
		return nil
	}
	globalShell := cfg.Shell
	if globalShell == "" {
		globalShell = "bash"
	}
	teamShell := session.Config.Shell
	if teamShell == "" {
		teamShell = globalShell
	}
	manager := mcp.NewMCPToolManager(globalShell, teamShell)
	if len(session.MCPServers) > 0 {
		stderrLog("%s Loading MCP servers...\n", stepStyle.Render("⟳"))
		if err := manager.LoadTools(ctx, session.MCPServers); err != nil {
			stderrLog("%s MCP loading failed: %v\n", errStyle.Render("⚠"), err)
		} else {
			stderrLog("%s MCP tools: %d loaded\n", doneStyle.Render("✓"), len(manager.GetTools()))
		}
	}
	return manager
}

// buildMemoryStore is retained as a compatibility seam while legacy stores
// are migrated. New runs use context.sqlite plus its rebuildable canonical
// vector index; they must not create a second MemoryRecord source of truth.
func buildMemoryStore(resolvedProviderURL string) *memory.MemoryStore {
	_ = resolvedProviderURL
	if !opts.memoryEnabled || opts.tempWorkspace {
		if !opts.memoryEnabled {
			stderrLog("%s Memory: disabled\n", dimStyle.Render("○"))
		}
		return nil
	}
	stderrLog("%s Memory: canonical context index enabled (model: %s)\n", doneStyle.Render("✓"), config.ResolveEmbeddingModel(opts.memoryModel))
	return nil
}

// resolveAndCheckModel pulls the model from hufu.yaml and validates
// that something is set, returning a clear actionable error otherwise.
func resolveAndCheckModel(session *team.TeamSession, cfg *config.Config) error {
	session.Config.Generation.Model = cfg.ResolveModel(session.Config.Generation.Model)
	if session.Config.Generation.Model == "" {
		return fmt.Errorf("no model specified for team %q\n  Set --model <name>, add 'model:' to the team's team.yaml, or add 'model:' to ~/.config/hufu/hufu.yaml\n  Run 'hufu doctor' to see which model is currently resolved", session.Config.Name)
	}
	return nil
}

// registerHooks builds a hook registry and registers any shell hooks
// declared in hufu.yaml. Errors are non-fatal; warnings are printed.
func registerHooks(cfg *config.Config) *hooks.HookRegistry {
	registry := hooks.NewHookRegistry()
	configHooks := cfg.GetHooks()
	if len(configHooks) == 0 {
		return registry
	}
	if err := hooks.RegisterShellHooks(registry, configHooks); err != nil {
		stderrLog("%s Invalid hooks config: %v\n", errStyle.Render("⚠"), err)
		return registry
	}
	for k := range configHooks {
		stderrLog("%s Hook: %s\n", dimStyle.Render("◆"), k)
	}
	return registry
}

// resolveRestrictedPath picks the restricted path (for rbash) from
// hufu.yaml, the team config, or the rbash-bin convention.
func resolveRestrictedPath(session *team.TeamSession, cfg *config.Config) string {
	resolved := cfg.RestrictedPath
	if session.Config.RestrictedPath != "" {
		resolved = session.Config.RestrictedPath
	}
	if opts.rbashMode && resolved == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			rbashBin := filepath.Join(home, ".rbash-bin")
			if fi, err := os.Stat(rbashBin); err == nil && fi.IsDir() {
				resolved = rbashBin
			}
		}
	}
	return resolved
}

// displayResolvedConfig prints the resolved skill/model/sidecar/guard/
// max-concurrent and execution profile settings for the user. Called after the coordinator
// is created.
func displayResolvedConfig(session *team.TeamSession, resolvedModelList []config.ModelEntry, resolvedSidecarModel, resolvedGuardModel, resolvedJudgeModel, resolvedPlanReviewerModel string, resolvedMaxConcurrent int, execProfile team.ExecutionProfile) {
	if execProfile.Name != "" {
		stderrLog("%s %s (v%d)\n", boldStyle.Render("Profile:"), execProfile.Name, execProfile.SchemaVersion)
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
	if resolvedJudgeModel != "" {
		stderrLog("%s %s\n", boldStyle.Render("Judge:"), resolvedJudgeModel)
	}
	if resolvedPlanReviewerModel != "" {
		stderrLog("%s %s\n", boldStyle.Render("Plan Reviewer:"), resolvedPlanReviewerModel)
	}
	if resolvedMaxConcurrent != 8 {
		stderrLog("%s %d\n", boldStyle.Render("Max concurrent:"), resolvedMaxConcurrent)
	}
}

// buildNotifier resolves the notify config and constructs a Notifier
// if any channel is enabled. Wires up the "needs_human" hook so
// unattended runs can surface ask_user events to operators.
func buildNotifier(cfg *config.Config, session *team.TeamSession) *notify.Notifier {
	resolvedNotify := cfg.ResolveNotify(session.Config.Notify)
	if !resolvedNotify.Enabled() {
		return nil
	}
	notifierInst := notify.NewNotifier(resolvedNotify, os.Stderr)
	if resolvedNotify.OSC {
		stderrLog("%s %s\n", dimStyle.Render("◆"), "Notify: OSC enabled")
	}
	if resolvedNotify.Command != "" {
		stderrLog("%s %s %s\n", dimStyle.Render("◆"), "Notify:", resolvedNotify.Command)
	}
	tools.SetOnNeedsHuman(func(question string) {
		notifierInst.Notify("needs_human", "", question, "")
	})
	return notifierInst
}

// archiveToMemory stores old session entries in canonical long-term context.
// memStore remains in the signature temporarily for caller compatibility; it
// is deliberately never used as a runtime source of truth.
func archiveToMemory(ctx context.Context, memStore *memory.MemoryStore, coordinator *team.Coordinator, session *team.TeamSession, oldSessionEntries []memory.SessionSummaryEntry) {
	if len(oldSessionEntries) == 0 {
		return
	}
	if coordinator != nil && coordinator.ExecutionProfile().DisableHistoricalMemory {
		return
	}
	if coordinator != nil {
		if handled, err := coordinator.ArchiveSessionSummary(ctx, oldSessionEntries); handled {
			if err != nil {
				stderrLog("%s Failed to archive session to canonical context: %v\n", errStyle.Render("⚠"), err)
			}
			return
		}
	}
	_ = memStore
	_ = session
	stderrLog("%s Session archive skipped: canonical context is not configured.\n", errStyle.Render("⚠"))
}
