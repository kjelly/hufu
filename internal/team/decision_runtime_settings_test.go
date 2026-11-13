package team

import (
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/decisionrt/settings"
)

func TestDecisionRuntimeMergesTeamOverGlobalAliases(t *testing.T) {
	session := &TeamSession{
		GlobalControlDecisions: control.Config{Model: "global", Endpoint: "http://127.0.0.1:11434/v1/systemone", Timeout: time.Minute},
		Config:                 agent.TeamConfig{DecisionRuntime: settings.Config{SystemOne: settings.SystemOne{Model: "team"}}.WithSource("team")},
	}
	got, err := session.ResolveDecisionRuntime()
	if err != nil || got.Backend != "systemone" || got.SystemOne.Model != "team" || got.SystemOne.Timeout != time.Minute || got.Sources["systemone.model"] != "team" || !strings.Contains(got.Sources["systemone.endpoint"], "global:control-decisions") {
		t.Fatalf("shared=%+v err=%v", got, err)
	}
	session.Config.ControlDecisions.Model = "conflicting-team"
	if _, err := session.ResolveDecisionRuntime(); err == nil {
		t.Fatal("same-layer team conflict accepted")
	}
}

func TestDecisionRuntimeLegacyPolicyResume(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "changed"
		}
		t.Run(name, func(t *testing.T) {
			session := loadControlDecisionTeam(t, "control-decisions:\n  model: legacy-frozen\n  endpoint: http://127.0.0.1:1\n", control.Config{})
			first, err := newControlDecisionCoordinator(t, session)
			if err != nil {
				t.Fatal(err)
			}
			first.initEventStore()
			if err := first.checkRunAdmission(); err != nil {
				t.Fatal(err)
			}
			original := first.ExecutionPolicySnapshot()
			if original == nil || original.DecisionRuntimeHash == "" {
				t.Fatal("legacy shared transport not pinned")
			}
			if err := first.eventStore.Close(); err != nil {
				t.Fatal(err)
			}
			first.eventStore = nil
			checkpoint := LoadSession(session.Workspace)
			if checkpoint == nil || checkpoint.ExecutionPolicySnapshot == nil {
				t.Fatal("snapshot not persisted")
			}
			resumedSession := cloneSession(session, session.Workspace)
			if changed {
				resumedSession.Config.DecisionRuntime.SystemOne.Model = "changed-model"
			}
			second, err := newControlDecisionCoordinator(t, resumedSession)
			if err != nil {
				t.Fatal(err)
			}
			second.SetSessionData(checkpoint)
			second.initEventStore()
			err = second.checkRunAdmission()
			if changed {
				if err == nil || !strings.Contains(err.Error(), "drift") {
					t.Fatalf("changed resume accepted: %v", err)
				}
			} else if err != nil {
				t.Fatalf("identical resume rejected: %v", err)
			}
			historical, err := second.canonicalExecutionPolicySnapshot()
			if err != nil || historical == nil || historical.ConfigurationHash != original.ConfigurationHash {
				t.Fatalf("historical snapshot changed: %v", err)
			}
		})
	}
}
