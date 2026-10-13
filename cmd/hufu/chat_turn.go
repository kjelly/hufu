package main

import (
	"context"
	"errors"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

// chatTurnCoordinator is the narrow coordinator surface one chat turn needs.
type chatTurnCoordinator interface {
	Run(ctx context.Context, prompt string) (string, error)
	ContinueWithPrompt(ctx context.Context, prompt string) (string, error)
	SetInvocationID(invocationID string)
	LastRunResult() *team.RunResult
}

// chatTurnDeps binds one chat turn to its team. Every chat front end (plain
// REPL and the chat TUI) builds it with newChatTurnDeps so prompt preparation,
// delivery classification, and persistence stay identical across them.
type chatTurnDeps struct {
	coordinator  chatTurnCoordinator
	prepare      func(line string) string
	deliver      func(answer string, err error) (string, error)
	snapshotRuns func() map[string]int
	collectRuns  func(prior map[string]int) []cliRunIdentity
	persist      func(line string)
}

func newChatTurnDeps(tc *teamContext, teamName string) chatTurnDeps {
	loaded := func() map[string]*teamContext { return map[string]*teamContext{teamName: tc} }
	return chatTurnDeps{
		coordinator: tc.coordinator,
		prepare: func(line string) string {
			prompt, _ := injectFileContexts(line)
			if opts.projectContext || config.LoadConfig().ProjectContext || tc.session.Config.ProjectContext {
				prompt = injectProjectContext(prompt)
			}
			return prompt
		},
		deliver: func(answer string, err error) (string, error) {
			return presentExploratoryDelivery(tc, answer, err)
		},
		snapshotRuns: func() map[string]int { return snapshotExecutionRunCounts(loaded()) },
		collectRuns: func(prior map[string]int) []cliRunIdentity {
			return collectExecutionRunIdentities(loaded(), prior)
		},
		persist: func(line string) {
			go savePromptToHistory(context.Background(), line, opts.providerURL)
			// Persist after every turn so the session survives a crash mid-chat.
			_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
			_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
		},
	}
}

// chatTurnSequencer owns the Run/ContinueWithPrompt choice. The first turn in
// a process calls Run; later turns continue the conversation. A cancelled
// turn still established coordinator history, so it counts as a turn; a turn
// that failed for any other reason does not.
type chatTurnSequencer struct{ turns int }

func (s *chatTurnSequencer) first() bool { return s.turns == 0 }

func (s *chatTurnSequencer) record(outcome chatTurnOutcome) {
	if outcome.Err == nil || outcome.Cancelled {
		s.turns++
	}
}

func (s *chatTurnSequencer) reset() { s.turns = 0 }

// chatTurnOutcome is everything a front end needs to present one turn.
type chatTurnOutcome struct {
	Answer    string
	Err       error
	Cancelled bool
	// Result is the RunResult produced by this turn, or nil when the
	// coordinator did not publish a new one (a stale result is never reused).
	Result        *team.RunResult
	RunIdentities []cliRunIdentity
}

// runChatTurn executes one conversational turn. It never writes to stdout or
// stderr itself; presentation stays with the caller.
func runChatTurn(ctx context.Context, deps chatTurnDeps, seq *chatTurnSequencer, invocationID, line string) chatTurnOutcome {
	prompt := deps.prepare(line)
	opts.invocationID = invocationID
	deps.coordinator.SetInvocationID(invocationID)
	priorRuns := deps.snapshotRuns()
	previous := deps.coordinator.LastRunResult()

	var answer string
	var err error
	if seq.first() {
		answer, err = deps.coordinator.Run(ctx, prompt)
	} else {
		answer, err = deps.coordinator.ContinueWithPrompt(ctx, prompt)
	}
	answer, err = deps.deliver(answer, err)

	outcome := chatTurnOutcome{
		Answer:        answer,
		Err:           err,
		Cancelled:     err != nil && errors.Is(ctx.Err(), context.Canceled),
		RunIdentities: deps.collectRuns(priorRuns),
	}
	if current := deps.coordinator.LastRunResult(); current != nil && current != previous {
		outcome.Result = current
	}
	if err == nil {
		deps.persist(line)
	}
	seq.record(outcome)
	return outcome
}
