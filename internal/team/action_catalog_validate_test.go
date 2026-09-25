package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestActionCatalogLintFindings(t *testing.T) {
	entry := actionCatalogTestEntry
	replace := func(old, new string) string {
		if !strings.Contains(entry, old) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return strings.Replace(entry, old, new, 1)
	}
	tests := []struct {
		name     string
		header   string
		entries  string
		wantCode string
	}{
		{name: "invalid ID", entries: replace("  collect-debug-bundle:", "  Collect_Bundle:"), wantCode: FindingActionCatalogIDInvalid},
		{name: "missing description", entries: replace("    description: Collect bounded runtime diagnostics for one service.\n", ""), wantCode: FindingActionCatalogEntryInvalid},
		{name: "unknown key", entries: replace("    side-effect: none\n", "    side-effect: none\n    phase: execute\n"), wantCode: FindingActionCatalogEntryInvalid},
		{name: "provider missing", entries: replace("capability: diagnostics", "capability: unconfigured"), wantCode: FindingActionProviderMissing},
		{name: "external write", entries: replace("side-effect: none", "side-effect: external_write"), wantCode: FindingActionCatalogSideEffectUnsupported},
		{name: "reconcile recovery", entries: replace("side-effect: none\n", "side-effect: none\n    recovery: reconcile\n"), wantCode: FindingActionCatalogRecoveryInvalid},
		{name: "workspace write without recovery", entries: replace("side-effect: none", "side-effect: workspace_write"), wantCode: FindingActionCatalogRecoveryRequired},
		{name: "missing input schema", entries: replace("    input-schema:\n      type: object\n      properties:\n        service:", "    unused-schema:\n      type: object\n      properties:\n        service:"), wantCode: FindingActionCatalogEntryInvalid},
		{name: "number input", entries: replace("        include-thread-dump:\n          type: boolean", "        include-thread-dump:\n          type: number"), wantCode: FindingActionCatalogInputSchemaInvalid},
		{name: "open input object", entries: replace("      additional-properties: false\n", ""), wantCode: FindingActionCatalogInputSchemaInvalid},
		{name: "redacted input property", entries: replace("        include-thread-dump:", "        password:"), wantCode: FindingActionCatalogInputSchemaInvalid},
		{name: "non-object output", entries: replace("    output-schema:\n      type: object\n      properties:\n        summary:\n          type: string\n      required-properties: [summary]\n", "    output-schema:\n      type: string\n"), wantCode: FindingActionCatalogOutputSchemaInvalid},
		{name: "unknown executing agent", entries: replace("agent: runtime-engineer", "agent: ghost"), wantCode: FindingActionCatalogAgentUnknown},
		{name: "unknown discover role", entries: replace("discover: [runtime-engineer, network-engineer, critic]", "discover: [runtime-engineer, network-engineer, critic, ghost]"), wantCode: FindingActionCatalogAgentUnknown},
		{name: "coordinator as agent", entries: replace("agent: runtime-engineer", "agent: coordinator"), wantCode: FindingActionCatalogAgentUnreachable},
		{name: "helper as agent", entries: replace("agent: runtime-engineer", "agent: helper"), wantCode: FindingActionCatalogAgentUnreachable},
		{name: "agent outside allowed workers", header: "delegation:\n  allowed-workers: [network-engineer, critic]\n", entries: entry, wantCode: FindingActionCatalogAgentUnreachable},
		{name: "propose outside discover", entries: replace("propose: [runtime-engineer, network-engineer]", "propose: [runtime-engineer, network-engineer, coordinator]"), wantCode: FindingActionCatalogAccessInvalid},
		{name: "duplicate discover role", entries: replace("discover: [runtime-engineer, network-engineer, critic]", "discover: [runtime-engineer, Runtime-Engineer, network-engineer]"), wantCode: FindingActionCatalogAccessInvalid},
		{name: "require proposal without proposers", entries: replace("      propose: [runtime-engineer, network-engineer]\n", ""), wantCode: FindingActionCatalogProposerUnreachable},
		{name: "propose tool denied", header: "tools:\n  denied: [team_action_propose]\n", entries: entry, wantCode: FindingActionCatalogProposerUnreachable},
		{name: "proposers on codex", header: "subagent-provider-default: codex\n", entries: entry, wantCode: FindingActionCatalogProposerUnreachable},
		{name: "zero invocations", entries: replace("max-invocations: 4", "max-invocations: 0"), wantCode: FindingActionCatalogInvocationInvalid},
		{name: "too many invocations", entries: replace("max-invocations: 4", "max-invocations: 65"), wantCode: FindingActionCatalogInvocationInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeActionCatalogTeam(t, actionCatalogTestManifest(tt.header, tt.entries))
			result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatalf("LintTeam: %v", err)
			}
			var codes []string
			for _, finding := range result.Findings {
				if strings.HasPrefix(finding.Code, "action_") {
					codes = append(codes, finding.Code)
				}
				if finding.Code == tt.wantCode && finding.Severity == FindingSeverityError {
					return
				}
			}
			t.Fatalf("lint codes = %v, want error %s", codes, tt.wantCode)
		})
	}
}

func TestLoadTeamRejectsInvalidActionCatalog(t *testing.T) {
	entries := strings.Replace(actionCatalogTestEntry, "side-effect: none", "side-effect: workspace_write", 1)
	_, err := LoadTeam(writeActionCatalogTeam(t, actionCatalogTestManifest("", entries)), nil, nil, DefaultProviderRegistry)
	if err == nil || !strings.Contains(err.Error(), "must set recovery") {
		t.Fatalf("LoadTeam error = %v, want the recovery-required finding", err)
	}
}

func TestLintModeDropsInvalidActionCatalogEntries(t *testing.T) {
	invalid := strings.Replace(actionCatalogTestEntry, "  collect-debug-bundle:", "  broken-bundle:", 1)
	invalid = strings.Replace(invalid, "agent: runtime-engineer", "agent: ghost", 1)
	dir := writeActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry+invalid))
	inspection, err := InspectTeam(dir, nil, nil, DefaultProviderRegistry, TeamCompileLint)
	if err != nil {
		t.Fatal(err)
	}
	catalog := inspection.Session.ActionCatalog
	if _, ok := catalog.Lookup("broken-bundle"); ok {
		t.Fatal("lint-mode snapshot kept an entry with a semantic error")
	}
	valid, ok := catalog.Lookup("collect-debug-bundle")
	if !ok {
		t.Fatal("lint-mode snapshot dropped the valid entry")
	}
	alone := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	if catalog.Hash != alone.ActionCatalog.Hash || valid.Hash == "" {
		t.Fatalf("pruned snapshot hash %q, want the single-entry hash %q", catalog.Hash, alone.ActionCatalog.Hash)
	}
}

func TestValidateActionCatalogPhaseAndToolSequence(t *testing.T) {
	entry := func(sideEffect SideEffectClass) ActionCatalogEntry {
		return ActionCatalogEntry{ID: "probe", Capability: "diagnostics", Type: "probe", Agent: "worker", SideEffect: sideEffect}
	}
	tests := []struct {
		name       string
		phases     []string
		sideEffect SideEffectClass
		want       bool
	}{
		{name: "dynamic team", sideEffect: SideEffectWorkspaceWrite},
		{name: "none in prepare", phases: []string{"prepare", "verify"}, sideEffect: SideEffectNone},
		{name: "workspace write needs execute", phases: []string{"prepare", "verify"}, sideEffect: SideEffectWorkspaceWrite, want: true},
		{name: "workspace write in execute", phases: []string{"execute", "verify"}, sideEffect: SideEffectWorkspaceWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &TeamSession{Config: agent.TeamConfig{Workflow: agent.WorkflowConfig{Phases: tt.phases}}}
			findings := validateActionCatalogPhase(session, entry(tt.sideEffect))
			if got := len(findings) == 1 && findings[0].Code == FindingActionCatalogPhaseUnreachable; got != tt.want {
				t.Fatalf("phase findings = %#v, want unreachable %v", findings, tt.want)
			}
		})
	}
	session := &TeamSession{ContractTasks: []TaskDef{
		{ID: "ok", Execution: ExecutionContract{ToolSequence: []string{"view"}}},
		{ID: "bad", Execution: ExecutionContract{ToolSequence: []string{"view", "Team_Action_Propose"}}},
	}}
	findings := validateActionCatalogToolSequences(session)
	if len(findings) != 1 || findings[0].Code != FindingActionCatalogToolSequenceUnsupported || findings[0].Field != "tasks[1].execution.tool-sequence" {
		t.Fatalf("tool sequence findings = %#v", findings)
	}
}

func TestTeamLintIgnoreAcceptsActionCatalogCodes(t *testing.T) {
	for _, code := range []string{
		FindingActionCatalogIDInvalid, FindingActionCatalogEntryInvalid, FindingActionCatalogSideEffectUnsupported,
		FindingActionCatalogRecoveryInvalid, FindingActionCatalogRecoveryRequired, FindingActionCatalogInputSchemaInvalid,
		FindingActionCatalogOutputSchemaInvalid, FindingActionCatalogAgentUnknown, FindingActionCatalogAgentUnreachable,
		FindingActionCatalogAgentUnsupported, FindingActionCatalogAccessInvalid, FindingActionCatalogProposerUnreachable,
		FindingActionCatalogInvocationInvalid, FindingActionCatalogPhaseUnreachable, FindingActionCatalogToolSequenceUnsupported,
	} {
		if _, err := ParseTeamLintIgnoreSelector(code); err != nil {
			t.Errorf("ParseTeamLintIgnoreSelector(%q): %v", code, err)
		}
	}
}
