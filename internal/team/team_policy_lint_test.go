package team

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func policyTestSession() *TeamSession {
	worker := &agent.AgentDef{Name: "worker", Role: "worker", Tools: "bash,view"}
	coordinator := &agent.AgentDef{Name: "coordinator", Role: "coordinator"}
	return &TeamSession{
		Config: agent.TeamConfig{
			Delegation: agent.DelegationPolicy{
				AllowedWorkers: []string{"worker"},
				InitialBatch:   []string{"worker"},
			},
		},
		Agents: map[string]*agent.AgentDef{
			"worker":      worker,
			"coordinator": coordinator,
		},
	}
}

func TestValidateTeamPolicyContractsAcceptsConsistentTeam(t *testing.T) {
	session := policyTestSession()
	session.Config.Requirements = agent.ContractRequirements{Tools: []string{"bash"}}
	session.Agents["worker"].Requirements = agent.ContractRequirements{Tools: []string{"view"}}

	if findings := ValidateTeamPolicyContracts(session); len(findings) != 0 {
		t.Fatalf("ValidateTeamPolicyContracts() = %#v, want no findings", findings)
	}
}

func TestValidateTeamPolicyContractsFindsStructuralConflicts(t *testing.T) {
	tests := []struct {
		name string
		edit func(*TeamSession)
		code string
	}{
		{
			name: "unknown allowed worker",
			edit: func(s *TeamSession) { s.Config.Delegation.AllowedWorkers = []string{"missing"} },
			code: FindingDelegationWorkerUnknown,
		},
		{
			name: "coordinator delegated as worker",
			edit: func(s *TeamSession) { s.Config.Delegation.AllowedWorkers = []string{"coordinator"} },
			code: FindingDelegationWorkerRole,
		},
		{
			name: "initial worker excluded",
			edit: func(s *TeamSession) {
				s.Config.Delegation.AllowedWorkers = []string{"worker"}
				s.Config.Delegation.InitialBatch = []string{"helper"}
				s.Agents["helper"] = &agent.AgentDef{Name: "helper", Role: "worker", Tools: "view"}
			},
			code: FindingDelegationWorkerDenied,
		},
		{
			name: "tool both allowed and denied",
			edit: func(s *TeamSession) {
				s.Config.ToolsAllowed = []string{"bash"}
				s.Config.ToolsDenied = []string{"bash"}
			},
			code: FindingToolPolicyConflict,
		},
		{
			name: "deprecated memory alias explicit opt-in",
			edit: func(s *TeamSession) { s.Config.ToolsAllowed = []string{"memory_save"} },
			code: FindingDeprecatedMemoryTool,
		},
		{
			name: "required tool denied",
			edit: func(s *TeamSession) {
				s.Config.ToolsDenied = []string{"bash"}
				s.Agents["worker"].Requirements.Tools = []string{"bash"}
			},
			code: FindingRequiredToolDenied,
		},
		{
			name: "required tool not declared",
			edit: func(s *TeamSession) { s.Agents["worker"].Requirements.Tools = []string{"write"} },
			code: FindingRequiredToolUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := policyTestSession()
			tt.edit(session)
			findings := ValidateTeamPolicyContracts(session)
			if !hasFindingCode(findings, tt.code) {
				t.Fatalf("findings = %#v, want code %q", findings, tt.code)
			}
		})
	}
}

func TestLintEffectiveTeamContractsFindsResolvedPolicyConflicts(t *testing.T) {
	allowedRoot := t.TempDir()
	outside := t.TempDir()
	session := policyTestSession()
	session.Config.Requirements = agent.ContractRequirements{
		Environment: []string{"TEAM_TOKEN"},
		Paths:       []string{outside},
		Interactive: true,
		Network:     true,
		PlanFirst:   true,
	}
	session.Agents["worker"].Requirements.Tools = []string{"bash"}

	findings := LintEffectiveTeamContracts(session, EffectiveTeamContractContext{
		Unattended:        true,
		ForceMCP:          true,
		NoNet:             true,
		PlanFirst:         boolPointer(false),
		AllowedPaths:      []string{allowedRoot},
		EnvironmentLookup: func(string) (string, bool) { return "", false },
	})
	for _, code := range []string{
		FindingInteractiveUnattended,
		FindingNetworkDisabled,
		FindingPlanFirstRequired,
		FindingRequiredEnvMissing,
		FindingRequiredPathDenied,
		FindingRequiredToolDenied,
	} {
		if !hasFindingCode(findings, code) {
			t.Errorf("findings = %#v, want code %q", findings, code)
		}
	}
}

func TestLintEffectiveTeamContractsAcceptsSatisfiedRequirements(t *testing.T) {
	root := t.TempDir()
	session := policyTestSession()
	session.Config.Requirements = agent.ContractRequirements{
		Environment: []string{"TEAM_TOKEN"},
		Paths:       []string{filepath.Join(root, "artifacts")},
		PlanFirst:   true,
	}
	session.Agents["worker"].Requirements.Tools = []string{"view"}

	findings := LintEffectiveTeamContracts(session, EffectiveTeamContractContext{
		PlanFirst:    boolPointer(true),
		AllowedPaths: []string{root},
		EnvironmentLookup: func(name string) (string, bool) {
			return "present", name == "TEAM_TOKEN"
		},
	})
	if len(findings) != 0 {
		t.Fatalf("LintEffectiveTeamContracts() = %#v, want no findings", findings)
	}
}

func TestLoadTeamParsesGenericRequirements(t *testing.T) {
	dir := t.TempDir()
	teamYAML := `name: contract-team
requires:
  environment: [TEAM_TOKEN]
  paths: [artifacts]
delegation:
  allowed-workers: [worker]
  initial-batch:
    agents: [worker]
    exact: true
`
	agentMarkdown := `---
name: worker
role: worker
tools: [bash, view]
requires:
  tools: [bash]
  environment: [WORKER_TOKEN]
  paths: [artifacts]
  network: true
  plan-first: true
---
Run the assigned task.
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(teamYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte(agentMarkdown), 0o600); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam() error = %v", err)
	}
	if len(session.Config.Requirements.Environment) != 1 {
		t.Fatalf("team requirements = %#v", session.Config.Requirements)
	}
	worker := session.Agents["worker"]
	if worker == nil || !worker.Requirements.Network || !worker.Requirements.PlanFirst || len(worker.Requirements.Tools) != 1 || worker.Requirements.Tools[0] != "bash" {
		t.Fatalf("worker requirements = %#v", worker)
	}
}

func hasFindingCode(findings []ContractFinding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func boolPointer(value bool) *bool { return &value }
