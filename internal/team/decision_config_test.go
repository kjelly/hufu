package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func decisionConfigFixture() DecisionConfig {
	policy := DecisionPolicy{
		IndependentJudgments: 3,
		ContextIsolation:     agent.DecisionIsolationStrict,
		ScoreScale:           agent.DecisionScoreScale,
	}
	return DecisionConfig{
		DefaultProfile: "standard",
		Profiles: map[string]DecisionPolicy{
			"light":       policy,
			"standard":    policy,
			"high-stakes": policy,
		},
	}
}

// Precedence is CLI/request > task contract > team default > built-in off
// (spec §8).
func TestResolveDecisionProfilePrecedence(t *testing.T) {
	cfg := decisionConfigFixture()
	tests := []struct {
		name        string
		cfg         DecisionConfig
		request     string
		task        TaskDef
		wantProfile string
		wantSource  string
	}{
		{
			name:        "request override wins",
			cfg:         cfg,
			request:     "high-stakes",
			task:        TaskDef{DecisionProfile: "light"},
			wantProfile: "high-stakes",
			wantSource:  DecisionProfileSourceRequest,
		},
		{
			name:        "task override beats team default",
			cfg:         cfg,
			task:        TaskDef{DecisionProfile: "light"},
			wantProfile: "light",
			wantSource:  DecisionProfileSourceTask,
		},
		{
			name:        "team default applies",
			cfg:         cfg,
			wantProfile: "standard",
			wantSource:  DecisionProfileSourceTeam,
		},
		{
			name:        "built-in default is off",
			cfg:         DecisionConfig{},
			wantProfile: DecisionProfileOff,
			wantSource:  DecisionProfileSourceDefault,
		},
		{
			name:        "explicit off is honored as a request override",
			cfg:         cfg,
			request:     DecisionProfileOff,
			wantProfile: DecisionProfileOff,
			wantSource:  DecisionProfileSourceRequest,
		},
		{
			name:        "whitespace-only override falls through",
			cfg:         cfg,
			request:     "   ",
			wantProfile: "standard",
			wantSource:  DecisionProfileSourceTeam,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveDecisionProfile(tt.cfg, tt.request, tt.task)
			if err != nil {
				t.Fatalf("ResolveDecisionProfile = %v", err)
			}
			if got.Profile != tt.wantProfile || got.Source != tt.wantSource {
				t.Fatalf("resolution = %#v, want profile %q from %q", got, tt.wantProfile, tt.wantSource)
			}
		})
	}
}

// An unknown profile name must fail closed rather than silently degrade to a
// weaker profile (spec §8, §9).
func TestResolveDecisionProfileRejectsUnknownName(t *testing.T) {
	cfg := decisionConfigFixture()
	for _, tc := range []struct {
		name    string
		request string
		task    TaskDef
		source  string
	}{
		{name: "request", request: "paranoid", source: "request"},
		{name: "task", task: TaskDef{DecisionProfile: "paranoid"}, source: "task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveDecisionProfile(cfg, tc.request, tc.task)
			if err == nil {
				t.Fatal("ResolveDecisionProfile = nil error, want rejection")
			}
			if !strings.Contains(err.Error(), ReasonDecisionProfileUnknown) || !strings.Contains(err.Error(), tc.source) {
				t.Fatalf("error = %v, want %s naming the %s layer", err, ReasonDecisionProfileUnknown, tc.source)
			}
		})
	}
}

func TestDecisionProfileOffIsNotEnabled(t *testing.T) {
	resolution := DecisionProfileResolution{Profile: DecisionProfileOff, Source: DecisionProfileSourceTeam}
	if resolution.Enabled() {
		t.Fatal("off profile reported as enabled")
	}
	if _, ok := DecisionPolicyFor(decisionConfigFixture(), DecisionProfileOff); ok {
		t.Fatal("off profile resolved to a policy")
	}
	if _, ok := DecisionPolicyFor(decisionConfigFixture(), "standard"); !ok {
		t.Fatal("standard profile did not resolve to a policy")
	}
}

func TestValidateTaskDecisionProfiles(t *testing.T) {
	cfg := decisionConfigFixture()
	ok := []TaskDef{
		{ID: "t1"},
		{ID: "t2", DecisionProfile: "light"},
		{ID: "t3", DecisionProfile: DecisionProfileOff},
	}
	if err := ValidateTaskDecisionProfiles(cfg, ok); err != nil {
		t.Fatalf("ValidateTaskDecisionProfiles = %v", err)
	}

	bad := []TaskDef{{ID: "t1"}, {ID: "t2", DecisionProfile: "paranoid"}}
	err := ValidateTaskDecisionProfiles(cfg, bad)
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionProfileUnknown) || !strings.Contains(err.Error(), "t2") {
		t.Fatalf("error = %v, want %s naming task t2", err, ReasonDecisionProfileUnknown)
	}

	unnamed := []TaskDef{{DecisionProfile: "paranoid"}}
	if err := ValidateTaskDecisionProfiles(cfg, unnamed); err == nil || !strings.Contains(err.Error(), "index 0") {
		t.Fatalf("error = %v, want the task index for an unnamed task", err)
	}
}
