package main

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestApplyCLIVerifyTimeoutOverrides_NoOverride(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{Name: "test", VerifyTimeout: 120},
	}

	applyCLIVerifyTimeoutOverrides(session, VerifyTimeoutCLIOverrides{VerifyTimeout: 0})

	if session.Config.VerifyTimeout != 120 {
		t.Fatalf("VerifyTimeout = %d, want 120 (unchanged)", session.Config.VerifyTimeout)
	}
}

func TestApplyCLIVerifyTimeoutOverrides_OverridesTeam(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{Name: "test", VerifyTimeout: 120},
	}

	applyCLIVerifyTimeoutOverrides(session, VerifyTimeoutCLIOverrides{VerifyTimeout: 45})

	if session.Config.VerifyTimeout != 45 {
		t.Fatalf("VerifyTimeout = %d, want 45", session.Config.VerifyTimeout)
	}
}

func TestApplyCLIVerifyTimeoutOverrides_NilSessionSafe(t *testing.T) {
	applyCLIVerifyTimeoutOverrides(nil, VerifyTimeoutCLIOverrides{VerifyTimeout: 45})
}

func TestCurrentVerifyTimeoutOverrides_DefaultsToZero(t *testing.T) {
	got := currentVerifyTimeoutOverrides()
	if got.VerifyTimeout != 0 {
		t.Fatalf("currentVerifyTimeoutOverrides().VerifyTimeout = %d, want 0", got.VerifyTimeout)
	}
}
