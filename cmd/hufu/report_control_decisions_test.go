package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestControlDecisionJSONAndReportProjection(t *testing.T) {
	workspace := t.TempDir()
	session := &team.TeamSession{Dir: workspace, Workspace: workspace, Config: agent.TeamConfig{Name: "control-json"}}
	c, err := team.NewCoordinator(session, "", "", nil, nil, nil, team.RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	store, err := team.NewEventStore(workspace, "run-control", "session-control")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, payload := range []map[string]any{
		{"version": 1, "point": "path-reviewer", "mode": "shadow", "applied": "legacy", "status": "decided", "value": "false", "confidence": 0.97, "accepted": true, "threshold": 0.9, "duration_ms": 180, "legacy": "false", "agree": true},
		{"version": 1, "point": "path-reviewer", "mode": "shadow", "applied": "legacy", "status": "error", "error_code": "timeout", "threshold": 0.9, "duration_ms": 5000, "legacy": "true"},
	} {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendPersisted(team.RunEvent{Type: string(team.EventControlDecisionObserved), BranchID: "main", RunID: "run-control", TaskID: "1", Actor: "coder", Attempt: 1, Payload: data}); err != nil {
			t.Fatal(err)
		}
	}
	c.SetEventJournal(jsonOutputEventJournal{store: store})
	tc := &teamContext{teamName: "control-json", session: session, coordinator: c}
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	os.Stdout = w
	err = printResultJSON("done", map[string]*teamContext{"control-json": tc}, nil)
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var out jsonRunOutput
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Teams) != 1 || len(out.Teams[0].ControlDecisions) != 1 {
		t.Fatalf("JSON lost control decisions: %#v", out.Teams)
	}
	summary := out.Teams[0].ControlDecisions[0]
	if summary.Point != "path-reviewer" || summary.Calls != 2 || summary.Agreed != 1 || summary.Compared != 1 || summary.ErrorCodes["timeout"] != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	report := buildReportMD(gatherReportData(tc, "control-json"), "control-json", "done")
	for _, want := range []string{"## Control Decisions", "| path-reviewer | shadow | 2 | 1 / 1 | 0 | 1 |", "not a calibrated accuracy"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report omitted %q:\n%s", want, report)
		}
	}
	if strings.Contains(buildReportMD(&reportData{}, "plain", "done"), "Control Decisions") {
		t.Fatal("a run without observations rendered a control decisions section")
	}
}
