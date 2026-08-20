package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func findingCodes(findings []ContractFinding) map[string]bool {
	codes := make(map[string]bool, len(findings))
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	return codes
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
	registry.Register("producer", mockActionProvider{})
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

type mockActionProvider struct{}

func (mockActionProvider) Validate(Action) error { return nil }
func (mockActionProvider) Execute(context.Context, Action) (interface{}, error) {
	return ActionResult{}, nil
}
