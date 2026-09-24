package team

import (
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// maxConsecutiveCoordinatorToolErrors bounds how many coordinator tool error
// responses in a row the model may try to correct before the run stops. A
// successful coordinator tool call resets the streak.
const maxConsecutiveCoordinatorToolErrors = 3

// errCoordinatorFatal marks a coordinator tool failure the model cannot repair
// by issuing a different call: a durable journal, admission, or scheduler
// failure, or a runtime stop (no-progress, terminal unresolved worker outcome)
// that forbids another coordinator turn. A tool reports it as a Go error so
// the policy gate ends the stream.
var errCoordinatorFatal = errors.New("coordinator fatal tool error")

type coordinatorFatalError struct{ err error }

func (e *coordinatorFatalError) Error() string   { return e.err.Error() }
func (e *coordinatorFatalError) Unwrap() []error { return []error{e.err, errCoordinatorFatal} }

// markCoordinatorFatal wraps err so errors.Is(err, errCoordinatorFatal) holds
// while every sentinel err already carries stays reachable.
func markCoordinatorFatal(err error) error {
	if err == nil || errors.Is(err, errCoordinatorFatal) {
		return err
	}
	return &coordinatorFatalError{err: err}
}

// coordinatorToolFailureResult decides what a failed coordinator tool call
// returns to the stream. An error response is feedback the model can act on
// (the tools phrase their errors as instructions: acknowledge failed tasks,
// fix a verify command, call finish), so it is returned to the model, bounded
// by maxConsecutiveCoordinatorToolErrors. A Go error is a hard boundary and
// becomes errCoordinatorToolFailure.
func (c *Coordinator) coordinatorToolFailureResult(name, input string, response fantasy.ToolResponse, err error) (fantasy.ToolResponse, error) {
	if err == nil && name == "agent" && c != nil && c.coordinatorPolicyRepairPending.Load() {
		// The policy repair prompt carries its own bounded budget.
		return response, nil
	}
	if isReadOnlyToolCall(name, input) {
		// A failed observation has no side effect and its error is useful
		// evidence to the coordinator (for example, view was given a
		// directory and should be followed by ls).
		if err != nil {
			return fantasy.NewTextErrorResponse(joinToolFailureDetail(response, err)), nil
		}
		return response, nil
	}
	if err != nil {
		return fantasy.ToolResponse{}, newCoordinatorToolFailure(name, joinToolFailureDetail(response, err), err)
	}
	if c == nil {
		return response, nil
	}
	if streak := c.coordinatorToolErrorStreak.Add(1); streak > maxConsecutiveCoordinatorToolErrors {
		return fantasy.ToolResponse{}, fmt.Errorf("%w: tool %q failed after %d consecutive coordinator tool errors: %s",
			errCoordinatorToolFailure, name, maxConsecutiveCoordinatorToolErrors, joinToolFailureDetail(response, nil))
	}
	return response, nil
}

// coordinatorToolFailure is the hard stream boundary for a coordinator tool.
// It matches errCoordinatorToolFailure and keeps the runtime sentinels the
// cause carried, because attemptWrapUpRecovery chooses an LLM-free
// finalization from them (an exhausted policy repair, a fatal runtime stop).
// Other causes, such as context cancellation, stay out of the chain so a tool
// failure is never reclassified as an operator cancellation.
type coordinatorToolFailure struct {
	message   string
	sentinels []error
}

var coordinatorToolFailureSentinels = []error{errCoordinatorPolicyRepairExhausted, errCoordinatorFatal}

func newCoordinatorToolFailure(name, detail string, cause error) error {
	failure := &coordinatorToolFailure{message: fmt.Sprintf("%s: tool %q failed: %s", errCoordinatorToolFailure, name, detail)}
	for _, sentinel := range coordinatorToolFailureSentinels {
		if errors.Is(cause, sentinel) {
			failure.sentinels = append(failure.sentinels, sentinel)
		}
	}
	return failure
}

func (e *coordinatorToolFailure) Error() string { return e.message }
func (e *coordinatorToolFailure) Unwrap() []error {
	return append([]error{errCoordinatorToolFailure}, e.sentinels...)
}

// noteCoordinatorToolSuccess resets the consecutive error streak.
func (c *Coordinator) noteCoordinatorToolSuccess() {
	if c != nil {
		c.coordinatorToolErrorStreak.Store(0)
	}
}

func joinToolFailureDetail(response fantasy.ToolResponse, err error) string {
	detail := strings.TrimSpace(response.Content)
	if err != nil {
		if detail != "" {
			detail += ": "
		}
		detail += err.Error()
	}
	return detail
}
