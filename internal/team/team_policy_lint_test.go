package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
			name: "unknown worker requires a task contract",
			edit: func(s *TeamSession) { s.Config.Delegation.RequireTaskContract = []string{"missing"} },
			code: FindingDelegationWorkerUnknown,
		},
		{
			name: "request shared with the coordinator",
			edit: func(s *TeamSession) { s.Config.Delegation.ShareRequestWith = []string{"coordinator"} },
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

func findingCodes(findings []ContractFinding) map[string]bool {
	codes := make(map[string]bool, len(findings))
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	return codes
}

func boolPointer(value bool) *bool { return &value }

// policyLintActionProvider is an action provider that accepts every action.
type policyLintActionProvider struct{}

func (policyLintActionProvider) Validate(Action) error { return nil }
func (policyLintActionProvider) Execute(context.Context, Action) (any, error) {
	return ActionResult{}, nil
}

func TestValidateWorksetAndActionContractsRejectsImpossiblePolicies(t *testing.T) {
	session := &TeamSession{
		Config: agent.TeamConfig{Unattended: true, ActionProviders: map[string]agent.ActionProviderConfig{}},
		ContractTasks: []TaskDef{
			{ID: "prepare", Agent: "producer", FanOut: &FanOutSpec{
				Source: "manifest.json", SourceArtifact: FactRef{TaskID: "producer", Artifact: "manifest"}, GoalTemplate: "process {item}",
			}, Verify: "test -f {item}"},
			{ID: "consumer", Agent: "worker", Action: &Action{Capability: "missing-capability"}, VerifySpec: &VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "missing-source"}},
		},
	}
	codes := findingCodes(ValidateTeamPolicyContracts(session))
	for _, code := range []string{
		FindingWorksetSourceConflict, FindingWorksetCommandBinding, FindingActionProviderMissing,
		FindingWorksetReceiptSource, FindingUnattendedWorksetBudget, FindingUnattendedAcceptance,
	} {
		if !codes[code] {
			t.Errorf("missing finding code %q in %#v", code, codes)
		}
	}
}

func TestValidateWorksetAndActionContractsAcceptsBoundedValidContract(t *testing.T) {
	registry := NewProviderRegistry()
	registry.Register("producer", policyLintActionProvider{})
	session := &TeamSession{
		ProviderRegistry: registry,
		Config: agent.TeamConfig{
			MaxWallClock: 60, Unattended: true,
			AcceptanceSpec: &agent.AcceptanceSpec{Mode: "blocking", Verifications: []agent.VerificationSpec{{
				Type: agent.VerifyWorksetComplete, WorksetSourceTask: "prepare", WorksetRequireVerified: true,
			}}},
		},
		ContractTasks: []TaskDef{{
			ID: "prepare", Agent: "producer", FanOut: &FanOutSpec{Source: "manifest.json", GoalTemplate: "process {item}"},
			VerifySpec: &VerificationSpec{Type: VerifyFileExists, Path: "out.json"},
		}},
	}
	for _, finding := range ValidateTeamPolicyContracts(session) {
		if finding.Severity == FindingSeverityError {
			t.Fatalf("valid bounded contract produced error: %#v", finding)
		}
	}
}

func TestValidateWorksetVerificationMatchesCaseInsensitiveFanOutID(t *testing.T) {
	session := &TeamSession{ContractTasks: []TaskDef{
		{ID: "FanOut", Agent: "producer", FanOut: &FanOutSpec{Source: "manifest.json", GoalTemplate: "process {item}"}},
		{ID: "consumer", Agent: "worker", VerifySpec: &VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: " fanout "}},
	}}
	for _, finding := range ValidateTeamPolicyContracts(session) {
		if finding.Severity == FindingSeverityError {
			t.Fatalf("mixed-case workset source-task produced error: %#v", finding)
		}
	}
}

func TestValidateWorksetReferencesRejectsIncompleteUnknownAmbiguousAndUnordered(t *testing.T) {
	consumer := func(taskID, artifact string) TaskDef {
		return TaskDef{ID: "consumer", Agent: "worker", FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: taskID, Artifact: artifact}, GoalTemplate: "review {item}"}}
	}
	producer := TaskDef{ID: "producer", Agent: "producer"}
	tests := []struct {
		name  string
		tasks []TaskDef
		want  string
	}{
		{"partial reference", []TaskDef{producer, consumer("producer", "")}, "both task_id and artifact"},
		{"misspelled task", []TaskDef{consumer("producerr", "manifest")}, "does not identify a declared task"},
		{"ambiguous task", []TaskDef{producer, {ID: "producer", Agent: "producer-2"}, consumer("producer", "manifest")}, "ambiguous"},
		{"consumer before producer", []TaskDef{consumer("producer", "manifest"), producer}, "must identify an earlier task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := ValidateTeamPolicyContracts(&TeamSession{ContractTasks: tt.tasks})
			for _, finding := range findings {
				if finding.Severity == FindingSeverityError && strings.Contains(finding.Message, tt.want) {
					return
				}
			}
			t.Fatalf("findings = %#v, want error containing %q", findings, tt.want)
		})
	}
}

func TestDryRunUsesTheSameContractFindingCodes(t *testing.T) {
	c := newTestCoordinatorForDryRun(t, nil, nil, agent.TeamConfig{})
	c.session.ContractTasks = []TaskDef{{
		ID: "prepare",
		FanOut: &FanOutSpec{
			Source:         "manifest.json",
			SourceArtifact: FactRef{TaskID: "producer", Artifact: "manifest"},
			GoalTemplate:   "process {item}",
		},
	}}

	staticCodes := findingCodes(LintTeamContracts(c.session))
	result, err := c.DryRun(context.Background(), "preview")
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	dryRunCodes := findingCodes(result.ContractFindings)
	for code := range staticCodes {
		if !dryRunCodes[code] {
			t.Errorf("dry-run missing static finding code %q", code)
		}
	}
}
