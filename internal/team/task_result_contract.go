package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// taskResultSubmissionContract is the worker-facing projection of a task's
// effective result assertions. It is deliberately derived from the bound
// TaskDef, rather than from agent prose, so prompt, schema, and commit-time
// validation have one source of truth.
type taskResultSubmissionContract struct {
	RequiredFields       []string
	TaskResultAssertions []TaskResultAssertion
	// SubmissionTaskResultAssertions excludes only runtime transcript fields
	// that are populated by the owning verbatim finalization phase. The full
	// assertion set remains in TaskResultAssertions for that late verifier.
	SubmissionTaskResultAssertions []TaskResultAssertion
	FilesReadMinItems              int
	AllowEvidence                  bool
	AllowArtifacts                 bool
}

func taskResultSubmissionContractForTask(task TaskDef) taskResultSubmissionContract {
	contract := taskResultSubmissionContract{AllowEvidence: true, AllowArtifacts: !task.Execution.ForbidArtifacts}
	spec := task.VerifySpec
	if spec == nil && strings.TrimSpace(task.Verify) != "" {
		spec = &VerificationSpec{Type: VerifyCommandExit, Mode: task.VerifyMode, Command: task.Verify}
	}
	if spec == nil {
		return contract
	}
	normalized := NormalizeVerificationSpec(*spec, task.Verify, task.VerifyMode)
	if normalized.Type != VerifyTaskResultAssert {
		return contract
	}
	for _, assertion := range normalized.TaskResultAssertions {
		contract.TaskResultAssertions = append(contract.TaskResultAssertions, assertion)
		if !taskUsesVerbatimTranscript(task) || !taskResultAssertionUsesTranscriptFinalization(assertion) {
			contract.SubmissionTaskResultAssertions = append(contract.SubmissionTaskResultAssertions, assertion)
		}
		if field := taskResultAssertionRootField(assertion.Pointer); field != "" {
			contract.RequiredFields = appendUniqueString(contract.RequiredFields, field)
		}
		if strings.TrimSpace(assertion.Pointer) == "/files_read" {
			switch assertion.Op {
			case "min_items":
				if min, ok := taskResultAssertionInt(assertion.Value); ok && min > contract.FilesReadMinItems {
					contract.FilesReadMinItems = min
				}
			case "exists", "non_empty":
				// The assertion itself is retained below for admission validation;
				// these operators also make files_read the task's evidence channel.
			}
		}
	}
	if len(contract.TaskResultAssertions) > 0 {
		contract.TaskResultAssertions = canonicalTaskResultAssertions(contract.TaskResultAssertions)
	}
	if len(contract.SubmissionTaskResultAssertions) > 0 {
		contract.SubmissionTaskResultAssertions = canonicalTaskResultAssertions(contract.SubmissionTaskResultAssertions)
	}
	if contract.FilesReadMinItems > 0 || taskResultContractRequiresFilesRead(contract.TaskResultAssertions) {
		contract.AllowEvidence = false
	}
	sort.Strings(contract.RequiredFields)
	return contract
}

func taskResultAssertionRootField(pointer string) string {
	pointer = strings.TrimSpace(pointer)
	if pointer == "" || pointer[0] != '/' {
		return ""
	}
	field := strings.Split(pointer[1:], "/")[0]
	field = strings.ReplaceAll(strings.ReplaceAll(field, "~1", "/"), "~0", "~")
	return field
}

func taskResultContractRequiresFilesRead(assertions []TaskResultAssertion) bool {
	for _, assertion := range assertions {
		if taskResultAssertionRootField(assertion.Pointer) == "files_read" {
			return true
		}
	}
	return false
}

func taskResultAssertionUsesTranscriptFinalization(assertion TaskResultAssertion) bool {
	switch taskResultAssertionRootField(assertion.Pointer) {
	case "raw_output_ref", "outputs":
		return true
	default:
		return false
	}
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// validateWorkerClaims checks only fields and task-specific legality that the
// worker is allowed to claim. Runtime-owned identity, defaults, artifact IDs,
// and transcript fields are intentionally not checked here.
func (contract taskResultSubmissionContract) validateWorkerClaims(result *TaskResult) error {
	if result == nil {
		return nil
	}
	if !contract.AllowArtifacts && len(result.Artifacts) > 0 {
		return fmt.Errorf("artifacts are forbidden by this task's execution contract; omit the artifacts field")
	}
	if !taskResultStatusIsSuccessful(result.Status) {
		return nil
	}
	if contract.FilesReadMinItems > 0 && len(result.FilesRead) < contract.FilesReadMinItems {
		return fmt.Errorf("successful result requires files_read with at least %d path %s", contract.FilesReadMinItems, pluralSuffix(contract.FilesReadMinItems, "entry", "entries"))
	}
	if contract.FilesReadMinItems > 0 {
		for i, ref := range result.FilesRead {
			if strings.TrimSpace(ref.Path) == "" {
				return fmt.Errorf("files_read[%d].path is required for a successful result", i)
			}
		}
	}
	if !contract.AllowEvidence && len(result.Evidence) > 0 {
		return fmt.Errorf("evidence is not a legal field for this task; report observed files in files_read")
	}
	return nil
}

// validateFinalizableResult evaluates assertions against the canonical result
// candidate after runtime hydration and artifact materialization. Verbatim
// transcript assertions remain for the owning late finalization verifier.
func (contract taskResultSubmissionContract) validateFinalizableResult(result *TaskResult) error {
	if result == nil || !taskResultStatusIsSuccessful(result.Status) || len(contract.SubmissionTaskResultAssertions) == 0 {
		return nil
	}
	document, err := canonicalTaskResultDocument(result)
	if err != nil {
		return fmt.Errorf("cannot canonicalize task result for admission validation: %w", err)
	}
	if failures := evaluateTaskResultAssertions(document, contract.SubmissionTaskResultAssertions); len(failures) > 0 {
		return fmt.Errorf("task_result_assert admission failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

// validateTranscriptFinalization evaluates only assertions whose values are
// supplied by the runner after the transcript is sealed. Worker submission
// validates the remaining assertions; keeping this check at the owning
// finalization boundary prevents a local, not-yet-persisted candidate from
// being the only value that satisfies the transcript contract.
func (contract taskResultSubmissionContract) validateTranscriptFinalization(result *TaskResult) error {
	if result == nil || !taskResultStatusIsSuccessful(result.Status) || len(contract.TaskResultAssertions) == 0 {
		return nil
	}
	transcriptAssertions := make([]TaskResultAssertion, 0, len(contract.TaskResultAssertions))
	for _, assertion := range contract.TaskResultAssertions {
		if taskResultAssertionUsesTranscriptFinalization(assertion) {
			transcriptAssertions = append(transcriptAssertions, assertion)
		}
	}
	if len(transcriptAssertions) == 0 {
		return nil
	}
	document, err := canonicalTaskResultDocument(result)
	if err != nil {
		return fmt.Errorf("cannot canonicalize finalized task result for transcript validation: %w", err)
	}
	if failures := evaluateTaskResultAssertions(document, transcriptAssertions); len(failures) > 0 {
		return fmt.Errorf("task_result_assert transcript finalization failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (contract taskResultSubmissionContract) validate(result *TaskResult) error {
	if err := contract.validateWorkerClaims(result); err != nil {
		return err
	}
	if result == nil || !taskResultStatusIsSuccessful(result.Status) || len(contract.TaskResultAssertions) == 0 {
		return nil
	}
	document, err := canonicalTaskResultDocument(result)
	if err != nil {
		return fmt.Errorf("cannot canonicalize task result for admission validation: %w", err)
	}
	if failures := evaluateTaskResultAssertions(document, contract.TaskResultAssertions); len(failures) > 0 {
		return fmt.Errorf("task_result_assert admission failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func canonicalTaskResultDocument(result *TaskResult) (any, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("canonical task result encoded more than one JSON document")
		}
		return nil, err
	}
	return document, nil
}

func pluralSuffix(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
