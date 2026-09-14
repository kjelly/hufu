package operator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWorkspacePathModes(t *testing.T) {
	project := t.TempDir()
	tests := []struct {
		name      string
		request   WorkspaceRequest
		wantRoot  string
		wantExact string
		wantErr   bool
	}{
		{name: "legacy", request: WorkspaceRequest{RequestedPath: filepath.Join(project, "legacy"), Mode: "legacy_base", TeamName: "demo"}, wantRoot: filepath.Join(project, "legacy"), wantExact: filepath.Join(project, "legacy", "demo")},
		{name: "root", request: WorkspaceRequest{RequestedPath: filepath.Join(project, "root"), Mode: "root", TeamName: "demo"}, wantRoot: filepath.Join(project, "root"), wantExact: filepath.Join(project, "root", "demo")},
		{name: "exact", request: WorkspaceRequest{RequestedPath: filepath.Join(project, "exact"), Mode: "exact", TeamName: "demo"}, wantRoot: project, wantExact: filepath.Join(project, "exact")},
		{name: "default team", request: WorkspaceRequest{Mode: "default", TeamName: "demo", ProjectDir: project}, wantRoot: filepath.Join(project, "workspace"), wantExact: filepath.Join(project, "workspace", "demo")},
		{name: "root requires team", request: WorkspaceRequest{RequestedPath: project, Mode: "root"}, wantErr: true},
		{name: "exact requires path", request: WorkspaceRequest{Mode: "exact"}, wantErr: true},
		{name: "team traversal rejected", request: WorkspaceRequest{RequestedPath: project, Mode: "root", TeamName: "../demo"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveWorkspacePath(tt.request)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveWorkspacePath() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveWorkspacePath() error = %v", err)
			}
			if got.WorkspaceRoot != tt.wantRoot || got.WorkspaceExact != tt.wantExact {
				t.Fatalf("ResolveWorkspacePath() = root %q exact %q, want root %q exact %q", got.WorkspaceRoot, got.WorkspaceExact, tt.wantRoot, tt.wantExact)
			}
		})
	}
}

func TestResolveWorkspacePathCanonicalizesExistingSymlink(t *testing.T) {
	physical := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveWorkspacePath(WorkspaceRequest{RequestedPath: alias, Mode: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceExact != physical {
		t.Fatalf("workspace exact = %q, want %q", got.WorkspaceExact, physical)
	}
	if got.RequestedPath != alias {
		t.Fatalf("requested path = %q, want original %q", got.RequestedPath, alias)
	}
}

func TestResolveWorkspacePathCanonicalizesJoinedTeamSymlink(t *testing.T) {
	root := t.TempDir()
	physical := t.TempDir()
	alias := filepath.Join(root, "demo")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveWorkspacePath(WorkspaceRequest{RequestedPath: root, Mode: "root", TeamName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceRoot != root || got.WorkspaceExact != physical {
		t.Fatalf("joined resolution = root %q exact %q, want %q and %q", got.WorkspaceRoot, got.WorkspaceExact, root, physical)
	}
}

func TestDeriveActivityPrecedenceAndUnknownStatus(t *testing.T) {
	tests := []struct {
		name  string
		facts ActivityFacts
		want  string
	}{
		{name: "ambiguous beats terminal", facts: ActivityFacts{BindingStatus: "ambiguous", HasTerminalRun: true}, want: ActivityUnknown},
		{name: "terminal beats input", facts: ActivityFacts{BindingStatus: "verified", EventChain: "verified", HasTerminalRun: true, PendingInput: true}, want: ActivityFinished},
		{name: "input beats approval", facts: ActivityFacts{BindingStatus: "verified", EventChain: "verified", PendingInput: true, PendingApproval: true}, want: ActivityWaitingInput},
		{name: "interruption beats task", facts: ActivityFacts{BindingStatus: "verified", EventChain: "verified", DurablyInterrupted: true, TaskStates: []string{"in_progress"}}, want: ActivityInterrupted},
		{name: "blocked", facts: ActivityFacts{BindingStatus: "verified", EventChain: "verified", TaskStates: []string{"error", "in_progress"}}, want: ActivityBlocked},
		{name: "verifying", facts: ActivityFacts{BindingStatus: "verified", EventChain: "verified", TaskStates: []string{"verifying"}}, want: ActivityVerifying},
		{name: "executing", facts: ActivityFacts{BindingStatus: "verified", EventChain: "verified", TaskStates: []string{"in_progress"}}, want: ActivityExecuting},
		{name: "unknown task status", facts: ActivityFacts{BindingStatus: "verified", EventChain: "verified", TaskStates: []string{"future_state"}}, want: ActivityUnknown},
		{name: "unavailable event chain", facts: ActivityFacts{BindingStatus: "verified", EventChain: "unavailable", HasTerminalRun: true}, want: ActivityUnknown},
		{name: "projection drift", facts: ActivityFacts{BindingStatus: "verified", EventChain: "verified", RequiredProjection: "drift", HasTerminalRun: true}, want: ActivityUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveActivity(tt.facts); got.State != tt.want {
				t.Fatalf("DeriveActivity() = %q, want %q", got.State, tt.want)
			}
		})
	}
}

func TestSnapshotIDIgnoresQueryTimeLiveStateAndActions(t *testing.T) {
	base := testSnapshot()
	first, err := FinalizeSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Freshness.QueriedAt = "later"
	changed.Freshness.LiveState = "connected"
	wait, err := BuildAction(ActionWaitRuntime, ActionBuildInput{Availability: "available", ReasonCode: "runtime_active"})
	if err != nil {
		t.Fatal(err)
	}
	changed.PrimaryAction = &wait
	changed.Blockers = []DiagnosticView{{Code: "blocked", Severity: "error", Message: "translated presentation text", Ref: "task-1"}}
	base.Blockers = []DiagnosticView{{Code: "blocked", Severity: "error", Message: "original presentation text", Ref: "task-1"}}
	first, err = FinalizeSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FinalizeSnapshot(changed)
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotID != second.SnapshotID {
		t.Fatalf("presentation-only fields changed snapshot ID: %q != %q", first.SnapshotID, second.SnapshotID)
	}
	changed.Scope.RunID = "other"
	third, err := FinalizeSnapshot(changed)
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotID == third.SnapshotID {
		t.Fatal("target change did not change snapshot ID")
	}
}

func TestNormalizeSnapshotClonesPointersAndRejectsUnknownEnums(t *testing.T) {
	goal := true
	exposures := int64(2)
	snapshot := testSnapshot()
	snapshot.Outcome.GoalSatisfied = &goal
	snapshot.Learning.Exposures = &exposures
	normalized := NormalizeSnapshot(snapshot)
	goal = false
	exposures = 9
	if normalized.Outcome.GoalSatisfied == nil || !*normalized.Outcome.GoalSatisfied || normalized.Learning.Exposures == nil || *normalized.Learning.Exposures != 2 {
		t.Fatalf("normalized snapshot aliases source pointers: %#v", normalized)
	}
	snapshot.Activity.State = "future_activity"
	if _, err := FinalizeSnapshot(snapshot); err == nil {
		t.Fatal("unknown activity was accepted")
	}
}

func testSnapshot() OperatorSnapshot {
	return OperatorSnapshot{
		SchemaVersion: SchemaVersion,
		Scope: ResolvedScope{
			RequestedSemantics: "exact", WorkspaceExact: "/workspace", RunID: "run-1",
			BranchID: "main", SelectionSource: "explicit", BindingStatus: "verified",
		},
		Activity:  ActivityView{State: ActivityFinished, RawTaskStates: []string{"done"}, RawReasonCodes: []string{}},
		Outcome:   OutcomeView{RunOutcome: "completed", GoalSatisfied: new(true)},
		Integrity: IntegrityView{Status: "valid", EventChain: "verified", Projection: "consistent", ReasonCodes: []string{}},
		Freshness: FreshnessView{QueriedAt: "now", EventID: "event-1", EventHash: "hash-1", EventOrdinal: 1, LiveState: "not_applicable"},
		Attention: AttentionNone,
		Blockers:  []DiagnosticView{}, LatestChanges: []ChangeView{}, RoleTargets: []RoleTargetView{},
		Learning:         LearningView{Status: "unavailable", UnavailableReason: "phase_not_implemented"},
		SecondaryActions: []ActionSuggestion{},
	}
}
