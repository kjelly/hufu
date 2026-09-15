package team

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTaskResourceScopeSnapshotFallbackAndRoundTrip(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	for _, test := range []struct {
		name   string
		effect SideEffectClass
		mode   ResourceClaimMode
	}{
		{name: "reader", effect: SideEffectNone, mode: ResourceRead},
		{name: "writer", effect: SideEffectWorkspaceWrite, mode: ResourceExclusive},
		{name: "external", effect: SideEffectExternalWrite, mode: ResourceExclusive},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := TaskDef{Agent: "worker", Goal: test.name, SideEffect: test.effect, Resources: []ResourceClaim{{Resource: "db:test", Mode: ResourceRead}}}
			item := &TodoItem{ID: test.name, Agent: "worker", Goal: test.name, SideEffect: test.effect}
			snapshot, err := c.resolveNewTaskResourceScope(task, item, ResolvedWorkerTools{})
			if err != nil {
				t.Fatal(err)
			}
			root, err := NewWorkspacePathResourceClaim(".", test.mode)
			if err != nil || !resourceClaimCovered(root, snapshot.Claims) {
				t.Fatalf("fallback claims = %#v, want %#v", snapshot.Claims, root)
			}
			if snapshot.Source != resourceScopeSourceRuntimeFallback || snapshot.BoundedReadScope || snapshot.BoundedWriteScope {
				t.Fatalf("fallback snapshot = %#v", snapshot)
			}
			item.ResourceScopeSnapshot = snapshot
			projection, err := newTaskOccurrenceProjection(item)
			if err != nil {
				t.Fatal(err)
			}
			if projection.ResourceScopeSnapshot == snapshot {
				t.Fatal("projection aliases resource snapshot")
			}
			payload, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}})
			if len(replayed) != 1 || replayed[0].ResourceScopeSnapshot == nil || replayed[0].ResourceScopeSnapshot.Digest != snapshot.Digest {
				t.Fatalf("replayed snapshot = %#v", replayed)
			}
		})
	}
}

func TestTaskResourceScopeSnapshotRejectsTampering(t *testing.T) {
	root, err := NewWorkspacePathResourceClaim(".", ResourceRead)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &TaskResourceScopeSnapshot{Version: taskResourceScopeSnapshotVersion, Claims: []ResourceClaim{root}, Source: resourceScopeSourceRuntimeFallback}
	snapshot.Digest = taskResourceScopeDigest(snapshot)
	if err := validateTaskResourceScopeSnapshot(snapshot, SideEffectNone); err != nil {
		t.Fatal(err)
	}
	snapshot.Claims[0].Mode = ResourceExclusive
	if err := validateTaskResourceScopeSnapshot(snapshot, SideEffectNone); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered snapshot error = %v", err)
	}
}

func TestIntersectPathScopeCeilings(t *testing.T) {
	root := t.TempDir()
	requested := []string{filepath.Join(root, "internal", "team") + string(filepath.Separator)}
	got, err := IntersectPathScopeCeilings(requested, []string{root}, []string{filepath.Join(root, "internal")})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, requested) {
		t.Fatalf("intersection = %#v, want %#v", got, requested)
	}
	if _, err := IntersectPathScopeCeilings(requested, []string{filepath.Join(root, "cmd")}); err == nil {
		t.Fatal("disjoint ceiling accepted")
	}
	if got, err := IntersectReadPathScopes([]string{root}, nil); err != nil || !slices.Equal(got, []string{root}) {
		t.Fatalf("nil requested scope = %#v, %v", got, err)
	}
	if _, err := IntersectWritePathScopes([]string{root}, []string{}); err == nil {
		t.Fatal("explicit empty bounded scope accepted")
	}
}

func TestSerializeConflictingMutationTasksUsesEnvelopeClaims(t *testing.T) {
	left, _ := NewWorkspacePathResourceClaim("internal/team", ResourceWrite)
	right, _ := NewWorkspacePathResourceClaim("cmd/hufu", ResourceWrite)
	overlap, _ := NewWorkspacePathResourceClaim("internal", ResourceWrite)
	tasks := []TaskDef{{SideEffect: SideEffectWorkspaceWrite}, {SideEffect: SideEffectWorkspaceWrite}}
	disjoint := serializeConflictingMutationTasks(tasks, []TaskExecutionEnvelope{
		{ResourceScope: EffectiveTaskResourceScope{Claims: []ResourceClaim{left}}},
		{ResourceScope: EffectiveTaskResourceScope{Claims: []ResourceClaim{right}}},
	})
	if len(disjoint[1].DependsOn) != 0 {
		t.Fatalf("disjoint dependencies = %v", disjoint[1].DependsOn)
	}
	conflicting := serializeConflictingMutationTasks(tasks, []TaskExecutionEnvelope{
		{ResourceScope: EffectiveTaskResourceScope{Claims: []ResourceClaim{left}}},
		{ResourceScope: EffectiveTaskResourceScope{Claims: []ResourceClaim{overlap}}},
	})
	if !slices.Equal(conflicting[1].DependsOn, []int{0}) {
		t.Fatalf("conflicting dependencies = %v", conflicting[1].DependsOn)
	}
}

func TestNewDAGSchedulerRejectsMissingEnvelope(t *testing.T) {
	if _, err := newDAGScheduler(nil, []TaskDef{{Agent: "worker"}}, []*TodoItem{{ID: "1"}}, nil, nil); err == nil {
		t.Fatal("missing execution envelope accepted")
	}
}

func TestExecuteTasksRejectsInvalidResourceClaimBeforeCreation(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	c.delegatedTasks = make(map[string]int)
	_, err := c.ExecuteTasks(t.Context(), []TaskDef{{
		Agent: "worker", Goal: "inspect", Resources: []ResourceClaim{{Resource: "workspace:path:../escape", Mode: ResourceRead}},
	}})
	if err == nil || !strings.Contains(err.Error(), "resource claims") {
		t.Fatalf("invalid resource claim error = %v", err)
	}
	if items := c.taskTracker.TodoList().Items(); len(items) != 0 {
		t.Fatalf("invalid batch created %d Todo items", len(items))
	}
}

func TestRecordResourceClaimsResolvedIsContentFree(t *testing.T) {
	journal := &uniqueRecordingJournal{}
	c := &Coordinator{executionRunID: "run-resource", projectDir: "/home/secret", reportStatus: func(StatusEvent) {}}
	c.SetEventJournal(journal)
	root, err := NewWorkspacePathResourceClaim("internal/team", ResourceRead)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &TaskResourceScopeSnapshot{Version: taskResourceScopeSnapshotVersion, Claims: []ResourceClaim{root}, ReadPaths: []string{"internal/team/"}, BoundedReadScope: true, Source: resourceScopeSourceAuthoredWorkset}
	snapshot.Digest = taskResourceScopeDigest(snapshot)
	item := &TodoItem{ID: "1", OccurrenceRevision: 1, SideEffect: SideEffectNone, ResourceScopeSnapshot: snapshot}
	if err := c.recordResourceClaimsResolved(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(journal.events) != 1 || journal.events[0].Type != string(EventResourceClaimsResolved) {
		t.Fatalf("events = %#v", journal.events)
	}
	var payload map[string]any
	if err := json.Unmarshal(journal.events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	encoded := string(journal.events[0].Payload)
	if strings.Contains(encoded, c.projectDir) || strings.Contains(encoded, "input_schema") {
		t.Fatalf("resource event leaked runtime data: %s", encoded)
	}
}
