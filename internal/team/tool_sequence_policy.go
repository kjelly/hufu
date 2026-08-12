package team

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/utils"
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
	canonicalInputs   []bool
	inputTransforms   []string
	inputField        string
	inputValues       []string
	expectedExitCodes [][]int
	next              int
	failed            bool
	failure           *taskToolSequenceFailure
}

// taskToolSequenceFailure is runtime-owned evidence for the first failure in
// a closed sequence. It intentionally records only generic protocol facts;
// tool-specific output remains in the sealed transcript.
type taskToolSequenceFailure struct {
	Position int
	Length   int
	Tool     string
	ExitCode *int
}

func newTaskToolSequence(sequence []string, inputs []map[string]any, inputField string, inputValues []string, expectedExitCodes ...[][]int) *taskToolSequence {
	var codes [][]int
	if len(expectedExitCodes) > 0 {
		codes = expectedExitCodes[0]
	}
	return newTaskToolSequenceWithCanonicalInputs(sequence, inputs, inputField, inputValues, codes, nil)
}

func newTaskToolSequenceWithCanonicalInputs(sequence []string, inputs []map[string]any, inputField string, inputValues []string, expectedExitCodes [][]int, canonicalInputs []bool) *taskToolSequence {
	return newTaskToolSequenceWithBindings(sequence, inputs, inputField, inputValues, expectedExitCodes, canonicalInputs, nil)
}

func newTaskToolSequenceWithBindings(sequence []string, inputs []map[string]any, inputField string, inputValues []string, expectedExitCodes [][]int, canonicalInputs []bool, inputTransforms []string) *taskToolSequence {
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
		copyExpectedExitCodes = make([][]int, len(expectedExitCodes))
		for index, codes := range expectedExitCodes {
			copyExpectedExitCodes[index] = append([]int(nil), codes...)
		}
	}
	return &taskToolSequence{
		sequence:          copyOf,
		inputs:            copyInputs,
		canonicalInputs:   append([]bool(nil), canonicalInputs...),
		inputTransforms:   append([]string(nil), inputTransforms...),
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
func (s *taskToolSequence) reserve(tool string, input string, allowEarlyTerminal bool) (int, string, bool, string) {
	if s == nil {
		return -1, input, false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		if tool == "submit_result" && allowEarlyTerminal {
			s.next = len(s.sequence)
			return -1, input, false, ""
		}
		return -1, input, false, "closed tool sequence after a failed tool result; submit a failed or blocked result"
	}
	if s.next >= len(s.sequence) {
		return -1, input, false, "closed tool sequence is complete; do not call another tool"
	}
	expected := s.sequence[s.next]
	if tool != expected {
		if tool == "submit_result" && allowEarlyTerminal {
			s.next = len(s.sequence)
			return -1, input, false, ""
		}
		return -1, input, false, fmt.Sprintf("closed tool sequence violation: expected tool %q at position %d of %d, got %q; do not call it", expected, s.next+1, len(s.sequence), tool)
	}
	if expectedInput := s.expectedInput(s.next); expectedInput != nil {
		if s.canonicalInput(s.next) {
			canonical, err := json.Marshal(expectedInput)
			if err != nil {
				return -1, input, false, inputViolationMessage(s.next, len(s.sequence), expectedInput, input)
			}
			reserved := s.next
			s.next++
			return reserved, string(canonical), !matchesExactJSON(string(canonical), input), ""
		}
		if !matchesJSONFields(expectedInput, input) {
			return -1, input, false, inputViolationMessage(s.next, len(s.sequence), expectedInput, input)
		}
	}
	if s.inputField != "" && s.next < len(s.inputValues) && s.inputValues[s.next] != "" && !matchesJSONField(s.inputField, s.inputValues[s.next], input) {
		return -1, input, false, inputViolationMessage(s.next, len(s.sequence), map[string]any{s.inputField: s.inputValues[s.next]}, input)
	}
	reserved := s.next
	s.next++
	return reserved, input, false, ""
}

func (s *taskToolSequence) canonicalInput(index int) bool {
	return index >= 0 && index < len(s.canonicalInputs) && s.canonicalInputs[index]
}

func (s *taskToolSequence) inputTransform(index int) string {
	if index < 0 || index >= len(s.inputTransforms) {
		return ""
	}
	return s.inputTransforms[index]
}

func matchesExactJSON(expected, actual string) bool {
	var value any
	if json.Unmarshal([]byte(actual), &value) != nil {
		return false
	}
	canonical, err := json.Marshal(value)
	return err == nil && string(canonical) == expected
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

// inputViolationMessage reports which pinned fields did not match, so a
// coordinator or worker can correct its next call within the same attempt
// instead of guessing blindly at what "input violation" meant. Before this,
// the message named only the sequence position, so a worker whose task goal
// disagreed with a coordinator-authored scalar pin (e.g. it was told to
// `mkdir` first while the contract pinned a `date` command at slot 1) had no
// way to tell field mismatch from tool mismatch from a typo, and the only
// recovery path was an early-terminal submit_result that discarded the whole
// attempt. Field-level detail lets it retry the very next call correctly.
func inputViolationMessage(position, length int, expected map[string]any, actual string) string {
	base := fmt.Sprintf("closed tool sequence input violation at position %d of %d; do not call it", position+1, length)
	if detail := fieldMismatchDetail(expected, actual); detail != "" {
		return base + " (" + detail + ")"
	}
	return base
}

// fieldMismatchDetail summarizes exactly which pinned fields disagree with
// the actual tool call, rendering every value through the same redaction
// pipeline as the rest of the runtime (redactedFieldValue) so a
// credential-shaped field never appears in a tool-visible error message. It
// returns "" when actual is not a JSON object, leaving the caller's generic
// message as the only diagnostic.
func fieldMismatchDetail(expected map[string]any, actual string) string {
	var actualValue map[string]any
	if json.Unmarshal([]byte(actual), &actualValue) != nil {
		return ""
	}
	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		expectedField := expected[key]
		actualField, present := actualValue[key]
		if present && matchesJSONValue(expectedField, actualField) {
			continue
		}
		actualPreview := "<missing>"
		if present {
			actualPreview = redactedFieldValue(key, actualField)
		}
		parts = append(parts, fmt.Sprintf("field %q expected %s, got %s", key, redactedFieldValue(key, expectedField), actualPreview))
	}
	return strings.Join(parts, "; ")
}

// redactedFieldValue renders one field's value as it would appear in the
// diagnostic message, routed through utils.RedactJSON first. Reusing that
// pipeline (rather than a bespoke check here) means a field named
// `password`/`token`/etc. is masked by the same key-name rule already
// trusted elsewhere in the runtime, and a value that itself looks like a
// previously learned credential is masked wherever it appears, not only
// under a secret-shaped key.
func redactedFieldValue(field string, value any) string {
	data, err := json.Marshal(map[string]any{field: value})
	if err != nil {
		return "<unrepresentable>"
	}
	redacted, err := utils.RedactJSON(data)
	if err != nil {
		return "<unrepresentable>"
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		return "<unrepresentable>"
	}
	raw, ok := decoded[field]
	if !ok {
		return "<missing>"
	}
	return string(raw)
}

// markFailed closes the evidence sequence after a tool reports an execution
// error. A closed task must not improvise a repair with a later slot: the only
// admissible next action is an honest early terminal submit_result. This is
// generic for every closed sequence, so failed commands cannot silently turn
// an atomic checkpoint into an unplanned retry workflow.
func (s *taskToolSequence) markFailedAt(slot int, tool, output string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = true
	if s.failure != nil {
		return
	}
	position := slot + 1
	if slot < 0 {
		position = s.next + 1
	}
	if position < 1 {
		position = 1
	}
	if position > len(s.sequence) {
		position = len(s.sequence)
	}
	var exitCode *int
	if code, ok := transcriptExitCode(tool, output); ok {
		copied := code
		exitCode = &copied
	}
	s.failure = &taskToolSequenceFailure{
		Position: position,
		Length:   len(s.sequence),
		Tool:     strings.TrimSpace(tool),
		ExitCode: exitCode,
	}
}

func (s *taskToolSequence) failureSummary() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		return ""
	}
	fact := fmt.Sprintf("closed sequence failed at position %d of %d", s.failure.Position, s.failure.Length)
	if s.failure.Tool != "" {
		fact += fmt.Sprintf(" (tool %q", s.failure.Tool)
		if s.failure.ExitCode != nil {
			fact += fmt.Sprintf(", exit code %d", *s.failure.ExitCode)
		}
		fact += ")"
	} else if s.failure.ExitCode != nil {
		fact += fmt.Sprintf(" (exit code %d)", *s.failure.ExitCode)
	}
	return fact
}

// protocolRepairSequence returns the one-call sequence for result
// finalization. A success claim is allowed only when every non-terminal slot
// in the original sequence completed, or when the only failure was the
// terminal submit_result call itself (for example a schema error). An earlier
// execution failure, a denied out-of-order call, or an unfinished work slot
// carries into repair as failed state, so repair can report only an honest
// failed/blocked/partial result.
func (s *taskToolSequence) protocolRepairSequence() *taskToolSequence {
	repair := newTaskToolSequence([]string{"submit_result"}, nil, "", nil)
	if s == nil {
		return repair
	}

	s.mu.Lock()
	next := s.next
	length := len(s.sequence)
	var failure *taskToolSequenceFailure
	if s.failure != nil {
		copyFailure := *s.failure
		failure = &copyFailure
	}
	s.mu.Unlock()

	readyForTerminal := next >= length-1
	terminalResultFailure := failure != nil && failure.Position == failure.Length && failure.Tool == "submit_result"
	if !readyForTerminal || (failure != nil && !terminalResultFailure) {
		tool := "closed_sequence"
		if failure != nil && failure.Tool != "" {
			tool = failure.Tool
		}
		repair.markFailedAt(0, tool, "")
	}
	return repair
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
