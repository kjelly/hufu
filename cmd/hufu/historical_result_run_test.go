package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestCanonicalRunHistoricalResultOptions(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	for _, test := range []struct {
		name string
		args []string
		pass bool
	}{
		{name: "explicit_team", args: []string{"--team", "review"}, pass: true},
		{name: "legacy_team_alias", args: []string{"--agent-team", "review"}, pass: true},
		{name: "no_team"},
		{name: "default", args: []string{"--default"}},
		{name: "auto_team", args: []string{"--auto-team"}},
		{name: "new", args: []string{"--team", "review", "--new"}},
		{name: "temp", args: []string{"--team", "review", "--temp"}},
		{name: "dry_run", args: []string{"--team", "review", "--dry-run"}},
		{name: "decision", args: []string{"--team", "review", "--intent", "decision"}},
		{name: "fast", args: []string{"--team", "review", "--route", "fast"}},
		{name: "invalid_route", args: []string{"--team", "review", "--route", "invalid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts = runOptions{}
			command, options := newRunCommandWithOptions()
			args := append(test.args, "--inherit-result", "worker=res-"+strings.Repeat("a", 64), "--inherit-result", "observer=res-"+strings.Repeat("b", 64))
			if err := command.ParseFlags(args); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveCanonicalRunOptions(command, options)
			if (err == nil) != test.pass {
				t.Fatalf("historical flags resolved: %v", err)
			}
			if test.pass && (len(resolved.inheritResults) != 2 || resolved.routeMode != "team") {
				t.Fatal("repeatable bindings or coordinator route were lost")
			}
		})
	}
	for _, name := range []string{"decide", "chat", "resume"} {
		root := newRootCommand()
		root.SetArgs([]string{name, "--inherit-result", "worker=res-" + strings.Repeat("a", 64)})
		if err := root.Execute(); err == nil {
			t.Fatalf("unsupported entry %s accepted new grants", name)
		}
	}
}

func TestHistoricalRunRejectsMultipleRequestsBeforeLoading(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts = runOptions{canonicalRun: true, agentTeamName: "review", inheritResults: []string{"worker=res-" + strings.Repeat("a", 64)}}
	for _, segments := range [][]team.PromptSegment{
		{{Type: team.SegmentSwitchTeam, Name: "review", Content: "request"}, {Type: team.SegmentSwitchTeam, Name: "other", Content: "request"}},
		{{Type: team.SegmentInvokeAgent, Name: "worker", Content: "request"}},
		{{Type: team.SegmentSwitchTeam, Name: "review", Content: "request", IsPiped: true}},
	} {
		if _, err := historicalRunContext(context.Background(), segments, nil); err == nil {
			t.Fatal("unsupported historical request reached team loading")
		}
	}
}

func TestReportHistoricalGrantsWithoutTasks(t *testing.T) {
	var report strings.Builder
	writeContextRoutingReport(&report, team.ContextManifestSummary{HistoricalGrants: []team.HistoricalResultGrantReport{
		{CycleID: "cycle", GrantEventID: "grant", State: "not_dispatched", Grant: team.HistoricalResultGrant{Recipient: "worker", ResultID: "res-source", PresentationSHA256: "hash", PresentationBytes: 123}},
	}})
	if !strings.Contains(report.String(), "not_dispatched") || !strings.Contains(report.String(), "res-source") || !strings.Contains(report.String(), "123 bytes") {
		t.Fatal("report omitted an authorized recipient with no task")
	}
}
