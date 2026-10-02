package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestGatherSkillPatternsReadsTheRunSnapshot(t *testing.T) {
	tests := []struct {
		name     string
		snapshot bool
		session  bool
		runID    string
		want     []SkillPatternReport
	}{
		{name: "no snapshot", session: true, runID: "run-test"},
		{name: "snapshot from an earlier run", snapshot: true, session: true, runID: "run-other"},
		{name: "no session", snapshot: true, runID: "run-test"},
		{
			name: "snapshot from this run", snapshot: true, session: true, runID: "run-test",
			want: []SkillPatternReport{
				{Name: "pat_a", Tools: []string{"view", "write"}, Count: 2},
				{Name: "draft-bash-write", Tools: []string{"bash", "write"}, Count: 4, Saved: true},
				{Name: "pat_c", Tools: []string{"grep", "view"}, Count: 5},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			if tt.snapshot {
				writeSkillGraphTestSnapshot(t, workspace)
			}
			tc := &teamContext{}
			if tt.session {
				tc.session = &team.TeamSession{Workspace: workspace}
			}
			got := gatherSkillPatterns(tc, tt.runID)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("gatherSkillPatterns() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReportMarksOnlySavedSkillPatterns(t *testing.T) {
	data := &reportData{SkillPatterns: []SkillPatternReport{
		{Name: "draft-bash-write", Tools: []string{"bash", "write"}, Count: 4, Saved: true},
		{Name: "pat_a", Tools: []string{"view", "write"}, Count: 2},
	}}
	report := buildReportMD(data, "demo", "")
	for _, want := range []string{
		"✓ **draft-bash-write** (×4)\n   Pattern: bash → write",
		"○ **pat_a** (×2)\n   Pattern: view → write",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "were detected and saved as skill drafts") {
		t.Errorf("report claims every pattern was saved:\n%s", report)
	}
}
