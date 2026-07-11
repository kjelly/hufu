package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/team"
)

var taskStatusIcons = map[team.TaskStatus]string{
	team.TaskDone:      "●",
	team.TaskError:     "✗",
	team.TaskBlocked:   "⚠",
	team.TaskSkipped:   "—",
	team.TaskPending:   "○",
	team.TaskPlanned:   "◎",
	team.TaskVerifying: "◔",
}

// generateReport creates a markdown execution report for each loaded team
// and saves it to the team's workspace directory.
func generateReport(loadedTeams map[string]*teamContext, combinedResult string) {
	for teamName, tc := range loadedTeams {
		if tc == nil {
			continue
		}

		data := gatherReportData(tc, teamName)
		content := buildReportMD(data, teamName, combinedResult)

		path := filepath.Join(tc.session.Workspace, "report.md")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "%s Failed to write report for team %q: %v\n",
				errStyle.Render("✗"), teamName, err)
			continue
		}

		fmt.Fprintf(os.Stderr, "%s Report saved: %s\n",
			doneStyle.Render("✓"),
			path,
		)
	}
}

type reportData struct {
	Todos         []*team.TodoItem
	STM           string
	Skills        []team.SkillUsageEntry
	SessionData   *team.SessionData
	TaskHistory   map[string]string
	StartedAt     time.Time
	SkillPatterns []SkillPatternReport
}

// SkillPatternReport holds detected skill pattern info for reports
type SkillPatternReport struct {
	Name  string
	Tools []string
	Count int
	Desc  string
	Saved bool
}

func gatherReportData(tc *teamContext, teamName string) *reportData {
	d := &reportData{
		TaskHistory: make(map[string]string),
		StartedAt:   time.Now(),
	}

	if tc.sessionData != nil {
		if t, err := time.Parse(time.RFC3339, tc.sessionData.CreatedAt); err == nil {
			d.StartedAt = t
		}
		d.SessionData = tc.sessionData
	}

	if tc.coordinator != nil {
		d.Todos = tc.coordinator.TaskTracker().TodoList().Items()
		d.Skills = tc.coordinator.SkillUsage()
		d.SkillPatterns = gatherSkillPatterns(tc.coordinator)
	}

	if tc.session != nil {
		d.STM = team.LoadSTM(tc.session.Workspace)

		tasksDir := filepath.Join(tc.session.Workspace, "tasks", teamName)
		entries, err := os.ReadDir(tasksDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				agentDir := filepath.Join(tasksDir, entry.Name())
				taskEntries, err := os.ReadDir(agentDir)
				if err != nil {
					continue
				}
				var mdEntries []os.DirEntry
				for _, te := range taskEntries {
					if strings.HasSuffix(te.Name(), ".md") {
						mdEntries = append(mdEntries, te)
					}
				}
				sort.Slice(mdEntries, func(i, j int) bool {
					return mdEntries[i].Name() > mdEntries[j].Name()
				})
				var b strings.Builder
				count := 0
				for _, te := range mdEntries {
					if count >= 10 {
						break
					}
					data, err := os.ReadFile(filepath.Join(agentDir, te.Name()))
					if err != nil {
						continue
					}
					fmt.Fprintf(&b, "### %s\n```\n%s\n```\n\n",
						strings.TrimSuffix(te.Name(), ".md"),
						limitStr(string(data), 1500))
					count++
				}
				if count > 0 {
					d.TaskHistory[entry.Name()] = b.String()
				}
			}
		}
	}

	return d
}

// gatherSkillPatterns extracts detected skill patterns from coordinator
func gatherSkillPatterns(coordinator *team.Coordinator) []SkillPatternReport {
	detector := coordinator.SkillDetector()
	if detector == nil {
		return nil
	}
	candidates := detector.FindCandidates(context.Background())
	var reports []SkillPatternReport
	for _, cand := range candidates {
		reports = append(reports, SkillPatternReport{
			Name:  cand.SuggestedName,
			Tools: cand.Sequence.Tools,
			Count: cand.Sequence.Count,
			Desc:  cand.SuggestedDesc,
			Saved: true,
		})
	}
	return reports
}

func buildReportMD(data *reportData, teamName string, finalResult string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Execution Report — %s\n\n", teamName)
	fmt.Fprintf(&b, "**Generated:** %s\n\n", time.Now().Format(time.RFC3339))

	duration := time.Since(data.StartedAt).Round(time.Second)
	fmt.Fprintf(&b, "**Duration:** %s\n\n", duration)
	b.WriteString("---\n\n")

	if finalResult != "" {
		b.WriteString("## Final Result\n\n")
		b.WriteString(finalResult)
		b.WriteString("\n\n---\n\n")
	}

	if len(data.Todos) > 0 {
		b.WriteString("## Task Summary\n\n")
		b.WriteString("| ID | Status | Agent | Description | Detail | Duration |\n")
		b.WriteString("|----|--------|-------|-------------|--------|----------|\n")
		for _, t := range data.Todos {
			statusIcon := taskStatusIcons[t.Status]
			if statusIcon == "" {
				statusIcon = "◑"
			}
			detail := ""
			if t.Detail != "" && (t.Status == team.TaskError || t.Status == team.TaskBlocked) {
				detail = t.Detail
			}
			var dur string
			if !t.EndedAt.IsZero() && !t.StartedAt.IsZero() {
				dur = t.EndedAt.Sub(t.StartedAt).Round(time.Second).String()
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
				t.ID, statusIcon, t.Agent, t.Desc, detail, dur)
		}
		b.WriteString("\n---\n\n")
	}

	if len(data.Skills) > 0 {
		b.WriteString("## Skills Used\n\n")
		for _, s := range data.Skills {
			fmt.Fprintf(&b, "- **%s** (×%d) — %s\n",
				s.Name, s.Count, strings.Join(s.Agents, ", "))
		}
		b.WriteString("\n---\n\n")
	}

	if len(data.SkillPatterns) > 0 {
		b.WriteString("## Auto-Detected Skill Patterns\n\n")
		b.WriteString("The following repeating patterns were detected and saved as skill drafts:\n\n")
		for _, p := range data.SkillPatterns {
			status := "○"
			if p.Saved {
				status = "✓"
			}
			fmt.Fprintf(&b, "%s **%s** (×%d)\n", status, p.Name, p.Count)
			fmt.Fprintf(&b, "   Pattern: %s\n", strings.Join(p.Tools, " → "))
			fmt.Fprintf(&b, "   Description: %s\n\n", p.Desc)
		}
		b.WriteString("\n---\n\n")
	}

	if data.STM != "" {
		b.WriteString("## Session Context (STM)\n\n")
		b.WriteString(data.STM)
		b.WriteString("\n\n---\n\n")
	}

	if len(data.TaskHistory) > 0 {
		b.WriteString("## Agent Task Details\n\n")
		agentNames := make([]string, 0, len(data.TaskHistory))
		for name := range data.TaskHistory {
			agentNames = append(agentNames, name)
		}
		sort.Strings(agentNames)
		for _, name := range agentNames {
			fmt.Fprintf(&b, "### %s\n\n", name)
			b.WriteString(data.TaskHistory[name])
		}
	}

	return b.String()
}
