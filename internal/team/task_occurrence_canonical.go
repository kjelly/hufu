package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// canonicalizeTaskOccurrence freezes the effective task contract shared by
// decision admission, task_created, checkpoint replay, and execution. The
// caller supplies the model selected at the creation boundary; no later
// consumer is allowed to re-resolve it from mutable team configuration. The
// canonical target is resolved here, before any TODO/model call; legacy
// SubagentProvider is retained solely as a dual-write compatibility shadow.
func (c *Coordinator) canonicalizeTaskOccurrence(task TaskDef, def *agent.AgentDef, resolvedModel string) (TaskDef, error) {
	var inputErr error
	task, inputErr = c.bindTaskRunInputs(task)
	if inputErr != nil {
		return task, inputErr
	}
	if def != nil {
		task.Agent = strings.ToLower(strings.TrimSpace(def.Name))
	}
	task.Model = resolvedModel
	if c != nil {
		task.SideEffect, task.Recovery, task.ReconcileTool = c.PolicyEngine().ResolveRecoveryPolicy(def, task)
	}
	if task.CatalogAction == nil {
		task.ResultContract = c.admittedResultContract(task, def)
	}
	legacyProvider := strings.TrimSpace(task.SubagentProvider)
	if legacyProvider == "" && def != nil {
		legacyProvider = strings.TrimSpace(def.SubagentProvider)
	}
	if legacyProvider == "" && c != nil && c.session != nil {
		legacyProvider = strings.TrimSpace(c.session.Config.SubagentProviderDefault)
	}
	target, err := c.resolveCanonicalTaskTarget(task.Model, legacyProvider)
	if err != nil {
		return task, err
	}
	task.ResolvedExecutionTarget = target
	if execution.IsOllamaBackend(target.Backend) {
		task.SubagentProvider = localSubagentProviderName
	} else {
		task.SubagentProvider = target.Backend
	}
	task.ExecutionTopology, err = c.resolveCanonicalTaskTopology(task.ModelTopology, target, task.SubagentProvider)
	if err != nil {
		return task, err
	}
	if err := c.validateExtraModelExecutionTopology(task); err != nil {
		return task, err
	}
	if task.CatalogAction == nil {
		route, err := c.admittedExecutionRoute(task, def)
		if err != nil {
			return task, err
		}
		task.ExecutionRoute = route
	}
	if err := c.validatePlanFirstExecutionTarget(task); err != nil {
		return task, err
	}
	workspacePolicy, err := c.admittedWorkerWorkspace(task, def)
	if err != nil {
		return task, err
	}
	task.WorkerWorkspace = workspacePolicy
	if task.ResultContract != nil {
		compiled, err := c.compiledResultContract(task.ResultContract)
		if err != nil {
			return task, err
		}
		if hasResultDecisions(compiled) {
			if err := validateResultDecisionCatalog(c.session, def, compiled); err != nil {
				return task, err
			}
			targets := []execution.ExecutionTarget{target}
			if task.ExecutionRoute != nil {
				targets = append(targets, task.ExecutionRoute.Candidates...)
			}
			for _, candidate := range targets {
				backend, err := c.ExecutionRegistry().ResolveBackend(candidate.Backend)
				if err != nil {
					return task, err
				}
				if backend.Kind() != execution.BackendKindLLM {
					return task, fmt.Errorf("bound decisions require a direct language-model execution target")
				}
			}
		}
	}
	return c.bindRequestAcceptanceTask(task)
}
