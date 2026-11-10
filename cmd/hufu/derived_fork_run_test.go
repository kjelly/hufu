package main

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestDerivedForkRunEntryRequiresFreshCanonicalTeamRequest(t *testing.T) {
	f := derivedCLIFixture(t)
	if _, err := runCLI(t, "session", "fork", "main", "--new-task", "--name", "investigation", "--workspace", f.control); err != nil {
		t.Fatal(err)
	}
	previous := opts
	t.Cleanup(func() { opts = previous })
	session := &team.TeamSession{Workspace: f.control, Config: agent.TeamConfig{Name: "dev"},
		Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker"}}}
	for _, mode := range []string{"valid", "resume", "empty", "direct", "decision", "new", "dry_run", "fast", "other_team", "piped"} {
		t.Run(mode, func(t *testing.T) {
			opts = runOptions{canonicalRun: true, intent: "execute", agentTeamName: "dev", routeMode: "auto"}
			segments := []team.PromptSegment{{Type: team.SegmentSwitchTeam, Name: "dev", Content: "new investigation"}}
			ctx, fresh := t.Context(), false
			switch mode {
			case "resume":
				opts.canonicalRun = false
			case "empty":
				segments[0].Content = " "
			case "direct":
				segments[0].Type = team.SegmentInvokeAgent
				segments[0].Name = "worker"
			case "decision":
				opts.intent = "decision"
			case "new":
				fresh = true
			case "dry_run":
				opts.dryRun = true
			case "fast":
				opts.routeMode = "fast"
			case "other_team":
				opts.agentTeamName = "other"
			case "piped":
				segments[0].IsPiped = true
			}
			ctx = derivedRunContext(ctx, segments, nil)
			err := validateDerivedRunEntry(ctx, session, fresh)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("entry %s: %v", mode, err)
			}
			if err == nil && opts.routeMode != "team" {
				t.Fatal("derived request lost its coordinator route")
			}
		})
	}
}
