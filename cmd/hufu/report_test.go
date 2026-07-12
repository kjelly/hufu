package main

import (
	"strings"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/team"
)

func TestBuildReportMDIncludesVerificationEvidence(t *testing.T) {
	data := &reportData{
		StartedAt: time.Now().Add(-2 * time.Minute),
		Todos: []*team.TodoItem{
			{
				ID:     "1",
				Agent:  "researcher",
				Desc:   "Produce report",
				Status: team.TaskDone,
				Verify: "test -f report.md",
				VerifyResult: &team.VerificationResult{
					Command:  "test -f report.md",
					WorkDir:  "/tmp/project",
					ExitCode: 0,
					Stdout:   "ok",
					Stderr:   "",
					Duration: 2 * time.Second,
					TimedOut: false,
				},
			},
		},
	}

	report := buildReportMD(data, "demo", "finished")

	if !strings.Contains(report, "## Verification Evidence") {
		t.Fatalf("report missing verification section:\n%s", report)
	}
	if !strings.Contains(report, "test -f report.md") {
		t.Fatalf("report missing verify command:\n%s", report)
	}
	if !strings.Contains(report, "Stdout") || !strings.Contains(report, "ok") {
		t.Fatalf("report missing verify stdout:\n%s", report)
	}
	if !strings.Contains(report, "Working directory") || !strings.Contains(report, "/tmp/project") {
		t.Fatalf("report missing verify working directory:\n%s", report)
	}
	if !strings.Contains(report, "Verify") {
		t.Fatalf("task summary missing verify column:\n%s", report)
	}
}
