package team

// Progressive model escalation: when an escalation-enabled task fails and is
// retried, it re-runs on the next stronger model instead of the same one
// (cheap model first, escalate on failure). The model-list order doubles as
// the strength order — entries are listed weakest→strongest, matching how the
// prompt presents them to the coordinator LLM.

import (
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

// nextStrongerModel returns the model that follows current in the list, or ""
// when there is nothing to escalate to: the list has fewer than two entries,
// current is already the strongest, or current is not in the list at all
// (e.g. it came from an agent definition rather than the model-list).
func nextStrongerModel(modelList []config.ModelEntry, current string) string {
	if len(modelList) < 2 || current == "" {
		return ""
	}
	for i, m := range modelList {
		if m.ID == current {
			if i+1 < len(modelList) {
				return modelList[i+1].ID
			}
			return ""
		}
	}
	return ""
}

// taskEscalationEnabled reports whether retries of this task should escalate
// to a stronger model, either via the per-task flag or the team-wide default.
func taskEscalationEnabled(task TaskDef, cfg *agent.TeamConfig, modelListLen int) bool {
	if modelListLen < 2 {
		return false
	}
	return task.Escalate || (cfg != nil && cfg.EscalateOnRetry)
}

// escalateTaskModelForRetry returns the model an on_failure DAG retry of task
// should use, or "" to keep the current one. When the task has no explicit
// model, the agent's resolved default is used as the starting point.
func (c *Coordinator) escalateTaskModelForRetry(task TaskDef) string {
	var cfg *agent.TeamConfig
	if c.session != nil {
		cfg = &c.session.Config
	}
	if !taskEscalationEnabled(task, cfg, len(c.modelList)) {
		return ""
	}
	current := task.Model
	if current == "" {
		if def, _, err := c.AgentPool().ResolveAgentName(task.Agent); err == nil && def != nil {
			current = c.resolveAgentModel(def, "")
		}
	}
	return nextStrongerModel(c.modelList, current)
}
