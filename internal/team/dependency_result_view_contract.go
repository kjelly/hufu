package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

const selectedDependencyContractRevision = 2
const selectedDependencyPolicyVersion = 6

func dependencyResultViewsHashForVersion(session *TeamSession, version int) (string, error) {
	hash, err := dependencyResultViewsHash(session)
	if err != nil {
		return "", err
	}
	if hash != "" && version < selectedDependencyPolicyVersion {
		return "", fmt.Errorf("historical execution policy cannot admit selected dependency views")
	}
	if hash == "" && version == selectedDependencyPolicyVersion {
		return "", fmt.Errorf("selected dependency policy requires its original view configuration")
	}
	return hash, nil
}

func dependencyContractRevision(view *DependencyResultViewSpec) int {
	if view != nil {
		return selectedDependencyContractRevision
	}
	return effectiveTaskContractRevision
}

func validateDependencyResultViewTask(task TaskDef, def *agent.AgentDef, bound bool) error {
	view, err := task.DependencyResultView.normalized()
	if err != nil || view == nil {
		return err
	}
	if def == nil || isCoordinatorRole(def) || normalizedName(def.Role) == "sidecar" {
		return fmt.Errorf("selected dependency-result-view requires a known ordinary worker")
	}
	if bound && (task.ContractID == "" || task.ContractHash == "" || task.ContractRevision != selectedDependencyContractRevision) {
		return fmt.Errorf("selected dependency-result-view requires a bound static contract")
	}
	if task.Execution.RequiresEvidence || len(task.EvidenceFrom) > 0 || task.ResultContractSpec != nil || task.ResultContract != nil {
		return fmt.Errorf("selected dependency-result-view cannot restrict required evidence or result contracts")
	}
	if task.Action != nil || task.CatalogAction != nil || len(task.Execution.Steps) > 0 || task.Sidecar || task.PlanFirst || len(task.Execution.ToolSequence) > 0 {
		return fmt.Errorf("selected dependency-result-view supports ordinary workers without plan-first or closed tool sequences")
	}
	if def != nil && (def.Requirements.PlanFirst || def.ResultContract != nil) {
		return fmt.Errorf("selected dependency-result-view conflicts with the worker's plan/result contract")
	}
	if !bound && len(task.DependsOnContracts) == 0 && len(task.DependsOn) == 0 && !task.Pipeline {
		return fmt.Errorf("selected dependency-result-view requires same-batch dependencies")
	}
	return nil
}

func dependencyResultViewFindings(session *TeamSession, index int, task TaskDef) []ContractFinding {
	view, err := task.DependencyResultView.normalized()
	if err == nil && view == nil {
		return nil
	}
	if err == nil && session.Config.Requirements.PlanFirst {
		err = fmt.Errorf("selected dependency-result-view cannot restrict plan-first workers")
	}
	if err == nil && !session.Config.Delegation.BindInitialTaskContracts && !session.Config.Delegation.BindTaskGoalContracts {
		err = fmt.Errorf("selected dependency-result-view requires static contract binding")
	}
	if err == nil {
		err = validateDependencyResultViewTask(task, session.Agents[strings.ToLower(strings.TrimSpace(task.Agent))], false)
	}
	if err == nil {
		for _, invariant := range session.Config.Delegation.TaskGoalInvariants {
			if contractEdgeKey(invariant.ContractID) == contractEdgeKey(task.ID) && len(invariant.RequiredToolSequence) > 0 {
				err = fmt.Errorf("selected dependency-result-view cannot restrict a closed tool sequence")
				break
			}
		}
	}
	if err == nil {
		return nil
	}
	return []ContractFinding{contractFinding(fmt.Sprintf("tasks[%d].dependency-result-view", index), "dependency_result_view_invalid", err.Error())}
}

// A separate optional hash pins selected policies even for positional static
// dependencies that do not participate in the named-edge workflow hash.
func dependencyResultViewsHash(session *TeamSession) (string, error) {
	type binding struct {
		ID    string                    `json:"id"`
		Agent string                    `json:"agent"`
		View  *DependencyResultViewSpec `json:"view"`
	}
	var bindings []binding
	for _, task := range session.ContractTasks {
		view, err := task.DependencyResultView.normalized()
		if err != nil {
			return "", err
		}
		if view != nil {
			bindings = append(bindings, binding{ID: contractEdgeKey(task.ID), Agent: strings.ToLower(strings.TrimSpace(task.Agent)), View: view})
		}
	}
	if len(bindings) == 0 {
		return "", nil
	}
	slices.SortFunc(bindings, func(a, b binding) int {
		if n := strings.Compare(a.ID, b.ID); n != 0 {
			return n
		}
		return strings.Compare(a.Agent, b.Agent)
	})
	data, err := json.Marshal(bindings)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
