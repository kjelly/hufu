package team

import (
	"fmt"
	"reflect"
	"strings"
)

// TaskOccurrenceProjection is the immutable execution contract of one Todo
// occurrence. It is formed only after the runtime has assigned the Todo ID and
// resolved all batch-local edges and workset receipts. Mutable lifecycle state
// (status, output, receipts, verification results, and retry counters) is not
// part of this snapshot.
//
// Keeping this as a typed projection is important: admission and task_created
// must hash and persist the same finalized object. A TaskDef has index-based
// DAG edges and configuration-only fields, so it is not a safe durable
// representation of an executable occurrence.
type TaskOccurrenceProjection struct {
	ID                  string
	PlanTaskID          string
	PlanFirst           bool
	PlanID              string
	Phase               Phase
	Action              *Action
	ContractID          string
	ContractHash        string
	ContractRevision    int
	Agent               string
	Desc                string
	Goal                string
	Constraints         string
	Model               string
	ModelTopology       []string
	Sidecar             bool
	Summarize           bool
	OutputMode          string
	ContextFiles        []string
	Requires            []string
	Source              string
	ParentID            string
	DependsOn           []string
	OnFailure           string
	Verify              string
	VerifyMode          string
	VerifySpec          *VerificationSpec
	WorksetBinding      *WorksetBinding
	WorksetReceipt      *WorksetExpansionReceipt
	MaxRetries          int
	SideEffect          SideEffectClass
	Escalate            bool
	AdversarialVerify   int
	Recovery            RecoveryPolicy
	ReconcileTool       string
	Kind                TaskKind
	Advances            []string
	ExpectedStateChange string
	RecoveryHypothesis  *RecoveryHypothesis
	Execution           ExecutionContract
	Optional            bool
	ResourceClaims      []string
	Resources           []ResourceClaim

	DecisionProfile     string
	DecisionOptions     []DecisionOption
	DecisionAssumptions []DecisionAssumption
	DecisionFacts       map[string]any
	DecisionArtifacts   []ArtifactRef
	DecisionBaseRates   []BaseRateEvidence
	DecisionProvenance  []EvidenceProvenance
}

func newTaskOccurrenceProjection(item *TodoItem) (TaskOccurrenceProjection, error) {
	if item == nil || strings.TrimSpace(item.ID) == "" {
		return TaskOccurrenceProjection{}, fmt.Errorf("task occurrence projection requires a Todo ID")
	}
	return TaskOccurrenceProjection{
		ID: item.ID, PlanTaskID: item.PlanTaskID, PlanFirst: item.PlanFirst, PlanID: item.PlanID,
		Phase: item.Phase, Action: cloneActionPtr(item.Action), ContractID: item.ContractID,
		ContractHash: item.ContractHash, ContractRevision: item.ContractRevision, Agent: item.Agent,
		Desc: item.Desc, Goal: item.Goal, Constraints: item.Constraints, Model: item.Model,
		ModelTopology: cloneModelTopology(item.ModelTopology), Source: item.Source, ParentID: item.ParentID,
		Sidecar: item.Sidecar, Summarize: item.Summarize, OutputMode: item.OutputMode,
		ContextFiles: append([]string(nil), item.ContextFiles...), Requires: append([]string(nil), item.Requires...),
		DependsOn: append([]string(nil), item.DependsOn...), OnFailure: item.OnFailure,
		Verify: item.Verify, VerifyMode: item.VerifyMode, VerifySpec: cloneVerificationSpecPtr(item.VerifySpec),
		WorksetBinding: cloneWorksetBinding(item.WorksetBinding), WorksetReceipt: cloneWorksetReceipt(item.WorksetReceipt),
		MaxRetries: item.MaxRetries, SideEffect: item.SideEffect, Recovery: item.Recovery,
		Escalate: item.Escalate, AdversarialVerify: item.AdversarialVerify,
		ReconcileTool: item.ReconcileTool, Kind: item.Kind, Advances: append([]string(nil), item.Advances...),
		ExpectedStateChange: item.ExpectedStateChange, RecoveryHypothesis: cloneRecoveryHypothesis(item.RecoveryHypothesis),
		Execution: cloneExecutionContract(item.Execution), Optional: item.Optional,
		ResourceClaims: append([]string(nil), item.ResourceClaims...), Resources: append([]ResourceClaim(nil), item.Resources...),
		DecisionProfile:     item.DecisionProfile,
		DecisionOptions:     append([]DecisionOption(nil), item.DecisionOptions...),
		DecisionAssumptions: cloneDecisionAssumptions(item.DecisionAssumptions),
		DecisionFacts:       cloneDecisionFacts(item.DecisionFacts), DecisionArtifacts: append([]ArtifactRef(nil), item.DecisionArtifacts...),
		DecisionBaseRates: cloneBaseRateEvidence(item.DecisionBaseRates), DecisionProvenance: cloneEvidenceProvenance(item.DecisionProvenance),
	}, nil
}

func taskOccurrenceProjectionFromSpec(spec TodoSpec, id string) (TaskOccurrenceProjection, error) {
	return newTaskOccurrenceProjection(todoItemFromSpec(spec, id))
}

// taskOccurrenceProjectionForRetry retains every immutable input from the
// current durable Todo. Retry changes only the attempt number in its admission
// key and the mutable lifecycle projection.
func taskOccurrenceProjectionForRetry(item *TodoItem) (TaskOccurrenceProjection, error) {
	return newTaskOccurrenceProjection(item)
}

// taskOccurrenceProjectionFromTaskDef is a compatibility adapter for pure
// decision-engine callers. It deliberately leaves the runtime ID empty: a
// TaskDef carries a logical plan ID, not the ID of a Todo occurrence. Durable
// coordinator paths must start from TodoSpec/TodoItem and never use this
// adapter to validate an executable occurrence.
func taskOccurrenceProjectionFromTaskDef(task TaskDef, runtimeID string) (TaskOccurrenceProjection, error) {
	goal := task.Goal
	desc := goal
	item := todoItemFromSpec(TodoSpec{
		PlanTaskID: task.ID, PlanFirst: task.PlanFirst, PlanID: task.PlanID, Phase: task.Phase,
		Action: cloneActionPtr(task.Action), ContractID: task.ContractID, ContractHash: task.ContractHash,
		ContractRevision: task.ContractRevision, Agent: task.Agent, Desc: desc, Goal: goal,
		Constraints: task.Constraints, Model: task.Model, ModelTopology: cloneModelTopology(task.ModelTopology),
		Sidecar: task.Sidecar, Summarize: task.Summarize, OutputMode: task.OutputMode,
		ContextFiles: append([]string(nil), task.ContextFiles...), Requires: append([]string(nil), task.Requires...),
		Verify: task.Verify, VerifyMode: task.VerifyMode,
		VerifySpec: cloneVerificationSpecPtr(task.VerifySpec), MaxRetries: task.MaxRetries,
		SideEffect: task.SideEffect, Escalate: task.Escalate, AdversarialVerify: task.AdversarialVerify,
		Recovery: task.Recovery, ReconcileTool: task.ReconcileTool,
		Source: TaskSourceCoordinator,
		Kind:   task.Kind, Advances: append([]string(nil), task.Advances...), ExpectedStateChange: task.ExpectedStateChange,
		RecoveryHypothesis: cloneRecoveryHypothesis(task.RecoveryHypothesis), Execution: cloneExecutionContract(task.Execution),
		Optional: task.Optional, ResourceClaims: append([]string(nil), task.ResourceClaims...), Resources: append([]ResourceClaim(nil), task.Resources...),
		DecisionProfile: task.DecisionProfile, DecisionOptions: append([]DecisionOption(nil), task.DecisionOptions...),
		DecisionAssumptions: cloneDecisionAssumptions(task.DecisionAssumptions), DecisionFacts: cloneDecisionFacts(task.DecisionFacts),
		DecisionArtifacts: append([]ArtifactRef(nil), task.DecisionArtifacts...), DecisionBaseRates: cloneBaseRateEvidence(task.DecisionBaseRates),
		DecisionProvenance: cloneEvidenceProvenance(task.DecisionProvenance),
	}, runtimeID)
	if strings.TrimSpace(runtimeID) == "" {
		// Digest-only compatibility callers do not have an occurrence ID. The
		// projection remains valid for hashing because runtime identity is
		// intentionally represented separately from the logical PlanTaskID.
		item.ID = "compatibility-occurrence"
	}
	return newTaskOccurrenceProjection(item)
}

// compareTaskDefWithTodoOccurrence compares the complete executable contract
// at the scheduler boundary. PlanID is deliberately excluded because it is a
// plan lifecycle marker owned by approval/rejection, not an execution input.
// Every other field used by scheduling or execution is compared exactly;
// empty values are not treated as unspecified.
func compareTaskDefWithTodoOccurrence(task TaskDef, item *TodoItem, indexByID map[string]int) error {
	if item == nil {
		return fmt.Errorf("task occurrence comparison requires a Todo item")
	}
	want := taskDefFromTodoItem(item)
	want.DependsOn = nil
	for _, depID := range item.DependsOn {
		depIndex, ok := indexByID[depID]
		if !ok {
			return fmt.Errorf("task %s durable dependency %q is not in the scheduler batch", item.ID, depID)
		}
		want.DependsOn = append(want.DependsOn, depIndex)
	}
	if item.OnFailure != "" {
		failureIndex, ok := indexByID[item.OnFailure]
		if !ok {
			return fmt.Errorf("task %s durable on_failure target %q is not in the scheduler batch", item.ID, item.OnFailure)
		}
		want.OnFailure = &failureIndex
	}
	// Pipeline and plan ID are lifecycle/compile-time conveniences and have no
	// meaning after the durable occurrence is created.
	task.Pipeline = false
	want.Pipeline = false
	task.PlanID = ""
	want.PlanID = ""
	if !reflect.DeepEqual(task, want) {
		return fmt.Errorf("task %s scheduler contract differs from durable Todo occurrence", item.ID)
	}
	return nil
}

// reconstructSchedulerTaskDefs resolves executable TaskDefs from the current
// Todo occurrences. The caller-supplied slice is used only for the exact
// boundary comparison and for the sanctioned extra-model leaf metadata; no
// mutable TaskDef field becomes an execution input after this function.
func (c *Coordinator) reconstructSchedulerTaskDefs(tasks []TaskDef, items []*TodoItem, duplicates map[int]bool) ([]TaskDef, error) {
	if len(tasks) != len(items) {
		return nil, fmt.Errorf("scheduler task/Todo occurrence count mismatch: %d/%d", len(tasks), len(items))
	}
	indexByID := make(map[string]int, len(items))
	for i, item := range items {
		if item == nil || strings.TrimSpace(item.ID) == "" {
			return nil, fmt.Errorf("scheduler task %d has no durable Todo occurrence", i)
		}
		indexByID[item.ID] = i
	}
	reconstructed := make([]TaskDef, len(items))
	for i, supplied := range tasks {
		item := items[i]
		if err := compareTaskDefWithTodoOccurrence(supplied, item, indexByID); err != nil {
			return nil, err
		}
		canonical := item
		if c != nil {
			if current := c.todoItemByID(item.ID); current != nil {
				canonical = current
			} else if !duplicates[i] {
				return nil, fmt.Errorf("task %s durable Todo occurrence disappeared before scheduler construction", item.ID)
			}
		}
		reconstructed[i] = taskDefFromTodoItem(canonical)
		for _, depID := range canonical.DependsOn {
			depIndex, ok := indexByID[depID]
			if !ok {
				return nil, fmt.Errorf("task %s durable dependency %q is not in the scheduler batch", canonical.ID, depID)
			}
			reconstructed[i].DependsOn = append(reconstructed[i].DependsOn, depIndex)
		}
		if canonical.OnFailure != "" {
			failureIndex, ok := indexByID[canonical.OnFailure]
			if !ok {
				return nil, fmt.Errorf("task %s durable on_failure target %q is not in the scheduler batch", canonical.ID, canonical.OnFailure)
			}
			reconstructed[i].OnFailure = &failureIndex
		}
	}
	return reconstructed, nil
}
