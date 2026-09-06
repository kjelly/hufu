package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// DecisionAdmissionSchemaVersion versions the immutable decision admission
// made for one durable task occurrence. It deliberately exists before the
// later DecisionRunEnvelope: off-profile occurrences also need a durable
// admission, and an interrupted occurrence must never re-resolve live config.
const DecisionAdmissionSchemaVersion = 1

// DecisionAdmission is the immutable effective decision contract for one
// (task, attempt) occurrence. Policy is meaningful only when Enabled is true.
type DecisionAdmission struct {
	SchemaVersion           int             `json:"schema_version"`
	RunID                   string          `json:"run_id"`
	TaskID                  string          `json:"task_id"`
	Attempt                 int             `json:"attempt"`
	Profile                 string          `json:"profile"`
	Source                  string          `json:"source"`
	Enabled                 bool            `json:"enabled"`
	Policy                  *DecisionPolicy `json:"policy,omitempty"`
	DecisionID              string          `json:"decision_id,omitempty"`
	RequestContractRef      string          `json:"request_contract_ref,omitempty"`
	RequestContractRevision uint64          `json:"request_contract_revision,omitempty"`
	RequestContractArtifact ArtifactRef     `json:"request_contract_artifact,omitempty"`
	TaskInputDigest         string          `json:"task_input_digest"`
}

func (a DecisionAdmission) Validate() error {
	if a.SchemaVersion != DecisionAdmissionSchemaVersion {
		return fmt.Errorf("unsupported decision admission schema version %d", a.SchemaVersion)
	}
	if strings.TrimSpace(a.RunID) == "" || strings.TrimSpace(a.TaskID) == "" || a.Attempt < 1 || strings.TrimSpace(a.Profile) == "" || strings.TrimSpace(a.Source) == "" || strings.TrimSpace(a.TaskInputDigest) == "" {
		return fmt.Errorf("decision admission identity is incomplete")
	}
	if a.Enabled != (a.Profile != DecisionProfileOff) {
		return fmt.Errorf("decision admission enabled state conflicts with profile %q", a.Profile)
	}
	if a.Enabled {
		if strings.TrimSpace(a.DecisionID) == "" {
			return fmt.Errorf("enabled decision admission has no decision id")
		}
		if a.Policy == nil {
			return fmt.Errorf("enabled decision admission has no policy")
		}
		if err := a.Policy.Validate(); err != nil {
			return fmt.Errorf("decision admission policy: %w", err)
		}
	} else if strings.TrimSpace(a.DecisionID) != "" {
		return fmt.Errorf("off decision admission has a decision id")
	}
	return nil
}

func decisionAdmissionKey(taskID string, attempt int) string {
	return "decision-admission:" + strings.TrimSpace(taskID) + fmt.Sprintf(":%d", attempt)
}

// decisionTaskInputDigest binds admission to immutable task decision inputs
// without persisting mutable coordinator configuration a second time.
func decisionTaskInputDigest(input any) (string, error) {
	projection, err := taskOccurrenceProjectionInput(input)
	if err != nil {
		return "", err
	}
	return decisionOccurrenceInputDigest(projection)
}

// taskOccurrenceProjectionInput keeps old package-level test helpers and
// non-runtime decision-engine callers source-compatible. Production task
// creation, retry, and validation paths pass TaskOccurrenceProjection
// directly; they never reconstruct one from TaskDef.
func taskOccurrenceProjectionInput(input any) (TaskOccurrenceProjection, error) {
	switch value := input.(type) {
	case TaskOccurrenceProjection:
		return value, nil
	case *TaskOccurrenceProjection:
		if value == nil {
			return TaskOccurrenceProjection{}, fmt.Errorf("decision task input is nil")
		}
		return *value, nil
	case *TodoItem:
		return newTaskOccurrenceProjection(value)
	case TodoItem:
		return newTaskOccurrenceProjection(&value)
	case TaskDef:
		// Compatibility for direct decision-engine unit callers. This adapter is
		// intentionally not used by durable coordinator validation paths.
		return taskOccurrenceProjectionFromTaskDef(value, "")
	default:
		return TaskOccurrenceProjection{}, fmt.Errorf("unsupported decision task input %T", input)
	}
}

func decisionOccurrenceInputDigest(task TaskOccurrenceProjection) (string, error) {
	// The empty side-effect class is the legacy spelling of the read-only
	// default. Canonical execution materializes it as "none", so normalize
	// that semantic equivalent before hashing.
	sideEffect := task.SideEffect
	if sideEffect == "" {
		sideEffect = SideEffectNone
	}
	// Keep this material deliberately broader than the evidence packet. An
	// admission authorizes execution, so changing an execution contract,
	// recovery class, task goal, or verification rule must be detected just as
	// surely as changing the decision alternatives.
	v := struct {
		ID, PlanTaskID, Phase, Agent, Desc, Goal, Constraints, Model, Source string
		PlanFirst                                                            bool
		ModelTopology                                                        []string
		Sidecar                                                              bool
		Summarize                                                            bool
		OutputMode                                                           string
		ContextFiles, Requires                                               []string
		ParentID, OnFailure                                                  string
		DependsOn                                                            []string
		Action                                                               *Action
		ContractID, ContractHash                                             string
		ContractRevision                                                     int
		MaxRetries                                                           int
		Verify, VerifyMode                                                   string
		VerifySpec                                                           *VerificationSpec
		SideEffect                                                           SideEffectClass
		Escalate                                                             bool
		AdversarialVerify                                                    int
		Recovery                                                             RecoveryPolicy
		ReconcileTool                                                        string
		Execution                                                            ExecutionContract
		Optional                                                             bool
		ResourceClaims                                                       []string
		Resources                                                            []ResourceClaim
		WorksetBinding                                                       *WorksetBinding
		WorksetReceipt                                                       *WorksetExpansionReceipt
		Kind                                                                 TaskKind
		Advances                                                             []string
		ExpectedStateChange                                                  string
		RecoveryHypothesis                                                   *RecoveryHypothesis
		Profile                                                              string
		Options                                                              []DecisionOption
		Assumptions                                                          []DecisionAssumption
		Facts                                                                map[string]any
		Artifacts                                                            []ArtifactRef
		BaseRates                                                            []BaseRateEvidence
		Provenance                                                           []EvidenceProvenance
	}{
		ID: task.ID, PlanTaskID: task.PlanTaskID, Phase: string(task.Phase), Agent: task.Agent, Desc: task.Desc, Goal: task.Goal, Constraints: task.Constraints, Model: task.Model, Source: task.Source, PlanFirst: task.PlanFirst, ModelTopology: task.ModelTopology,
		Sidecar: task.Sidecar, Summarize: task.Summarize, OutputMode: task.OutputMode, ContextFiles: task.ContextFiles, Requires: task.Requires,
		ParentID: task.ParentID, OnFailure: task.OnFailure, DependsOn: task.DependsOn,
		Action: task.Action, ContractID: task.ContractID, ContractHash: task.ContractHash, ContractRevision: task.ContractRevision,
		MaxRetries: task.MaxRetries, Verify: task.Verify, VerifyMode: task.VerifyMode, VerifySpec: task.VerifySpec, SideEffect: sideEffect, Escalate: task.Escalate, AdversarialVerify: task.AdversarialVerify, Recovery: task.Recovery,
		ReconcileTool: task.ReconcileTool, Execution: cloneExecutionContract(task.Execution), Optional: task.Optional, ResourceClaims: task.ResourceClaims, Resources: task.Resources, WorksetBinding: task.WorksetBinding, WorksetReceipt: task.WorksetReceipt, Kind: task.Kind, Advances: task.Advances, ExpectedStateChange: task.ExpectedStateChange,
		RecoveryHypothesis: task.RecoveryHypothesis,
		Profile:            task.DecisionProfile, Options: task.DecisionOptions, Assumptions: task.DecisionAssumptions, Facts: task.DecisionFacts,
		Artifacts: task.DecisionArtifacts, BaseRates: task.DecisionBaseRates, Provenance: task.DecisionProvenance,
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func appendDecisionAdmission(ctx context.Context, journal EventJournal, admission DecisionAdmission) (DecisionAdmission, error) {
	if journal == nil {
		return DecisionAdmission{}, fmt.Errorf("decision admission journal is unavailable")
	}
	if err := admission.Validate(); err != nil {
		return DecisionAdmission{}, err
	}
	existing, found, err := loadDecisionAdmission(ctx, journal, admission.TaskID, admission.Attempt)
	if err != nil {
		return DecisionAdmission{}, err
	}
	if found {
		if !reflect.DeepEqual(existing, admission) {
			return DecisionAdmission{}, fmt.Errorf("conflicting decision admission for task %s attempt %d", admission.TaskID, admission.Attempt)
		}
		return existing, nil
	}
	payload, err := json.Marshal(admission)
	if err != nil {
		return DecisionAdmission{}, err
	}
	_, err = journal.Append(ctx, RunEvent{Type: agent.EventDecisionAdmitted, Actor: decisionActor, RunID: admission.RunID, TaskID: admission.TaskID, Attempt: admission.Attempt, IdempotencyKey: decisionAdmissionKey(admission.TaskID, admission.Attempt), Payload: payload})
	if err != nil {
		return DecisionAdmission{}, fmt.Errorf("append decision admission: %w", err)
	}
	return admission, nil
}

// loadDecisionAdmission is intentionally task/attempt based: a recovery run
// has a new run ID and must not use it to select a historical occurrence.
func loadDecisionAdmission(ctx context.Context, journal EventJournal, taskID string, attempt int) (DecisionAdmission, bool, error) {
	if journal == nil {
		return DecisionAdmission{}, false, fmt.Errorf("decision admission journal is unavailable")
	}
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return DecisionAdmission{}, false, err
	}
	var found []DecisionAdmission
	for _, event := range events {
		if event.Type != agent.EventDecisionAdmitted || event.TaskID != taskID || event.Attempt != attempt {
			continue
		}
		var a DecisionAdmission
		if err := json.Unmarshal(event.Payload, &a); err != nil {
			return DecisionAdmission{}, false, fmt.Errorf("decode decision admission: %w", err)
		}
		if a.TaskID != event.TaskID || a.Attempt != event.Attempt || event.RunID != "" && a.RunID != event.RunID {
			return DecisionAdmission{}, false, fmt.Errorf("decision admission event identity does not match payload")
		}
		if err := a.Validate(); err != nil {
			return DecisionAdmission{}, false, err
		}
		found = append(found, a)
	}
	if len(found) == 0 {
		return DecisionAdmission{}, false, nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].RunID < found[j].RunID })
	for _, a := range found[1:] {
		if !reflect.DeepEqual(found[0], a) {
			return DecisionAdmission{}, false, fmt.Errorf("conflicting durable decision admissions for task %s attempt %d", taskID, attempt)
		}
	}
	return found[0], true, nil
}

// canonicalizeTaskOccurrence freezes the effective task contract shared by
// decision admission, task_created, checkpoint replay, and execution. The
// caller supplies the model selected at the creation boundary; no later
// consumer is allowed to re-resolve it from mutable team configuration.
func (c *Coordinator) canonicalizeTaskOccurrence(task TaskDef, def *agent.AgentDef, resolvedModel string) TaskDef {
	if def != nil {
		task.Agent = strings.ToLower(strings.TrimSpace(def.Name))
	}
	task.Model = resolvedModel
	if c != nil {
		task.SideEffect, task.Recovery, task.ReconcileTool = c.PolicyEngine().ResolveRecoveryPolicy(def, task)
	}
	return task
}

// validateTaskOccurrenceAdmission validates the one durable marker that
// authorizes an executable task occurrence. It is intentionally shared by
// normal execution, retry/resume, and protocol-only repair so no entry point
// can reach provider work with a markerless or mismatched projection.
//
// The only compatibility exception is an exact, valid pre-admission decision
// run envelope anchored to this task/attempt. It is read-only compatibility;
// this function never creates an admission from live configuration.
func (c *Coordinator) validateTaskOccurrenceAdmission(ctx context.Context, task any, taskID string, attempt int) (DecisionAdmission, bool, error) {
	if c == nil || !c.hasDurableEventJournal() {
		return DecisionAdmission{}, false, nil
	}
	if strings.TrimSpace(taskID) == "" || attempt < 1 {
		return DecisionAdmission{}, false, fmt.Errorf("decision admission validation requires task identity and positive attempt")
	}
	// Unit-level/in-memory callers may form a prospective decision before a
	// todo exists. Only a persisted executable todo is subject to the durable
	// marker requirement; creation paths append the marker before making it
	// visible and then call this function after task_created.
	if c.todoItemByID(taskID) == nil {
		return DecisionAdmission{}, false, nil
	}
	// The Todo is the canonical owner of the executable occurrence. In
	// particular, do not reconstruct it from TaskDef: TaskDef.ID is a logical
	// PlanTaskID and loses the runtime Todo identity and finalized batch edges.
	item := c.todoItemByID(taskID)
	projection, err := newTaskOccurrenceProjection(item)
	if err != nil {
		return DecisionAdmission{}, false, fmt.Errorf("build task occurrence projection: %w", err)
	}
	if err := validateTaskDefAgainstOccurrence(task, item); err != nil {
		return DecisionAdmission{}, false, err
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		return DecisionAdmission{}, false, err
	}
	admission, found, err := loadDecisionAdmission(ctx, journal, taskID, attempt)
	if err != nil {
		return DecisionAdmission{}, false, fmt.Errorf("load decision admission: %w", err)
	}
	if found {
		if admission.TaskID != taskID || admission.Attempt != attempt {
			return DecisionAdmission{}, false, fmt.Errorf("decision admission identity does not match task %s attempt %d", taskID, attempt)
		}
		digest, err := decisionTaskInputDigest(projection)
		if err != nil {
			return DecisionAdmission{}, false, fmt.Errorf("compute decision admission digest: %w", err)
		}
		if digest != admission.TaskInputDigest {
			return DecisionAdmission{}, false, fmt.Errorf("decision admission task input does not match task %s", taskID)
		}
		return admission, true, nil
	}
	legacy, err := c.validateLegacyDecisionEnvelopeForTaskOccurrence(ctx, journal, projection, taskID, attempt)
	if err != nil {
		return DecisionAdmission{}, false, err
	}
	if !legacy {
		return DecisionAdmission{}, false, fmt.Errorf("durable task %s attempt %d has no decision admission", taskID, attempt)
	}
	return DecisionAdmission{}, false, nil
}

// validateTaskDefAgainstOccurrence is an independent consistency check for
// callers that still carry a scheduler TaskDef. Admission and digesting use
// the Todo-owned projection above; this check only proves that the scheduler
// did not silently retarget immutable execution inputs on the way to the
// worker boundary. Runtime-only IDs are never accepted as TaskDef.ID.
func validateTaskDefAgainstOccurrence(input any, item *TodoItem) error {
	if item == nil {
		return fmt.Errorf("task occurrence validation requires a Todo item")
	}
	task, ok := input.(TaskDef)
	if !ok {
		if pointer, isPointer := input.(*TaskDef); isPointer && pointer != nil {
			task, ok = *pointer, true
		}
	}
	if !ok {
		return nil
	}
	if task.ID != "" {
		if item.PlanTaskID == "" && task.ID == item.ID {
			return fmt.Errorf("task %s uses runtime Todo ID as TaskDef.ID; use PlanTaskID", item.ID)
		}
		if item.PlanTaskID != "" && task.ID != item.PlanTaskID {
			return fmt.Errorf("task %s TaskDef.ID %q does not match PlanTaskID %q", item.ID, task.ID, item.PlanTaskID)
		}
	}
	if task.Agent != "" && !strings.EqualFold(task.Agent, item.Agent) {
		return fmt.Errorf("task %s immutable agent does not match Todo occurrence", item.ID)
	}
	if task.Goal != "" && task.Goal != item.Goal {
		return fmt.Errorf("task %s immutable goal does not match Todo occurrence", item.ID)
	}
	if task.Constraints != "" && task.Constraints != item.Constraints {
		return fmt.Errorf("task %s immutable constraints do not match Todo occurrence", item.ID)
	}
	if task.Model != "" && task.Model != item.Model {
		return fmt.Errorf("task %s immutable model does not match Todo occurrence", item.ID)
	}
	if len(task.ModelTopology) > 0 && !reflect.DeepEqual(task.ModelTopology, item.ModelTopology) {
		return fmt.Errorf("task %s immutable model topology does not match Todo occurrence", item.ID)
	}
	if task.ContractID != "" && task.ContractID != item.ContractID {
		return fmt.Errorf("task %s immutable contract does not match Todo occurrence", item.ID)
	}
	if task.ContractHash != "" && task.ContractHash != item.ContractHash {
		return fmt.Errorf("task %s immutable contract hash does not match Todo occurrence", item.ID)
	}
	if task.Verify != "" && task.Verify != item.Verify {
		return fmt.Errorf("task %s immutable verification command does not match Todo occurrence", item.ID)
	}
	if task.VerifyMode != "" && task.VerifyMode != item.VerifyMode {
		return fmt.Errorf("task %s immutable verification mode does not match Todo occurrence", item.ID)
	}
	if task.DecisionProfile != "" && task.DecisionProfile != item.DecisionProfile {
		return fmt.Errorf("task %s immutable decision profile does not match Todo occurrence", item.ID)
	}
	if len(task.DecisionOptions) > 0 && !reflect.DeepEqual(task.DecisionOptions, item.DecisionOptions) {
		return fmt.Errorf("task %s immutable decision options do not match Todo occurrence", item.ID)
	}
	if !reflect.DeepEqual(task.Execution, ExecutionContract{}) && !reflect.DeepEqual(task.Execution, item.Execution) {
		return fmt.Errorf("task %s immutable execution contract does not match Todo occurrence", item.ID)
	}
	return nil
}

// validateLegacyDecisionEnvelopeForTaskOccurrence permits only an already
// anchored envelope whose complete identity and task-input digest match. A
// pre-envelope finalized record remains readable through decision APIs, but
// cannot authorize provider repair or worker execution here.
func (c *Coordinator) validateLegacyDecisionEnvelopeForTaskOccurrence(ctx context.Context, journal EventJournal, task TaskOccurrenceProjection, taskID string, attempt int) (bool, error) {
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return false, fmt.Errorf("read decision admission compatibility history: %w", err)
	}
	var envelopeRef *ArtifactRef
	var decisionID string
	for _, event := range events {
		if event.Type != agent.EventDecisionRunEnvelopeAnchored || event.TaskID != taskID || event.Attempt != attempt || len(event.Payload) == 0 {
			continue
		}
		var payload decisionEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return false, fmt.Errorf("decode legacy decision envelope anchor: %w", err)
		}
		if payload.DecisionID == "" || payload.EnvelopeRef.ID == "" || payload.EnvelopeRef.SHA256 == "" || payload.RunID != event.RunID || payload.TaskID != taskID || payload.Attempt != attempt {
			return false, fmt.Errorf("legacy decision envelope anchor identity is incomplete for task %s attempt %d", taskID, attempt)
		}
		if envelopeRef != nil && (!sameArtifactIdentity(*envelopeRef, payload.EnvelopeRef) || decisionID != payload.DecisionID) {
			return false, fmt.Errorf("conflicting legacy decision envelope anchors for task %s attempt %d", taskID, attempt)
		}
		ref := payload.EnvelopeRef
		envelopeRef = &ref
		decisionID = payload.DecisionID
	}
	if envelopeRef == nil {
		return false, nil
	}
	envelope, err := loadDecisionRunEnvelope(ctx, c.decisionArtifactStore(), *envelopeRef)
	if err != nil {
		return false, fmt.Errorf("validate legacy decision envelope: %w", err)
	}
	if envelope.DecisionID != decisionID || envelope.TaskID != taskID || envelope.Attempt != attempt {
		return false, fmt.Errorf("legacy decision envelope identity does not match task %s attempt %d", taskID, attempt)
	}
	digest, err := decisionTaskInputDigest(task)
	if err != nil {
		return false, fmt.Errorf("compute legacy decision envelope digest: %w", err)
	}
	if digest != envelope.Request.AdmissionInputDigest {
		return false, fmt.Errorf("legacy decision envelope task input does not match task %s", taskID)
	}
	return true, nil
}

// admitTaskOccurrence resolves live configuration exactly once, while a new
// durable occurrence is being created. Every later consumer must load this
// record instead of consulting mutable coordinator configuration.
func (c *Coordinator) admitTaskOccurrence(ctx context.Context, input any, taskID string, attempt int) (DecisionAdmission, error) {
	if c == nil || !c.hasDurableEventJournal() {
		return DecisionAdmission{}, nil
	}
	var task TaskOccurrenceProjection
	var err error
	switch value := input.(type) {
	case TaskOccurrenceProjection:
		task = value
	case *TaskOccurrenceProjection:
		if value == nil {
			return DecisionAdmission{}, fmt.Errorf("decision admission task input is nil")
		}
		task = *value
	case *TodoItem:
		task, err = newTaskOccurrenceProjection(value)
	case TodoItem:
		task, err = newTaskOccurrenceProjection(&value)
	case TaskDef:
		// Compatibility for callers that are creating a new occurrence but do
		// not yet have a TodoItem. Bind only the supplied runtime identity here;
		// never treat TaskDef.ID as that runtime identity.
		task, err = taskOccurrenceProjectionFromTaskDef(value, taskID)
	default:
		return DecisionAdmission{}, fmt.Errorf("unsupported decision task input %T", input)
	}
	if err != nil {
		return DecisionAdmission{}, err
	}
	if strings.TrimSpace(task.ID) == "" {
		task.ID = taskID
	}
	if strings.TrimSpace(task.ID) != strings.TrimSpace(taskID) {
		return DecisionAdmission{}, fmt.Errorf("decision admission task ID %q does not match Todo ID %q", task.ID, taskID)
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		return DecisionAdmission{}, err
	}
	resolution, err := ResolveDecisionProfile(c.decisionConfig(), c.DecisionProfileOverride(), task)
	if err != nil {
		return DecisionAdmission{}, err
	}
	digest, err := decisionTaskInputDigest(task)
	if err != nil {
		return DecisionAdmission{}, err
	}
	a := DecisionAdmission{SchemaVersion: DecisionAdmissionSchemaVersion, RunID: c.executionRunID, TaskID: taskID, Attempt: attempt, Profile: resolution.Profile, Source: resolution.Source, Enabled: resolution.Enabled(), TaskInputDigest: digest}
	if a.RunID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		a.RunID = c.taskTracker.TodoList().RunID()
	}
	// Legacy in-memory coordinators used by local callers/tests may not yet
	// have an execution run ID. Keep their admission deterministic rather than
	// inventing a time-based identity that cannot be replayed.
	if a.RunID == "" {
		a.RunID = "admission:" + taskID
	}
	if !a.Enabled {
		return appendDecisionAdmission(ctx, journal, a)
	}
	if !c.decisionConfig().RequestContract.Enabled {
		return DecisionAdmission{}, fmt.Errorf("decision request contract is required when profile %q is enabled", a.Profile)
	}
	if err := ValidateTaskDecisionEvidence(task); err != nil {
		return DecisionAdmission{}, fmt.Errorf("decision task evidence: %w", err)
	}
	policy, ok := DecisionPolicyFor(c.decisionConfig(), a.Profile)
	if !ok {
		return DecisionAdmission{}, fmt.Errorf("%s: decision profile %q resolved but has no policy", ReasonDecisionProfileUnknown, a.Profile)
	}
	contract, err := c.requestContractFor(ctx, c.decisionConfig().RequestContract)
	if err != nil {
		return DecisionAdmission{}, err
	}
	a.Policy, a.DecisionID = &policy, "decision:"+taskID+fmt.Sprintf(":%d", attempt)
	a.RequestContractRef, a.RequestContractRevision, a.RequestContractArtifact = contract.artifact.ID, contract.envelope.Revision, contract.artifact
	return appendDecisionAdmission(ctx, journal, a)
}

func validateDecisionAdmissionEnvelope(admission DecisionAdmission, envelope DecisionRunEnvelope) error {
	if admission.TaskID != envelope.TaskID || admission.Attempt != envelope.Attempt || admission.RunID != envelope.RunID || !admission.Enabled {
		return fmt.Errorf("decision admission occurrence does not match run envelope")
	}
	if admission.DecisionID != envelope.DecisionID || admission.Profile != envelope.Profile || admission.Policy == nil || !reflect.DeepEqual(*admission.Policy, envelope.Policy) {
		return fmt.Errorf("decision admission profile or policy does not match run envelope")
	}
	if admission.RequestContractRef != envelope.Request.RequestContractRef || admission.RequestContractRevision != envelope.Request.RequestContractRevision || !sameArtifactIdentity(admission.RequestContractArtifact, envelope.Request.RequestContractArtifact) {
		return fmt.Errorf("decision admission request contract does not match run envelope")
	}
	if admission.TaskInputDigest != envelope.Request.AdmissionInputDigest {
		return fmt.Errorf("decision admission task input does not match run envelope")
	}
	return nil
}
