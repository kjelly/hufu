package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/kjelly/hufu/internal/agent"
)

// A runner's result is not an attestation. Only the coordinator's wrapper can
// implement this package-private interface and supply provider observations.
type structuredStepAttributor interface {
	runAttributedStructuredStep(context.Context, StructuredStepRequest, string) (ExecutionStepResult, structuredStepAttribution, error)
}

type structuredStepAttribution struct {
	kind        string
	invocations []string
}

type observedStructuredStepRunner struct {
	owner *Coordinator
	inner StructuredStepRunner
}

func (r *observedStructuredStepRunner) RunStructuredStep(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
	return r.inner.RunStructuredStep(ctx, request)
}

// Keep the existing artifact TOCTOU check when installing the observation
// wrapper; an immutable runner that has no inspector retains its old behavior.
func (r *observedStructuredStepRunner) InspectStructuredArtifact(ctx context.Context, ref ArtifactRef) (ArtifactRef, error) {
	if inspector, ok := r.inner.(StructuredArtifactInspector); ok {
		return inspector.InspectStructuredArtifact(ctx, ref)
	}
	return ref, nil
}

func (r *observedStructuredStepRunner) runAttributedStructuredStep(ctx context.Context, request StructuredStepRequest, receiptID string) (ExecutionStepResult, structuredStepAttribution, error) {
	if declared, ok := r.inner.(*coordinatorDeclaredToolRunner); ok && declared.c == r.owner {
		// Describe the directly executed tool. Auxiliary calls made inside that
		// tool retain their ordinary task-level accounting and provenance.
		result, err := r.inner.RunStructuredStep(ctx, request)
		return result, structuredStepAttribution{kind: "tool_call"}, err
	}
	if r.owner.EventStore() == nil {
		result, err := r.inner.RunStructuredStep(ctx, request)
		return result, structuredStepAttribution{kind: "unknown"}, err
	}
	identity, _ := r.owner.activeTaskResultOccurrence(request.TaskID)
	scope := &structuredStepScope{
		revision: identity.OccurrenceRevision,
		owner:    r.owner, runID: r.owner.executionRunID, branchID: r.owner.EventStore().BranchID(),
		taskID: request.TaskID, stepID: request.Step.ID, receiptID: receiptID,
		attempt: request.Attempt, repairAttempt: request.RepairAttempt,
		committed: make(map[string]structuredStepInvocationSource),
	}
	ctx = context.WithValue(ctx, structuredStepScopeKey{}, scope)
	ctx = context.WithValue(ctx, todoIDKey{}, request.TaskID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, request.Attempt)
	result, err := r.inner.RunStructuredStep(ctx, request)
	ids, observationErr := scope.finish()
	kind := "unknown"
	if len(ids) > 0 {
		kind = "model_generation"
	}
	return result, structuredStepAttribution{kind: kind, invocations: ids}, errors.Join(err, observationErr)
}

type structuredStepScopeKey struct{}

type structuredStepInvocationSource struct {
	profileEventID            string
	profileHash               string
	modelID                   string
	providerIdentity          string
	providerConfigurationHash string
}

type structuredStepScope struct {
	owner                                      *Coordinator
	runID, branchID, taskID, stepID, receiptID string
	attempt, repairAttempt, revision           int
	mu                                         sync.Mutex
	closed                                     bool
	active                                     int
	committed                                  map[string]structuredStepInvocationSource
	invocationIDs                              []string
	evidenceErr                                error
}

func structuredStepScopeFromContext(ctx context.Context) *structuredStepScope {
	scope, _ := ctx.Value(structuredStepScopeKey{}).(*structuredStepScope)
	return scope
}

func (s *structuredStepScope) acquire(c *Coordinator) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != c || s.closed {
		return nil, fmt.Errorf("structured step invocation scope is unavailable")
	}
	s.active++
	return sync.OnceFunc(func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}), nil
}

func (s *structuredStepScope) validateIdentity(ctx context.Context, c *Coordinator, request agent.ProviderRequest) error {
	if s.owner != c || s.closed || c.EventStore() == nil || c.EventStore().BranchID() != s.branchID {
		return fmt.Errorf("structured step invocation owner or branch differs")
	}
	identity, err := c.providerCostIdentity(ctx, request.InvocationID)
	if err != nil {
		return err
	}
	if identity.RunID != s.runID || identity.TaskID != s.taskID || identity.OccurrenceAttempt != s.attempt {
		return fmt.Errorf("structured step invocation occurrence differs")
	}
	if !request.AdmissionContext.IsBound() || request.InvocationID == "" {
		return fmt.Errorf("structured step invocation has no admitted identity")
	}
	return nil
}

func (s *structuredStepScope) commit(ctx context.Context, c *Coordinator, request agent.ProviderRequest, profile RunEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateIdentity(ctx, c, request); err != nil {
		return err
	}
	if _, exists := s.committed[request.InvocationID]; exists {
		return fmt.Errorf("structured step invocation is already committed")
	}
	if profile.ID == "" || profile.Hash == "" || profile.RunID != s.runID || profile.BranchID != s.branchID || profile.Type != string(EventModelProfileResolved) {
		return fmt.Errorf("structured step invocation profile source differs")
	}
	var identity struct {
		InvocationID    string `json:"invocation_id"`
		ExecutionTarget string `json:"execution_target"`
	}
	if err := json.Unmarshal(profile.Payload, &identity); err != nil || identity.InvocationID != request.InvocationID {
		return fmt.Errorf("structured step invocation profile identity differs")
	}
	target, known, err := boundProviderInvocationTarget(request)
	if err != nil {
		return err
	}
	if known && identity.ExecutionTarget != target.String() {
		return fmt.Errorf("persisted provider target differs from immutable binding")
	}
	s.committed[request.InvocationID] = structuredStepInvocationSource{
		profileEventID: profile.ID, profileHash: profile.Hash,
		modelID: request.ModelID, providerIdentity: request.AdmissionContext.ProviderIdentity,
		providerConfigurationHash: request.AdmissionContext.ProviderConfigurationHash,
	}
	return nil
}

func (s *structuredStepScope) finish() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.active != 0 {
		s.evidenceErr = errors.Join(s.evidenceErr, fmt.Errorf("structured step returned with unjoined provider invocations"))
	}
	return slices.Clone(s.invocationIDs), s.evidenceErr
}

func executeAttributedStructuredStep(ctx context.Context, runner StructuredStepRunner, request StructuredStepRequest, receiptID string) (ExecutionStepResult, structuredStepAttribution, error) {
	if observed, ok := runner.(structuredStepAttributor); ok {
		return observed.runAttributedStructuredStep(ctx, request, receiptID)
	}
	result, err := runner.RunStructuredStep(ctx, request)
	return result, structuredStepAttribution{kind: "unknown"}, err
}

func (e *structuredExecutionRun) receiptID(step ExecutionStep, repairAttempt int) string {
	legacy := structuredReceiptID(e.request.TaskID, e.request.Attempt, step.ID, repairAttempt, e.receiptCount)
	if e.request.receiptScope == "" {
		return legacy
	}
	hash := sha256.Sum256([]byte(e.request.receiptScope + "\x00" + legacy))
	return "step-" + hex.EncodeToString(hash[:])
}

func (s *structuredStepScope) preflight(ctx context.Context, c *Coordinator, request agent.ProviderRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validateIdentity(ctx, c, request)
}

func (s *structuredStepScope) observe(ctx context.Context, c *Coordinator, request agent.ProviderRequest, result agent.ProviderInvocationResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateIdentity(ctx, c, request); err != nil {
		s.evidenceErr = errors.Join(s.evidenceErr, err)
		return
	}
	source, exists := s.committed[request.InvocationID]
	if !exists || slices.Contains(s.invocationIDs, request.InvocationID) || source.modelID != request.ModelID ||
		source.providerIdentity != request.AdmissionContext.ProviderIdentity || source.providerConfigurationHash != request.AdmissionContext.ProviderConfigurationHash {
		s.evidenceErr = errors.Join(s.evidenceErr, fmt.Errorf("structured step invocation is missing, duplicated or changed"))
		return
	}
	payload, err := json.Marshal(structuredStepInvocationObserved{
		Version: 1, ReceiptID: s.receiptID, StepID: s.stepID, RepairAttempt: s.repairAttempt, OccurrenceRevision: s.revision,
		ModelExecutionID: request.InvocationID, ProfileEventID: source.profileEventID, ProfileHash: source.profileHash, Outcome: string(result.Outcome),
	})
	if err == nil {
		_, err = c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{
			Type: string(EventStructuredStepInvocationObserved), RunID: s.runID, BranchID: s.branchID,
			TaskID: s.taskID, Attempt: s.attempt, Actor: "runtime",
			IdempotencyKey: s.branchID + ":step-invocation:" + s.receiptID + ":" + request.InvocationID, Payload: payload,
		})
	}
	if err != nil {
		s.evidenceErr = errors.Join(s.evidenceErr, err)
		return
	}
	s.invocationIDs = append(s.invocationIDs, request.InvocationID)
}
