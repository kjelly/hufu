package team

import (
	"strings"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
)

// selectWorkerTools is the single worker-facing tool boundary. Team-level
// denials take precedence over both agent frontmatter and alwaysIncludeTools,
// so a collaboration/memory convenience cannot bypass a read-only contract.
func (c *Coordinator) selectWorkerTools(def *agent.AgentDef) []fantasy.AgentTool {
	if def == nil {
		return nil
	}
	return c.filterDeniedWorkerTools(agent.SelectTools(c.coreTools, def.Tools))
}

// filterDeniedWorkerTools removes denied tools before they reach a worker
// model. Result-protocol tools are appended by the caller after this filter so
// a team cannot accidentally remove the only terminal reporting mechanism.
func (c *Coordinator) filterDeniedWorkerTools(candidate []fantasy.AgentTool) []fantasy.AgentTool {
	if c == nil || c.session == nil || len(c.session.Config.ToolsDenied) == 0 {
		return candidate
	}
	denied := make(map[string]bool, len(c.session.Config.ToolsDenied))
	for _, name := range c.session.Config.ToolsDenied {
		if name = strings.TrimSpace(name); name != "" {
			denied[name] = true
		}
	}
	filtered := make([]fantasy.AgentTool, 0, len(candidate))
	for _, tool := range candidate {
		if tool != nil && !denied[tool.Info().Name] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func (c *Coordinator) filterDeniedToolNames(candidate []string) []string {
	if c == nil || c.session == nil || len(c.session.Config.ToolsDenied) == 0 {
		return candidate
	}
	denied := make(map[string]bool, len(c.session.Config.ToolsDenied))
	for _, name := range c.session.Config.ToolsDenied {
		if name = strings.TrimSpace(name); name != "" {
			denied[name] = true
		}
	}
	filtered := make([]string, 0, len(candidate))
	for _, name := range candidate {
		if name = strings.TrimSpace(name); name != "" && !denied[name] {
			filtered = append(filtered, name)
		}
	}
	return filtered
}
