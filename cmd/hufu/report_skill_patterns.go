package main

import "github.com/kjelly/hufu/internal/skill"

// gatherSkillPatterns reads the skill-pattern snapshot that the run persisted
// at each round boundary. It never re-runs detection: that needs the run's
// sidecar, which belongs to the finished invocation, and a run without
// --auto-skills detects nothing. A snapshot left by an earlier run is not
// this run's result.
func gatherSkillPatterns(tc *teamContext, runID string) []SkillPatternReport {
	if tc == nil || tc.session == nil || runID == "" {
		return nil
	}
	snapshot, available, err := skill.LoadSkillPatternSnapshot(skill.SkillPatternSnapshotPath(tc.session.Workspace))
	if err != nil || !available || snapshot.RunID != runID {
		return nil
	}
	reports := make([]SkillPatternReport, 0, len(snapshot.Patterns))
	for _, pattern := range snapshot.Patterns {
		name := pattern.DraftName
		if name == "" {
			name = pattern.ID
		}
		reports = append(reports, SkillPatternReport{
			Name:  name,
			Tools: pattern.Tools,
			Count: pattern.Count,
			Saved: pattern.DraftName != "",
		})
	}
	return reports
}
