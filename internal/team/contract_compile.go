package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// EffectiveTaskContract is the immutable, machine-enforced contract attached
// to a dispatched task. Coordinator and worker prose may describe the goal,
// but execution, output, and evidence behavior originate here.
type EffectiveTaskContract struct {
	ID         string            `json:"id"`
	Revision   int               `json:"revision"`
	Hash       string            `json:"hash"`
	Agent      string            `json:"agent"`
	Execution  ExecutionContract `json:"execution"`
	OutputMode string            `json:"output_mode"`
}

const effectiveTaskContractRevision = 1

// CompileInitialTaskContracts creates effective contracts for either the full
// initial batch or, when exact batching is disabled, an ordered subset of that
// batch. The coordinator is responsible for ensuring that a subset contains
// only workers that have not already been dispatched in the initial phase.
// It rejects absent, duplicate, and conflicting static contracts before any
// TODO, provider call, or retry accounting can be created.
func CompileInitialTaskContracts(session *TeamSession, tasks []TaskDef) ([]TaskDef, []EffectiveTaskContract, error) {
	if session == nil || !session.Config.Delegation.BindInitialTaskContracts || !matchesInitialContractBatch(tasks, session.Config.Delegation) {
		return tasks, nil, nil
	}
	contracts := make(map[string]TaskDef, len(session.ContractTasks))
	for _, contract := range session.ContractTasks {
		name := strings.ToLower(strings.TrimSpace(contract.Agent))
		if name == "" {
			return nil, nil, fmt.Errorf("initial task contract is missing an agent name")
		}
		if _, exists := contracts[name]; exists {
			return nil, nil, fmt.Errorf("initial task contract is duplicated for agent %q", contract.Agent)
		}
		contracts[name] = contract
	}

	bound := append([]TaskDef(nil), tasks...)
	effective := make([]EffectiveTaskContract, 0, len(bound))
	for i := range bound {
		name := strings.ToLower(strings.TrimSpace(bound[i].Agent))
		contract, ok := contracts[name]
		if !ok {
			return nil, nil, fmt.Errorf("initial task contract is missing for agent %q", bound[i].Agent)
		}
		if !executionContractsEqualOrEmpty(bound[i].Execution, contract.Execution) {
			return nil, nil, fmt.Errorf("initial task contract conflict for agent %q: execution", bound[i].Agent)
		}
		if bound[i].OutputMode != "" && bound[i].OutputMode != contract.OutputMode {
			return nil, nil, fmt.Errorf("initial task contract conflict for agent %q: output_mode", bound[i].Agent)
		}
		contractID := strings.TrimSpace(contract.ID)
		if contractID == "" {
			contractID = name
		}
		hash, err := effectiveContractHash(contractID, name, contract.Execution, contract.OutputMode)
		if err != nil {
			return nil, nil, fmt.Errorf("hash initial task contract %q: %w", contractID, err)
		}
		bound[i].Execution = contract.Execution
		bound[i].OutputMode = contract.OutputMode
		bound[i].ContractID = contractID
		bound[i].ContractHash = hash
		bound[i].ContractRevision = effectiveTaskContractRevision
		effective = append(effective, EffectiveTaskContract{ID: contractID, Revision: effectiveTaskContractRevision, Hash: hash, Agent: name, Execution: contract.Execution, OutputMode: contract.OutputMode})
	}
	return bound, effective, nil
}

func matchesInitialContractBatch(tasks []TaskDef, policy agent.DelegationPolicy) bool {
	if sameAgentSequence(tasks, policy.InitialBatch) {
		return true
	}
	if policy.RequireExactInitialBatch || len(tasks) == 0 {
		return false
	}
	initial := make(map[string]bool, len(policy.InitialBatch))
	for _, name := range policy.InitialBatch {
		initial[strings.ToLower(strings.TrimSpace(name))] = true
	}
	seen := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		name := strings.ToLower(strings.TrimSpace(task.Agent))
		if name == "" || !initial[name] || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

func executionContractsEqualOrEmpty(got, want ExecutionContract) bool {
	if len(got.Steps) == 0 && len(got.ToolSequence) == 0 && got.Kind == "" && !got.RequiresResult && !got.RequiresVerification && got.AllowsReplay == nil && !got.ForbidArtifacts && len(got.ToolInputSequence) == 0 && got.ToolInputField == "" && len(got.ToolInputValueSequence) == 0 && len(got.ToolExpectedExitCodes) == 0 {
		return true
	}
	left, leftErr := json.Marshal(got)
	right, rightErr := json.Marshal(want)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func effectiveContractHash(id, agent string, execution ExecutionContract, outputMode string) (string, error) {
	payload := struct {
		ID         string            `json:"id"`
		Revision   int               `json:"revision"`
		Agent      string            `json:"agent"`
		Execution  ExecutionContract `json:"execution"`
		OutputMode string            `json:"output_mode"`
	}{id, effectiveTaskContractRevision, agent, execution, outputMode}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// ValidateTeamTaskContracts validates static contracts without interpreting
// worker/coordinator prose. It is safe to run at load time and from the CLI.
func ValidateTeamTaskContracts(session *TeamSession) []ContractFinding {
	if session == nil {
		return nil
	}
	var findings []ContractFinding
	if minimum := session.Config.MinimumCoordinatorRounds; minimum > 0 && session.Config.MaxRounds > 0 && session.Config.MaxRounds < minimum {
		findings = append(findings, contractFinding("max-rounds", "max_rounds_below_minimum_coordinator_rounds", fmt.Sprintf("max-rounds (%d) must be at least minimum-coordinator-rounds (%d); coordinator progress and task retry budgets are separate", session.Config.MaxRounds, minimum)))
	}
	for index, invariant := range session.Config.Delegation.TaskGoalInvariants {
		field := fmt.Sprintf("delegation.task-goal-invariants[%d]", index)
		if strings.TrimSpace(invariant.Agent) == "" {
			findings = append(findings, contractFinding(field+".agent", "task_goal_invariant_agent_missing", "task-goal invariant must name an agent"))
		}
		if strings.TrimSpace(invariant.WhenGoalContains) == "" {
			findings = append(findings, contractFinding(field+".when-goal-contains", "task_goal_invariant_selector_missing", "task-goal invariant must select a goal substring"))
		}
		if len(invariant.RequiredLiterals) == 0 && len(invariant.ForbiddenLiterals) == 0 && len(invariant.RequiredToolSequence) == 0 && len(invariant.ForbiddenExecutionFields) == 0 {
			findings = append(findings, contractFinding(field, "task_goal_invariant_empty", "task-goal invariant must constrain a literal or execution contract field"))
		}
		for literalIndex, literal := range append(append([]string(nil), invariant.RequiredLiterals...), invariant.ForbiddenLiterals...) {
			if strings.TrimSpace(literal) == "" {
				findings = append(findings, contractFinding(fmt.Sprintf("%s.literals[%d]", field, literalIndex), "task_goal_invariant_literal_empty", "task-goal invariant literals must not be empty"))
			}
		}
		for fieldIndex, executionField := range invariant.ForbiddenExecutionFields {
			if !knownExecutionContractField(executionField) {
				findings = append(findings, contractFinding(fmt.Sprintf("%s.forbidden-execution-fields[%d]", field, fieldIndex), "task_goal_invariant_execution_field_unknown", "task-goal invariant names an unknown execution contract field"))
			}
		}
	}
	if !session.Config.Delegation.BindInitialTaskContracts {
		return findings
	}
	initial := session.Config.Delegation.InitialBatch
	if len(initial) == 0 {
		return []ContractFinding{{Severity: FindingSeverityError, Code: "initial_contract_batch_missing", Field: "delegation.initial-batch", Message: "bind-contracts requires a non-empty initial-batch.agents list"}}
	}
	byAgent := make(map[string]TaskDef)
	for index, task := range session.ContractTasks {
		field := fmt.Sprintf("tasks[%d]", index)
		name := strings.ToLower(strings.TrimSpace(task.Agent))
		if name == "" {
			findings = append(findings, contractFinding(field+".agent", "initial_contract_agent_missing", "static task contract has no agent"))
			continue
		}
		if _, exists := byAgent[name]; exists {
			findings = append(findings, contractFinding(field+".agent", "initial_contract_duplicate", fmt.Sprintf("multiple static contracts exist for agent %q", task.Agent)))
			continue
		}
		byAgent[name] = task
		def := session.Agents[name]
		if def == nil {
			findings = append(findings, contractFinding(field+".agent", "initial_contract_agent_unknown", fmt.Sprintf("static contract agent %q is not a loaded worker", task.Agent)))
		} else {
			findings = append(findings, staticContractToolFindings(field, task, def.Tools, def.MCPTools, session.Config.ToolsDenied)...)
		}
		if err := validateTaskOutputMode(task); err != nil {
			findings = append(findings, contractFinding(field+".output_mode", "initial_contract_output_mode", err.Error()))
		}
		for _, finding := range ValidateExecutionContractFull(task, "error").Findings {
			if finding.Severity == FindingSeverityError {
				finding.Field = field + "." + finding.Field
				findings = append(findings, finding)
			}
		}
	}
	seenIDs := map[string]bool{}
	for _, name := range initial {
		key := strings.ToLower(strings.TrimSpace(name))
		task, ok := byAgent[key]
		if !ok {
			findings = append(findings, contractFinding("delegation.initial-batch", "initial_contract_missing", fmt.Sprintf("initial worker %q has no static task contract", name)))
			continue
		}
		id := strings.TrimSpace(task.ID)
		if id == "" {
			id = key
		}
		if seenIDs[id] {
			findings = append(findings, contractFinding("tasks", "initial_contract_id_duplicate", fmt.Sprintf("static contract ID %q is not unique", id)))
		}
		seenIDs[id] = true
	}
	return findings
}

func staticContractToolFindings(field string, task TaskDef, declaredTools string, mcpTools map[string]agent.MCPToolConfig, denied []string) []ContractFinding {
	// An empty worker tool declaration retains legacy "all available" behavior;
	// it cannot be rejected statically. Otherwise every closed/structured tool
	// must be declared by the worker (submit_result is coordinator supplied).
	if strings.TrimSpace(declaredTools) == "" {
		return nil
	}
	allowed := make(map[string]bool)
	for _, tool := range strings.Split(declaredTools, ",") {
		allowed[strings.TrimSpace(tool)] = true
	}
	for name := range mcpTools {
		allowed[name] = true
	}
	deniedSet := make(map[string]bool)
	for _, tool := range denied {
		deniedSet[strings.TrimSpace(tool)] = true
	}
	tools := append([]string(nil), task.Execution.ToolSequence...)
	for _, step := range task.Execution.Steps {
		tools = append(tools, step.Tool)
	}
	var findings []ContractFinding
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if tool == "" || tool == "submit_result" {
			continue
		}
		if deniedSet[tool] {
			findings = append(findings, contractFinding(field+".execution", "initial_contract_tool_denied", fmt.Sprintf("contract tool %q is denied by team policy", tool)))
		} else if !allowed[tool] {
			findings = append(findings, contractFinding(field+".execution", "initial_contract_tool_unauthorized", fmt.Sprintf("contract tool %q is not authorized for its worker", tool)))
		}
	}
	return findings
}

func contractFinding(field, code, message string) ContractFinding {
	return ContractFinding{Severity: FindingSeverityError, Code: code, Field: field, Message: message}
}

func sortedContractFindingMessages(findings []ContractFinding) []string {
	messages := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.Severity == FindingSeverityError {
			messages = append(messages, fmt.Sprintf("%s: %s", finding.Field, finding.Message))
		}
	}
	sort.Strings(messages)
	return messages
}
