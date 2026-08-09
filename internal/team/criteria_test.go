package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func TestNamedCriteriaPersistAndRespectDependencies(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "test"}}, projectDir: dir, taskTracker: NewTaskTracker(), sessionData: NewSession()}
	c.acceptanceSpec = &AcceptanceSpec{Criteria: []AcceptanceCriterion{
		{ID: "build", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"}},
		{ID: "report", Required: true, DependsOn: []string{"build"}, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"}},
	}}
	result, err := c.runAcceptance(context.Background())
	if err != nil || !result.Passed {
		t.Fatalf("named criteria should pass: result=%#v err=%v", result, err)
	}
	if len(result.CriterionResults) != 2 || len(c.sessionData.CriterionResults) != 2 {
		t.Fatalf("criterion results were not persisted: %#v / %#v", result.CriterionResults, c.sessionData.CriterionResults)
	}

	c.acceptanceSpec.Criteria[0].Verify.Command = "false"
	result, err = c.runAcceptance(context.Background())
	if err == nil || result.Passed {
		t.Fatalf("failed required criterion must fail acceptance: %#v", result)
	}
	if result.CriterionResults[1].State != CriterionBlocked {
		t.Fatalf("dependent criterion = %s, want blocked", result.CriterionResults[1].State)
	}
}

func TestCriterionFingerprintIncludesRevisionAndSecurityInputs(t *testing.T) {
	criteria := []AcceptanceCriterion{{ID: "ready", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"}}}
	newCoordinator := func(revision int, noNet bool) *Coordinator {
		return &Coordinator{
			session:    &TeamSession{Config: agent.TeamConfig{Name: "test", NoNet: noNet}},
			projectDir: dirForCriteriaTest(t), sessionData: NewSession(), acceptanceContractRevision: revision,
		}
	}
	c1 := newCoordinator(1, false)
	r1, err := c1.evaluateCriteria(context.Background(), criteria)
	if err != nil || len(r1) != 1 {
		t.Fatalf("first evaluation failed: %#v %v", r1, err)
	}
	c2 := newCoordinator(2, true)
	r2, err := c2.evaluateCriteria(context.Background(), criteria)
	if err != nil || len(r2) != 1 {
		t.Fatalf("second evaluation failed: %#v %v", r2, err)
	}
	if r1[0].InputFingerprint == "" || r1[0].InputFingerprint == r2[0].InputFingerprint {
		t.Fatalf("criterion fingerprint ignored revision/security inputs: %q == %q", r1[0].InputFingerprint, r2[0].InputFingerprint)
	}
}

func TestDiagnosticTaskRequiresUncertaintyDeclaration(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "test", GoalMode: "outcome"}}}
	if err := c.validateTaskCriterionLinks([]TaskDef{{Kind: TaskKindDiagnostic, Goal: "inspect"}}); err == nil {
		t.Fatal("diagnostic task without expected state change should be rejected")
	}
	if err := c.validateTaskCriterionLinks([]TaskDef{{Kind: TaskKindDiagnostic, Goal: "inspect", ExpectedStateChange: "resolve whether artifact is stale"}}); err != nil {
		t.Fatalf("declared diagnostic uncertainty should be accepted: %v", err)
	}
}

func TestOutcomeTasksRequireSemanticAcceptanceVerifier(t *testing.T) {
	newCoordinator := func() *Coordinator {
		return &Coordinator{
			session: &TeamSession{Config: agent.TeamConfig{Name: "test", GoalMode: "outcome"}},
			acceptanceSpec: &AcceptanceSpec{Criteria: []AcceptanceCriterion{{
				ID: "inventory-roles", Required: true,
				Verify: VerificationSpec{Type: VerifyCommandExit, Command: "test -s hosts.yml && grep -qx controller hosts.yml"},
			}}},
		}
	}

	tests := []struct {
		name    string
		task    TaskDef
		wantErr string
	}{
		{
			name:    "untyped task cannot evade acceptance links",
			task:    TaskDef{Goal: "write inventory", Verify: "test -s hosts.yml"},
			wantErr: "must reference",
		},
		{
			name:    "outcome task requires a verifier",
			task:    TaskDef{Kind: TaskKindOutcome, Goal: "write inventory", Advances: []string{"inventory-roles"}},
			wantErr: "objective verifier",
		},
		{
			name:    "simple artifact test cannot prove role outcome",
			task:    TaskDef{Goal: "write inventory", Advances: []string{"inventory-roles"}, Verify: "test -s hosts.yml"},
			wantErr: "artifact-only",
		},
		{
			name: "semantic command verifier is accepted",
			task: TaskDef{Goal: "write inventory", Advances: []string{"inventory-roles"},
				Verify: "test -s hosts.yml && grep -qx controller hosts.yml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCoordinator().validateTaskCriterionLinks([]TaskDef{tt.task})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTaskCriterionLinks() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateTaskCriterionLinks() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeOutcomeTaskKindsPreservesSidecars(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{Name: "test", GoalMode: "outcome"}},
		// Promotion is conditional on there being an acceptance contract for an
		// outcome task to advance; with none configured an inferred outcome kind
		// would make every untyped task unschedulable.
		acceptanceSpec: &AcceptanceSpec{Criteria: []AcceptanceCriterion{{ID: "c1", Required: true}}},
	}
	tasks := []TaskDef{
		{Goal: "legacy outcome"},
		{Goal: "auxiliary", Sidecar: true},
		{Goal: "diagnose", Kind: TaskKindDiagnostic},
	}
	c.normalizeOutcomeTaskKinds(tasks)
	if tasks[0].Kind != TaskKindOutcome {
		t.Fatalf("untyped outcome kind = %q, want %q", tasks[0].Kind, TaskKindOutcome)
	}
	if tasks[1].Kind != "" {
		t.Fatalf("sidecar kind = %q, want empty", tasks[1].Kind)
	}
	if tasks[2].Kind != TaskKindDiagnostic {
		t.Fatalf("explicit kind = %q, want diagnostic", tasks[2].Kind)
	}
}

func TestCriterionRetryTargetsFailedCriteria(t *testing.T) {
	tasks := []TaskDef{
		{Kind: TaskKindOutcome, Advances: []string{"build"}},
		{Kind: TaskKindDiagnostic, Advances: []string{"build"}, ExpectedStateChange: "inspect build failure"},
		{Kind: TaskKindRepair, Advances: []string{"build"}},
		{Kind: TaskKindRepair, Advances: []string{"test"}},
	}
	got := criterionRetryTargets(tasks, []string{"build"})
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("criterion retry targets = %#v, want [0 2]", got)
	}
}

func TestSuccessfulNoProgressWithFailedCriterionIsRetryEligible(t *testing.T) {
	if !criterionRetryEligible(ProgressNoChange, []string{"build"}) {
		t.Fatal("successful task with no criterion progress should route to remediation")
	}
	if criterionRetryEligible(ProgressAdvanced, []string{"build"}) {
		t.Fatal("advanced criterion must not route as no-progress")
	}
	if criterionRetryEligible(ProgressNoChange, nil) {
		t.Fatal("no failed criterion must not route")
	}
}

func dirForCriteriaTest(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestCriteriaUseConfiguredVerificationShell(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "test", Shell: "bash"}},
		projectDir:  dir,
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}
	c.acceptanceSpec = &AcceptanceSpec{Criteria: []AcceptanceCriterion{{
		ID: "bash-expression", Required: true,
		Verify: VerificationSpec{Type: VerifyCommandExit, Command: "[[ 1 -eq 1 ]]"},
	}}}
	result, err := c.runAcceptance(context.Background())
	if err != nil || !result.Passed {
		t.Fatalf("configured shell should evaluate criterion: result=%#v err=%v", result, err)
	}
}

func TestCriterionAndTaskProgressPersistInSession(t *testing.T) {
	dir := t.TempDir()
	session := NewSession()
	session.CriterionResults = []CriterionResult{{ID: "artifact", State: CriterionPassed, EvaluatedAt: time.Now().UTC(), InputFingerprint: "vfp_test"}}
	session.Tasks = []*TodoItem{{ID: "1", Kind: TaskKindRepair, Advances: []string{"artifact"}, ExpectedStateChange: "artifact exists", Progress: ProgressAdvanced}}
	if err := SaveSession(dir, session); err != nil {
		t.Fatal(err)
	}
	loaded := LoadSession(dir)
	if loaded == nil || len(loaded.CriterionResults) != 1 || loaded.CriterionResults[0].State != CriterionPassed {
		t.Fatalf("criterion result did not persist: %#v", loaded)
	}
	if len(loaded.Tasks) != 1 || loaded.Tasks[0].Kind != TaskKindRepair || loaded.Tasks[0].Progress != ProgressAdvanced || len(loaded.Tasks[0].Advances) != 1 {
		t.Fatalf("task progress metadata did not persist: %#v", loaded.Tasks)
	}
}

func TestCriterionAndTaskProgressReplayFromEvents(t *testing.T) {
	encode := func(value any) json.RawMessage {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	events := []RunEvent{
		{Type: "task_created", TaskID: "1", Payload: encode(map[string]any{"id": "1", "desc": "repair", "kind": TaskKindRepair, "advances": []string{"artifact"}, "progress": ProgressAdvanced})},
		{Type: "criterion_re_evaluated", Payload: encode(map[string]any{"progress": ProgressAdvanced, "progressed_at": "2026-07-31T12:00:00Z", "after": []CriterionResult{{ID: "artifact", State: CriterionPassed}}})},
	}
	items := ReduceToTodoList(events)
	if len(items) != 1 || items[0].Kind != TaskKindRepair || items[0].Progress != ProgressAdvanced || len(items[0].Advances) != 1 {
		t.Fatalf("task metadata replay mismatch: %#v", items)
	}
	session := ReduceToSessionData(events)
	if len(session.CriterionResults) != 1 || session.CriterionResults[0].State != CriterionPassed {
		t.Fatalf("criterion replay mismatch: %#v", session.CriterionResults)
	}
	if session.LastCriterionProgressAt != "2026-07-31T12:00:00Z" {
		t.Fatalf("criterion progress timestamp replay mismatch: %q", session.LastCriterionProgressAt)
	}
}

func TestOutcomeTaskReevaluatesAdvancedCriterion(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "test"}}, projectDir: dir, taskTracker: NewTaskTracker(), sessionData: NewSession()}
	c.acceptanceSpec = &AcceptanceSpec{Criteria: []AcceptanceCriterion{{ID: "artifact", Required: true, Verify: VerificationSpec{Type: VerifyFileExists, Path: "report.txt"}}}}
	if _, err := c.evaluateCriteria(context.Background(), c.acceptanceSpec.Criteria); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	item := &TodoItem{ID: "1", Kind: TaskKindOutcome, Advances: []string{"artifact"}}
	c.reEvaluateAffectedCriteria(context.Background(), item)
	if item.Progress != ProgressAdvanced {
		t.Fatalf("progress = %s, want advanced", item.Progress)
	}
	if got := c.sessionData.CriterionResults[0].State; got != CriterionPassed {
		t.Fatalf("criterion state = %s, want passed", got)
	}
}

func TestValidateAcceptanceSpec_VacuousContract(t *testing.T) {
	// In outcome mode, empty acceptance contracts must fail validation with acceptance_vacuous error.
	if err := ValidateAcceptanceSpec(nil, "outcome"); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("expected acceptance_vacuous error for nil spec in outcome mode, got %v", err)
	}
	if err := ValidateAcceptanceSpec(&AcceptanceSpec{}, "outcome"); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("expected acceptance_vacuous error for empty spec in outcome mode, got %v", err)
	}
	if err := ValidateAcceptanceSpec(&AcceptanceSpec{Commands: []string{"   "}}, "outcome"); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("expected acceptance_vacuous error for whitespace command in outcome mode, got %v", err)
	}

	// Non-outcome mode (e.g. exploratory) allows empty contract.
	if err := ValidateAcceptanceSpec(&AcceptanceSpec{}, "exploratory"); err != nil {
		t.Fatalf("exploratory mode should allow empty acceptance spec, got %v", err)
	}

	// Non-empty spec in outcome mode must pass.
	validSpec := &AcceptanceSpec{Commands: []string{"test -f report.md"}}
	if err := ValidateAcceptanceSpec(validSpec, "outcome"); err != nil {
		t.Fatalf("valid spec should pass validation, got %v", err)
	}
}

func TestSetAcceptance_RejectsVacuousContract(t *testing.T) {
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "test", GoalMode: "outcome"}},
		projectDir:  t.TempDir(),
		sessionData: NewSession(),
	}

	// Calling SetAcceptance with empty string in outcome mode must fail.
	if err := c.SetAcceptance(""); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("SetAcceptance(\"\") should fail with acceptance_vacuous, got %v", err)
	}

	// Calling SetAcceptanceSpec with empty spec in outcome mode must fail.
	if err := c.SetAcceptanceSpec(AcceptanceSpec{}); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("SetAcceptanceSpec(empty) should fail with acceptance_vacuous, got %v", err)
	}

	// Valid acceptance command must succeed.
	if err := c.SetAcceptance("test -f report.md"); err != nil {
		t.Fatalf("SetAcceptance(valid) failed: %v", err)
	}
}

func TestOutcomeModeMissingAcceptanceRejected(t *testing.T) {
	// 1. YAML with no acceptance field uses the default profile's exploratory mode.
	dir1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir1, "team.yml"), []byte("name: no-acc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir1, nil)
	if err != nil {
		t.Fatalf("default-profile team without acceptance should parse: %v", err)
	}
	if cfg.GoalMode != "" {
		t.Fatalf("parse should preserve omitted goal-mode for profile resolution, got %q", cfg.GoalMode)
	}

	// 2. YAML with explicit goal-mode: outcome and no acceptance
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "team.yml"), []byte("name: outcome-no-acc\ngoal-mode: outcome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir2, nil); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("parseTeamYML with explicit outcome mode and no acceptance should fail with acceptance_vacuous, got %v", err)
	}

	// 3. MD-only team directory (no team.yml) also uses the default profile.
	dir3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir3, "helper.md"), []byte("---\nname: helper\nrole: worker\n---\nWorker prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTeam(dir3, nil, nil); err != nil {
		t.Fatalf("default-profile MD-only team without acceptance should load: %v", err)
	}

	// Case 4: Default session (LoadDefaultTeam) uses the default profile's
	// exploratory mode and remains constructible without acceptance.
	defaultSession, err := LoadDefaultTeam(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam failed: %v", err)
	}
	defaultSession.Config.Acceptance = ""
	defaultSession.Config.AcceptanceSpec = nil
	defaultCoordinator, err := NewCoordinator(defaultSession, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("default-profile coordinator without acceptance should construct: %v", err)
	}
	if got := defaultCoordinator.GoalMode(); got != GoalModeExploratory {
		t.Fatalf("default-profile goal mode = %q, want exploratory", got)
	}

	// 5. An explicitly outcome-oriented team without acceptance remains
	// rejected by the WP-15 preflight.
	dir5 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir5, "team.yml"), []byte("name: outcome-no-acc\ngoal-mode: outcome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir5, nil); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("explicit outcome team without acceptance should fail with acceptance_vacuous, got %v", err)
	}

	// 6. An outcome-default execution profile also rejects an omitted
	// goal-mode and empty acceptance contract.
	dir6 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir6, "team.yml"), []byte("name: unattended-no-acc\nexecution-profile: unattended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir6, nil); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("unattended-profile team without acceptance should fail with acceptance_vacuous, got %v", err)
	}
}

func TestDefaultProfileGoalModeResolution(t *testing.T) {
	mode, err := ResolveEffectiveGoalMode("", "")
	if err != nil {
		t.Fatalf("default profile resolution failed: %v", err)
	}
	if mode != GoalModeExploratory {
		t.Fatalf("default profile mode = %q, want exploratory", mode)
	}
	mode, err = ResolveEffectiveGoalMode("", string(ProfileUnattended))
	if err != nil {
		t.Fatalf("unattended profile resolution failed: %v", err)
	}
	if mode != GoalModeOutcome {
		t.Fatalf("unattended profile mode = %q, want outcome", mode)
	}
	if _, err := ResolveEffectiveGoalMode("not-a-mode", ""); err == nil {
		t.Fatal("invalid explicit goal mode unexpectedly resolved")
	}
}

func TestDefaultTeamWithoutAcceptanceGateIsUnverified(t *testing.T) {
	// Exercise the real default coordinator's finish path without contacting a
	// provider. This catches regressions where omitted goal-mode is treated as
	// outcome before the default profile has resolved it.
	session, err := LoadDefaultTeam(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam failed: %v", err)
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	tool := &finishTool{coordinator: c}
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"default run completed"}`})
	if err != nil {
		t.Fatalf("finishTool.Run failed: %v", err)
	}
	if !c.finishCalled.Load() {
		t.Fatal("default team finish path did not complete")
	}
	if got := fmt.Sprintf("%+v", response); !strings.Contains(got, "FINISHED:default run completed") {
		t.Fatalf("finish response = %q, want successful FINISHED response", got)
	}
	result := c.LastRunResult()
	if result == nil {
		t.Fatal("default team finish path did not record a run result")
	}
	if result.Outcome != RunOutcomeUnverified || result.ExitCode != 7 || result.GoalSatisfied {
		t.Fatalf("default team outcome = %q (goal %t, exit %d), want unverified (goal false, exit 7)", result.Outcome, result.GoalSatisfied, result.ExitCode)
	}
	if result.GoalMode != GoalModeExploratory {
		t.Fatalf("default team result goal mode = %q, want exploratory", result.GoalMode)
	}
	if result.Acceptance == nil || result.Acceptance.State != AcceptanceNotConfigured {
		t.Fatalf("default team acceptance = %#v, want not_configured", result.Acceptance)
	}
}

func TestSetAcceptance_RejectedEmptyUpdateAudited(t *testing.T) {
	ws := t.TempDir()
	session := &TeamSession{
		Workspace: ws,
		Dir:       ws,
		Config:    agent.TeamConfig{Name: "audit-test", GoalMode: "outcome", AcceptanceSpec: &agent.AcceptanceSpec{Commands: []string{"test -f valid.txt"}}},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	// Enable contract fixing so audit events and persistAcceptanceAuditEvent run
	c.acceptanceContractFixed = true

	// Attempting to set empty acceptance spec on an existing valid contract must be rejected
	err = c.SetAcceptance("")
	if err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("SetAcceptance(\"\") on active contract should be rejected with acceptance_vacuous, got %v", err)
	}

	// Active contract must remain unchanged
	if c.acceptanceSpec == nil || len(c.acceptanceSpec.Commands) != 1 || c.acceptanceSpec.Commands[0] != "test -f valid.txt" {
		t.Fatalf("c.acceptanceSpec was modified on rejection: %#v", c.acceptanceSpec)
	}

	// Verify that acceptance_audit.jsonl logged the rejected contract update
	auditPath := filepath.Join(ws, "logs", "acceptance_audit.jsonl")
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("failed to read acceptance_audit.jsonl: %v", err)
	}
	logStr := string(data)
	if !strings.Contains(logStr, "test -f valid.txt") || !strings.Contains(logStr, "rejected") {
		t.Fatalf("acceptance audit log does not record attempted rejected change: %s", logStr)
	}
	if !strings.Contains(logStr, `"event":"acceptance_contract_rejected"`) ||
		!strings.Contains(logStr, `"old_state":"configured"`) ||
		!strings.Contains(logStr, `"new_state":"empty"`) {
		t.Fatalf("acceptance audit log does not preserve configured-to-empty distinction: %s", logStr)
	}
}

func TestAcceptanceAuditDistinguishesUnsetFromExplicitEmpty(t *testing.T) {
	workspace := t.TempDir()
	events := make([]StatusEvent, 0, 2)
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "audit-states", GoalMode: "exploratory"}},
		sessionData: NewSession(),
		reportStatus: func(event StatusEvent) {
			events = append(events, event)
		},
	}

	if err := c.SetAcceptance(""); err != nil {
		t.Fatalf("exploratory mode should accept an explicit empty contract for auditability: %v", err)
	}
	if len(c.sessionData.AcceptanceContractRevisions) != 1 {
		t.Fatalf("expected one contract revision, got %d", len(c.sessionData.AcceptanceContractRevisions))
	}
	revision := c.sessionData.AcceptanceContractRevisions[0]
	if revision.OldSpec != nil || AcceptanceContractStateOf(revision.OldSpec) != AcceptanceContractUnset {
		t.Fatalf("initial contract must record an unset old state: %#v", revision)
	}
	if AcceptanceContractStateOf(&revision.NewSpec) != AcceptanceContractEmpty {
		t.Fatalf("explicit empty contract must record empty new state: %#v", revision.NewSpec)
	}

	var modified *StatusEvent
	for i := range events {
		if events[i].Type == "acceptance_contract_modified" {
			modified = &events[i]
			break
		}
	}
	if modified == nil {
		t.Fatal("missing acceptance contract audit status event")
	}
	if got := modified.Data["old_state"]; got != AcceptanceContractUnset {
		t.Fatalf("old audit state = %#v, want %q", got, AcceptanceContractUnset)
	}
	if got := modified.Data["new_state"]; got != AcceptanceContractEmpty {
		t.Fatalf("new audit state = %#v, want %q", got, AcceptanceContractEmpty)
	}
}

func TestSetGoalModeRejectsExistingVacuousAcceptance(t *testing.T) {
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "mode-switch", GoalMode: "exploratory"}},
		sessionData: NewSession(),
		goalMode:    GoalModeExploratory,
	}
	if err := c.SetAcceptance(""); err != nil {
		t.Fatalf("exploratory mode should allow an empty contract before mode switch: %v", err)
	}
	if err := c.SetGoalMode(GoalModeOutcome); err == nil || !strings.Contains(err.Error(), FindingAcceptanceVacuous) {
		t.Fatalf("outcome mode switch should reject vacuous acceptance, got %v", err)
	}
	if got := c.GoalMode(); got != GoalModeExploratory {
		t.Fatalf("failed mode switch changed goal mode to %q", got)
	}
}
