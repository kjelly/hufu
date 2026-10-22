package main

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestRequestAcceptanceCLIAndTeamPrecedence(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := opts
	t.Cleanup(func() { opts = previous })
	for _, test := range []struct {
		name        string
		teamEnabled bool
		args        []string
		want        bool
	}{
		{name: "default off"},
		{name: "team opt in", teamEnabled: true, want: true},
		{name: "CLI opt in", args: []string{"--request-acceptance"}, want: true},
		{name: "explicit disable", teamEnabled: true, args: []string{"--request-acceptance=false"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts = runOptions{}
			command, options := newRunCommandWithOptions()
			if err := command.ParseFlags(test.args); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveCanonicalRunOptions(command, options)
			if err != nil {
				t.Fatal(err)
			}
			opts = resolved
			session := &team.TeamSession{Config: agent.TeamConfig{RequestAcceptance: test.teamEnabled}}
			if _, err := applyCLIRuntimeOverrides(session); err != nil {
				t.Fatal(err)
			}
			if session.Config.RequestAcceptance != test.want {
				t.Fatalf("enabled=%t want=%t", session.Config.RequestAcceptance, test.want)
			}
		})
	}
}

func TestRequestAcceptanceFlagOnExecutionCommands(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	root := newRootCommand()
	if root.Flags().Lookup("request-acceptance") == nil {
		t.Fatal("compatibility root has no request acceptance flag")
	}
	for _, name := range []string{"run", "decide", "chat", "resume", "retry", "reconcile"} {
		command, _, err := root.Find([]string{name})
		if err != nil || command.Flags().Lookup("request-acceptance") == nil {
			t.Fatalf("%s has no request acceptance flag: %v", name, err)
		}
	}
}
