package team

import (
	"cmp"
	"fmt"
	"slices"
)

// ContractRecoveryChange summarizes a binding change without exposing schema
// bodies, commands, or historical model output in the preview.
type ContractRecoveryChange struct {
	Owner    string            `json:"owner"`
	Original ResultContractRef `json:"original"`
	Accepted ResultContractRef `json:"accepted"`
}

func (c *Coordinator) describeContractRecovery(plan *ContractRecoveryPlan, source *SessionData, old, current *ExecutionPolicySnapshot) {
	oldRefs := map[string]ResultContractRef{}
	for _, binding := range old.ResultContracts {
		oldRefs[binding.Owner] = recoveryBindingRef(binding)
	}
	for _, binding := range current.ResultContracts {
		ref := recoveryBindingRef(binding)
		if previous := oldRefs[binding.Owner]; previous != ref {
			plan.ContractChanges = append(plan.ContractChanges, ContractRecoveryChange{Owner: binding.Owner, Original: previous, Accepted: ref})
		}
	}
	slices.SortFunc(plan.ContractChanges, func(a, b ContractRecoveryChange) int { return cmp.Compare(a.Owner, b.Owner) })
	patches := map[string]*ContractRecoveryTask{}
	for i := range plan.Tasks {
		patches[plan.Tasks[i].ID] = &plan.Tasks[i]
	}
	for _, item := range source.Tasks {
		if item == nil {
			continue
		}
		patch := patches[item.ID]
		if patch == nil || patch.Action != "retry" {
			continue
		}
		for _, id := range append(slices.Clone(item.DependsOn), item.EvidenceFrom...) {
			if parent := patches[id]; parent != nil && (parent.Action == "retry" || !sameCanonicalEvidence(parent.ResultContract, findRecoverySourceRef(source, id))) {
				patch.AffectedBy = append(patch.AffectedBy, id)
			}
		}
		slices.Sort(patch.AffectedBy)
		patch.AffectedBy = slices.Compact(patch.AffectedBy)
		if patch.Reason == "accepted_result" || patch.Reason == "result_revalidated" {
			patch.Reason = "upstream_changed"
		}
		if item.Verify != "" || item.VerifySpec != nil {
			plan.ReopenedVerification = append(plan.ReopenedVerification, item.ID)
		}
	}
	c.mu.RLock()
	var spec *AcceptanceSpec
	if c.acceptanceSpec != nil {
		value := cloneAcceptanceSpec(*c.acceptanceSpec)
		spec = &value
	}
	command := c.acceptanceCmd
	c.mu.RUnlock()
	if n := len(source.AcceptanceContractRevisions); n > 0 {
		value := source.AcceptanceContractRevisions[n-1].NewSpec
		spec = &value
	}
	if spec == nil && command != "" {
		spec = &AcceptanceSpec{Commands: []string{command}}
	}
	if spec != nil {
		plan.ReopenedAcceptance = recoveryAcceptanceItems(*spec)
	}
	// Frozen request requirements are independent of the team's acceptance spec.
	selected := map[string]bool{}
	for _, item := range source.Tasks {
		if item != nil {
			selected[item.RunInputSnapshotID] = true
		}
	}
	for i := range source.RunInputSnapshots {
		snapshot := &source.RunInputSnapshots[i]
		if !selected[snapshot.ID] {
			continue
		}
		contract, err := RequestAcceptanceContractFromSnapshot(snapshot)
		if err != nil {
			continue
		}
		for _, requirement := range contract.Requirements {
			plan.ReopenedAcceptance = append(plan.ReopenedAcceptance, "request:"+snapshot.ID+"/"+requirement.ID)
		}
	}
	slices.Sort(plan.ReopenedAcceptance)
	plan.ReopenedAcceptance = slices.Compact(plan.ReopenedAcceptance)
	slices.Sort(plan.ReopenedVerification)
}

func findRecoverySourceRef(source *SessionData, id string) *ResultContractRef {
	for _, item := range source.Tasks {
		if item != nil && item.ID == id {
			return item.ResultContract
		}
	}
	return nil
}

func recoveryBindingRef(binding ExecutionResultContractPolicySnapshot) ResultContractRef {
	return ResultContractRef{ID: binding.ID, SchemaSHA256: binding.SchemaSHA256, RequireStructured: binding.RequireStructured}
}

func recoveryAcceptanceItems(spec AcceptanceSpec) []string {
	var items []string
	for _, criterion := range spec.Criteria {
		items = append(items, "criterion:"+criterion.ID)
	}
	for i := range spec.Commands {
		items = append(items, fmt.Sprintf("command[%d]", i))
	}
	for i := range spec.Verifications {
		items = append(items, fmt.Sprintf("verification[%d]", i))
	}
	for _, worker := range spec.RequiredWorkers {
		items = append(items, "required_worker:"+worker)
	}
	for i := range spec.RequiredArtifacts {
		items = append(items, fmt.Sprintf("required_artifact[%d]", i))
	}
	if spec.RequireNoUnresolvedTasks {
		items = append(items, "no_unresolved_tasks")
	}
	return items
}
