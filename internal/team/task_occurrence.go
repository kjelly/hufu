package team

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

var (
	errTaskResultStale     = errors.New("task result provenance does not match active runtime occurrence")
	errTaskResultDuplicate = errors.New("task result already submitted for active runtime occurrence")
)

// taskOccurrenceController is the coordinator-owned transaction boundary for
// one todo. Its mutex is held from reservation through commit or rollback, so
// opening a retry cannot interleave with a delayed submission.
type taskOccurrenceController struct {
	mu sync.Mutex

	identity submitResultRuntimeIdentity
	opened   bool
	reserved bool
	result   *TaskResult
	pending  []ArtifactRef
}

type taskResultOccurrenceTransaction struct {
	coordinator *Coordinator
	controller  *taskOccurrenceController
	identity    submitResultRuntimeIdentity
	finished    bool
}

func cloneTaskResult(result *TaskResult) *TaskResult {
	if result == nil {
		return nil
	}
	copyResult := *result
	copyResult.Artifacts = append([]ArtifactRef(nil), result.Artifacts...)
	copyResult.Evidence = append([]EvidenceRef(nil), result.Evidence...)
	copyResult.FilesRead = append([]FileRef(nil), result.FilesRead...)
	copyResult.FilesModified = append([]FileRef(nil), result.FilesModified...)
	copyResult.Commands = append([]CommandResult(nil), result.Commands...)
	if result.Verification != nil {
		copyResult.Verification = make([]VerificationResult, len(result.Verification))
		for i := range result.Verification {
			copyResult.Verification[i] = *cloneVerificationResult(&result.Verification[i])
		}
	}
	copyResult.Decisions = append([]Decision(nil), result.Decisions...)
	copyResult.Findings = append([]Finding(nil), result.Findings...)
	copyResult.Risks = append([]Risk(nil), result.Risks...)
	copyResult.OpenQuestions = append([]string(nil), result.OpenQuestions...)
	copyResult.SuggestedNextTasks = append([]TaskProposal(nil), result.SuggestedNextTasks...)
	copyResult.ReceiptIDs = append([]string(nil), result.ReceiptIDs...)
	copyResult.MemoryUses = append([]MemoryUseRef(nil), result.MemoryUses...)
	if result.RawOutputRef != nil {
		copyRef := *result.RawOutputRef
		copyResult.RawOutputRef = &copyRef
	}
	if result.Outputs != nil {
		copyResult.Outputs = make(map[string]StructuredOutputValue, len(result.Outputs))
		for key, value := range result.Outputs {
			copyValue := value
			if value.Artifact != nil {
				copyRef := *value.Artifact
				copyValue.Artifact = &copyRef
			}
			if value.Fact != nil {
				copyFact := *value.Fact
				copyFact.Value = cloneTaskResultValue(value.Fact.Value)
				copyValue.Fact = &copyFact
			}
			copyResult.Outputs[key] = copyValue
		}
	}
	if result.Facts != nil {
		copyResult.Facts = make(map[string]any, len(result.Facts))
		for key, value := range result.Facts {
			copyResult.Facts[key] = cloneTaskResultValue(value)
		}
	}
	return &copyResult
}

// cloneTaskResultValue detaches JSON-like values carried by Facts, structured
// facts, and verification assertions while preserving their concrete Go type.
// These values originate at JSON boundaries in normal execution, but keeping
// the clone generic also protects tests and internal callers that construct
// typed maps or slices directly.
func cloneTaskResultValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneTaskResultReflectValue(reflect.ValueOf(value)).Interface()
}

func cloneTaskResultReflectValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return reflect.Value{}
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneTaskResultReflectValue(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			result.SetMapIndex(iter.Key(), cloneTaskResultReflectValue(iter.Value()))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneTaskResultReflectValue(value.Index(i)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneTaskResultReflectValue(value.Index(i)))
		}
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(cloneTaskResultReflectValue(value.Elem()))
		return result
	default:
		return value
	}
}

func (c *Coordinator) occurrenceController(todoID string) *taskOccurrenceController {
	if c == nil || strings.TrimSpace(todoID) == "" {
		return nil
	}
	c.occurrenceControllersMu.Lock()
	defer c.occurrenceControllersMu.Unlock()
	if c.occurrenceControllers == nil {
		c.occurrenceControllers = make(map[string]*taskOccurrenceController)
	}
	controller := c.occurrenceControllers[todoID]
	if controller == nil {
		controller = &taskOccurrenceController{}
		c.occurrenceControllers[todoID] = controller
	}
	return controller
}

func (c *Coordinator) openTaskOccurrence(identity submitResultRuntimeIdentity) {
	if c == nil || !validSubmitResultIdentity(identity) {
		return
	}
	controller := c.occurrenceController(identity.TaskID)
	if controller == nil {
		return
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.identity = identity
	controller.opened = true
	controller.reserved = false
	controller.result = nil
	controller.pending = nil
	c.clearTaskResultProjection(identity.TaskID)
}

func validSubmitResultIdentity(identity submitResultRuntimeIdentity) bool {
	return strings.TrimSpace(identity.RunID) != "" &&
		strings.TrimSpace(identity.TaskID) != "" &&
		identity.Attempt > 0 && strings.TrimSpace(identity.Agent) != ""
}

func (c *Coordinator) beginTaskResultSubmission(identity submitResultRuntimeIdentity) (*taskResultOccurrenceTransaction, error) {
	if c == nil || !validSubmitResultIdentity(identity) {
		return nil, errTaskResultStale
	}
	controller := c.occurrenceController(identity.TaskID)
	if controller == nil {
		return nil, errTaskResultStale
	}
	controller.mu.Lock()
	if !controller.opened || !sameTaskResultOccurrence(controller.identity, identity) {
		controller.mu.Unlock()
		return nil, errTaskResultStale
	}
	if controller.reserved || controller.result != nil {
		controller.mu.Unlock()
		return nil, errTaskResultDuplicate
	}
	controller.reserved = true
	return &taskResultOccurrenceTransaction{coordinator: c, controller: controller, identity: identity}, nil
}

func (tx *taskResultOccurrenceTransaction) addMaterialized(refs []ArtifactRef) {
	if tx == nil || tx.controller == nil {
		return
	}
	tx.controller.pending = append([]ArtifactRef(nil), refs...)
}

func (tx *taskResultOccurrenceTransaction) consumePending(refs []ArtifactRef) error {
	if tx == nil || tx.controller == nil || len(refs) == 0 {
		return fmt.Errorf("no materialized submit_result artifacts are pending")
	}
	pending := tx.controller.pending
	if len(pending) != len(refs) {
		return fmt.Errorf("artifacts were not materialized by this runtime invocation")
	}
	matched := make([]bool, len(pending))
	for _, ref := range refs {
		found := false
		for i, candidate := range pending {
			if !matched[i] && candidate == ref {
				matched[i] = true
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("artifact %q was not materialized by this runtime invocation", ref.ID)
		}
	}
	tx.controller.pending = nil
	return nil
}

func (tx *taskResultOccurrenceTransaction) commit(result *TaskResult) error {
	if tx == nil || tx.controller == nil || tx.finished || !tx.controller.reserved {
		return errTaskResultStale
	}
	if result == nil {
		return fmt.Errorf("task result is nil")
	}
	copyResult := cloneTaskResult(result)
	copyResult.TaskID = tx.identity.TaskID
	copyResult.Attempt = tx.identity.Attempt
	copyResult.Agent = tx.identity.Agent
	tx.controller.result = copyResult
	tx.controller.reserved = false
	tx.controller.pending = nil
	tx.coordinator.publishTaskResultProjection(tx.identity.TaskID, copyResult)
	return nil
}

// finalizeTaskResultOccurrence seals and validates runner-owned transcript
// fields inside the occurrence transaction. The canonical result is cloned
// only after the exact captured identity is verified under the controller
// lock; no caller-owned result is mutated before the transaction commits.
func (c *Coordinator) finalizeTaskResultOccurrence(identity submitResultRuntimeIdentity, transcript *taskTranscript, contract taskResultSubmissionContract) (string, error) {
	if c == nil || !validSubmitResultIdentity(identity) || transcript == nil {
		return "", errTaskResultStale
	}
	if transcript.todoID != identity.TaskID || transcript.runID != identity.RunID || transcript.attempt != identity.Attempt {
		return "", errTaskResultStale
	}
	controller := c.occurrenceController(identity.TaskID)
	if controller == nil {
		return "", errTaskResultStale
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if !controller.opened || !sameTaskResultOccurrence(controller.identity, identity) || controller.reserved || controller.result == nil {
		return "", errTaskResultStale
	}
	finalized := cloneTaskResult(controller.result)
	output, err := finalizeVerbatimTaskResult(transcript, finalized)
	if err != nil {
		return "", err
	}
	if err := contract.validateTranscriptFinalization(finalized); err != nil {
		return "", err
	}
	finalized.TaskID = identity.TaskID
	finalized.Attempt = identity.Attempt
	finalized.Agent = identity.Agent
	controller.result = finalized
	c.publishTaskResultProjection(identity.TaskID, finalized)
	return output, nil
}

func (tx *taskResultOccurrenceTransaction) rollback() error {
	if tx == nil || tx.controller == nil || tx.finished {
		return nil
	}
	tx.controller.pending = nil
	tx.controller.reserved = false
	tx.finished = true
	tx.controller.mu.Unlock()
	return nil
}

func (tx *taskResultOccurrenceTransaction) finish() {
	if tx == nil || tx.controller == nil || tx.finished {
		return
	}
	tx.controller.pending = nil
	tx.finished = true
	tx.controller.mu.Unlock()
}

func (c *Coordinator) publishTaskResultProjection(todoID string, result *TaskResult) {
	if c == nil || result == nil {
		return
	}
	c.taskResultsMu.Lock()
	if c.taskResults == nil {
		c.taskResults = make(map[string]*TaskResult)
	}
	c.taskResults[todoID] = cloneTaskResult(result)
	c.taskResultsMu.Unlock()
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		_ = c.taskTracker.TodoList().SetTypedResult(todoID, result)
	}
}

func (c *Coordinator) clearTaskResultProjection(todoID string) {
	if c == nil || todoID == "" {
		return
	}
	c.taskResultsMu.Lock()
	if c.taskResults != nil {
		delete(c.taskResults, todoID)
	}
	c.taskResultsMu.Unlock()
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		_ = c.taskTracker.TodoList().SetTypedResult(todoID, nil)
	}
}

// submitResultRuntimeIdentityKey is deliberately private: only the
// coordinator can bind it at dispatch, while package-local direct callers may
// use withSubmitResultRuntimeIdentity in focused tests.
type submitResultRuntimeIdentityKey struct{}

func withSubmitResultRuntimeIdentity(ctx context.Context, identity submitResultRuntimeIdentity) context.Context {
	return context.WithValue(ctx, submitResultRuntimeIdentityKey{}, identity)
}

func submitResultRuntimeIdentityFromContext(ctx context.Context, _ *Coordinator, taskID string) (submitResultRuntimeIdentity, error) {
	if ctx == nil {
		return submitResultRuntimeIdentity{}, fmt.Errorf("submit_result runtime identity is missing")
	}
	if identity, ok := ctx.Value(submitResultRuntimeIdentityKey{}).(submitResultRuntimeIdentity); ok {
		if identity.TaskID != taskID || !validSubmitResultIdentity(identity) {
			return submitResultRuntimeIdentity{}, fmt.Errorf("submit_result runtime identity is invalid")
		}
		return identity, nil
	}
	if metadata, ok := invocationMetadataFromContext(ctx); ok {
		identity := submitResultRuntimeIdentity{RunID: metadata.RunID, TaskID: metadata.TaskID, Attempt: metadata.Attempt, Agent: metadata.AgentName}
		if identity.TaskID != taskID || !validSubmitResultIdentity(identity) {
			return submitResultRuntimeIdentity{}, fmt.Errorf("submit_result runtime identity is missing or invalid")
		}
		return identity, nil
	}
	return submitResultRuntimeIdentity{}, fmt.Errorf("submit_result runtime identity is missing")
}
