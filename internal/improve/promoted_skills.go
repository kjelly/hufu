package improve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// PromotedSkillUsage summarizes selected-run tasks that used a skill created
// by an applied LTM promotion, counted from the apply time. It shows
// association, not causal attribution.
type PromotedSkillUsage struct {
	ProposalID        string `json:"proposal_id"`
	SkillName         string `json:"skill_name"` // directory of skills/<name>/SKILL.md
	AppliedAt         string `json:"applied_at"` // RFC3339Nano UTC
	TasksSinceApplied int    `json:"tasks_since_applied"`
	Done              int    `json:"done"`
	Error             int    `json:"error"`
	RetriedTasks      int    `json:"retried_tasks"`
	// UntimedTasks used the skill but have no parseable event timestamp, so
	// they cannot be placed before or after the apply.
	UntimedTasks int `json:"untimed_tasks"`
}

// promotedSkillUsageQuery uses the task_summary/task_skills semantics of
// groupBySkillQuery, restricted to one skill and split by each task's
// earliest parseable execution event time.
const promotedSkillUsageQuery = `
WITH first_event AS (
    SELECT e.run_id, e.task_id, MIN(e.timestamp_unix_ns) AS first_ns
    FROM execution_events e
    JOIN selected_runs sr ON sr.run_id = e.run_id
    WHERE e.task_id <> '' AND e.team <> ''
    GROUP BY e.run_id, e.task_id
)
SELECT
    COALESCE(SUM(CASE WHEN f.first_ns >= ?1 THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN f.first_ns >= ?1 AND t.terminal = 'done' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN f.first_ns >= ?1 AND t.terminal = 'error' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN f.first_ns >= ?1 AND t.attempts > 1 THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN f.first_ns IS NULL THEN 1 ELSE 0 END), 0)
FROM task_summary t
JOIN task_skills ts ON ts.run_id = t.run_id AND ts.task_id = t.task_id AND ts.skill = ?2
LEFT JOIN first_event f ON f.run_id = t.run_id AND f.task_id = t.task_id`

func promotedSkillName(targetPath string) string {
	parts := strings.Split(targetPath, "/")
	if len(parts) != 3 || parts[0] != "skills" || parts[2] != "SKILL.md" {
		return ""
	}
	return parts[1]
}

func (s *sqliteAnalyticsSession) sqlPromotedSkillUsage(ctx context.Context, proposals []contextstore.PromotionProposal) ([]PromotedSkillUsage, error) {
	var out []PromotedSkillUsage
	for _, proposal := range proposals {
		name := promotedSkillName(proposal.TargetPath)
		if name == "" || proposal.AppliedAt == nil {
			continue
		}
		usage := PromotedSkillUsage{ProposalID: proposal.ID, SkillName: name, AppliedAt: proposal.AppliedAt.UTC().Format(time.RFC3339Nano)}
		if err := s.executor.QueryRowContext(ctx, promotedSkillUsageQuery, proposal.AppliedAt.UnixNano(), name).Scan(&usage.TasksSinceApplied, &usage.Done, &usage.Error, &usage.RetriedTasks, &usage.UntimedTasks); err != nil {
			return nil, fmt.Errorf("query promoted skill usage for %s: %w", proposal.ID, err)
		}
		out = append(out, usage)
	}
	return out, nil
}

// collectPromotedSkills reads applied skill promotions from the workspace
// context store. A missing store omits the section silently; any failure
// omits it with the reason code query_failed and never fails the report.
func collectPromotedSkills(ctx context.Context, analytics *sqliteAnalyticsSession, workspace, teamName string) ([]PromotedSkillUsage, string) {
	path := filepath.Join(workspace, "context.sqlite")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ""
	}
	repo, err := contextstore.OpenSQLiteReadOnly(path)
	if err != nil {
		return nil, "query_failed"
	}
	defer func() { _ = repo.Close() }()
	proposals, err := repo.ListAppliedSkillPromotions(ctx, teamName)
	if err != nil {
		return nil, "query_failed"
	}
	if len(proposals) == 0 {
		return nil, ""
	}
	usage, err := analytics.sqlPromotedSkillUsage(ctx, proposals)
	if err != nil {
		return nil, "query_failed"
	}
	return usage, ""
}
