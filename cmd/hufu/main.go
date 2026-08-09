package main

import (
	"errors"
	"os"
	"sync/atomic"

	"github.com/kjelly/hufu/internal/readline"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
)

// globalPromptReader is the shared readline reader; stored atomically so the
// SIGINT handler and tool callbacks can access it from other goroutines.
var globalPromptReader atomic.Pointer[readline.PromptReader]

type errInterrupted struct{}

func (errInterrupted) Error() string { return "interrupted" }

var version = "dev"

func main() {
	// Pin the interactivity decision before any prompt widget can take over
	// stdin; a live per-call probe would flip tool permissions mid-session.
	tools.CaptureInteractiveEnvironment()
	exitCode := 0
	if err := newRootCommand().Execute(); err != nil {
		var outcomeErr interface{ ProcessExitCode() int }
		var interrupted errInterrupted
		if errors.As(err, &outcomeErr) {
			exitCode = outcomeErr.ProcessExitCode()
		} else if errors.Is(err, interrupted) {
			exitCode = 130
		} else if errors.Is(err, team.ErrTasksUnresolved) {
			exitCode = 7
		} else {
			exitCode = 1
		}
	}
	if pr := globalPromptReader.Load(); pr != nil {
		_ = pr.Close()
	}
	os.Exit(exitCode)
}
