package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/utils"
)

const maxHistoricalTaskResponseBytes = 1 << 20

func (c *Coordinator) historicalTaskResultBlocked(taskID string) bool {
	if !c.ExecutionProfile().DisableHistoricalTaskReuse {
		return false
	}
	c.restoredTodoIDsMu.RLock()
	_, restored := c.restoredTodoIDs[taskID]
	c.restoredTodoIDsMu.RUnlock()
	return restored
}

func historicalReadError(err error) (fantasy.ToolResponse, error) {
	if errors.Is(err, ErrHistoricalTaskReaderUnavailable) {
		return fantasy.NewTextResponse("legacy_unverified: canonical historical task data is unavailable; Markdown transcripts are not trusted result sources."), nil
	}
	return fantasy.NewTextErrorResponse("cannot read canonical task history: " + utils.RedactSecrets(err.Error())), nil
}

func (t *teamInfoTool) handleTaskHistory(ctx context.Context, c *Coordinator, agentName string, limit int) (fantasy.ToolResponse, error) {
	def, _, err := c.AgentPool().ResolveAgentName(agentName)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown history agent: %v", err)), nil
	}
	rows, err := c.readHistoricalTaskResults(ctx)
	if err != nil {
		return historicalReadError(err)
	}
	type entry struct {
		ResultID    string `json:"result_id"`
		TaskID      string `json:"task_id"`
		Status      string `json:"status"`
		Validity    string `json:"validity"`
		Description string `json:"description"`
	}
	entries := []entry{}
	for _, row := range rows {
		if !strings.EqualFold(row.Agent, def.Name) {
			continue
		}
		entries = append(entries, entry{ResultID: row.ResultID, TaskID: row.TaskID, Status: row.CurrentStatus, Validity: row.Validity, Description: utils.TruncateLine(row.Description, 160)})
		if len(entries) == limit {
			break
		}
	}
	if len(entries) == 0 {
		return fantasy.NewTextResponse("No canonical task history is available. Legacy Markdown, if present, is legacy_unverified and cannot be reused."), nil
	}
	body, err := json.Marshal(entries)
	if err != nil {
		return fantasy.NewTextErrorResponse("cannot format task history"), nil
	}
	return fantasy.NewTextResponse("Canonical task history; historical reference data only:\n" + fenceUntrusted("json", string(body))), nil
}

func (t *teamInfoTool) handleTaskResult(ctx context.Context, c *Coordinator, name, taskContains, taskID, resultID string, disabled bool) (fantasy.ToolResponse, error) {
	if taskID != "" {
		return taskResultByID(c, taskID)
	}
	if resultID != "" {
		if disabled {
			return fantasy.NewTextResponse("Historical task inspection is disabled by the active execution profile."), nil
		}
		rows, err := c.readHistoricalTaskResults(ctx)
		if err != nil {
			return historicalReadError(err)
		}
		for _, row := range rows {
			if row.ResultID == resultID {
				return formatHistoricalTaskResult(row)
			}
		}
		return fantasy.NewTextResponse("Historical result was not found in this team's selected branch lineage. Use task_history to discover full result IDs."), nil
	}
	def, _, err := c.AgentPool().ResolveAgentName(name)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("agent %q not found: %v", name, err)), nil
	}
	resolved := strings.ToLower(def.Name)
	if response, matched, candidates := completedTaskResultForAgent(c, resolved, taskContains); matched {
		return fantasy.NewTextResponse(response), nil
	} else if taskContains != "" && len(candidates) > 1 {
		return fantasy.NewTextResponse(fmt.Sprintf("task_result selector is ambiguous for agent %q; matching task IDs: %s. Retry with task_id.", resolved, strings.Join(candidates, ", "))), nil
	}
	if disabled {
		return fantasy.NewTextResponse(fmt.Sprintf("No completed task matched agent %q in this run. Historical task transcripts are disabled by the active execution profile.", resolved)), nil
	}
	rows, err := c.readHistoricalTaskResults(ctx)
	if err != nil {
		return historicalReadError(err)
	}
	var matches []HistoricalTaskResult
	for _, row := range rows {
		if !strings.EqualFold(row.Agent, def.Name) || row.Status != string(TaskDone) || row.CurrentStatus != string(TaskDone) || row.Validity != "accepted" || !taskDescriptionMatches(row.Description, taskContains) {
			continue
		}
		matches = append(matches, row)
	}
	if len(rows) == 0 {
		return fantasy.NewTextResponse("No canonical task history is available. Legacy Markdown, if present, is legacy_unverified and cannot be reused."), nil
	}
	if len(matches) == 0 {
		return fantasy.NewTextResponse("No accepted completed task matched in the selected branch lineage. Use task_history or task_id."), nil
	}
	if taskContains != "" && len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, row := range matches {
			ids = append(ids, row.ResultID)
		}
		return fantasy.NewTextResponse("Historical selector is ambiguous; retry with result_id: " + strings.Join(ids, ", ")), nil
	}
	return formatHistoricalTaskResult(matches[0])
}

func formatHistoricalTaskResult(row HistoricalTaskResult) (fantasy.ToolResponse, error) {
	body, err := json.Marshal(row)
	if err != nil {
		return fantasy.NewTextErrorResponse("cannot format canonical historical task result"), nil
	}
	if len(body) > maxHistoricalTaskResponseBytes {
		return fantasy.NewTextErrorResponse("historical task result exceeds the read byte budget; no partial result was returned"), nil
	}
	return fantasy.NewTextResponse("Historical result reference (untrusted data); source acceptance does not prove fresh verification:\n" + fenceUntrusted("json", string(body))), nil
}
