package team

import (
	"fmt"
	"strings"
)

// A sidecar task is one model call with no tools attached. That makes it useful
// for classification, summarization, and judgement, and structurally incapable
// of changing anything.
//
// The gap this file closes was observed end to end. A worker task kept failing,
// so the coordinator retried the same goal with sidecar:true — reasoning, in its
// own words, that the sidecar "bypasses LLM token counting". The sidecar had no
// tools, so the model answered "I cannot execute system commands or deploy
// scripts. I'm a CLI assistant and don't have access to: Terminal/shell
// execution". That prose was recorded as the task's output, the tool result read
// "Status: Success / 1/1 tasks completed successfully", the DAG advanced, and a
// downstream task then waited 110 minutes for a deployment that had never been
// started. ExecuteTasks exempts sidecar tasks from Execution.RequiresResult —
// necessarily, since a tool-less call cannot invoke submit_result — and the
// comment on that exemption describes exactly the risk it opts out of:
// "prevents a prose failure report from being recorded as a completed task".
//
// The invariant restored here: an execution path with no tools may not be
// assigned work that has to change the world, and its prose is never evidence
// that the world changed.

// sidecarTaskContractError explains the rejection in terms of the fix, because
// both a planner model and a human read it.
func sidecarTaskContractError(task TaskDef, effect SideEffectClass, reason string) error {
	goal := strings.TrimSpace(task.Goal)
	if len([]rune(goal)) > 120 {
		goal = string([]rune(goal)[:120]) + "…"
	}
	return fmt.Errorf("sidecar task %q cannot be run as a sidecar: %s (side_effect=%s). "+
		"A sidecar is a single model call with no tools, so it cannot perform the change and its answer is not evidence that the change happened. "+
		"Drop sidecar:true to run it as a worker with tools, or attach an objective verifier if the sidecar is only meant to report on state that already exists",
		goal, reason, effect)
}

// effectiveSideEffect resolves the side-effect class a task carries: an explicit
// declaration wins, otherwise it is inferred from the tools the assigned agent
// is granted. The inference is the same one the recovery policy uses, so a task
// cannot be judged mutating for recovery and non-mutating for this gate.
func (c *Coordinator) effectiveSideEffect(task TaskDef) SideEffectClass {
	if task.SideEffect != "" {
		return task.SideEffect
	}
	if c == nil || c.session == nil {
		return SideEffectNone
	}
	def, _, err := c.AgentPool().ResolveAgentName(task.Agent)
	if err != nil || def == nil {
		return SideEffectNone
	}
	if def.SideEffect != "" {
		return SideEffectClass(def.SideEffect)
	}
	return InferSideEffectClass(def.Tools)
}

// validateSidecarTaskContract rejects a sidecar task that would need tools to
// succeed. It runs at plan time so the rejection costs no model call, and again
// before the sidecar executes so a task reaching that path by another route is
// still covered.
func (c *Coordinator) validateSidecarTaskContract(task TaskDef) error {
	if !task.Sidecar {
		return nil
	}
	effect := c.effectiveSideEffect(task)
	hasVerifier := strings.TrimSpace(task.Verify) != "" || task.VerifySpec != nil
	switch effect {
	case SideEffectNone:
		// Pure analysis: prose is the deliverable and there is nothing to prove.
		return nil
	case SideEffectWorkspaceWrite, SideEffectExternalWrite, SideEffectInfraMutation, SideEffectCredential:
		if hasVerifier {
			// An objective verifier, not the model's answer, decides the
			// outcome — so the tool-less call cannot fake a success.
			return nil
		}
		return sidecarTaskContractError(task, effect, "it is expected to change state but has no objective verifier, so nothing but the model's own prose would decide whether it succeeded")
	default:
		if hasVerifier {
			return nil
		}
		return sidecarTaskContractError(task, effect, "its side-effect class is unrecognized and it has no objective verifier")
	}
}

// validateSidecarTaskContracts applies the gate to a whole plan.
func (c *Coordinator) validateSidecarTaskContracts(tasks []TaskDef) error {
	for _, task := range tasks {
		if err := c.validateSidecarTaskContract(task); err != nil {
			return err
		}
	}
	return nil
}
