package team

import (
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/agent"
)

func selectedLegacySubmission(session *SessionData, id string) *TodoItem {
	if session == nil || id == "" {
		return nil
	}
	item := todoItemByID(session.Tasks, id)
	if legacySubmissionTarget(item) {
		return item
	}
	return nil
}

// Only this agent's explicit data channel may change. Defaults, generation,
// other agents, tool ceilings, inputs, models and response channels remain pinned.
func normalizeLegacyTransport(old, current *ExecutionPolicySnapshot, agentName string) error {
	if old.ResultCollection == nil || current.ResultCollection == nil {
		return fmt.Errorf("legacy migration requires pinned result transport")
	}
	var before, after agent.ResultTransport
	for _, e := range old.ResultCollection.Agents {
		if e.Agent == agentName {
			before = e.Transport
		}
	}
	for _, e := range current.ResultCollection.Agents {
		if e.Agent == agentName {
			after = e.Transport
		}
	}
	if before != agent.ResultTransportTool || after != agent.ResultTransportRuntime {
		return fmt.Errorf("legacy migration only permits explicit tool to runtime transport")
	}
	current.ResultCollection = cloneExecutionResultCollectionPolicy(current.ResultCollection)
	for i := range current.ResultCollection.Agents {
		if current.ResultCollection.Agents[i].Agent == agentName {
			current.ResultCollection.Agents[i].Transport = before
		}
	}
	return nil
}

func legacyPlanSource(plan *ContractRecoveryPlan, tasks []*TodoItem) (*TodoItem, *LegacySubmissionRecovery, error) {
	var item *TodoItem
	var proof *LegacySubmissionRecovery
	for _, patch := range plan.Tasks {
		if patch.LegacySubmission != nil {
			if proof != nil {
				return nil, nil, fmt.Errorf("only one legacy submission may be migrated")
			}
			item = todoItemByID(tasks, patch.ID)
			proof = patch.LegacySubmission
			if err := validateLegacySubmissionSource(item, proof, plan); err != nil {
				return nil, nil, err
			}
			var sources []*TodoItem
			for _, id := range resultDependencyIDs(item) {
				source := todoItemByID(tasks, id)
				if source == nil || source.Status != TaskDone || source.TypedResult == nil {
					return nil, nil, fmt.Errorf("legacy canonical source missing")
				}
				sources = append(sources, source)
			}
			digest, err := collectionDigest(canonicalCollectionSources(sources))
			if err != nil || digest != proof.EvidenceSHA256 {
				return nil, nil, fmt.Errorf("legacy canonical source binding changed")
			}
			if patch.Collection != nil || patch.ResultContract == nil || *patch.ResultContract != *item.ResultContract {
				return nil, nil, fmt.Errorf("legacy migration cannot change result contracts")
			}
		}
	}
	return item, proof, nil
}

func validateLegacyPolicyRecovery(old, current *ExecutionPolicySnapshot, plan *ContractRecoveryPlan, tasks []*TodoItem) error {
	item, p, err := legacyPlanSource(plan, tasks)
	if err != nil || p == nil {
		return err
	}
	if !sameCanonicalEvidence(old.ResultContracts, current.ResultContracts) {
		return fmt.Errorf("legacy recovery cannot also upgrade schemas")
	}
	if err := normalizeLegacyTransport(old, current, item.Agent); err != nil {
		return err
	}
	for _, patch := range plan.Tasks {
		if patch.ID != item.ID && patch.Action != "retain" {
			return fmt.Errorf("legacy recovery cannot replay other tasks")
		}
	}
	return nil
}

func legacyAdmissionMatches(item *TodoItem, admission DecisionAdmission) bool {
	cp := currentResultCollection(item)
	return cp != nil && cp.LegacySubmission != nil && validateLegacyCollection(cp) == nil && !admission.Enabled && admission.Profile == DecisionProfileOff && admission.TaskInputDigest == cp.LegacySubmission.OriginalTaskInputSHA256
}

func validateLegacyExecutionChange(original, accepted ExecutionContract) bool {
	if !slices.Equal(original.ToolSequence, []string{submitResultToolName}) {
		return false
	}
	expected := cloneExecutionContract(original)
	expected.ToolSequence = nil
	return sameCanonicalEvidence(expected, accepted)
}
