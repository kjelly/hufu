package main

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/team"
)

func TestApplyCLITimeoutOverrides_NoOverride(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{Name: "test", Timeout: 600, MaxRounds: 10},
		Agents: map[string]*agent.AgentDef{
			"a": {Name: "a", Timeout: 300},
			"b": {Name: "b", Timeout: 0},
		},
	}

	// Zero override = no change.
	applyCLITimeoutOverrides(session, TimeoutCLIOverrides{Timeout: 0})

	if session.Config.Timeout != 600 {
		t.Errorf("Config.Timeout = %d, want 600 (unchanged)", session.Config.Timeout)
	}
	if session.Agents["a"].Timeout != 300 {
		t.Errorf("agent a Timeout = %d, want 300 (unchanged)", session.Agents["a"].Timeout)
	}
}

func TestApplyCLITimeoutOverrides_NegativeIgnored(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{Name: "test", Timeout: 600},
		Agents: map[string]*agent.AgentDef{
			"a": {Name: "a", Timeout: 300},
		},
	}

	// Negative override = no change (treated as invalid).
	applyCLITimeoutOverrides(session, TimeoutCLIOverrides{Timeout: -1})

	if session.Config.Timeout != 600 {
		t.Errorf("Config.Timeout = %d, want 600 (negative override ignored)", session.Config.Timeout)
	}
	if session.Agents["a"].Timeout != 300 {
		t.Errorf("agent a Timeout = %d, want 300 (negative override ignored)", session.Agents["a"].Timeout)
	}
}

func TestApplyCLITimeoutOverrides_OverridesTeamAndAgents(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{Name: "test", Timeout: 600, MaxRounds: 10},
		Agents: map[string]*agent.AgentDef{
			"a":      {Name: "a", Timeout: 300},
			"b":      {Name: "b", Timeout: 0},
			"helper": {Name: "helper", Timeout: 0},
		},
	}

	applyCLITimeoutOverrides(session, TimeoutCLIOverrides{Timeout: 1800})

	if session.Config.Timeout != 1800 {
		t.Errorf("Config.Timeout = %d, want 1800", session.Config.Timeout)
	}
	if session.Agents["a"].Timeout != 1800 {
		t.Errorf("agent a Timeout = %d, want 1800", session.Agents["a"].Timeout)
	}
	if session.Agents["b"].Timeout != 1800 {
		t.Errorf("agent b Timeout = %d, want 1800 (was 0, now overridden)", session.Agents["b"].Timeout)
	}
	if session.Agents["helper"].Timeout != 1800 {
		t.Errorf("agent helper Timeout = %d, want 1800", session.Agents["helper"].Timeout)
	}
}

func TestApplyCLITimeoutOverrides_NilSessionSafe(t *testing.T) {
	// Should not panic.
	applyCLITimeoutOverrides(nil, TimeoutCLIOverrides{Timeout: 1800})
}

func TestApplyCLITimeoutOverrides_NilAgentDefSafe(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{Name: "test", Timeout: 600},
		Agents: map[string]*agent.AgentDef{
			"a":      {Name: "a", Timeout: 300},
			"nilkey": nil,
		},
	}

	applyCLITimeoutOverrides(session, TimeoutCLIOverrides{Timeout: 1800})

	if session.Config.Timeout != 1800 {
		t.Errorf("Config.Timeout = %d, want 1800", session.Config.Timeout)
	}
	if session.Agents["a"].Timeout != 1800 {
		t.Errorf("agent a Timeout = %d, want 1800", session.Agents["a"].Timeout)
	}
}

func TestApplyCLITimeoutOverrides_PerAgentOverrideReplaced(t *testing.T) {
	// If an agent had its own per-agent timeout set (e.g. from .md),
	// --timeout should override it for uniformity.
	session := &team.TeamSession{
		Config: agent.TeamConfig{Name: "test", Timeout: 600},
		Agents: map[string]*agent.AgentDef{
			"a": {Name: "a", Timeout: 9999}, // per-agent override
		},
	}

	applyCLITimeoutOverrides(session, TimeoutCLIOverrides{Timeout: 1800})

	if session.Agents["a"].Timeout != 1800 {
		t.Errorf("agent a Timeout = %d, want 1800 (CLI override beats per-agent)", session.Agents["a"].Timeout)
	}
}

func TestCurrentTimeoutOverrides_DefaultsToZero(t *testing.T) {
	got := currentTimeoutOverrides()
	if got.Timeout != 0 {
		t.Errorf("currentTimeoutOverrides().Timeout = %d, want 0 (flag not set)", got.Timeout)
	}
}
