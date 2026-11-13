package inspect

import (
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

func TestEconomicsComparisonAggregation(t *testing.T) {
	known := func(group, outcome, acceptance string, terminal bool, amount *int64, tasks, verified int, reasons ...string) EconomicsComparedRun {
		return EconomicsComparedRun{Selection: EconomicsRunSelection{Group: group}, Terminal: terminal, CostReasons: reasons, Economics: EconomicsData{RunOutcome: outcome, Acceptance: acceptance, UsageCostMicros: amount, TaskCount: tasks, VerifiedTaskCount: verified}}
	}
	for _, tc := range []struct {
		name                                  string
		runs                                  []EconomicsComparedRun
		total, per                            *int64
		accepted, incomplete, tasks, verified int
		rate                                  *float64
		integrity                             bool
	}{
		{name: "failed spending and weighted tasks", runs: []EconomicsComparedRun{known("g", "completed", "passed", true, new(int64(7)), 1, 1), known("g", "failed", "failed", true, new(int64(11)), 9, 0), known("g", "completed", "passed", true, new(int64(3)), 0, 0)}, total: new(int64(21)), per: new(int64(11)), accepted: 2, tasks: 10, verified: 1, rate: new(0.1)},
		{name: "mixed unknown", runs: []EconomicsComparedRun{known("g", "completed", "passed", true, new(int64(7)), 1, 1), known("g", "failed", "failed", true, nil, 1, 0, "unknown_cost")}, accepted: 1, incomplete: 1, tasks: 2, verified: 1, rate: new(0.5)},
		{name: "zero accepted and zero tasks", runs: []EconomicsComparedRun{known("g", "completed", "", true, new(int64(4)), 0, 0), known("g", "partial", "passed", true, new(int64(5)), 0, 0)}, total: new(int64(9))},
		{name: "nonterminal cost", runs: []EconomicsComparedRun{known("g", "", "", false, new(int64(3)), 0, 0, "run_in_progress")}, incomplete: 1},
		{name: "cost overflow", runs: []EconomicsComparedRun{known("g", "", "", true, new(int64(math.MaxInt64)), 0, 0), known("g", "", "", true, new(int64(1)), 0, 0)}, integrity: true},
		{name: "task overflow", runs: []EconomicsComparedRun{known("g", "", "", true, new(int64(1)), math.MaxInt, 0), known("g", "", "", true, new(int64(1)), 1, 0)}, integrity: true},
		{name: "invalid tasks", runs: []EconomicsComparedRun{known("g", "", "", true, nil, 0, 1)}, integrity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groups, err := aggregateEconomicsComparisons(tc.runs)
			if tc.integrity {
				if !errors.Is(err, ErrIntegrity) {
					t.Fatalf("want integrity: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			g := groups[0]
			if !reflect.DeepEqual(g.TotalCostMicros, tc.total) || !reflect.DeepEqual(g.CostPerAcceptedRunMicros, tc.per) || g.AcceptedRunCount != tc.accepted || g.IncompleteCostRunCount != tc.incomplete || g.TaskCount != tc.tasks || g.VerifiedTaskCount != tc.verified || !reflect.DeepEqual(g.VerifiedTaskRate, tc.rate) {
				t.Fatalf("wrong aggregate: %+v", g)
			}
		})
	}
}

func comparisonManifestFixture(t *testing.T, selections []EconomicsRunSelection) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	raw, err := json.Marshal(EconomicsComparisonManifest{SchemaVersion: 1, WorkloadID: "declared-fixture", Runs: selections})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func comparisonSelection(t *testing.T, group string, fixture runFixture) EconomicsRunSelection {
	t.Helper()
	query, bound, err := bindObservationTarget(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	return EconomicsRunSelection{Group: group, Workspace: fixture.workspace, BranchID: bound.Scope.BranchID, RunID: query.RunID}
}

func TestInspectEconomicsComparisonReadOnlyAndFrozen(t *testing.T) {
	a := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent { return economicsFixtureEvents(t, events) })
	b := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent { return economicsFixtureEvents(t, events) })
	selections := []EconomicsRunSelection{comparisonSelection(t, "a", a), comparisonSelection(t, "b", b)}
	manifest := comparisonManifestFixture(t, selections)
	beforeA, beforeB := snapshotInspectWorkspace(t, a.workspace), snapshotInspectWorkspace(t, b.workspace)
	first, err := InspectEconomicsComparison(t.Context(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := InspectEconomicsComparison(t.Context(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(beforeA, snapshotInspectWorkspace(t, a.workspace)) || !reflect.DeepEqual(beforeB, snapshotInspectWorkspace(t, b.workspace)) {
		t.Fatal("read-only comparison changed sources or output")
	}
	data := first.Data.(EconomicsComparisonData)
	if data.WorkloadSource != "operator_declared" || len(data.Runs) != 2 || len(data.Groups) != 2 {
		t.Fatalf("missing scope: %+v", data)
	}
	for _, g := range data.Groups {
		if g.AcceptedRunCount != 1 || g.TotalCostMicros == nil || *g.TotalCostMicros != 21 || g.CostPerAcceptedRunMicros == nil || *g.CostPerAcceptedRunMicros != 21 {
			t.Fatalf("wrong fixture group: %+v", g)
		}
	}
	query, bound, err := bindObservationTarget(t.Context(), InspectQuery{Workspace: a.workspace, RunID: a.runID, BranchID: selections[0].BranchID})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := projectEconomicsComparisonRun(t.Context(), selections[0], query, bound)
	if err != nil {
		t.Fatal(err)
	}
	store, err := team.NewEventStore(a.workspace, "run-later", "session-evidence")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{Type: string(team.EventRunStarted), Actor: "coordinator", Payload: []byte(`{"goal":"later"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := projectEconomicsComparisonRun(t.Context(), selections[0], query, bound)
	if err != nil || !reflect.DeepEqual(frozen, after) {
		t.Fatalf("bound view changed: %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := InspectEconomicsComparison(cancelled, manifest); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel not propagated: %v", err)
	}
}

func TestEconomicsComparisonManifestValidation(t *testing.T) {
	workspace := t.TempDir()
	other := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	valid := EconomicsComparisonManifest{SchemaVersion: 1, WorkloadID: "fixture", Runs: []EconomicsRunSelection{{Group: "a", Workspace: workspace, BranchID: "branch", RunID: "run"}, {Group: "b", Workspace: other, BranchID: "branch", RunID: "run"}}}
	for _, name := range []string{"unknown field", "duplicate field", "duplicate run", "symlink duplicate", "one group", "too many runs", "too many groups", "oversize", "version", "empty label"} {
		t.Run(name, func(t *testing.T) {
			manifest := valid
			manifest.Runs = slices.Clone(valid.Runs)
			switch name {
			case "duplicate run":
				manifest.Runs[1].Workspace = workspace
			case "symlink duplicate":
				manifest.Runs[1].Workspace = alias
			case "one group":
				manifest.Runs[1].Group = "a"
			case "too many runs":
				for len(manifest.Runs) <= 100 {
					manifest.Runs = append(manifest.Runs, manifest.Runs[0])
				}
			case "too many groups":
				for i := range 11 {
					manifest.Runs = append(manifest.Runs, EconomicsRunSelection{Group: strings.Repeat("g", i+1), Workspace: workspace, BranchID: "branch", RunID: "run"})
				}
			case "version":
				manifest.SchemaVersion = 2
			case "empty label":
				manifest.WorkloadID = " "
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "unknown field":
				raw = append([]byte(`{"unexpected":true,`), raw[1:]...)
			case "duplicate field":
				raw = append([]byte(`{"schema_version":1,`), raw[1:]...)
			case "oversize":
				raw = []byte(strings.Repeat(" ", maxEconomicsComparisonManifestBytes+1))
			}
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadEconomicsComparisonManifest(t.Context(), path); !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("must reject invalid manifest: %v", err)
			}
		})
	}
	// Relative paths are relative to the manifest rather than process cwd.
	path := comparisonManifestFixture(t, valid.Runs)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var relative EconomicsComparisonManifest
	if err := json.Unmarshal(raw, &relative); err != nil {
		t.Fatal(err)
	}
	for i := range relative.Runs {
		relative.Runs[i].Workspace, err = filepath.Rel(filepath.Dir(path), relative.Runs[i].Workspace)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err = json.Marshal(relative)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadEconomicsComparisonManifest(t.Context(), path)
	if err != nil || loaded.Runs[0].Workspace != workspace {
		t.Fatalf("relative workspace failed: %+v %v", loaded, err)
	}
}

func TestInspectEconomicsComparisonSourceRejection(t *testing.T) {
	for _, mode := range []string{"branch_alias", "missing_run", "corrupt_chain"} {
		t.Run(mode, func(t *testing.T) {
			a, b := observabilityEvidenceFixture(t), observabilityEvidenceFixture(t)
			selections := []EconomicsRunSelection{comparisonSelection(t, "a", a), comparisonSelection(t, "b", b)}
			var expected error
			switch mode {
			case "branch_alias":
				tree, err := team.LoadSessionTree(a.workspace)
				if err != nil {
					t.Fatal(err)
				}
				tree.Labels["alias"] = selections[0].BranchID
				if err := team.SaveSessionTree(a.workspace, tree); err != nil {
					t.Fatal(err)
				}
				selections[0].BranchID = "alias"
				expected = ErrInvalidQuery
			case "missing_run":
				selections[0].RunID = "absent"
				expected = ErrNotFound
			case "corrupt_chain":
				store, err := runtimestore.Open(t.Context(), a.workspace)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().ExecContext(t.Context(), `UPDATE events SET payload = ? WHERE sequence = 2`, []byte(`{"tampered":true}`)); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				expected = ErrIntegrity
			}
			before := snapshotInspectWorkspace(t, a.workspace)
			if _, err := InspectEconomicsComparison(t.Context(), comparisonManifestFixture(t, selections)); !errors.Is(err, expected) {
				t.Fatalf("want %v: %v", expected, err)
			}
			if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, a.workspace)) {
				t.Fatal("rejected read changed source")
			}
		})
	}
}

func TestInspectEconomicsComparisonOutcomeAndCoverage(t *testing.T) {
	complete := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent { return economicsFixtureEvents(t, events) })
	failed := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent {
		events = economicsFixtureEvents(t, events)
		for i := range events {
			if events[i].Type == string(team.EventRunFinished) {
				var result team.RunResult
				if err := json.Unmarshal(events[i].Payload, &result); err != nil {
					t.Fatal(err)
				}
				result.Outcome = team.RunOutcomeFailed
				result.GoalSatisfied = false
				result.Acceptance = &team.AcceptanceResult{State: team.AcceptanceFailed}
				events[i].Payload = jsonBytes(t, result)
			}
		}
		return events
	})
	unknown := observabilityCostFixture(t, true)
	running := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent {
		return slices.DeleteFunc(economicsFixtureEvents(t, events), func(event team.RunEvent) bool { return event.Type == string(team.EventRunFinished) })
	})
	selections := []EconomicsRunSelection{comparisonSelection(t, "complete", complete), comparisonSelection(t, "complete", failed), comparisonSelection(t, "incomplete", unknown), comparisonSelection(t, "incomplete", running)}
	view, err := InspectEconomicsComparison(t.Context(), comparisonManifestFixture(t, selections))
	if err != nil {
		t.Fatal(err)
	}
	data := view.Data.(EconomicsComparisonData)
	g := data.Groups[0]
	if g.Group != "complete" || g.RunCount != 2 || g.AcceptedRunCount != 1 || g.OutcomeCounts["failed"] != 1 || g.TotalCostMicros == nil || *g.TotalCostMicros != 42 || g.CostPerAcceptedRunMicros == nil || *g.CostPerAcceptedRunMicros != 42 {
		t.Fatalf("failed spending excluded: %+v", g)
	}
	g = data.Groups[1]
	if g.TotalCostMicros != nil || g.CostPerAcceptedRunMicros != nil || g.IncompleteCostRunCount != 2 || g.NonterminalRunCount != 1 || g.CostUnavailableReasons["unknown_cost"] != 1 || g.CostUnavailableReasons["run_in_progress"] != 1 || g.KnownSubtotalMicros == nil || *g.KnownSubtotalMicros != 42 {
		t.Fatalf("incomplete cost fabricated: %+v", g)
	}
}

func TestInspectEconomicsComparisonSnapshotObservations(t *testing.T) {
	var selections []EconomicsRunSelection
	for _, group := range []string{"a", "b"} {
		fixture := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent {
			snapshot, err := team.ResolveRunInputSnapshot(nil, nil, "run-evidence", "invocation-"+group, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			events = economicsFixtureEvents(t, events)
			events = slices.Insert(events, 1, team.RunEvent{Type: string(team.EventRunInputsResolved), Actor: "coordinator", Payload: jsonBytes(t, snapshot)})
			for i := range events {
				if events[i].Type == string(team.EventRunFinished) {
					var result team.RunResult
					if err := json.Unmarshal(events[i].Payload, &result); err != nil {
						t.Fatal(err)
					}
					result.RunInputs = snapshot
					events[i].Payload = jsonBytes(t, result)
				}
			}
			return events
		})
		selections = append(selections, comparisonSelection(t, group, fixture))
	}
	result, err := InspectEconomicsComparison(t.Context(), comparisonManifestFixture(t, selections))
	if err != nil {
		t.Fatal(err)
	}
	data := result.Data.(EconomicsComparisonData)
	for _, run := range data.Runs {
		if len(run.Snapshots.Unavailable) != 0 || run.Snapshots.InputSnapshotID == "" || run.Snapshots.PolicyHash == "" {
			t.Fatalf("lost observed refs: %+v", run.Snapshots)
		}
	}
	for _, observation := range data.SnapshotObservations {
		if observation.UnavailableRunCount != 0 {
			t.Fatalf("invented missing refs: %+v", observation)
		}
		if observation.Field == "input_schema_hash" && (observation.Different || len(observation.ObservedValues) != 1) {
			t.Fatalf("same schema changed: %+v", observation)
		}
	}
	// Different hashes and absent observations are reported without claiming
	// semantic equivalence or inventing references for the missing run.
	observations := observeEconomicsSnapshots([]EconomicsComparedRun{{Snapshots: EconomicsSnapshotRefs{PolicyHash: "a"}}, {Snapshots: EconomicsSnapshotRefs{PolicyHash: "b"}}, {}})
	for _, observation := range observations {
		if observation.Field == "policy_hash" && (!observation.Different || observation.UnavailableRunCount != 1 || len(observation.ObservedValues) != 2) {
			t.Fatalf("difference lost: %+v", observation)
		}
	}
}
