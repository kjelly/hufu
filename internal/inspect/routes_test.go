package inspect

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectRoutesVerifiedReadOnlyAndHistorical(t *testing.T) {
	fixture := observabilityEvidenceFixture(t)
	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
	before := snapshotInspectWorkspace(t, fixture.workspace)
	first, err := InspectRoutes(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	data := first.Data.(RoutesData)
	if data.Coverage != (RouteCoverage{Tasks: 1, Verified: 1}) || len(data.Attempts) != 1 || !data.Attempts[0].Winning ||
		data.Attempts[0].ExecutionTarget != "ollama/fixture" || data.Attempts[0].DurationMillis != 1000 ||
		data.Tasks[0].CreatedEventID == "" || data.Tasks[0].RouteKind != "unbound" ||
		data.SourceHead.EventHash == "" || data.Groups[0].VerifiedTasks != 1 {
		t.Fatalf("verified projection = %+v", data)
	}
	second, err := InspectRoutes(t.Context(), query)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("unstable replay: %v", err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic transcript", "fixture-verifier-must-not-run", fixture.workspace} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("metadata leaked %q", secret)
		}
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, fixture.workspace)) {
		t.Fatal("route read changed workspace")
	}

	// A later run modifies the same task. Historical queries must still see
	// the original done state, winner, and input head.
	store, err := team.NewEventStore(fixture.workspace, "run-later", "session-evidence")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []team.RunEvent{
		{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "later"})},
		{Type: "task_failed", TaskID: fixture.taskID, Actor: "worker", Payload: jsonBytes(t, map[string]any{
			"id": fixture.taskID, "status": team.TaskError,
		})},
	} {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	historical, err := InspectRoutes(t.Context(), query)
	if err != nil || !reflect.DeepEqual(first, historical) {
		t.Fatalf("later run changed historical output: %v", err)
	}
	query.RunID = "run-later"
	later, err := InspectRoutes(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	current := later.Data.(RoutesData)
	if current.Coverage.Tasks != 1 || current.Coverage.Verified != 0 || !current.Tasks[0].CrossRunScope ||
		current.Tasks[0].OriginRunID != fixture.runID || current.Tasks[0].CreatedEventID != data.Tasks[0].CreatedEventID {
		t.Fatalf("resume identity = %+v", current)
	}
}

func TestInspectRoutesNamedRouteRetainsDurableDigest(t *testing.T) {
	route := &team.ExecutionRouteBinding{
		Name: "fixture-route", Digest: "4693ec62769a975e12ba006ea0cfa52180045e29faf4e545256c4e3094373e42",
		Candidates: []execution.ExecutionTarget{{Backend: "ollama", Model: "fixture"}},
	}
	fixture := observabilityEvidenceFixture(t, route)
	envelope, err := InspectRoutes(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(RoutesData)
	if data.Tasks[0].RouteKind != "named" || data.Tasks[0].RouteDigest != route.Digest || len(data.Groups) != 1 ||
		data.Groups[0].RouteName != route.Name || data.Groups[0].RouteDigest != route.Digest || data.Groups[0].VerifiedTasks != 1 {
		t.Fatalf("named route projection = %+v", data)
	}
}

func TestInspectRoutesScopeAndMissingWorkspace(t *testing.T) {
	fixture := observabilityEvidenceFixture(t)
	before := snapshotInspectWorkspace(t, fixture.workspace)
	base := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
	cases := []struct {
		query InspectQuery
		want  error
	}{
		{InspectQuery{Workspace: fixture.workspace}, ErrInvalidQuery},
		{InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, TaskID: "absent"}, ErrNotFound},
		{InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, SessionID: "wrong"}, ErrScopeConflict},
		{InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, AllBranches: true}, ErrInvalidQuery},
		{InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, Attempt: 1}, ErrInvalidQuery},
		{InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, TeamID: "team"}, ErrInvalidQuery},
	}
	for _, tt := range cases {
		if _, err := InspectRoutes(t.Context(), tt.query); !errors.Is(err, tt.want) {
			t.Fatalf("query %+v: got %v, want %v", tt.query, err, tt.want)
		}
	}
	base.TaskID = fixture.taskID
	if _, err := InspectRoutes(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, fixture.workspace)) {
		t.Fatal("failed or filtered query wrote workspace")
	}
	base.Workspace = filepath.Join(t.TempDir(), "missing")
	if _, err := InspectRoutes(t.Context(), base); err == nil {
		t.Fatal("missing workspace was accepted")
	}
	if _, err := os.Stat(base.Workspace); !os.IsNotExist(err) {
		t.Fatalf("missing workspace was created: %v", err)
	}
}

func TestRouteAttemptsKeepExtraModelsAndResetDispatchDistinct(t *testing.T) {
	var events workerEvents
	events.add("run", "task", "task_created", 1, 0, map[string]any{"status": "pending"})
	events.start("run", "task", 1, 1, 1)
	events.start("run", "task", 1, 2, 10)
	todo := &team.TodoItem{ID: "task", Agent: "worker", Status: team.TaskDone, ExecutionTarget: primaryTarget()}
	for index, model := range []string{"extra-a", "extra-b", "retry"} {
		occurrence := 1
		if index == 2 {
			occurrence = 2
		}
		todo.ExecutionReceipts = append(todo.ExecutionReceipts, team.ExecutionReceipt{
			RunID: "run", TaskID: "task", Attempt: 1, OccurrenceAttempt: occurrence,
			ModelExecutionID: model, ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: model},
			ExitCode: new(0), StartedAt: workerEpoch.Add(time.Duration(occurrence) * time.Second),
			FinishedAt: workerEpoch.Add(time.Duration(occurrence+1) * time.Second),
		})
	}
	winner, err := auditverify.HashExecutionReceipt(todo.ExecutionReceipts[2])
	if err != nil {
		t.Fatal(err)
	}
	task := RouteTask{TaskID: "task", OriginRunID: "run", RouteKind: "unbound",
		WinningReceiptHash: winner, ChangedInRun: true, CompletionEvidence: "verified"}
	views := ProjectWorkerAttempts([]*team.TodoItem{todo}, events.events, time.Time{})
	rows, err := routeAttempts(todo, task, views, events.events, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].AttemptKey != rows[1].AttemptKey || rows[0].ExecutionKey == rows[1].ExecutionKey ||
		rows[2].AttemptKey == rows[0].AttemptKey || rows[2].AttemptClass != "retry" {
		t.Fatalf("dispatch/model identity = %+v", rows)
	}
	groups, coverage := summarizeRoutes([]RouteTask{task}, rows)
	if coverage.Verified != 1 || len(groups) != 3 || groups[0].VerifiedTasks != 0 || groups[1].VerifiedTasks != 0 || groups[2].VerifiedTasks != 1 {
		t.Fatalf("completion credit duplicated: %+v, %+v", groups, coverage)
	}
	for index, row := range rows {
		if row.ExecutionTarget != todo.ExecutionReceipts[index].ExecutionTarget.String() || row.RouteDigest != task.RouteDigest {
			t.Fatalf("actual target/digest lost: %+v", row)
		}
	}
}

func TestRouteEvidenceDoesNotUpgradeUnverifiedTasks(t *testing.T) {
	for _, tt := range []struct {
		name    string
		receipt team.ExecutionReceipt
		want    string
		reason  string
	}{
		{"exit zero alone", team.ExecutionReceipt{ExitCode: new(0)}, "not_verified", "missing_verification"},
		{"missing success receipt", team.ExecutionReceipt{ExitCode: new(1)}, "unavailable", "missing_receipt"},
		{"verification without binding", team.ExecutionReceipt{ExitCode: new(0), VerifyResult: &team.VerificationResult{ExitCode: 0}}, "unavailable", "evidence_unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			receipt := tt.receipt
			receipt.RunID, receipt.TaskID = "run", "task"
			state, reason, winner := routeEvidence(&team.TodoItem{ID: "task", Status: team.TaskDone, ExecutionReceipts: []team.ExecutionReceipt{receipt}},
				&team.RunResult{RunID: "run"}, nil)
			if state != tt.want || reason != tt.reason || winner != "" {
				t.Fatalf("got %s %s %s", state, reason, winner)
			}
		})
	}
}

func TestRouteTaskReuseAndResetDoNotCreateOccurrences(t *testing.T) {
	var w workerEvents
	created := w.add("origin", "1", "task_created", 1, 0, map[string]any{"status": "pending"})
	w.status("origin", "1", team.TaskDone, 1, nil)
	w.status("next", "1", team.TaskDone, 2, nil) // unchanged replay/recommit
	rows, err := routeTaskIdentities(w.events, "next")
	if err != nil || rows["1"].ChangedInRun {
		t.Fatalf("unchanged ancestor became run work: %+v %v", rows, err)
	}
	w.add("next", "1", "task_created", 2, 3, map[string]any{"status": "pending"})
	rows, err = routeTaskIdentities(w.events, "next")
	if err != nil || !rows["1"].CrossRunScope || rows["1"].CreatedEventID != created || len(rows) != 1 {
		t.Fatalf("reset changed occurrence: %+v %v", rows, err)
	}
}

func TestInspectRoutesBranchInheritanceAndArtifactCorruption(t *testing.T) {
	fixture := observabilityEvidenceFixture(t)
	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, BranchID: "main"}
	parent, err := InspectRoutes(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	parentData := parent.Data.(RoutesData)
	tree := team.NewSessionTree()
	tree.Branches["child"] = &team.SessionBranch{
		ID: "child", Name: "child", ParentID: "main", ForkEventID: parentData.SourceHead.EventID,
		CreatedAt: workerEpoch.Format(time.RFC3339),
	}
	if err := team.SaveSessionTree(fixture.workspace, tree); err != nil {
		t.Fatal(err)
	}
	store, err := team.NewEventStore(fixture.workspace, "run-child", "session-evidence")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("child")
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"sibling secret"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	query.BranchID = "child"
	inherited, err := InspectRoutes(t.Context(), query)
	if err != nil || !reflect.DeepEqual(parentData, inherited.Data) {
		t.Fatalf("inherited events changed or duplicated: %v", err)
	}
	query.BranchID, query.RunID = "main", "run-child"
	if _, err := InspectRoutes(t.Context(), query); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sibling run was visible: %v", err)
	}

	files, err := filepath.Glob(filepath.Join(fixture.workspace, "logs", "artifacts", "data", "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("artifact fixture: %v %v", files, err)
	}
	if err := os.WriteFile(files[0], []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	query.RunID = fixture.runID
	before := snapshotInspectWorkspace(t, fixture.workspace)
	if _, err := InspectRoutes(t.Context(), query); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt artifact did not fail closed: %v", err)
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, fixture.workspace)) {
		t.Fatal("corruption query repaired or changed files")
	}
}

func TestRouteAttemptsDistinguishFallbackFromRetryAndUnknownTarget(t *testing.T) {
	var events workerEvents
	events.add("run", "task", "task_created", 1, 0, map[string]any{"status": "pending"})
	events.start("run", "task", 1, 1, 1)
	fallback := execution.ExecutionTarget{Backend: "ollama", Model: "fallback"}
	events.add("run", "task", string(team.EventExecutionFallbackDecided), 1, 2, map[string]any{
		"from_attempt": 1, "to_target": fallback, "candidate_index": 1,
	})
	events.start("run", "task", 2, 1, 3)
	events.start("run", "task", 3, 1, 5)
	events.start("run", "task", 4, 1, 7) // no receipt: target must stay unavailable
	todo := &team.TodoItem{ID: "task", Status: team.TaskInProgress, ExecutionTarget: primaryTarget(),
		ExecutionRoute: &team.ExecutionRouteBinding{Name: "fixture-route", Digest: "fixture-digest", Candidates: []execution.ExecutionTarget{primaryTarget(), fallback}}}
	for i := range 3 {
		receipt := team.ExecutionReceipt{RunID: "run", TaskID: "task", Attempt: i + 1, OccurrenceAttempt: 1,
			ModelExecutionID: string(rune('a' + i)), ExecutionTarget: primaryTarget(), ExitCode: new(1), CandidateIndex: new(0)}
		if i == 1 {
			receipt.ExecutionTarget, receipt.FallbackFrom = fallback, new(primaryTarget())
			receipt.CandidateIndex = new(1)
		}
		todo.ExecutionReceipts = append(todo.ExecutionReceipts, receipt)
	}
	kind, name, digest := routeIdentity(todo)
	rows, err := routeAttempts(todo, RouteTask{RouteKind: kind, RouteName: name, RouteDigest: digest}, ProjectWorkerAttempts([]*team.TodoItem{todo}, events.events, time.Time{}), events.events, "run")
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	for index, want := range []string{"initial", "fallback", "retry", "unavailable"} {
		if rows[index].AttemptClass != want {
			t.Fatalf("attempt %d classified %q, want %q", index, rows[index].AttemptClass, want)
		}
	}
	if rows[1].ExecutionTarget != fallback.String() || rows[2].ExecutionTarget != primaryTarget().String() || rows[3].ExecutionTarget != "" {
		t.Fatalf("actual targets: %+v", rows)
	}
	if rows[1].RouteKind != "named" || rows[1].RouteName != "fixture-route" || rows[1].RouteDigest != "fixture-digest" {
		t.Fatalf("route identity lost: %+v", rows[1])
	}
	*rows[1].CandidateIndex = 99
	if *todo.ExecutionReceipts[1].CandidateIndex != 1 {
		t.Fatal("route output aliases its input receipt")
	}
}

func TestRouteAmbiguousReceiptDoesNotAddAnExecutionOrCredit(t *testing.T) {
	var w workerEvents
	w.start("run", "task", 1, 1, 1)
	w.start("run", "task", 1, 1, 10)
	receipt := team.ExecutionReceipt{RunID: "run", TaskID: "task", Attempt: 1, OccurrenceAttempt: 1,
		ModelExecutionID: "legacy", ExecutionTarget: primaryTarget(), ExitCode: new(0)}
	hash, err := auditverify.HashExecutionReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	todo := &team.TodoItem{ID: "task", Status: team.TaskDone, ExecutionReceipts: []team.ExecutionReceipt{receipt}}
	rows, err := routeAttempts(todo, RouteTask{WinningReceiptHash: hash},
		ProjectWorkerAttempts([]*team.TodoItem{todo}, w.events, time.Time{}), w.events, "run")
	if err != nil || len(rows) != 3 {
		t.Fatalf("lost ambiguous receipt metadata: %+v %v", rows, err)
	}
	if rows[2].Counted || rows[2].Winning || rows[2].ReceiptHash != hash {
		t.Fatalf("ambiguous receipt gained credit: %+v", rows[2])
	}
	groups, _ := summarizeRoutes(nil, rows)
	if len(groups) != 1 || groups[0].Attempts != 2 || groups[0].VerifiedTasks != 0 {
		t.Fatalf("receipt duplicated lifecycle executions: %+v", groups)
	}
}

func TestRouteEvidenceRequiresCurrentPassingObjectiveVerification(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*team.VerificationResult)
		state  string
	}{
		{"failed", func(v *team.VerificationResult) { v.ExitCode = 1 }, "not_verified"},
		{"timed out", func(v *team.VerificationResult) { v.TimedOut = true }, "not_verified"},
		{"overturned", func(v *team.VerificationResult) { v.Overturned = true }, "not_verified"},
		{"legacy fingerprint absent", func(v *team.VerificationResult) { v.Fingerprint = "" }, "unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := observabilityEvidenceFixture(t)
			lineage, err := LoadLineage(t.Context(), InspectQuery{Workspace: fixture.workspace})
			if err != nil {
				t.Fatal(err)
			}
			raw := make([]team.RunEvent, len(lineage.Events))
			for i, event := range lineage.Events {
				raw[i] = event.Event
			}
			todos, err := team.ReplayTodoList(raw)
			if err != nil {
				t.Fatal(err)
			}
			explanation, err := auditverify.ExplainLineageRun(t.Context(), fixture.workspace, fixture.runID, raw)
			if err != nil {
				t.Fatal(err)
			}
			result := team.ReduceToSessionData(raw).RunResult
			if state, _, _ := routeEvidence(todos[0], result, explanation); state != "verified" {
				t.Fatal("positive control was not verified")
			}
			tt.change(todos[0].ExecutionReceipts[0].VerifyResult)
			state, _, hash := routeEvidence(todos[0], result, explanation)
			if state != tt.state || hash != "" {
				t.Fatalf("invalid verifier received credit: %s %s", state, hash)
			}
		})
	}
}

func TestRouteReceiptCannotClaimAnotherFrozenCandidate(t *testing.T) {
	fallback := execution.ExecutionTarget{Backend: "ollama", Model: "fallback"}
	todo := &team.TodoItem{ExecutionRoute: &team.ExecutionRouteBinding{Candidates: []execution.ExecutionTarget{primaryTarget(), fallback}}}
	for _, receipt := range []team.ExecutionReceipt{
		{ExecutionTarget: primaryTarget(), CandidateIndex: new(1)},
		{ExecutionTarget: fallback, CandidateIndex: new(2)},
		{ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "outside"}},
	} {
		if err := routeReceiptTargetIntegrity(todo, receipt); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("conflicting target was accepted: %+v %v", receipt, err)
		}
	}
	if err := routeReceiptTargetIntegrity(todo, team.ExecutionReceipt{ExecutionTarget: fallback, CandidateIndex: new(1)}); err != nil {
		t.Fatalf("valid fallback target rejected: %v", err)
	}
}

func TestRouteOptionalPayloadAbsenceNeedsExplicitValidationEvidence(t *testing.T) {
	todo := &team.TodoItem{ResultContract: &team.ResultContractRef{ID: "schema.json", SchemaSHA256: "frozen-schema"}}
	receipt := &team.ExecutionReceipt{ResultContractID: "schema.json", ResultContractSHA256: "frozen-schema", ResultValidation: team.ResultValidationAbsent}
	if !routeResultContractAvailable(todo, receipt) {
		t.Fatal("explicitly optional absent payload was treated as missing evidence")
	}
	todo.ResultContract.RequireStructured = true
	if routeResultContractAvailable(todo, receipt) {
		t.Fatal("required payload absence was accepted")
	}
	todo.ResultContract.RequireStructured = false
	receipt.ResultValidation = ""
	if routeResultContractAvailable(todo, receipt) {
		t.Fatal("unknown validation state was treated as known absence")
	}
}
