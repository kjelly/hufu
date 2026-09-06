package team

import "fmt"

// ContextPurposePolicy is the closed contract for an auxiliary invocation.
// Callers select one of these names; they cannot invent a trigger or silently
// widen the source policy by passing an arbitrary string to a sidecar.
type ContextPurposePolicy struct {
	Trigger         ContextTrigger
	FallbackAllowed bool
	FallbackOutcome string
}

var contextPurposeRegistry = map[string]ContextPurposePolicy{
	// Primary coordinator/worker streams are model invocations too. Keep their
	// purpose explicit rather than relying on the trigger alone, so manifests
	// from a replay can distinguish a normal task turn, a retry, and a
	// coordinator orchestration turn without consulting prompt content.
	"coordinator_start":           {Trigger: ContextTriggerCoordinatorStart, FallbackAllowed: false},
	"coordinator_continuation":    {Trigger: ContextTriggerContinuation, FallbackAllowed: false},
	"task_execution":              {Trigger: ContextTriggerTaskDispatch, FallbackAllowed: false},
	"task_retry":                  {Trigger: ContextTriggerRetry, FallbackAllowed: false},
	"tool_failure_recovery":       {Trigger: ContextTriggerToolFailure, FallbackAllowed: false},
	"context_tool":                {Trigger: ContextTriggerAuxiliary, FallbackAllowed: false},
	"skill_matcher":               {Trigger: ContextTriggerSkillMatch, FallbackAllowed: true, FallbackOutcome: "keyword_fallback"},
	"agent_matcher":               {Trigger: ContextTriggerSkillMatch, FallbackAllowed: true, FallbackOutcome: "deterministic_agent_match"},
	"guard_reviewer":              {Trigger: ContextTriggerGuardReview, FallbackAllowed: false, FallbackOutcome: "deny"},
	"path_reviewer":               {Trigger: ContextTriggerGuardReview, FallbackAllowed: false, FallbackOutcome: "deny"},
	"plan_reviewer":               {Trigger: ContextTriggerPlanReview, FallbackAllowed: false, FallbackOutcome: "deny"},
	"judge":                       {Trigger: ContextTriggerJudge, FallbackAllowed: true, FallbackOutcome: "deterministic_judge"},
	"decision-reference-evidence": {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "deny"},
	// Every decision stage runs on the judge sidecar and none of them may
	// degrade: silently producing a decision with fewer judges, no challenge,
	// or no finalization is exactly what spec §34 forbids, so they fail
	// closed. They share the sidecar-task trigger rather than the judge
	// trigger, which is the result-judging path and demands candidate
	// identities a decision stage does not have.
	// TestEveryDecisionStagePurposeIsRegistered keeps this list complete.
	"decision-judge":                    {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "deny"},
	"decision-options":                  {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "deny"},
	"decision-challenge":                {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "deny"},
	"decision-premortem":                {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "deny"},
	"decision-revision":                 {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "deny"},
	"decision-finalization-coordinator": {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "deny"},
	"decision-finalization-judge":       {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "deny"},
	"skeptic":                           {Trigger: ContextTriggerSkeptic, FallbackAllowed: true, FallbackOutcome: "deterministic_skeptic"},
	"reflection":                        {Trigger: ContextTriggerRepair, FallbackAllowed: true, FallbackOutcome: "no_reflection"},
	"result_repair":                     {Trigger: ContextTriggerRepair, FallbackAllowed: false, FallbackOutcome: "repair_unavailable"},
	"final_summary_repair":              {Trigger: ContextTriggerRepair, FallbackAllowed: true, FallbackOutcome: "unrepaired_summary"},
	"protocol_repair":                   {Trigger: ContextTriggerRepair, FallbackAllowed: false, FallbackOutcome: "repair_unavailable"},
	"sidecar_task":                      {Trigger: ContextTriggerSidecarTask, FallbackAllowed: true, FallbackOutcome: "sidecar_unavailable"},
	"compactor":                         {Trigger: ContextTriggerSidecarTask, FallbackAllowed: true, FallbackOutcome: "uncompacted"},
	"team_selection":                    {Trigger: ContextTriggerCoordinatorStart, FallbackAllowed: true, FallbackOutcome: "keyword_fallback"},
	"fix_analysis":                      {Trigger: ContextTriggerSidecarTask, FallbackAllowed: true, FallbackOutcome: "deterministic_analysis"},
	"promotion_draft":                   {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "draft_unavailable"},
	"skill_learning":                    {Trigger: ContextTriggerSidecarTask, FallbackAllowed: true, FallbackOutcome: "deterministic_heuristic"},
	// Classifier is a compatibility name for legacy sidecar Execute callers.
	// It remains explicit so the audit can drive each caller toward a narrower
	// purpose without creating an ambient default path.
	"classifier": {Trigger: ContextTriggerSidecarTask, FallbackAllowed: true, FallbackOutcome: "classifier_unavailable"},
}

func contextPurposePolicy(purpose string) (ContextPurposePolicy, error) {
	policy, ok := contextPurposeRegistry[purpose]
	if !ok {
		return ContextPurposePolicy{}, fmt.Errorf("unsupported context invocation purpose %q", purpose)
	}
	return policy, nil
}
