package team

import (
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// HasAuthoredLegacyLocalExecutionBackend inspects the still-raw team and
// agent configuration immediately after loading. Selectors are canonicalized
// later by runtime target resolution, so this is the narrow point where the
// user-authored `local` spelling can be reported accurately without treating
// an internal provider identity or unrelated MCP `type: local` as an alias.
func HasAuthoredLegacyLocalExecutionBackend(session *TeamSession) bool {
	if session == nil {
		return false
	}
	config := session.Config
	if isAuthoredLegacyLocalBackend(config.DefaultLLMBackend) {
		return true
	}
	for _, model := range []string{
		config.WorkerModel,
		config.CoordinatorModel,
		config.Generation.Model,
		config.SidecarModel,
		config.GuardModel,
		config.JudgeModel,
		config.PlanReviewerModel,
	} {
		if isAuthoredLegacyLocalSelector(model) {
			return true
		}
	}
	seen := make(map[*agent.AgentDef]struct{}, len(session.Agents))
	for _, def := range session.Agents {
		if def == nil {
			continue
		}
		if _, exists := seen[def]; exists {
			continue
		}
		seen[def] = struct{}{}
		if isAuthoredLegacyLocalSelector(def.Generation.Model) {
			return true
		}
		for _, model := range def.ExtraModels {
			if isAuthoredLegacyLocalSelector(model) {
				return true
			}
		}
	}
	return false
}

func isAuthoredLegacyLocalBackend(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), execution.LegacyLocalBackendName)
}

func isAuthoredLegacyLocalSelector(value string) bool {
	backend, _, qualified := strings.Cut(strings.TrimSpace(value), "/")
	return qualified && isAuthoredLegacyLocalBackend(backend)
}
