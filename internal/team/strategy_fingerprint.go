package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// StrategyDimension names one structural part of how a task is attempted.
type StrategyDimension string

const (
	StrategyDimensionTaskShape       StrategyDimension = "task_shape"
	StrategyDimensionExecutionTarget StrategyDimension = "execution_target"
	StrategyDimensionDependencyShape StrategyDimension = "dependency_shape"
	StrategyDimensionEvidenceShape   StrategyDimension = "evidence_shape"
	StrategyDimensionToolSequence    StrategyDimension = "tool_sequence"
)

// strategyDimensions is the fixed comparison and output order.
var strategyDimensions = []StrategyDimension{
	StrategyDimensionTaskShape, StrategyDimensionExecutionTarget, StrategyDimensionDependencyShape,
	StrategyDimensionEvidenceShape, StrategyDimensionToolSequence,
}

// StrategyExecutionFingerprint is the runtime-owned structure of one way of
// attempting a task. Every hash covers identities, counts, and digests only:
// never goal or constraint text, tool arguments or output, or memory content.
// An empty hash means the dimension is unknown, such as the tool sequence of
// a task that has not run. RecoveryStrategy is the model-declared strategy;
// it is kept for reference and is not part of Digest, so renaming a strategy
// never changes the fingerprint.
type StrategyExecutionFingerprint struct {
	TaskShapeHash       string           `json:"task_shape_hash,omitempty"`
	ToolSequenceHash    string           `json:"tool_sequence_hash,omitempty"`
	ExecutionTargetHash string           `json:"execution_target_hash,omitempty"`
	DependencyShapeHash string           `json:"dependency_shape_hash,omitempty"`
	EvidenceShapeHash   string           `json:"evidence_shape_hash,omitempty"`
	RecoveryStrategy    RecoveryStrategy `json:"recovery_strategy,omitempty"`
	Digest              string           `json:"digest"`
}

func (f StrategyExecutionFingerprint) hash(dimension StrategyDimension) string {
	switch dimension {
	case StrategyDimensionTaskShape:
		return f.TaskShapeHash
	case StrategyDimensionExecutionTarget:
		return f.ExecutionTargetHash
	case StrategyDimensionDependencyShape:
		return f.DependencyShapeHash
	case StrategyDimensionEvidenceShape:
		return f.EvidenceShapeHash
	case StrategyDimensionToolSequence:
		return f.ToolSequenceHash
	}
	return ""
}

// strategyHash digests one known dimension. It never returns "", which
// means unknown.
func strategyHash(dimension StrategyDimension, parts ...string) string {
	sum := sha256.Sum256([]byte(string(dimension) + "\x1d" + strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:16])
}

// strategyFingerprint computes the fingerprint of item as planned, plus the
// tool sequence of receipt when it is a finished attempt that recorded one.
// agentOf resolves a dependency's todo ID to its agent, so dependency shape
// compares across batches whose IDs differ.
func strategyFingerprint(item *TodoItem, receipt *ExecutionReceipt, agentOf func(string) string) StrategyExecutionFingerprint {
	if item == nil {
		return StrategyExecutionFingerprint{}
	}
	f := StrategyExecutionFingerprint{
		TaskShapeHash:       strategyTaskShapeHash(item),
		DependencyShapeHash: strategyDependencyShapeHash(item, agentOf),
		EvidenceShapeHash:   strategyEvidenceShapeHash(item),
	}
	if item.ExecutionTarget.Validate() == nil {
		parts := []string{item.ExecutionTarget.String(), strings.Join(item.ModelTopology, "\x1e")}
		if route := item.ExecutionRoute; route != nil {
			parts = append(parts, route.Name)
			for _, candidate := range route.Candidates {
				parts = append(parts, candidate.String())
			}
		}
		f.ExecutionTargetHash = strategyHash(StrategyDimensionExecutionTarget, parts...)
	}
	if receipt != nil && receipt.ToolSequence != nil {
		f.ToolSequenceHash = strategyHash(StrategyDimensionToolSequence, append(slices.Clone(receipt.ToolSequence.Tools), "truncated="+strconv.Itoa(receipt.ToolSequence.Truncated))...)
	}
	if item.RecoveryHypothesis != nil {
		f.RecoveryStrategy = item.RecoveryHypothesis.Strategy
	}
	digest := make([]string, 0, len(strategyDimensions))
	for _, dimension := range strategyDimensions {
		digest = append(digest, string(dimension)+"="+f.hash(dimension))
	}
	f.Digest = strategyHash("digest", digest...)
	return f
}

// strategyTaskShapeHash covers who runs the task and under which contract:
// agent, kind, side effect, recovery policy, verification mode, decision
// profile, the bound action and inputs, and the tool surface the occurrence
// may expose.
func strategyTaskShapeHash(item *TodoItem) string {
	parts := []string{
		"agent=" + strings.ToLower(strings.TrimSpace(item.Agent)),
		"kind=" + string(item.Kind),
		"side_effect=" + string(item.SideEffect),
		"recovery=" + string(item.Recovery),
		"reconcile_tool=" + item.ReconcileTool,
		"verify_mode=" + item.VerifyMode,
		"adversarial_verify=" + strconv.Itoa(item.AdversarialVerify),
		"escalate=" + strconv.FormatBool(item.Escalate),
		"plan_first=" + strconv.FormatBool(item.PlanFirst),
		"decision_profile=" + item.DecisionProfile,
		"contract=" + item.ContractID,
		"run_inputs=" + item.RunInputSnapshotHash + "|" + sortedPairs(item.BoundInputs),
		"action_payload=" + item.MaterializedActionPayloadHash,
	}
	if item.Action != nil {
		parts = append(parts, "action="+item.Action.Capability+"/"+item.Action.Type)
	}
	if item.CatalogAction != nil {
		catalog, _ := json.Marshal(item.CatalogAction)
		parts = append(parts, "catalog_action="+string(catalog))
	}
	steps, _ := json.Marshal(item.Execution.Steps)
	parts = append(parts, "execution_steps="+string(steps))
	if auth := item.DynamicToolAuthorization; auth != nil {
		targets := make([]string, 0, len(auth.Targets))
		for _, target := range auth.Targets {
			targets = append(targets, target.Name)
		}
		parts = append(parts, "dynamic_tools="+sortedJoin(targets), "static_tools="+sortedJoin(auth.StaticToolCeiling))
	}
	return strategyHash(StrategyDimensionTaskShape, parts...)
}

// strategyDependencyShapeHash covers what the task waits for, by the agents
// of its dependencies rather than their per-batch todo IDs.
func strategyDependencyShapeHash(item *TodoItem, agentOf func(string) string) string {
	agents := make([]string, 0, len(item.DependsOn))
	for _, id := range item.DependsOn {
		agent := ""
		if agentOf != nil {
			agent = strings.ToLower(strings.TrimSpace(agentOf(id)))
		}
		agents = append(agents, agent)
	}
	return strategyHash(StrategyDimensionDependencyShape, "depends_on="+sortedJoin(agents), "order_after="+strconv.Itoa(len(item.OrderAfter)))
}

// strategyEvidenceShapeHash covers what the task is given and how success
// is proven: its verification, the completed tasks whose evidence it reads,
// the files it is pointed at, and its declared requirements.
func strategyEvidenceShapeHash(item *TodoItem) string {
	return strategyHash(StrategyDimensionEvidenceShape,
		"verification="+stableOperation(item),
		"evidence_from="+strconv.Itoa(len(item.EvidenceFrom)),
		"context_files="+sortedJoin(item.ContextFiles),
		"requires="+sortedJoin(item.Requires),
	)
}

// compareStrategyFingerprints lists the dimensions in which candidate
// differs from previous. A dimension unknown on either side cannot show a
// change and is listed as unknown instead.
func compareStrategyFingerprints(previous, candidate StrategyExecutionFingerprint) (changed, unknown []StrategyDimension) {
	for _, dimension := range strategyDimensions {
		before, after := previous.hash(dimension), candidate.hash(dimension)
		switch {
		case before == "" || after == "":
			unknown = append(unknown, dimension)
		case before != after:
			changed = append(changed, dimension)
		}
	}
	return changed, unknown
}

// MateriallyDifferentStrategy reports whether candidate differs from previous
// in at least one structural dimension both sides recorded. A different
// recovery strategy name or difference_from_prior text alone is not a
// material change, and neither is a dimension only one side recorded.
func MateriallyDifferentStrategy(previous, candidate StrategyExecutionFingerprint) bool {
	changed, _ := compareStrategyFingerprints(previous, candidate)
	return len(changed) > 0
}
