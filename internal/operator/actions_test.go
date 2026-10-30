package operator

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pending   bool
		integrity string
		binding   string
		want      bool
	}{
		{"collection with denied replay", true, "valid", "unique", true},
		{"ordinary protocol failure", false, "valid", "unique", false},
		{"invalid evidence", true, "invalid", "unique", false},
		{"ambiguous scope", true, "valid", "ambiguous", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := selectionTestSnapshot(ActivityBlocked, "blocked")
			snapshot.Integrity.Status = tc.integrity
			snapshot.Scope.BindingStatus = tc.binding
			snapshot.Scope.ProjectDir = "/project"
			primary, secondary, err := SelectActions(ActionSelectionFacts{Snapshot: snapshot, Recovery: &RecoveryEligibility{
				TaskID: "11", TaskStatus: "blocked", PolicyDenied: true, ResultCollectionBlocked: tc.pending,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if (len(secondary) != 0) != tc.want {
				t.Fatalf("primary=%+v secondary=%+v", primary, secondary)
			}
			if !tc.want {
				return
			}
			action := secondary[0]
			want := []string{"hufu", "resume", "--workspace", snapshot.Scope.WorkspaceExact, "--run", snapshot.Scope.RunID, "--branch", snapshot.Scope.BranchID, "--execution-profile", "default", "--repair-result", "11", "--dry-run"}
			if snapshot.Scope.TeamName != "" {
				want = append(want, "--team", snapshot.Scope.TeamName)
			}
			if action.ID != ActionAssessResultRepair || action.Kind != "inspect" || action.Risk != "read-only" || action.CWD != "/project" || !reflect.DeepEqual(action.Argv, want) {
				t.Fatalf("repair assessment = %+v", action)
			}
			incomplete := action.Target
			incomplete.BranchID = ""
			missing, err := BuildAction(ActionAssessResultRepair, ActionBuildInput{Target: incomplete})
			if err != nil || missing.Availability != "blocked" || len(missing.Argv) != 0 {
				t.Fatalf("incomplete target = %+v err=%v", missing, err)
			}
		})
	}
}

func TestSelectActionsAssessesEveryBlockedCollectionWithoutChangingSafetyPriority(t *testing.T) {
	snapshot := selectionTestSnapshot(ActivityBlocked, "blocked")
	snapshot.Scope.ProjectDir = "/project"
	snapshot.Scope.RequestedSemantics, snapshot.Scope.SelectionSource = "exact", "explicit"
	recoveries := []*RecoveryEligibility{
		{TaskID: "3", TaskStatus: "blocked", ResultCollectionBlocked: true, Attempt: 2},
		{TaskID: "1", TaskStatus: "blocked", ResultCollectionBlocked: true},
		{TaskID: "4", TaskStatus: "blocked", ResultCollectionBlocked: true},
		{TaskID: "2", TaskStatus: "blocked", ResultCollectionBlocked: true},
		nil, {TaskID: "ordinary", TaskStatus: "blocked"},
		{TaskID: "stale", TaskStatus: "done", ResultCollectionBlocked: true},
	}
	recoveries = append(recoveries, recoveries[0])
	for _, test := range []struct {
		name    string
		primary *RecoveryEligibility
	}{
		{"unknown external effect", &RecoveryEligibility{TaskID: "external", TaskStatus: "blocked", ExternalEffectState: "unknown", ReconcileEligible: true}},
		{"collection selected", recoveries[0]},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts := ActionSelectionFacts{Snapshot: snapshot, Recovery: test.primary, ResultCollectionRecoveries: recoveries}
			primary, secondary, err := SelectActions(facts)
			if err != nil {
				t.Fatal(err)
			}
			if primary.ID != ActionInspectTaskRecovery || primary.Target.TaskID != test.primary.TaskID {
				t.Fatalf("changed primary safety target: %+v", primary)
			}
			seen := make(map[string]bool)
			for _, action := range secondary {
				if action.ID != ActionAssessResultRepair {
					continue
				}
				if seen[action.Target.TaskID] || action.Kind != ActionKindInspect || action.Risk != "read-only" || action.Confirmation != "none" ||
					action.CWD != "/project" || action.Target.RunID != snapshot.Scope.RunID || action.Target.BranchID != snapshot.Scope.BranchID ||
					!strings.Contains(strings.Join(action.Argv, " "), "--dry-run") {
					t.Fatalf("invalid assessment: %+v", action)
				}
				seen[action.Target.TaskID] = true
			}
			if len(seen) != 4 {
				t.Fatalf("assessed %v; want all four collections", seen)
			}
			first := snapshot
			first.PrimaryAction, first.SecondaryActions = primary, secondary
			first.Attention, first.Learning.Status = AttentionReviewRequired, "unknown"
			normalized, err := FinalizeSnapshot(first)
			if err != nil {
				t.Fatal(err)
			}
			for index, taskID := range []string{"1", "2", "3", "4"} {
				if normalized.SecondaryActions[index].Target.TaskID != taskID {
					t.Fatalf("unstable task ordering: %+v", normalized.SecondaryActions)
				}
			}
		})
	}
}

func TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable(t *testing.T) {
	for _, test := range []struct{ name, binding, integrity, projection, activity string }{
		{"ambiguous", "ambiguous", "valid", "consistent", ActivityBlocked},
		{"unknown binding", "unknown", "valid", "consistent", ActivityBlocked},
		{"invalid integrity", "verified", "invalid", "consistent", ActivityBlocked},
		{"unknown integrity", "verified", "unknown", "consistent", ActivityBlocked},
		{"projection drift", "verified", "degraded", "drift", ActivityBlocked},
		{"runtime active", "verified", "valid", "consistent", ActivityExecuting},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := selectionTestSnapshot(test.activity, "")
			snapshot.Scope.BindingStatus = test.binding
			snapshot.Integrity.Status, snapshot.Integrity.Projection = test.integrity, test.projection
			_, secondary, err := SelectActions(ActionSelectionFacts{Snapshot: snapshot,
				ResultCollectionRecoveries: []*RecoveryEligibility{{TaskID: "collection", TaskStatus: "blocked", ResultCollectionBlocked: true}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range secondary {
				if action.ID == ActionAssessResultRepair {
					t.Fatalf("assessment escaped gate: %+v", action)
				}
			}
		})
	}
}

func TestBuildActionUsesOnlyRegisteredExistingInspectFlags(t *testing.T) {
	target := ActionTarget{Workspace: "/tmp/work space", TeamID: "ignored-team", RunID: "run-1", BranchID: "branch-1", TaskID: "task-1", Attempt: 2}
	action, err := BuildAction(ActionInspectTaskRecovery, ActionBuildInput{
		Target: target, ReasonCode: "external_effect_unknown", SourceRefs: []string{"receipt-z", "receipt-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hufu", "inspect", "task", "task-1", "--workspace", "/tmp/work space", "--run", "run-1", "--branch", "branch-1", "--attempt", "2"}
	if !reflect.DeepEqual(action.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", action.Argv, want)
	}
	if strings.Contains(strings.Join(action.Argv, " "), "--team") || strings.Contains(strings.Join(action.Argv, " "), "--task") {
		t.Fatalf("inspect task argv contains a non-existent or duplicate flag: %#v", action.Argv)
	}
	if !reflect.DeepEqual(action.SourceRefs, []string{"receipt-a", "receipt-z"}) {
		t.Fatalf("source refs = %#v", action.SourceRefs)
	}

	again, err := BuildAction(ActionInspectTaskRecovery, ActionBuildInput{
		Target: target, ReasonCode: "external_effect_unknown", SourceRefs: []string{"receipt-z", "receipt-a"},
	})
	if err != nil || !reflect.DeepEqual(action, again) {
		t.Fatalf("same facts produced a different action: %#v / %#v / %v", action, again, err)
	}
}

func TestMutationFacadesRemainBlockedWithEmptyArgv(t *testing.T) {
	for _, id := range []string{ActionResumeSession, ActionReconcileTask, ActionRetryTask} {
		action, err := BuildAction(id, ActionBuildInput{
			Target:        ActionTarget{Workspace: "/work", TeamID: "team", RunID: "run", BranchID: "branch", TaskID: "task", Attempt: 1},
			Preconditions: ActionPreconditions{ExpectedRevision: "sha256:revision"},
			Availability:  "available", ReasonCode: "eligible",
		})
		if err != nil {
			t.Fatal(err)
		}
		if action.Availability != "blocked" || len(action.Argv) != 0 || action.RevalidationKey == "" {
			t.Fatalf("%s escaped unpublished-facade gate: %#v", id, action)
		}
	}
}

func TestMutationActionPublishesOnlyWithFacadeAndRevision(t *testing.T) {
	target := ActionTarget{Workspace: "/work/team", TeamID: "team", RunID: "run", BranchID: "main", TaskID: "task", Attempt: 2}
	action, err := BuildAction(ActionRetryTask, ActionBuildInput{
		Target: target, Availability: "available", MutationFacadeAvailable: true,
		Preconditions: ActionPreconditions{ExpectedRevision: "sha256:revision"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hufu", "session", "retry", "--workspace", "/work/team", "--team", "team", "--run", "run", "--branch", "main", "--task", "task", "--attempt", "2"}
	if !reflect.DeepEqual(action.Argv, want) || action.Availability != "available" || action.RevalidationKey == "" {
		t.Fatalf("published action = %#v", action)
	}

	withoutRevision, err := BuildAction(ActionRetryTask, ActionBuildInput{
		Target: target, Availability: "available", MutationFacadeAvailable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if withoutRevision.Availability != "blocked" || len(withoutRevision.Argv) != 0 {
		t.Fatalf("revision-free mutation became executable: %#v", withoutRevision)
	}
}

func TestSelectActionsUnknownExternalEffectInspectsAndNeverRetries(t *testing.T) {
	snapshot := selectionTestSnapshot(ActivityBlocked, "")
	recovery := &RecoveryEligibility{
		TaskID: "task-7", Attempt: 2, TaskStatus: "blocked", ExternalEffectState: "unknown",
		ExpectedEventID: "task-event", ExpectedEventHash: "task-hash",
		EffectivePolicy: "reconcile", ExpectedRevision: "sha256:revision", ReconcileEligible: true,
		SourceRefs: []string{"receipt-9"},
	}
	primary, secondary, err := SelectActions(ActionSelectionFacts{Snapshot: snapshot, Recovery: recovery})
	if err != nil {
		t.Fatal(err)
	}
	if primary.ID != ActionInspectTaskRecovery || primary.Availability != "available" {
		t.Fatalf("primary = %#v", primary)
	}
	if primary.Preconditions.ExpectedEventID != "task-event" || primary.Preconditions.ExpectedEventHash != "task-hash" {
		t.Fatalf("task action used unrelated workspace head: %#v", primary.Preconditions)
	}
	if len(secondary) != 1 || secondary[0].ID != ActionReconcileTask || secondary[0].Availability != "blocked" || len(secondary[0].Argv) != 0 {
		t.Fatalf("secondary = %#v", secondary)
	}
	for _, action := range append([]ActionSuggestion{*primary}, secondary...) {
		if action.ID == ActionRetryTask {
			t.Fatalf("unknown external effect recommended retry: %#v", action)
		}
	}
}

func TestSelectActionsPolicyDenyHasNoBypass(t *testing.T) {
	snapshot := selectionTestSnapshot(ActivityBlocked, "")
	recovery := &RecoveryEligibility{TaskID: "task", TaskStatus: "blocked", PolicyDenied: true, EffectivePolicy: "manual", SourceRefs: []string{"policy-event"}}
	primary, secondary, err := SelectActions(ActionSelectionFacts{Snapshot: snapshot, Recovery: recovery})
	if err != nil {
		t.Fatal(err)
	}
	if primary.ID != ActionInspectTaskRecovery || primary.ReasonCode != "recovery_policy_denied" || len(secondary) != 0 {
		t.Fatalf("policy deny actions = %#v %#v", primary, secondary)
	}
	joined := strings.Join(primary.Argv, " ")
	if strings.Contains(joined, "force") || strings.Contains(joined, "retry") {
		t.Fatalf("policy denial exposed bypass argv: %q", joined)
	}
}

func TestSelectActionsRuntimeAndTerminalStates(t *testing.T) {
	active := selectionTestSnapshot(ActivityExecuting, "")
	primary, _, err := SelectActions(ActionSelectionFacts{Snapshot: active})
	if err != nil || primary.ID != ActionWaitRuntime || primary.Actor != "runtime" || len(primary.Argv) != 0 {
		t.Fatalf("active action = %#v, %v", primary, err)
	}
	finished := selectionTestSnapshot(ActivityFinished, "completed")
	primary, _, err = SelectActions(ActionSelectionFacts{Snapshot: finished})
	if err != nil || primary.ID != ActionNoneRequired || len(primary.Argv) != 0 {
		t.Fatalf("terminal action = %#v, %v", primary, err)
	}
}

func TestSelectActionsSurfacesExecutableTerminalRecovery(t *testing.T) {
	replanSnapshot := selectionTestSnapshot(ActivityFinished, "partial")
	replan := &RecoveryEligibility{TaskID: "budget", ResumeEligible: true, ReasonCode: "recovery_replan"}
	primary, secondary, err := SelectActions(ActionSelectionFacts{
		Snapshot: replanSnapshot, Recovery: replan, SessionExpectedRevision: "sha256:session",
		SessionSourceRefs: []string{"run"}, MutationFacadeAvailable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if primary.ID != ActionResumeSession || primary.ReasonCode != "replan_required" || primary.Availability != "available" || len(primary.Argv) == 0 || len(secondary) != 0 {
		t.Fatalf("replan actions = primary=%#v secondary=%#v", primary, secondary)
	}

	reconcileSnapshot := selectionTestSnapshot(ActivityFinished, "blocked")
	reconcile := &RecoveryEligibility{
		TaskID: "protocol", Attempt: 1, TaskStatus: "blocked", ReconcileEligible: true,
		ExpectedRevision: "sha256:task", ReasonCode: "recovery_reconcile",
	}
	primary, secondary, err = SelectActions(ActionSelectionFacts{Snapshot: reconcileSnapshot, Recovery: reconcile, MutationFacadeAvailable: true})
	if err != nil {
		t.Fatal(err)
	}
	if primary.ID != ActionInspectTaskRecovery || len(secondary) != 1 || secondary[0].ID != ActionReconcileTask || secondary[0].Availability != "available" || len(secondary[0].Argv) == 0 {
		t.Fatalf("reconcile actions = primary=%#v secondary=%#v", primary, secondary)
	}
}

func TestActionJourneyMatrix(t *testing.T) {
	tests := []struct {
		name         string
		snapshot     OperatorSnapshot
		recovery     *RecoveryEligibility
		sessionRev   string
		wantID       string
		availability string
	}{
		{name: "ambiguous scope", snapshot: func() OperatorSnapshot {
			s := selectionTestSnapshot(ActivityUnknown, "")
			s.Scope.BindingStatus = "ambiguous"
			return s
		}(), wantID: ActionSelectScope, availability: "blocked"},
		{name: "verifying", snapshot: selectionTestSnapshot(ActivityVerifying, ""), wantID: ActionWaitRuntime, availability: "available"},
		{name: "interrupted session", snapshot: selectionTestSnapshot(ActivityInterrupted, ""), sessionRev: "sha256:checkpoint", wantID: ActionResumeSession, availability: "blocked"},
		{name: "blocked task", snapshot: selectionTestSnapshot(ActivityBlocked, ""), recovery: &RecoveryEligibility{TaskID: "task", TaskStatus: "blocked"}, wantID: ActionInspectTaskRecovery, availability: "available"},
		{name: "partial terminal", snapshot: selectionTestSnapshot(ActivityFinished, "partial"), wantID: ActionReviewResult, availability: "available"},
		{name: "completed terminal", snapshot: selectionTestSnapshot(ActivityFinished, "completed"), wantID: ActionNoneRequired, availability: "available"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			primary, secondary, err := SelectActions(ActionSelectionFacts{Snapshot: test.snapshot, Recovery: test.recovery, SessionExpectedRevision: test.sessionRev})
			if err != nil {
				t.Fatal(err)
			}
			if primary.ID != test.wantID || primary.Availability != test.availability || len(secondary) > 2 {
				t.Fatalf("action = %#v secondary=%#v", primary, secondary)
			}
			if primary.Availability != "available" && len(primary.Argv) != 0 {
				t.Fatalf("unavailable action has argv: %#v", primary)
			}
		})
	}
}

func TestActionRevalidationIgnoresPresentationAndUnrelatedHead(t *testing.T) {
	input := ActionBuildInput{
		Target:        ActionTarget{Workspace: "/work", RunID: "run", BranchID: "branch", TaskID: "task", Attempt: 2},
		Preconditions: ActionPreconditions{ExpectedEventID: "task-event", ExpectedEventHash: "task-hash", ExpectedRevision: "task-revision"},
		ReasonCode:    "eligible", SourceRefs: []string{"receipt"},
	}
	expected, err := BuildAction(ActionRetryTask, input)
	if err != nil {
		t.Fatal(err)
	}
	current := expected
	current.ReasonCode = "translated prose changed"
	current.CWD = "/unrelated/presentation"
	current.Argv = []string{"ignored", "pretty", "command"}
	if err := RevalidateAction(expected, current); err != nil {
		t.Fatalf("presentation/unrelated state invalidated target action: %v", err)
	}
	current.Preconditions.ExpectedEventHash = "changed-task-hash"
	if err := RevalidateAction(expected, current); !errors.Is(err, ErrStaleAction) {
		t.Fatalf("changed target did not produce stale_action: %v", err)
	}
}

func TestShellRenderersQuoteMetacharactersWithoutChangingArgv(t *testing.T) {
	argv := []string{"hufu", "inspect", "task", "任務 '$(touch /tmp/pwn)' ; &", "--workspace", "C:\\work dir"}
	bash, ok := RenderArgv(argv, "bash")
	if !ok || !strings.Contains(bash, "'\"'\"'") || !strings.Contains(bash, "'$(touch /tmp/pwn)'") {
		t.Fatalf("bash rendering is not safely quoted: %q, %v", bash, ok)
	}
	powershell, ok := RenderArgv(argv, "pwsh")
	if !ok || !strings.HasPrefix(powershell, "& '") || !strings.Contains(powershell, "''") {
		t.Fatalf("PowerShell rendering is not safely quoted: %q, %v", powershell, ok)
	}
	if rendered, supported := RenderArgv(argv, "fish"); supported || rendered != "" {
		t.Fatalf("unknown shell got misleading rendering: %q, %v", rendered, supported)
	}
	if argv[3] != "任務 '$(touch /tmp/pwn)' ; &" {
		t.Fatalf("renderer mutated authoritative argv: %#v", argv)
	}
}

func TestBuildSummaryPreservesExecutableArgvWithoutTruncation(t *testing.T) {
	values := []string{"two  spaces", "single'quote", "Unicode-任務", strings.Repeat("long path ", 80)}
	argv := append([]string{"printf", "%s\\n"}, values...)
	snapshot := selectionTestSnapshot(ActivityBlocked, "")
	snapshot.PrimaryAction = &ActionSuggestion{ID: ActionInspectTaskRecovery, Kind: "inspect", Availability: "available", Argv: argv}

	summary := BuildSummaryForShell(snapshot, "sh")
	if strings.Contains(summary.Next, "…") || len(summary.Next) <= 320 {
		t.Fatalf("executable command was truncated: %q", summary.Next)
	}
	output, err := exec.Command("sh", "-c", summary.Next).Output()
	if err != nil {
		t.Fatalf("execute rendered command: %v", err)
	}
	if got, want := string(output), strings.Join(values, "\n")+"\n"; got != want {
		t.Fatalf("rendered argv round trip = %q, want %q", got, want)
	}
}

func TestBuildSummaryRejectsControlCharactersInsteadOfRenderingChangedCommand(t *testing.T) {
	snapshot := selectionTestSnapshot(ActivityBlocked, "")
	snapshot.PrimaryAction = &ActionSuggestion{
		ID: ActionInspectTaskRecovery, Kind: "inspect", Availability: "available",
		Argv: []string{"hufu", "inspect", "task", "line\nbreak"},
	}
	summary := BuildSummaryForShell(snapshot, "sh")
	if strings.Contains(summary.Next, "line break") || !strings.Contains(summary.Next, "cannot be rendered safely") {
		t.Fatalf("unsafe argv produced a misleading command: %q", summary.Next)
	}
}

func TestSafeDisplayTextStripsTerminalControlsRedactsAndBounds(t *testing.T) {
	input := "\x1b[31mRED\x1b[0m \x1b]8;;https://evil.example\x07click\x1b]8;;\x07 api_key=super-secret-value\n" + strings.Repeat("界", 400)
	got := SafeDisplayText(input, 120)
	if strings.ContainsAny(got, "\x1b\x07\n\r") || strings.Contains(got, "https://evil.example") || strings.Contains(got, "super-secret-value") {
		t.Fatalf("unsafe display text: %q", got)
	}
	if len([]rune(got)) > 120 {
		t.Fatalf("display text is unbounded: %d runes", len([]rune(got)))
	}
}

func selectionTestSnapshot(activity, outcome string) OperatorSnapshot {
	return OperatorSnapshot{
		SchemaVersion: SchemaVersion,
		Scope:         ResolvedScope{WorkspaceExact: "/work", ProjectID: "project", TeamName: "team", SessionID: "session", RunID: "run", BranchID: "branch", BindingStatus: "verified"},
		Activity:      ActivityView{State: activity, RawTaskStates: []string{}, RawReasonCodes: []string{}},
		Outcome:       OutcomeView{RunOutcome: outcome},
		Integrity:     IntegrityView{Status: "valid", EventChain: "verified", Projection: "consistent", ReasonCodes: []string{}},
		Freshness:     FreshnessView{EventID: "event", EventHash: "hash", EventOrdinal: 1, LiveState: "not_applicable"},
		Blockers:      []DiagnosticView{},
	}
}
