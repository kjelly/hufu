package team

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	errTaskResultStale     = errors.New("task result provenance does not match active runtime occurrence")
	errTaskResultDuplicate = errors.New("task result already submitted for active runtime occurrence")
	taskDispatchSeq        atomic.Uint64
)

// taskOccurrenceController is the coordinator-owned transaction boundary for
// one todo. mu protects the result reservation; projectionMu serializes the
// two-phase projection commit with lifecycle lease closure. Neither mutex is
// held while a TodoList checkpoint callback runs.
type taskOccurrenceController struct {
	mu           sync.Mutex
	projectionMu sync.Mutex

	identity submitResultRuntimeIdentity
	opened   bool
	reserved bool
	result   *TaskResult
	pending  []ArtifactRef
}

type taskOccurrenceControllerSnapshot struct {
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
	if c == nil || strings.TrimSpace(identity.TaskID) == "" {
		return
	}
	if identity.OccurrenceRevision <= 0 {
		identity.OccurrenceRevision = 1
	}
	if strings.TrimSpace(identity.DispatchID) == "" {
		identity.DispatchID = newTaskDispatchID()
	}
	if !validSubmitResultIdentity(identity) {
		return
	}
	// Compatibility callers (notably trusted coordinator-side seeds) may open
	// an occurrence directly. They must still receive the same Todo lease as a
	// dispatched worker; otherwise a later strict result projection would be
	// rejected after the controller has already latched the result.
	_ = c.activateTaskDispatch(identity)
}

func newTaskDispatchID() string {
	return fmt.Sprintf("dispatch-%d", taskDispatchSeq.Add(1))
}

func (controller *taskOccurrenceController) snapshot() taskOccurrenceControllerSnapshot {
	if controller == nil {
		return taskOccurrenceControllerSnapshot{}
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return taskOccurrenceControllerSnapshot{
		identity: controller.identity,
		opened:   controller.opened,
		reserved: controller.reserved,
		result:   cloneTaskResult(controller.result),
		pending:  append([]ArtifactRef(nil), controller.pending...),
	}
}

func (controller *taskOccurrenceController) restore(snapshot taskOccurrenceControllerSnapshot) {
	if controller == nil {
		return
	}
	controller.mu.Lock()
	controller.identity = snapshot.identity
	controller.opened = snapshot.opened
	controller.reserved = snapshot.reserved
	controller.result = cloneTaskResult(snapshot.result)
	controller.pending = append([]ArtifactRef(nil), snapshot.pending...)
	controller.mu.Unlock()
}

// activateTaskDispatch installs a fresh dispatch lease after the caller has
// completed the durable occurrence/admission boundary. It is deliberately a
// separate phase from result reservation so a retry or resume cannot expose a
// worker lease before its lifecycle event and admission are durable.
func (c *Coordinator) activateTaskDispatch(identity submitResultRuntimeIdentity) error {
	if c == nil || !validSubmitResultIdentity(identity) {
		return errTaskResultStale
	}
	controller := c.occurrenceController(identity.TaskID)
	if controller == nil {
		return errTaskResultStale
	}
	controller.projectionMu.Lock()
	defer controller.projectionMu.Unlock()
	return c.activateTaskDispatchLocked(identity, controller)
}

func (c *Coordinator) activateTaskDispatchLocked(identity submitResultRuntimeIdentity, controller *taskOccurrenceController) error {
	if c == nil || controller == nil || !validSubmitResultIdentity(identity) {
		return errTaskResultStale
	}
	previous := controller.snapshot()
	controller.mu.Lock()
	controller.identity = identity
	controller.opened = true
	controller.reserved = false
	controller.result = nil
	controller.pending = nil
	controller.mu.Unlock()
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		if err := c.taskTracker.TodoList().TrySetDispatchLease(identity.TaskID, identity.OccurrenceRevision, identity.DispatchID); err != nil {
			controller.restore(previous)
			return err
		}
	}
	c.clearTaskResultCache(identity.TaskID)
	return nil
}

// completeOccurrenceLease fills in the lease half of an identity that was
// built without one. It adopts the active occurrence only when the entire
// worker-visible half already matches, so a delayed prior attempt can never
// inherit the lease of the occurrence that replaced it.
func (c *Coordinator) completeOccurrenceLease(identity submitResultRuntimeIdentity) submitResultRuntimeIdentity {
	if identity.OccurrenceRevision > 0 && strings.TrimSpace(identity.DispatchID) != "" {
		return identity
	}
	active, ok := c.activeTaskResultOccurrence(identity.TaskID)
	if !ok || identity.RunID != active.RunID || identity.TaskID != active.TaskID ||
		identity.Attempt != active.Attempt || !strings.EqualFold(identity.Agent, active.Agent) {
		return identity
	}
	return active
}

func validSubmitResultIdentity(identity submitResultRuntimeIdentity) bool {
	return strings.TrimSpace(identity.RunID) != "" &&
		strings.TrimSpace(identity.TaskID) != "" &&
		identity.Attempt > 0 && strings.TrimSpace(identity.Agent) != "" &&
		identity.OccurrenceRevision > 0 && strings.TrimSpace(identity.DispatchID) != ""
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
	controller.mu.Unlock()
	return &taskResultOccurrenceTransaction{coordinator: c, controller: controller, identity: identity}, nil
}

func (tx *taskResultOccurrenceTransaction) addMaterialized(refs []ArtifactRef) {
	if tx == nil || tx.controller == nil {
		return
	}
	tx.controller.mu.Lock()
	defer tx.controller.mu.Unlock()
	if tx.finished || !tx.controller.reserved {
		return
	}
	tx.controller.pending = append([]ArtifactRef(nil), refs...)
}

func (tx *taskResultOccurrenceTransaction) consumePending(refs []ArtifactRef) error {
	if tx == nil || tx.controller == nil || len(refs) == 0 {
		return fmt.Errorf("no materialized submit_result artifacts are pending")
	}
	tx.controller.mu.Lock()
	defer tx.controller.mu.Unlock()
	if tx.finished || !tx.controller.reserved || !sameTaskResultOccurrence(tx.controller.identity, tx.identity) {
		return fmt.Errorf("task result occurrence is stale")
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
	if tx == nil || tx.controller == nil || tx.finished {
		return errTaskResultStale
	}
	if result == nil {
		return fmt.Errorf("task result is nil")
	}
	copyResult := cloneTaskResult(result)
	tx.controller.mu.Lock()
	if tx.finished || !tx.controller.reserved || !sameTaskResultOccurrence(tx.controller.identity, tx.identity) {
		tx.controller.mu.Unlock()
		return errTaskResultStale
	}
	copyResult.TaskID = tx.identity.TaskID
	copyResult.Attempt = tx.identity.Attempt
	copyResult.Agent = tx.identity.Agent
	tx.controller.result = copyResult
	tx.controller.reserved = false
	tx.controller.pending = nil
	tx.controller.mu.Unlock()
	if err := tx.coordinator.publishTaskResultProjection(tx.identity, copyResult); err != nil {
		tx.coordinator.clearPublishedTaskResult(tx.identity, copyResult)
		return err
	}
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
	controller.projectionMu.Lock()
	defer controller.projectionMu.Unlock()
	controller.mu.Lock()
	if !controller.opened || !sameTaskResultOccurrence(controller.identity, identity) || controller.reserved || controller.result == nil {
		controller.mu.Unlock()
		return "", errTaskResultStale
	}
	finalized := cloneTaskResult(controller.result)
	controller.mu.Unlock()
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
	controller.mu.Lock()
	if !controller.opened || !sameTaskResultOccurrence(controller.identity, identity) || controller.result == nil {
		controller.mu.Unlock()
		return "", errTaskResultStale
	}
	controller.result = finalized
	controller.mu.Unlock()
	if err := c.publishTaskResultProjectionLocked(identity, finalized, controller); err != nil {
		return "", err
	}
	return output, nil
}

func (tx *taskResultOccurrenceTransaction) rollback() error {
	if tx == nil || tx.controller == nil || tx.finished {
		return nil
	}
	tx.controller.mu.Lock()
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
	tx.controller.mu.Lock()
	tx.controller.pending = nil
	tx.finished = true
	tx.controller.mu.Unlock()
}

func (c *Coordinator) publishTaskResultProjection(identity submitResultRuntimeIdentity, result *TaskResult) error {
	if c == nil || result == nil || !validSubmitResultIdentity(identity) {
		return errTaskResultStale
	}
	controller := c.occurrenceController(identity.TaskID)
	if controller == nil {
		return errTaskResultStale
	}
	controller.projectionMu.Lock()
	defer controller.projectionMu.Unlock()
	return c.publishTaskResultProjectionLocked(identity, result, controller)
}

// publishTaskResultProjectionLocked is the non-reentrant half of result
// projection for callers that already own controller.projectionMu.
func (c *Coordinator) publishTaskResultProjectionLocked(identity submitResultRuntimeIdentity, result *TaskResult, controller *taskOccurrenceController) error {
	if c == nil || result == nil || controller == nil || !validSubmitResultIdentity(identity) {
		return errTaskResultStale
	}
	controller.mu.Lock()
	valid := controller.opened && sameTaskResultOccurrence(controller.identity, identity) && controller.result != nil
	controller.mu.Unlock()
	if !valid {
		return errTaskResultStale
	}
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		if err := c.taskTracker.TodoList().TrySetTypedResultForDispatch(identity.TaskID, identity.OccurrenceRevision, identity.DispatchID, result); err != nil {
			return err
		}
	}
	c.taskResultsMu.Lock()
	if c.taskResults == nil {
		c.taskResults = make(map[string]*TaskResult)
	}
	c.taskResults[identity.TaskID] = cloneTaskResult(result)
	c.taskResultsMu.Unlock()
	return nil
}

// revokeTaskOccurrence closes a prior worker lease after its replacement Todo
// projection is installed, preventing a stale controller result from being
// observed or published into the new lifecycle revision.
func (c *Coordinator) revokeTaskOccurrence(todoID string) {
	controller := c.occurrenceController(todoID)
	if controller == nil {
		return
	}
	controller.projectionMu.Lock()
	controller.mu.Lock()
	controller.opened = false
	controller.reserved = false
	controller.result = nil
	controller.pending = nil
	controller.identity = submitResultRuntimeIdentity{}
	controller.mu.Unlock()
	controller.projectionMu.Unlock()
	c.clearTaskResultCache(todoID)
}

// clearPublishedTaskResult restores the first-result latch when its Todo
// projection was rejected. Keeping an unprojected result authoritative would
// let a later terminal transition claim success without canonical evidence.
func (c *Coordinator) clearPublishedTaskResult(identity submitResultRuntimeIdentity, result *TaskResult) {
	controller := c.occurrenceController(identity.TaskID)
	if controller == nil {
		return
	}
	controller.mu.Lock()
	if controller.opened && sameTaskResultOccurrence(controller.identity, identity) && controller.result == result {
		controller.result = nil
	}
	controller.mu.Unlock()
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
	c.clearTaskResultCache(todoID)
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		_ = c.taskTracker.TodoList().SetTypedResult(todoID, nil)
	}
}

func (c *Coordinator) clearTaskResultCache(todoID string) {
	if c == nil || todoID == "" {
		return
	}
	c.taskResultsMu.Lock()
	if c.taskResults != nil {
		delete(c.taskResults, todoID)
	}
	c.taskResultsMu.Unlock()
}

// submitResultRuntimeIdentityKey is deliberately private: only the
// coordinator can bind it at dispatch, while package-local direct callers may
// use withSubmitResultRuntimeIdentity in focused tests.
type submitResultRuntimeIdentityKey struct{}

func withSubmitResultRuntimeIdentity(ctx context.Context, identity submitResultRuntimeIdentity) context.Context {
	return context.WithValue(ctx, submitResultRuntimeIdentityKey{}, identity)
}

func submitResultRuntimeIdentityFromContext(ctx context.Context, coordinator *Coordinator, taskID string) (submitResultRuntimeIdentity, error) {
	if ctx == nil {
		return submitResultRuntimeIdentity{}, fmt.Errorf("submit_result runtime identity is missing")
	}
	if identity, ok := ctx.Value(submitResultRuntimeIdentityKey{}).(submitResultRuntimeIdentity); ok {
		identity = coordinator.completeOccurrenceLease(identity)
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
