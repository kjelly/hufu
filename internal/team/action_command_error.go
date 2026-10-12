package team

import "fmt"

// ActionCommandError retains process evidence independently of adapter logs.
// Failure classification must never interpret successful stderr banners.
type ActionCommandError struct {
	Cause      error
	ExitCode   *int
	Class      TaskFailureClass
	Diagnostic string
}

func (e *ActionCommandError) Error() string {
	if e.ExitCode != nil {
		return fmt.Sprintf("adapter command failed (exit=%d): %s", *e.ExitCode, e.Diagnostic)
	}
	return fmt.Sprintf("adapter command failed: %s", e.Diagnostic)
}

func (e *ActionCommandError) Unwrap() error { return e.Cause }

func (e *ActionCommandError) FailureClassOverride() TaskFailureClass { return e.Class }

// actionCommandFailure is the optional, strict stdout failure envelope for
// command adapters. It is accepted only after a nonzero process exit.
type actionCommandFailure struct {
	Error struct {
		Class   TaskFailureClass `json:"failure_class"`
		Message string           `json:"message"`
	} `json:"error"`
}
