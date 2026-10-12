package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"

	"github.com/charmbracelet/x/term"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/readline"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

// chatUIMode selects the chat front end.
type chatUIMode string

const (
	chatUIPlain     chatUIMode = "plain"
	chatUIDashboard chatUIMode = "dashboard"
	chatUIChat      chatUIMode = "chat"
)

// resolveChatUIMode validates --ui and its legacy alias --tui
// (= --ui dashboard). Conflicting values are an error, never a silent pick.
func resolveChatUIMode(value string, uiChanged, tuiFlag bool) (chatUIMode, error) {
	mode := chatUIMode(value)
	switch mode {
	case chatUIPlain, chatUIDashboard, chatUIChat:
	default:
		return "", fmt.Errorf("invalid --ui %q: want plain, dashboard, or chat", value)
	}
	if tuiFlag {
		if uiChanged && mode != chatUIDashboard {
			return "", fmt.Errorf("--tui conflicts with --ui %s", mode)
		}
		return chatUIDashboard, nil
	}
	return mode, nil
}

// selectChatUIMode resolves the chat front end from the flags and refuses
// the chat TUI without an interactive terminal; it never falls back.
func selectChatUIMode(uiChanged bool) (chatUIMode, error) {
	mode, err := resolveChatUIMode(opts.chatUI, uiChanged, opts.tuiMode)
	if err != nil {
		return "", err
	}
	if mode == chatUIChat && !chatUITerminal() {
		return "", errors.New("--ui chat requires an interactive terminal; use --ui plain")
	}
	return mode, nil
}

// chatUITerminal reports whether stdin and stdout are both terminals.
var chatUITerminal = func() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// chatSession is the state every chat front end shares: one loaded team
// (replaceable by /team), its registry, and the readline prompt.
type chatSession struct {
	rootCtx     context.Context
	pr          *readline.PromptReader
	tc          *teamContext
	teamName    string
	registry    *team.TeamRegistry
	pathConsent *tools.PathConsent
	vars        map[string]string
}

func openChatSession() (*chatSession, error) {
	s := &chatSession{rootCtx: context.Background(), pathConsent: newPathConsent()}
	pr, err := readline.NewPromptReader(defaultHistoryPath())
	if err == nil {
		s.pr = pr
	}
	globalPromptReader.Store(s.pr)
	if s.vars, err = buildVars(); err != nil {
		return nil, errors.Join(err, s.closePrompt())
	}
	if err := s.loadInitialTeam(); err != nil {
		return nil, errors.Join(err, s.closePrompt())
	}
	if err := configureRunInputAssignments(map[string]*teamContext{s.teamName: s.tc}); err != nil {
		return nil, errors.Join(err, s.close())
	}
	s.installCompleter()
	return s, nil
}

func (s *chatSession) loadInitialTeam() error {
	var err error
	if opts.defaultTeam {
		s.teamName = "default"
		s.tc, err = loadDefaultTeam(s.rootCtx, opts.providerURL, opts.providerAPIKey, s.pathConsent, s.vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	} else {
		s.teamName, s.registry, err = pickChatTeam(s.pr)
		if err != nil {
			return err
		}
		s.tc, err = loadTeamByName(s.rootCtx, s.teamName, s.registry, opts.providerURL, opts.providerAPIKey, s.pathConsent, s.vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	}
	if err != nil {
		return fmt.Errorf("failed to load team %q: %w", s.teamName, err)
	}
	return nil
}

// installCompleter recreates the prompt reader with a team-aware tab
// completer; recreating is cheaper than mutable completer state in the
// underlying readline library.
func (s *chatSession) installCompleter() {
	if s.pr == nil {
		return
	}
	_ = s.pr.Close()
	s.pr = nil
	var agentNames []string
	for name := range s.tc.session.Agents {
		agentNames = append(agentNames, name)
	}
	pr, err := readline.NewPromptReaderWithCompleter(defaultHistoryPath(), newChatCompleter(s.teamName, []string{s.teamName}, agentNames))
	if err == nil {
		s.pr = pr
	}
	globalPromptReader.Store(s.pr)
}

// close releases the current team (coordinator, MCP, lease) and then the
// prompt reader.
func (s *chatSession) close() error {
	return errors.Join(s.tc.Close(), s.closePrompt())
}

func (s *chatSession) closePrompt() error {
	if s.pr == nil {
		return nil
	}
	pr := s.pr
	s.pr = nil
	return pr.Close()
}

// buildChatTeamInfo describes the loaded team for the TUI front ends.
func buildChatTeamInfo(tc *teamContext, registry *team.TeamRegistry) tuipkg.TeamInfo {
	var info tuipkg.TeamInfo
	if registry != nil {
		info.AvailableTeams = registry.ListTeams()
	}
	info.TeamName = tc.session.Config.Name
	info.Workspace = tc.session.Workspace
	info.TeamDir = tc.session.Dir
	info.DefaultModel = tc.session.Config.Generation.Model
	for _, ag := range sortedAgents(tc.session.Agents) {
		model := ag.Generation.Model
		if model == "" {
			model = tc.session.Config.Generation.Model
		}
		info.Agents = append(info.Agents, tuipkg.AgentInfoEntry{Name: ag.Name, Role: ag.Role, Model: model})
	}
	for _, skill := range tc.session.Skills {
		info.Skills = append(info.Skills, skill.Name)
	}
	info.SidecarModel = tc.session.Config.SidecarModel
	info.GuardModel = tc.session.Config.GuardModel
	info.MemoryEnabled = opts.memoryEnabled && !opts.tempWorkspace
	if info.MemoryEnabled {
		info.MemoryModel = config.ResolveEmbeddingModel(opts.memoryModel)
	}
	info.PTYEnabled = opts.enablePTYTerminal
	info.HufuBinary, _ = os.Executable()
	info.Decisions, _ = tc.coordinator.DecisionIndexEntries()
	info.Decisions = team.RedactedDecisionIndexEntries(info.Decisions)
	info.IsChat = true
	return info
}

// runDashboard runs the legacy task dashboard with prompt injection.
func (s *chatSession) runDashboard() error {
	opts.tuiMode = true
	opts.isChatTUI = true
	segments := []team.PromptSegment{{Type: team.SegmentSwitchTeam, Name: s.teamName}}
	loadedTeams := map[string]*teamContext{s.teamName: s.tc}
	turnCtx, cancel := signal.NotifyContext(s.rootCtx, os.Interrupt)
	defer cancel()
	_, err := runWithTUI(turnCtx, cancel, "", segments, s.registry, loadedTeams, newPromptInjector(nil),
		new(atomic.Pointer[team.Coordinator]), s.pathConsent, s.vars, buildChatTeamInfo(s.tc, s.registry),
		RouteDecision{Route: RouteTeam, Team: s.teamName})
	return err
}
