package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"
)

// taskToolSequenceKey carries one attempt-local closed tool sequence. It is
// deliberately task-scoped rather than agent-scoped: the same worker may run
// different atomic checkpoints in one team.
type taskToolSequenceKey struct{}

// taskToolSequence reserves each admitted call before its tool runs. Counting
// the invocation, rather than only successful tool responses, prevents a
// failed command from silently creating extra budget for retries or probes.
type taskToolSequence struct {
	mu                sync.Mutex
	sequence          []string
	inputs            []map[string]any
	inputField        string
	inputValues       []string
	expectedExitCodes [][]int
	next              int
	failed            bool
}

func newTaskToolSequence(sequence []string, inputs []map[string]any, inputField string, inputValues []string, expectedExitCodes ...[][]int) *taskToolSequence {
	if len(sequence) == 0 {
		return nil
	}
	copyOf := make([]string, len(sequence))
	for i, name := range sequence {
		copyOf[i] = strings.TrimSpace(name)
	}
	copyInputs := make([]map[string]any, len(inputs))
	copy(copyInputs, inputs)
	var copyExpectedExitCodes [][]int
	if len(expectedExitCodes) > 0 {
		copyExpectedExitCodes = make([][]int, len(expectedExitCodes[0]))
		for index, codes := range expectedExitCodes[0] {
			copyExpectedExitCodes[index] = append([]int(nil), codes...)
		}
	}
	return &taskToolSequence{
		sequence:          copyOf,
		inputs:            copyInputs,
		inputField:        inputField,
		inputValues:       append([]string(nil), inputValues...),
		expectedExitCodes: copyExpectedExitCodes,
	}
}

// reserve admits exactly the next configured tool. A mismatch never reaches
// the underlying tool, so a worker cannot spend post-checkpoint budget on an
// exploratory tool or delegate after the terminal action has been satisfied.
//
// One exception: an out-of-position submit_result call reporting a genuine
// early-terminal status (blocked/failed/partial — never success or
// completed_with_gaps, see earlyTerminalSubmitResult) is admitted anyway and
// closes the sequence immediately. A closed sequence exists to force
// complete evidence-gathering before a *success* claim; it must not also
// force a worker that has already discovered mid-checkpoint that it cannot
// proceed (e.g. a prerequisite file the sequence has no step to create) to
// choose between fabricating the remaining steps and having its honest
// submit_result rejected as a protocol violation. Before this exception
// existed, that rejection surfaced as "protocol incomplete: missing
// required result" even though the worker had tried to call submit_result —
// the gate, not the worker, discarded it — which then misled the
// downstream repair path into treating an honest early bail-out as an
// omitted result. allowEarlyTerminal carries the caller's judgment of
// whether this particular submit_result call qualifies; reserve only
// applies it at the mismatch branch below, never overriding a tool that is
// already in its correct sequence position.
func (s *taskToolSequence) reserve(tool string, input string, allowEarlyTerminal bool) (int, string) {
	if s == nil {
		return -1, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		if tool == "submit_result" && allowEarlyTerminal {
			s.next = len(s.sequence)
			return -1, ""
		}
		return -1, "closed tool sequence after a failed tool result; submit a failed or blocked result"
	}
	if s.next >= len(s.sequence) {
		return -1, "closed tool sequence is complete; do not call another tool"
	}
	expected := s.sequence[s.next]
	if tool != expected {
		if tool == "submit_result" && allowEarlyTerminal {
			s.next = len(s.sequence)
			return -1, ""
		}
		return -1, fmt.Sprintf("closed tool sequence violation: expected tool %q at position %d of %d, got %q; do not call it", expected, s.next+1, len(s.sequence), tool)
	}
	if expectedInput := s.expectedInput(s.next); expectedInput != nil && !matchesJSONFields(expectedInput, input) {
		return -1, fmt.Sprintf("closed tool sequence input violation at position %d of %d; do not call it", s.next+1, len(s.sequence))
	}
	if s.inputField != "" && s.next < len(s.inputValues) && s.inputValues[s.next] != "" && !matchesJSONField(s.inputField, s.inputValues[s.next], input) {
		return -1, fmt.Sprintf("closed tool sequence input violation at position %d of %d; do not call it", s.next+1, len(s.sequence))
	}
	reserved := s.next
	s.next++
	return reserved, ""
}

// allowsExpectedExitCode reports whether a tool error is an explicitly
// declared observation for the reserved sequence slot. The exit code is read
// from the normalized tool transcript, so an unparseable error never weakens
// the closed-sequence failure policy.
func (s *taskToolSequence) allowsExpectedExitCode(slot int, tool, output string) bool {
	if s == nil || slot < 0 {
		return false
	}
	code, ok := transcriptExitCode(tool, output)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot >= len(s.expectedExitCodes) {
		return false
	}
	for _, expected := range s.expectedExitCodes[slot] {
		if code == expected {
			return true
		}
	}
	return false
}

func matchesJSONField(field, expected, actual string) bool {
	var actualValue map[string]any
	if json.Unmarshal([]byte(actual), &actualValue) != nil {
		return false
	}
	value, ok := actualValue[field]
	return ok && matchesJSONValue(expected, value)
}

func (s *taskToolSequence) expectedInput(index int) map[string]any {
	if index >= len(s.inputs) || len(s.inputs[index]) == 0 {
		return nil
	}
	return s.inputs[index]
}

func matchesJSONFields(expected map[string]any, actual string) bool {
	var actualValue map[string]any
	if json.Unmarshal([]byte(actual), &actualValue) != nil {
		return false
	}
	for key, expectedValue := range expected {
		actualField, ok := actualValue[key]
		if !ok || !matchesJSONValue(expectedValue, actualField) {
			return false
		}
	}
	return true
}

func matchesJSONValue(expected, actual any) bool {
	expectedObject, expectedIsObject := expected.(map[string]any)
	if !expectedIsObject {
		expectedCanonical, expectedErr := json.Marshal(expected)
		actualCanonical, actualErr := json.Marshal(actual)
		return expectedErr == nil && actualErr == nil && string(expectedCanonical) == string(actualCanonical)
	}
	actualObject, actualIsObject := actual.(map[string]any)
	if !actualIsObject {
		return false
	}
	for key, expectedField := range expectedObject {
		actualField, ok := actualObject[key]
		if !ok || !matchesJSONValue(expectedField, actualField) {
			return false
		}
	}
	return true
}

// markFailed closes the evidence sequence after a tool reports an execution
// error. A closed task must not improvise a repair with a later slot: the only
// admissible next action is an honest early terminal submit_result. This is
// generic for every closed sequence, so failed commands cannot silently turn
// an atomic checkpoint into an unplanned retry workflow.
func (s *taskToolSequence) markFailed() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
}

func taskToolSequenceFromContext(ctx context.Context) *taskToolSequence {
	sequence, _ := ctx.Value(taskToolSequenceKey{}).(*taskToolSequence)
	return sequence
}

// filterToolsForSequence keeps only tools that can appear in the closed
// sequence. The gate remains the authoritative enforcement layer, while this
// removes unrelated convenience tools from the model's visible choices.
func filterToolsForSequence(agentTools []fantasy.AgentTool, sequence []string) []fantasy.AgentTool {
	if len(sequence) == 0 {
		return agentTools
	}
	allowed := make(map[string]bool, len(sequence))
	for _, name := range sequence {
		allowed[strings.TrimSpace(name)] = true
	}
	filtered := make([]fantasy.AgentTool, 0, len(agentTools))
	for _, tool := range agentTools {
		if tool != nil && allowed[strings.TrimSpace(tool.Info().Name)] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}
