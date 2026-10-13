package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	ergoreadline "github.com/ergochat/readline"

	"github.com/kjelly/hufu/internal/team"
)

// runPlain is the line-oriented REPL: one turn per prompt, each turn
// cancellable with Ctrl+C without leaving the REPL.
func (s *chatSession) runPlain() error {
	fmt.Fprintf(os.Stderr, "\n%s Chatting with team %s. Type %s to leave, %s to clear context.\n\n",
		boldStyle.Render("💬"), teamStyle.Render(s.teamName),
		dimStyle.Render("/exit"), dimStyle.Render("/reset"))
	var turns chatTurnSequencer
	for {
		line, err := readChatLine(s.pr)
		if err != nil {
			if err == io.EOF || err == ergoreadline.ErrInterrupt {
				fmt.Fprintln(os.Stderr)
				return nil
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		handled, exit := s.runPlainCommand(line, &turns)
		if exit {
			return nil
		}
		if !handled {
			s.runPlainTurn(line, &turns)
		}
	}
}

// runPlainCommand executes a REPL command. It reports whether line was a
// command and whether the REPL should exit.
func (s *chatSession) runPlainCommand(line string, turns *chatTurnSequencer) (handled, exit bool) {
	command, commandArg := splitChatCommand(line)
	switch command {
	case "/exit", "/quit", ":q", "exit", "quit":
		return true, true
	case "/reset":
		s.tc.coordinator.ResetConversation()
		turns.reset()
		fmt.Fprintf(os.Stderr, "%s Conversation context cleared.\n\n", dimStyle.Render("·"))
	case "/help", "help", "?":
		fmt.Fprint(os.Stderr, chatHelpText(s.tc.teamName))
	case "/team":
		s.switchPlainTeam(commandArg, turns)
	case "/agents":
		printChatAgents(s.tc)
	case "/skills":
		printChatSkills(s.tc)
	case "/config":
		fmt.Fprintf(os.Stderr, "Team: %s\nWorkspace: %s\nModel: %s\n\n", s.tc.session.Config.Name, s.tc.session.Workspace, s.tc.session.Config.Generation.Model)
	case "/save":
		if commandArg == "" {
			fmt.Fprintln(os.Stderr, "usage: /save <path>")
			break
		}
		if err := saveChatTranscript(s.tc, commandArg); err != nil {
			fmt.Fprintf(os.Stderr, "%s %v\n", errStyle.Render("✗"), err)
			break
		}
		fmt.Fprintf(os.Stderr, "%s Saved conversation to %s\n\n", doneStyle.Render("✓"), commandArg)
	default:
		return false, false
	}
	return true, false
}

func (s *chatSession) switchPlainTeam(name string, turns *chatTurnSequencer) {
	var newTC *teamContext
	var err error
	if name != "" {
		newTC, err = switchChatTeamByName(s.rootCtx, name, s.registry)
	} else {
		newTC, _, err = switchChatTeam(s.rootCtx, s.pr, s.tc, s.registry)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", errStyle.Render("✗"), err)
		return
	}
	if newTC == nil {
		return
	}
	oldTC := s.tc
	s.tc, s.teamName = newTC, newTC.teamName
	turns.reset()
	if err := oldTC.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "%s Failed to close previous team: %v\n", errStyle.Render("⚠"), err)
	}
	fmt.Fprintf(os.Stderr, "%s Switched to team %s. Conversation context cleared.\n\n", doneStyle.Render("✓"), teamStyle.Render(s.teamName))
}

func (s *chatSession) runPlainTurn(line string, turns *chatTurnSequencer) {
	turnInvocationID := team.NewInvocationID()
	stopIdentityHeartbeat := startInvocationIdentityHeartbeat(os.Stderr, turnInvocationID, invocationIdentityHeartbeatInterval)
	turnCtx, cancel := signal.NotifyContext(s.rootCtx, os.Interrupt)
	disp := newCoordDisplay(s.tc)
	outcome := runChatTurn(turnCtx, newChatTurnDeps(s.tc, s.teamName), turns, turnInvocationID, line)
	disp.stopTimer()
	cancel()
	stopIdentityHeartbeat()

	switch {
	case outcome.Cancelled:
		fmt.Fprintf(os.Stderr, "\n%s Turn cancelled.\n\n", errStyle.Render("⚠"))
	case outcome.Err != nil:
		fmt.Fprintf(os.Stderr, "%s Error executing: %v\n\n", errStyle.Render("✗"), outcome.Err)
	default:
		disp.finalizeTasks()
		fmt.Println(outcome.Answer)
	}
	emitExecutionIdentity(os.Stderr, turnInvocationID, outcome.RunIdentities)
	if outcome.Err == nil {
		fmt.Fprintln(os.Stderr)
	}
}

// saveChatTranscript exports the session conversation as Markdown; the
// plain REPL and the chat TUI share it. The chat TUI may save while a turn
// is running, so it reads the coordinator's locked snapshot instead of the
// session the turn keeps writing.
func saveChatTranscript(tc *teamContext, path string) error {
	data := tc.coordinator.SessionData()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(team.GenerateSessionMD(data, tc.session.Config.Name)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
