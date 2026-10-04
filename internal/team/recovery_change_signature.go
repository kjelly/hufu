package team

import (
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// RecoveryDimension names one input of an attempt that a retry may change.
type RecoveryDimension string

const (
	RecoveryDimensionExecutionTarget RecoveryDimension = "execution_target"
	RecoveryDimensionModelExecution  RecoveryDimension = "model_execution"
	RecoveryDimensionAction          RecoveryDimension = "action"
	RecoveryDimensionRunInputs       RecoveryDimension = "run_inputs"
	RecoveryDimensionDependencyGraph RecoveryDimension = "dependency_graph"
	RecoveryDimensionContextManifest RecoveryDimension = "context_manifest"
	RecoveryDimensionDynamicTools    RecoveryDimension = "dynamic_tools"
)

// recoveryDimensions is the fixed output order.
var recoveryDimensions = []RecoveryDimension{
	RecoveryDimensionExecutionTarget, RecoveryDimensionModelExecution, RecoveryDimensionAction, RecoveryDimensionRunInputs,
	RecoveryDimensionDependencyGraph, RecoveryDimensionContextManifest, RecoveryDimensionDynamicTools,
}

// recoveryNotTracked lists what this comparison never shows changed. Input
// hashes, artifact revisions, and the failed criterion are not durable per
// attempt; the tool sequence is (ExecutionReceipt.ToolSequence) but is the
// attempt's behavior, not an input. Every observation names them.
var recoveryNotTracked = []string{"tool_sequence", "tool_input_hashes", "artifact_revisions", "failed_criterion"}

// RecoveryDimensionState is a dimension's state for one attempt. Unknown
// means the attempt did not record enough to compare it, which is never read
// as "unchanged".
type RecoveryDimensionState string

const (
	RecoveryDimensionKnown         RecoveryDimensionState = "known"
	RecoveryDimensionUnknown       RecoveryDimensionState = "unknown"
	RecoveryDimensionNotApplicable RecoveryDimensionState = "not_applicable"
)

// RecoveryDimensionValue is one dimension of a signature. Digest is a hash of
// identities and hashes the attempt recorded, never content.
type RecoveryDimensionValue struct {
	State  RecoveryDimensionState `json:"state"`
	Digest string                 `json:"digest,omitempty"`
}

// RecoveryChangeSignature describes what an attempt ran with. It excludes
// run, task, and attempt identities, so two attempts with the same inputs
// have the same signature.
type RecoveryChangeSignature map[RecoveryDimension]RecoveryDimensionValue

// RecoveryComparison is the result of comparing an attempt with the previous
// attempt of the same task occurrence.
type RecoveryComparison string

const (
	RecoveryChangeDetected     RecoveryComparison = "change_detected"
	RecoveryNoStructuralChange RecoveryComparison = "no_structural_change"
	RecoveryComparisonUnknown  RecoveryComparison = "unknown"
)

// recoveryReasonNoPriorAttempt marks a first attempt, which has nothing to
// compare with.
const recoveryReasonNoPriorAttempt = "no_prior_attempt"

// recoveryPerAttemptContextIDs are context fragments the runtime writes for
// each attempt (the previous failure, the execution context). They differ on
// every retry by construction, so they are not part of the attempt's inputs.
var recoveryPerAttemptContextIDs = map[string]bool{"retry_failure_context": true, "runtime_context": true}

// recoveryChangeSignature computes the signature of one finished attempt from
// its Todo and receipt.
func recoveryChangeSignature(item *TodoItem, receipt ExecutionReceipt) RecoveryChangeSignature {
	signature := RecoveryChangeSignature{}
	unknown := RecoveryDimensionValue{State: RecoveryDimensionUnknown}
	known := func(parts ...string) RecoveryDimensionValue {
		return RecoveryDimensionValue{State: RecoveryDimensionKnown, Digest: hashContentKey(strings.Join(parts, "\x1f"))}
	}

	// A receipt without its own target (a coordinator-run task, or a legacy
	// in-dispatch escalation) cannot say which target ran.
	signature[RecoveryDimensionExecutionTarget] = unknown
	if receipt.ExecutionTarget.Validate() == nil {
		fallback, candidate := "", ""
		if receipt.FallbackFrom != nil {
			fallback = receipt.FallbackFrom.String()
		}
		if receipt.CandidateIndex != nil {
			candidate = strconv.Itoa(*receipt.CandidateIndex)
		}
		signature[RecoveryDimensionExecutionTarget] = known(receipt.ExecutionTarget.String(), fallback, candidate)
	}

	signature[RecoveryDimensionModelExecution] = unknown
	if id := strings.TrimSpace(receipt.ModelExecutionID); id != "" {
		signature[RecoveryDimensionModelExecution] = known(id)
	}

	signature[RecoveryDimensionAction] = RecoveryDimensionValue{State: RecoveryDimensionNotApplicable}
	if item.Action != nil || item.CatalogAction != nil {
		capability, actionType := "", ""
		if item.Action != nil {
			capability, actionType = item.Action.Capability, item.Action.Type
		}
		signature[RecoveryDimensionAction] = known(item.ContractID, capability, actionType, item.MaterializedActionPayloadHash)
	}

	signature[RecoveryDimensionRunInputs] = RecoveryDimensionValue{State: RecoveryDimensionNotApplicable}
	if item.RunInputSnapshotHash != "" || len(item.BoundInputs) > 0 {
		signature[RecoveryDimensionRunInputs] = known(item.RunInputSnapshotHash, sortedPairs(item.BoundInputs))
	}

	steps, _ := json.Marshal(item.Execution.Steps)
	signature[RecoveryDimensionDependencyGraph] = known(sortedJoin(item.DependsOn), sortedJoin(item.OrderAfter), string(steps))

	signature[RecoveryDimensionContextManifest] = unknown
	if receipt.ContextManifest != nil {
		var included []string
		for _, entry := range receipt.ContextManifest.Items {
			if entry.Included && !recoveryPerAttemptContextIDs[entry.ID] {
				included = append(included, entry.ID+"="+entry.ContentHash)
			}
		}
		sort.Strings(included)
		signature[RecoveryDimensionContextManifest] = known(included...)
	}

	signature[RecoveryDimensionDynamicTools] = unknown
	if receipt.ToolInvocationsTruncated == 0 {
		calls := make([]string, 0, len(receipt.ToolInvocations))
		for _, invocation := range receipt.ToolInvocations {
			calls = append(calls, invocation.LogicalTool+"@"+invocation.DescriptorSHA256)
		}
		signature[RecoveryDimensionDynamicTools] = known(calls...)
	}
	return signature
}

// compareRecoverySignatures compares an attempt with the previous one. A
// dimension that differs between known values is changed; one that is
// unknown on either side cannot be compared. Not-applicable dimensions are
// skipped unless only one side has them.
func compareRecoverySignatures(previous, current RecoveryChangeSignature) (RecoveryComparison, []RecoveryDimension, []RecoveryDimension) {
	var changed, unknown []RecoveryDimension
	for _, dimension := range recoveryDimensions {
		before, after := previous[dimension], current[dimension]
		switch {
		case before.State == RecoveryDimensionNotApplicable && after.State == RecoveryDimensionNotApplicable:
		case before.State == RecoveryDimensionUnknown || after.State == RecoveryDimensionUnknown || before.State == "" || after.State == "":
			unknown = append(unknown, dimension)
		case before.State != after.State || before.Digest != after.Digest:
			changed = append(changed, dimension)
		}
	}
	switch {
	case len(changed) > 0:
		return RecoveryChangeDetected, changed, unknown
	case len(unknown) == 0:
		return RecoveryNoStructuralChange, nil, nil
	default:
		return RecoveryComparisonUnknown, nil, unknown
	}
}

func sortedJoin(values []string) string {
	sorted := slices.Clone(values)
	sort.Strings(sorted)
	return strings.Join(sorted, "\x1e")
}

func sortedPairs(values map[string]string) string {
	pairs := make([]string, 0, len(values))
	for key, value := range values {
		pairs = append(pairs, key+"="+value)
	}
	return sortedJoin(pairs)
}
