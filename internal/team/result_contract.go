package team

import (
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kjelly/hufu/internal/agent"
)

// resultContractSpec names the authoring type where an `agent` identifier
// shadows the package (for example effectiveContractHash's parameter).
type resultContractSpec = agent.ResultContractSpec

// ResultContractRef is the durable identity of the result contract bound to
// one task occurrence. Only this reference is persisted; the schema itself
// lives in the loaded team and its hash is part of the execution policy
// snapshot, so a changed schema fails resume closed instead of silently
// re-validating an admitted occurrence against different rules.
type ResultContractRef struct {
	// ID is the schema path relative to the team directory, with forward
	// slashes.
	ID string `json:"id"`
	// SchemaSHA256 is the hex SHA-256 of the canonical schema bytes.
	SchemaSHA256      string `json:"schema_sha256"`
	RequireStructured bool   `json:"require_structured,omitempty"`
}

func (r *ResultContractRef) clone() *ResultContractRef {
	if r == nil {
		return nil
	}
	clone := *r
	return &clone
}

// CompiledResultContract is one compiled result schema. It exists only in
// the loaded TeamSession and is never persisted.
type CompiledResultContract struct {
	ID              string
	SchemaSHA256    string
	CanonicalSchema []byte
	schema          *jsonschema.Schema
}

// ref binds the compiled schema to one declaration's require-structured
// setting.
func (c *CompiledResultContract) ref(requireStructured bool) ResultContractRef {
	return ResultContractRef{ID: c.ID, SchemaSHA256: c.SchemaSHA256, RequireStructured: requireStructured}
}

// admittedResultContract returns the result contract a new occurrence is
// bound to: a matched static contract's own contract, otherwise the agent's
// default. Sidecar tasks never bind one. The static binding reaches the task
// only through CompileInitialTaskContracts or CompileTaskGoalContracts, and
// the coordinator's task payload cannot carry either field.
func (c *Coordinator) admittedResultContract(task TaskDef, def *agent.AgentDef) *ResultContractRef {
	if task.Sidecar {
		return nil
	}
	if task.ResultContract != nil {
		return task.ResultContract.clone()
	}
	if c == nil || c.session == nil || def == nil {
		return nil
	}
	if ref, ok := c.session.AgentResultContracts[strings.ToLower(strings.TrimSpace(def.Name))]; ok {
		return &ref
	}
	return nil
}

// ExecutionResultContractPolicySnapshot records one bound result contract in
// the execution policy snapshot. Owner is "agent:<name>" for an agent's
// default contract or "contract:<id>" for a static contract task's own.
type ExecutionResultContractPolicySnapshot struct {
	Owner             string `json:"owner"`
	ID                string `json:"id"`
	SchemaSHA256      string `json:"schema_sha256"`
	RequireStructured bool   `json:"require_structured,omitempty"`
}

// executionPolicyResultContracts lists every result contract binding in a
// deterministic order. A changed schema or binding therefore changes the
// policy snapshot and fails resume closed.
func executionPolicyResultContracts(session *TeamSession) []ExecutionResultContractPolicySnapshot {
	if session == nil {
		return nil
	}
	var bindings []ExecutionResultContractPolicySnapshot
	for name, ref := range session.AgentResultContracts {
		bindings = append(bindings, ExecutionResultContractPolicySnapshot{
			Owner: "agent:" + name, ID: ref.ID, SchemaSHA256: ref.SchemaSHA256, RequireStructured: ref.RequireStructured,
		})
	}
	for _, task := range session.ContractTasks {
		if task.ResultContract == nil {
			continue
		}
		bindings = append(bindings, ExecutionResultContractPolicySnapshot{
			Owner: "contract:" + strings.TrimSpace(task.ID), ID: task.ResultContract.ID,
			SchemaSHA256: task.ResultContract.SchemaSHA256, RequireStructured: task.ResultContract.RequireStructured,
		})
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Owner < bindings[j].Owner })
	return bindings
}
