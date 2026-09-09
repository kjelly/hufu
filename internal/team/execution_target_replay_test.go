package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

type replayProfileIntrospector struct{}

func (replayProfileIntrospector) InspectModel(_ context.Context, _ providerintrospection.ProviderRef, _ string) (providerintrospection.RuntimeModelInfo, error) {
	return providerintrospection.RuntimeModelInfo{ConfiguredContext: 32_768, MaxOutputTokens: 256}, nil
}

func TestReplayTodoListAcceptsMatchingExecutionIdentityDualWrite(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "local", Model: "qwen3:8b"}
	item := &TodoItem{
		ID:                "task-1",
		Desc:              "implement",
		Status:            TaskPending,
		Model:             "ollama/qwen3:8b",
		ModelTopology:     []string{"ollama/qwen3:8b"},
		ExecutionTarget:   target,
		ExecutionTopology: []execution.ExecutionTarget{target},
		ProviderBinding:   &ProviderBinding{Provider: "ollama", SessionID: "session-1", EffectiveModel: "qwen3:8b"},
		BackendBinding:    &BackendBinding{Backend: "local", SessionID: "session-1", EffectiveTarget: "qwen3:8b"},
	}
	payload := mustJSON(t, taskTransitionPayload(item))
	tasks, err := ReplayTodoList([]RunEvent{{ID: "evt-1", Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}})
	if err != nil {
		t.Fatalf("ReplayTodoList() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].ExecutionTarget != target || len(tasks[0].ExecutionTopology) != 1 || tasks[0].BackendBinding == nil {
		t.Fatalf("replayed execution identity = %#v", tasks)
	}
}

func TestReplayTodoListRejectsExecutionTargetConflict(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "local", Model: "qwen3:8b"}
	payload := mustJSON(t, map[string]any{
		"id":                 "task-1",
		"model":              "ollama/qwen3:8b",
		"model_topology":     []string{"ollama/qwen3:8b"},
		"execution_target":   execution.ExecutionTarget{Backend: "openai", Model: "qwen3:8b"},
		"execution_topology": []execution.ExecutionTarget{target},
	})
	_, err := ReplayTodoList([]RunEvent{{ID: "evt-1", Type: string(EventTaskCreated), TaskID: "task-1", Payload: payload}})
	if err == nil {
		t.Fatal("ReplayTodoList() error = nil, want execution identity conflict")
	}
	if _, ok := err.(*ExecutionIdentityConflictError); !ok {
		t.Fatalf("ReplayTodoList() error type = %T, want *ExecutionIdentityConflictError", err)
	}
}

func TestReplayTodoListRejectsBackendBindingConflict(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "local", Model: "qwen3:8b"}
	payload := mustJSON(t, map[string]any{
		"id":                 "task-1",
		"model":              "ollama/qwen3:8b",
		"model_topology":     []string{"ollama/qwen3:8b"},
		"execution_target":   target,
		"execution_topology": []execution.ExecutionTarget{target},
		"provider_binding":   &ProviderBinding{Provider: "ollama", SessionID: "session-1"},
		"backend_binding":    &BackendBinding{Backend: "openai", SessionID: "session-1"},
	})
	_, err := ReplayTodoList([]RunEvent{{ID: "evt-1", Type: string(EventTaskCreated), TaskID: "task-1", Payload: payload}})
	if err == nil {
		t.Fatal("ReplayTodoList() error = nil, want backend binding conflict")
	}
	if _, ok := err.(*ExecutionIdentityConflictError); !ok {
		t.Fatalf("ReplayTodoList() error type = %T, want *ExecutionIdentityConflictError", err)
	}
}

func TestTaskTransitionPayloadOmitsEmptyExecutionIdentity(t *testing.T) {
	payload, err := json.Marshal(taskTransitionPayload(&TodoItem{ID: "task-1", Status: TaskPending}))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, exists := decoded["execution_target"]; exists {
		t.Fatalf("empty execution target was persisted: %s", payload)
	}
	if _, exists := decoded["execution_topology"]; exists {
		t.Fatalf("empty execution topology was persisted: %s", payload)
	}
}

func TestCanonicalTaskDurabilityOmitsRetiredIdentityFields(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "local", Model: "qwen3:8b"}
	item := &TodoItem{
		ID:                "task-1",
		Status:            TaskPending,
		Model:             "legacy-model",
		ModelTopology:     []string{"legacy-model", "legacy-extra"},
		SubagentProvider:  "codex",
		ProviderBinding:   &ProviderBinding{Provider: "codex", SessionID: "legacy-session"},
		ExecutionTarget:   target,
		ExecutionTopology: []execution.ExecutionTarget{target},
		BackendBinding:    &BackendBinding{Backend: "local", SessionID: "session-1"},
	}
	for name, value := range map[string]any{
		"session": &SessionData{Tasks: []*TodoItem{item}},
		"event":   taskTransitionPayload(item),
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if name == "session" {
			var sessionFields map[string]json.RawMessage
			if err := json.Unmarshal(data, &sessionFields); err != nil {
				t.Fatalf("decode session envelope: %v", err)
			}
			var tasks []map[string]json.RawMessage
			if err := json.Unmarshal(sessionFields["tasks"], &tasks); err != nil {
				t.Fatalf("decode session tasks: %v", err)
			}
			fields = tasks[0]
		}
		for _, key := range []string{"model", "model_topology", "subagent_provider", "provider_binding"} {
			if _, exists := fields[key]; exists {
				t.Fatalf("canonical %s JSON persisted retired field %q: %s", name, key, data)
			}
		}
	}

	legacy := []byte(`{"id":"legacy-1","status":"pending","model":"qwen3:8b","model_topology":["qwen3:8b"],"subagent_provider":"codex","provider_binding":{"provider":"codex","session_id":"thread-1"}}`)
	var decoded TodoItem
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatalf("decode legacy checkpoint: %v", err)
	}
	if decoded.Model != "qwen3:8b" || decoded.SubagentProvider != "codex" || decoded.ProviderBinding == nil || decoded.ProviderBinding.SessionID != "thread-1" {
		t.Fatalf("legacy checkpoint compatibility fields = %#v", decoded)
	}
}

func TestReplayTodoListReadsLegacyAndCanonicalSessionBindingEvents(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "codex", Model: "gpt-5.6-luna"}
	item := &TodoItem{ID: "task-1", Model: "gpt-5.6-luna", SubagentProvider: "codex", ExecutionTarget: target, ExecutionTopology: []execution.ExecutionTarget{target}}
	created := RunEvent{ID: "created", Type: string(EventTaskCreated), TaskID: item.ID, Payload: mustJSON(t, taskTransitionPayload(item))}
	legacy := RunEvent{ID: "legacy", Type: string(EventProviderSessionBound), TaskID: item.ID, Attempt: 1, Payload: mustJSON(t, ProviderSessionBoundPayload{TaskID: item.ID, Attempt: 1, Provider: "codex", SessionID: "thread-1"})}
	canonical := RunEvent{ID: "canonical", Type: string(EventBackendSessionBound), TaskID: item.ID, Attempt: 1, Payload: mustJSON(t, BackendSessionBoundPayload{TaskID: item.ID, Attempt: 1, ExecutionTarget: target, Backend: "codex", SessionID: "thread-1"})}
	if _, err := ReplayTodoList([]RunEvent{created, legacy, canonical}); err != nil {
		t.Fatalf("ReplayTodoList() error = %v", err)
	}
}

func TestReplayTodoListRejectsConflictingSessionBindingEvents(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "codex", Model: "gpt-5.6-luna"}
	item := &TodoItem{ID: "task-1", Model: "gpt-5.6-luna", SubagentProvider: "codex", ExecutionTarget: target, ExecutionTopology: []execution.ExecutionTarget{target}}
	created := RunEvent{ID: "created", Type: string(EventTaskCreated), TaskID: item.ID, Payload: mustJSON(t, taskTransitionPayload(item))}
	legacy := RunEvent{ID: "legacy", Type: string(EventProviderSessionBound), TaskID: item.ID, Attempt: 1, Payload: mustJSON(t, ProviderSessionBoundPayload{TaskID: item.ID, Attempt: 1, Provider: "codex", SessionID: "thread-1", CWD: "/one"})}
	canonical := RunEvent{ID: "canonical", Type: string(EventBackendSessionBound), TaskID: item.ID, Attempt: 1, Payload: mustJSON(t, BackendSessionBoundPayload{TaskID: item.ID, Attempt: 1, ExecutionTarget: target, Backend: "codex", SessionID: "thread-1", CWD: "/two"})}
	_, err := ReplayTodoList([]RunEvent{created, legacy, canonical})
	if _, ok := err.(*ExecutionIdentityConflictError); !ok {
		t.Fatalf("ReplayTodoList() error = %T %v, want execution identity conflict", err, err)
	}
}

func TestReplayTodoListAppliesLegacyExecutionTargetMigration(t *testing.T) {
	legacy := &TodoItem{ID: "task-1", Model: "gpt-5.6-luna", SubagentProvider: "codex"}
	created := RunEvent{ID: "created", Type: string(EventTaskCreated), TaskID: legacy.ID, Payload: mustJSON(t, taskTransitionPayload(legacy))}
	target := execution.ExecutionTarget{Backend: "codex", Model: "gpt-5.6-luna"}
	migrated := RunEvent{ID: "migrated", Type: string(EventExecutionTargetMigrated), TaskID: legacy.ID, Payload: mustJSON(t, ExecutionTargetMigratedPayload{TaskID: legacy.ID, LegacyModel: legacy.Model, LegacySubagentProvider: legacy.SubagentProvider, ExecutionTarget: target, MigrationVersion: executionTargetMigrationVersion})}
	tasks, err := ReplayTodoList([]RunEvent{created, migrated})
	if err != nil {
		t.Fatalf("ReplayTodoList() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].ExecutionTarget != target {
		t.Fatalf("replayed tasks = %#v", tasks)
	}
}

func TestReplayTodoListAppliesLegacyExecutionTopologyMigration(t *testing.T) {
	legacy := &TodoItem{
		ID: "task-topology", Model: "primary", ModelTopology: []string{"primary", "extra"},
		SubagentProvider: localSubagentProviderName,
	}
	created := RunEvent{ID: "created-topology", Type: string(EventTaskCreated), TaskID: legacy.ID, Payload: mustJSON(t, taskTransitionPayload(legacy))}
	topology := []execution.ExecutionTarget{{Backend: "local", Model: "primary"}, {Backend: "local", Model: "extra"}}
	migrated := RunEvent{ID: "migrated-topology", BranchID: "main", Type: string(EventExecutionTargetMigrated), TaskID: legacy.ID, Payload: mustJSON(t, ExecutionTargetMigratedPayload{
		TaskID: legacy.ID, LegacyModel: legacy.Model, LegacySubagentProvider: legacy.SubagentProvider,
		ExecutionTarget: topology[0], ExecutionTopology: topology, MigrationVersion: executionTargetMigrationVersion, BranchID: "main",
	})}
	tasks, err := ReplayTodoList([]RunEvent{created, migrated})
	if err != nil {
		t.Fatalf("ReplayTodoList() error = %v", err)
	}
	if len(tasks) != 1 || !reflect.DeepEqual(tasks[0].ExecutionTopology, topology) {
		t.Fatalf("replayed topology = %#v, want %#v", tasks, topology)
	}
}

func TestReplayTodoListRejectsMigrationTargetOrTopologyConflict(t *testing.T) {
	legacy := &TodoItem{
		ID:               "task-migration-conflict",
		Model:            "primary",
		ModelTopology:    []string{"primary", "extra"},
		SubagentProvider: localSubagentProviderName,
	}
	created := RunEvent{
		ID:      "created-migration-conflict",
		Type:    string(EventTaskCreated),
		TaskID:  legacy.ID,
		Payload: mustJSON(t, taskTransitionPayload(legacy)),
	}
	wantTopology := []execution.ExecutionTarget{
		{Backend: "local", Model: "primary"},
		{Backend: "local", Model: "extra"},
	}
	cases := []struct {
		name     string
		target   execution.ExecutionTarget
		topology []execution.ExecutionTarget
	}{
		{
			name:   "target differs",
			target: execution.ExecutionTarget{Backend: "local", Model: "other"},
			topology: []execution.ExecutionTarget{
				{Backend: "local", Model: "other"},
				{Backend: "local", Model: "extra"},
			},
		},
		{
			name:   "topology differs",
			target: wantTopology[0],
			topology: []execution.ExecutionTarget{
				wantTopology[0],
				{Backend: "local", Model: "arbitrary"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			migrated := RunEvent{
				ID:     "migrated-" + tc.name,
				Type:   string(EventExecutionTargetMigrated),
				TaskID: legacy.ID,
				Payload: mustJSON(t, ExecutionTargetMigratedPayload{
					TaskID:                 legacy.ID,
					LegacyModel:            legacy.Model,
					LegacySubagentProvider: legacy.SubagentProvider,
					ExecutionTarget:        tc.target,
					ExecutionTopology:      tc.topology,
					MigrationVersion:       executionTargetMigrationVersion,
				}),
			}
			_, err := ReplayTodoList([]RunEvent{created, migrated})
			if _, ok := err.(*ExecutionIdentityConflictError); !ok {
				t.Fatalf("ReplayTodoList() error = %T %v, want execution identity conflict", err, err)
			}
		})
	}
}

func TestReplayTodoListRejectsMigrationEvidenceMismatch(t *testing.T) {
	legacy := &TodoItem{
		ID:               "task-migration-evidence",
		Model:            "openrouter/meta/llama",
		ModelTopology:    []string{"openrouter/meta/llama"},
		SubagentProvider: localSubagentProviderName,
	}
	created := RunEvent{
		ID:      "created-migration-evidence",
		Type:    string(EventTaskCreated),
		TaskID:  legacy.ID,
		RunID:   "run-migration-evidence",
		Payload: mustJSON(t, taskTransitionPayload(legacy)),
	}
	profile := RunEvent{
		ID:     "profile-migration-evidence",
		Type:   string(EventModelProfileResolved),
		TaskID: legacy.ID,
		RunID:  created.RunID,
		Payload: mustJSON(t, modelprofile.TelemetryProjection{
			SchemaVersion: 1,
			ModelID:       "meta/llama",
			Provider:      "openrouter",
		}),
	}
	target := execution.ExecutionTarget{Backend: "openrouter", Model: "meta/llama"}
	newMigration := func(evidence []string) RunEvent {
		return RunEvent{
			ID:     "migrated-evidence-" + strings.Join(evidence, "-"),
			Type:   string(EventExecutionTargetMigrated),
			TaskID: legacy.ID,
			RunID:  created.RunID,
			Payload: mustJSON(t, ExecutionTargetMigratedPayload{
				TaskID:                 legacy.ID,
				LegacyModel:            legacy.Model,
				LegacySubagentProvider: legacy.SubagentProvider,
				ExecutionTarget:        target,
				MigrationVersion:       executionTargetMigrationVersion,
				EvidenceEventIDs:       evidence,
			}),
		}
	}

	for _, tc := range []struct {
		name     string
		evidence []string
	}{
		{name: "missing evidence"},
		{name: "unrelated evidence", evidence: []string{"unrelated-event"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReplayTodoList([]RunEvent{created, profile, newMigration(tc.evidence)})
			if _, ok := err.(*ExecutionIdentityConflictError); !ok {
				t.Fatalf("ReplayTodoList() error = %T %v, want execution identity conflict", err, err)
			}
		})
	}

	if _, err := ReplayTodoList([]RunEvent{created, profile, newMigration([]string{profile.ID})}); err != nil {
		t.Fatalf("ReplayTodoList() matching evidence error = %v", err)
	}
}

func TestMigrateLegacyExecutionTargetPersistsOrderedTopology(t *testing.T) {
	tracker := NewTaskTracker()
	item := &TodoItem{ID: "legacy-topology", Agent: "worker", Desc: "legacy topology", Status: TaskPending, Model: "primary", ModelTopology: []string{"primary", "extra"}, SubagentProvider: localSubagentProviderName}
	tracker.TodoList().Restore([]*TodoItem{item})
	created := RunEvent{ID: "created-migration-topology", Type: string(EventTaskCreated), TaskID: item.ID, Payload: mustJSON(t, taskTransitionPayload(item))}
	journal := &recordingJournal{}
	coordinator := &Coordinator{taskTracker: tracker, eventJournal: journal}
	if err := coordinator.migrateLegacyExecutionTarget(t.Context(), item); err != nil {
		t.Fatalf("migrateLegacyExecutionTarget() error = %v", err)
	}
	wantTopology := []execution.ExecutionTarget{{Backend: "local", Model: "primary"}, {Backend: "local", Model: "extra"}}
	if !reflect.DeepEqual(item.ExecutionTopology, wantTopology) {
		t.Fatalf("migrated topology = %#v, want %#v", item.ExecutionTopology, wantTopology)
	}
	if len(journal.events) != 1 {
		t.Fatalf("migration events = %d, want 1", len(journal.events))
	}
	var payload ExecutionTargetMigratedPayload
	if err := json.Unmarshal(journal.events[0].Payload, &payload); err != nil {
		t.Fatalf("decode migration payload: %v", err)
	}
	if !reflect.DeepEqual(payload.ExecutionTopology, wantTopology) || payload.BranchID != "main" {
		t.Fatalf("migration payload = %#v, want topology and main branch", payload)
	}
	replayed, err := ReplayTodoList([]RunEvent{created, journal.events[0]})
	if err != nil {
		t.Fatalf("ReplayTodoList() migrated topology error = %v", err)
	}
	if len(replayed) != 1 || !reflect.DeepEqual(replayed[0].ExecutionTopology, wantTopology) {
		t.Fatalf("replayed migrated topology = %#v, want %#v", replayed, wantTopology)
	}
}

func TestLegacyExecutionMigrationIsBranchScoped(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-branch-migration", "session-branch-migration")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	defer store.Close()
	legacy := &TodoItem{ID: "task-branch", Status: TaskPending, Model: "primary", SubagentProvider: localSubagentProviderName}
	created, err := store.AppendPersisted(RunEvent{BranchID: "main", Actor: "coordinator", Type: string(EventTaskCreated), TaskID: legacy.ID, Payload: mustJSON(t, taskTransitionPayload(legacy))})
	if err != nil {
		t.Fatalf("append task creation: %v", err)
	}
	tree := NewSessionTree()
	left, err := tree.CreateBranch("left", created.ID, store)
	if err != nil {
		t.Fatalf("create left branch: %v", err)
	}
	right, err := tree.CreateBranch("right", created.ID, store)
	if err != nil {
		t.Fatalf("create right branch: %v", err)
	}
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatalf("save session tree: %v", err)
	}

	resume := func(branch *SessionBranch) RunEvent {
		tree.ActiveBranch = branch.ID
		if err := SaveSessionTree(workspace, tree); err != nil {
			t.Fatalf("save active branch %q: %v", branch.ID, err)
		}
		store.SetBranchID(branch.ID)
		tracker := NewTaskTracker()
		item := &TodoItem{ID: legacy.ID, Agent: "worker", Desc: "legacy branch", Status: TaskPending, Model: legacy.Model, SubagentProvider: localSubagentProviderName}
		tracker.TodoList().Restore([]*TodoItem{item})
		coordinator := &Coordinator{
			session: &TeamSession{Workspace: workspace}, taskTracker: tracker,
			eventJournal: eventStoreJournal{store: store},
		}
		if err := coordinator.migrateLegacyExecutionTarget(t.Context(), item); err != nil {
			t.Fatalf("migrate branch %q: %v", branch.ID, err)
		}
		events, err := store.ReadEvents()
		if err != nil {
			t.Fatalf("read events after branch %q: %v", branch.ID, err)
		}
		for index := len(events) - 1; index >= 0; index-- {
			if events[index].Type == string(EventExecutionTargetMigrated) && events[index].BranchID == branch.ID {
				return events[index]
			}
		}
		t.Fatalf("migration event for branch %q missing", branch.ID)
		return RunEvent{}
	}
	leftMigration := resume(left)
	rightMigration := resume(right)
	if leftMigration.IdempotencyKey == rightMigration.IdempotencyKey {
		t.Fatalf("branch migration keys collided: left=%q right=%q", leftMigration.IdempotencyKey, rightMigration.IdempotencyKey)
	}
	if leftMigration.ID == rightMigration.ID {
		t.Fatalf("branch migration append was globally deduplicated: left=%q right=%q", leftMigration.ID, rightMigration.ID)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatalf("read final events: %v", err)
	}
	for _, branch := range []*SessionBranch{left, right} {
		lineage, err := projectEventsForBranch(events, tree, branch.ID)
		if err != nil {
			t.Fatalf("project %q lineage: %v", branch.ID, err)
		}
		tasks, err := ReplayTodoList(lineage)
		if err != nil {
			t.Fatalf("replay %q lineage: %v", branch.ID, err)
		}
		if len(tasks) != 1 || tasks[0].ExecutionTarget != (execution.ExecutionTarget{Backend: "local", Model: "primary"}) {
			t.Fatalf("branch %q replay = %#v, want frozen local target", branch.ID, tasks)
		}
	}
}

func TestReplayedNamedLLMTargetRetainsQualifiedAdmissionIdentity(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "openrouter", Model: "meta/foo"}
	item := &TodoItem{
		ID: "task-named-llm", Model: target.Model,
		ExecutionTarget: target, ExecutionTopology: []execution.ExecutionTarget{target},
	}
	wire := mustJSON(t, item)
	var replayed TodoItem
	if err := json.Unmarshal(wire, &replayed); err != nil {
		t.Fatalf("decode canonical task: %v", err)
	}
	if replayed.Model != target.Model {
		t.Fatalf("replayed compatibility model = %q, want leaf %q", replayed.Model, target.Model)
	}
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeLanguageModelBackend{fakeExecutionBackend{
		name: "openrouter", kind: execution.BackendKindLLM,
		caps: execution.BackendCapabilities{DirectLanguageModel: true},
	}}); err != nil {
		t.Fatalf("register named LLM: %v", err)
	}
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.taskTracker.TodoList().Restore([]*TodoItem{&replayed})
	c.setRestoredTodoIDs([]*TodoItem{&replayed})
	c.SetExecutionRegistry(registry)
	got, err := c.resolveTaskExecutionModel(nil, taskDefFromTodoItem(&replayed), replayed.ID)
	if err != nil {
		t.Fatalf("resolve replayed task model: %v", err)
	}
	if got != target.String() {
		t.Fatalf("replayed admission model = %q, want qualified %q", got, target.String())
	}
}

func TestReplayedNamedLLMProviderAdmissionUsesQualifiedModel(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "openrouter", Model: "meta/foo"}
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", map[string]config.ProviderConfig{
		"openrouter": {ProviderURL: "http://127.0.0.1:1/v1"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}
	runtime := &ModelProfileRuntime{
		manager: manager,
		resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return replayProfileIntrospector{}
		}, modelprofile.ProfileCacheOptions{}),
	}
	c := &Coordinator{modelProfileRuntime: runtime}
	got, _, err := c.resolveProviderBoundInvocationContext(t.Context(), target.String(), nil)
	if err != nil {
		t.Fatalf("resolve named provider admission: %v", err)
	}
	bound, ok := providerBoundInvocationContextFromContext(got, target.String())
	if !ok {
		t.Fatal("qualified provider admission was not bound to context")
	}
	if bound.ModelID != target.String() || bound.AdmissionContext.ModelID != target.String() {
		t.Fatalf("provider admission identity = %q/%q, want %q", bound.ModelID, bound.AdmissionContext.ModelID, target.String())
	}
}

func TestReplayedTopologySelectorsAndIndexedTargetsPreserveSameModelBackends(t *testing.T) {
	task := TaskDef{
		ModelTopology: []string{"same-model", "same-model"},
		ExecutionTopology: []execution.ExecutionTarget{
			{Backend: "local", Model: "same-model"},
			{Backend: "codex", Model: "same-model"},
		},
	}
	selectors := executionTopologySelectors(task)
	wantSelectors := []string{"local/same-model", "codex/same-model"}
	if !reflect.DeepEqual(selectors, wantSelectors) {
		t.Fatalf("replayed topology selectors = %#v, want %#v", selectors, wantSelectors)
	}
	got, err := admittedExecutionTargetAt(task, 1)
	if err != nil {
		t.Fatalf("indexed target lookup: %v", err)
	}
	want := execution.ExecutionTarget{Backend: "codex", Model: "same-model"}
	if got != want {
		t.Fatalf("indexed replayed target = %#v, want %#v", got, want)
	}
}

func TestReadCheckedActiveExecutionTasksSeesCrashWindowEventWithoutCheckpointWrite(t *testing.T) {
	workspace := t.TempDir()
	target := execution.ExecutionTarget{Backend: "codex", Model: "gpt-5.6-luna"}
	item := &TodoItem{
		ID:                "crash-window-task",
		Status:            TaskInProgress,
		Model:             target.Model,
		ExecutionTarget:   target,
		ExecutionTopology: []execution.ExecutionTarget{target},
	}
	store, err := NewEventStore(workspace, "run-crash-window", "session-crash-window")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	if err := store.Append(RunEvent{
		Type:    string(EventTaskCreated),
		TaskID:  item.ID,
		Payload: mustJSON(t, taskTransitionPayload(item)),
	}); err != nil {
		_ = store.Close()
		t.Fatalf("append task event: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close event store: %v", err)
	}

	checkpointPath := filepath.Join(workspace, "session.json")
	if _, err := os.Stat(checkpointPath); !os.IsNotExist(err) {
		t.Fatalf("test checkpoint unexpectedly exists before read: %v", err)
	}
	replayed, err := ReadCheckedActiveExecutionTasks(workspace)
	if err != nil {
		t.Fatalf("ReadCheckedActiveExecutionTasks: %v", err)
	}
	if len(replayed) != 1 || replayed[0].ID != item.ID || replayed[0].ExecutionTarget != target {
		t.Fatalf("replayed crash-window tasks = %#v", replayed)
	}
	if _, err := os.Stat(checkpointPath); !os.IsNotExist(err) {
		t.Fatalf("read-only event replay created checkpoint: %v", err)
	}
}
