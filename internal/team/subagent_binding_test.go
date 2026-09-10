package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// Phase 1 tests (spec.md §36 PR-01/PR-02/PR-03): durable provider binding.
// PR-00's subagent_provider_baseline_test.go established the hufu-local
// parity baseline these tests build on top of.

// --- PR-01: config schema ---------------------------------------------------

// TestSubagentProviderConfigParsing proves the team-level provider registry
// config and the agent-level default both parse into their Go types
// (spec.md §6.1, §6.2). It intentionally does not construct or register any
// real provider — PR-01 is structural parsing only.
func TestSubagentProviderConfigParsing(t *testing.T) {
	dir := t.TempDir()
	teamYAML := `name: subagent-config-test
subagent-provider-default: codex
subagent-providers:
  codex:
    type: codex-app-server
    command: [codex, app-server]
    protocol: app-server-v2
    startup-timeout: 15s
    interrupt-grace: 3s
    shutdown-grace: 3s
    max-event-bytes: 1048576
    max-transcript-bytes: 16777216
    max-concurrent: 1
    execution-world: local-sandbox
    inherit-env: [PATH, CODEX_HOME]
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(teamYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if cfg.SubagentProviderDefault != "codex" {
		t.Fatalf("SubagentProviderDefault = %q, want %q", cfg.SubagentProviderDefault, "codex")
	}
	codex, ok := cfg.SubagentProviders["codex"]
	if !ok {
		t.Fatalf("SubagentProviders = %#v, want a codex entry", cfg.SubagentProviders)
	}
	if codex.Type != "codex-app-server" || !slices.Equal(codex.Command, []string{"codex", "app-server"}) ||
		codex.Protocol != "app-server-v2" || codex.StartupTimeout != "15s" || codex.InterruptGrace != "3s" ||
		codex.ShutdownGrace != "3s" || codex.MaxEventBytes != 1048576 || codex.MaxTranscriptBytes != 16777216 || codex.MaxConcurrent != 1 ||
		codex.ExecutionWorld != "local-sandbox" || !slices.Equal(codex.InheritEnv, []string{"PATH", "CODEX_HOME"}) {
		t.Fatalf("codex provider config = %#v, want the full parsed shape", codex)
	}

	agentMD := "---\nrole: worker\nsubagent-provider: codex\n---\nImplement the change.\n"
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte(agentMD), 0o644); err != nil {
		t.Fatal(err)
	}
	def, err := parseAgentFile(filepath.Join(dir, "worker.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if def.SubagentProvider != "codex" {
		t.Fatalf("AgentDef.SubagentProvider = %q, want %q", def.SubagentProvider, "codex")
	}
}

func TestMergeCodexBackendConfigPreservesRuntimeBounds(t *testing.T) {
	base := agent.SubagentProviderConfig{
		Type: codexAppServerProviderType, MaxEventBytes: 16 << 20,
		MaxTranscriptBytes: 16 << 20, MaxConcurrent: codexDefaultMaxConcurrent,
		InheritEnv: []string{"PATH", "CODEX_HOME"},
	}
	override := agent.SubagentProviderConfig{
		MaxEventBytes: 4 << 20, MaxTranscriptBytes: 8 << 20, MaxConcurrent: 4,
	}
	got := mergeCodexBackendConfig(base, override)
	if got.MaxEventBytes != override.MaxEventBytes || got.MaxTranscriptBytes != override.MaxTranscriptBytes || got.MaxConcurrent != codexDefaultMaxConcurrent {
		t.Fatalf("merged Codex resource settings = %#v, want bounds preserved and concurrency clamped to %d", got, codexDefaultMaxConcurrent)
	}

	inherited := mergeCodexBackendConfig(base, agent.SubagentProviderConfig{})
	if inherited.MaxEventBytes != base.MaxEventBytes || inherited.MaxTranscriptBytes != base.MaxTranscriptBytes || inherited.MaxConcurrent != base.MaxConcurrent {
		t.Fatalf("merged default Codex resource settings = %#v, want base defaults preserved", inherited)
	}
}

// TestReservedLocalProviderCannotBeOverridden proves the reserved
// "hufu-local" name (any case) cannot be shadowed by a team-configured
// external provider (spec.md §6.1).
func TestReservedLocalProviderCannotBeOverridden(t *testing.T) {
	for _, name := range []string{"hufu-local", "Hufu-Local", " HUFU-LOCAL "} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			teamYAML := "name: reserved-provider-test\nsubagent-providers:\n  " + yamlQuote(name) + ":\n    type: codex-app-server\n    command: [codex, app-server]\n"
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(teamYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := parseTeamYML(dir, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("parseTeamYML error = %v, want a reserved-provider-name rejection", err)
			}
		})
	}
}

// yamlQuote wraps a YAML mapping key in double quotes so a name containing
// spaces still parses as a single map key.
func yamlQuote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `\"`) + `"`
}

// TestTaskProviderIsNotCoordinatorJSON mirrors
// TestTaskMarshalOmitsDecisionProfile (decision_types_test.go):
// SubagentProvider must round-trip through team.yaml/task-contract YAML
// (configuration-owned) but never appear in, or be settable through, the
// coordinator's JSON task surface (spec.md §6.3).
func TestTaskProviderIsNotCoordinatorJSON(t *testing.T) {
	data, err := json.Marshal(TaskDef{Agent: "worker", Goal: "do it", SubagentProvider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "codex") || strings.Contains(strings.ToLower(string(data)), "subagent") {
		t.Fatalf("marshalled TaskDef = %s, want no subagent provider field", data)
	}

	var configured TaskDef
	if err := yaml.Unmarshal([]byte("agent: worker\ngoal: do it\nsubagent-provider: codex\n"), &configured); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	if configured.SubagentProvider != "codex" {
		t.Fatalf("SubagentProvider = %q, want %q", configured.SubagentProvider, "codex")
	}

	// A coordinator tool call is JSON, not YAML: json:"-" must make the field
	// unreachable even if the payload guesses at a plausible key name.
	var injected TaskDef
	raw := `{"agent":"worker","goal":"do it","subagent_provider":"codex","subagent-provider":"codex"}`
	if err := json.Unmarshal([]byte(raw), &injected); err != nil {
		t.Fatal(err)
	}
	if injected.SubagentProvider != "" {
		t.Fatalf("JSON-injected SubagentProvider = %q, want empty", injected.SubagentProvider)
	}
}

func newPrecedenceRegistry(extra ...string) *SubagentRegistry {
	registry := NewSubagentRegistry(&callTrackingSubagentProvider{name: localSubagentProviderName})
	for _, name := range extra {
		_ = registry.Register(&callTrackingSubagentProvider{name: name})
	}
	return registry
}

// TestLegacyProviderCompatibilityPrecedenceAtCanonicalAdmission proves legacy
// provider fields are consumed once, at canonical task admission. Scheduler
// dispatch never calls the legacy resolution chain.
func TestLegacyProviderCompatibilityPrecedenceAtCanonicalAdmission(t *testing.T) {
	admit := func(t *testing.T, c *Coordinator, task TaskDef, def *agent.AgentDef) execution.ExecutionTarget {
		t.Helper()
		registry := NewExecutionRegistry()
		for _, backend := range []ExecutionBackend{
			fakeLanguageModelBackend{fakeExecutionBackend{name: "local", kind: execution.BackendKindLLM, caps: execution.BackendCapabilities{DirectLanguageModel: true}}},
			fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent},
			fakeExecutionBackend{name: "shadow", kind: execution.BackendKindAgent},
		} {
			if err := registry.Register(backend); err != nil {
				t.Fatal(err)
			}
		}
		c.SetExecutionRegistry(registry)
		canonical, err := c.canonicalizeTaskOccurrence(task, def, "model-x")
		if err != nil {
			t.Fatal(err)
		}
		return canonical.ResolvedExecutionTarget
	}
	t.Run("defaults to hufu-local", func(t *testing.T) {
		c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{}}}
		got := admit(t, c, TaskDef{}, &agent.AgentDef{Name: "worker"})
		if got.Backend != "local" {
			t.Fatalf("target=%q, want local/model-x", got)
		}
	})
	t.Run("team default overrides hufu-local", func(t *testing.T) {
		c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{SubagentProviderDefault: "Codex"}}}
		c.SetSubagentRegistry(newPrecedenceRegistry("codex"))
		got := admit(t, c, TaskDef{}, &agent.AgentDef{Name: "worker"})
		if got.Backend != "codex" {
			t.Fatalf("target=%q, want codex/model-x", got)
		}
	})
	t.Run("agent default overrides team default", func(t *testing.T) {
		c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{SubagentProviderDefault: "codex"}}}
		c.SetSubagentRegistry(newPrecedenceRegistry("codex", "shadow"))
		got := admit(t, c, TaskDef{}, &agent.AgentDef{Name: "worker", SubagentProvider: "shadow"})
		if got.Backend != "shadow" {
			t.Fatalf("target=%q, want shadow/model-x", got)
		}
	})
	t.Run("task pin overrides agent default", func(t *testing.T) {
		c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{SubagentProviderDefault: "codex"}}}
		c.SetSubagentRegistry(newPrecedenceRegistry("codex", "shadow"))
		got := admit(t, c, TaskDef{SubagentProvider: "hufu-local"}, &agent.AgentDef{Name: "worker", SubagentProvider: "shadow"})
		if got.Backend != "local" {
			t.Fatalf("target=%q, want the task-pinned local/model-x", got)
		}
	})
	t.Run("qualified model overrides agent and team defaults", func(t *testing.T) {
		c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{SubagentProviderDefault: "codex"}}}
		c.SetSubagentRegistry(newPrecedenceRegistry("codex"))
		registry := NewExecutionRegistry()
		for _, backend := range []ExecutionBackend{
			fakeLanguageModelBackend{fakeExecutionBackend{name: "local", kind: execution.BackendKindLLM, caps: execution.BackendCapabilities{DirectLanguageModel: true}}},
			fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent},
		} {
			if err := registry.Register(backend); err != nil {
				t.Fatal(err)
			}
		}
		c.SetExecutionRegistry(registry)
		canonical, err := c.canonicalizeTaskOccurrence(TaskDef{}, &agent.AgentDef{Name: "worker", SubagentProvider: "codex"}, "local/qwen3")
		if err != nil {
			t.Fatal(err)
		}
		want := execution.ExecutionTarget{Backend: "local", Model: "qwen3"}
		if canonical.ResolvedExecutionTarget != want {
			t.Fatalf("target = %#v, want %#v", canonical.ResolvedExecutionTarget, want)
		}
	})
	t.Run("unknown provider fails closed", func(t *testing.T) {
		c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{}}}
		_, err := c.canonicalizeTaskOccurrence(TaskDef{SubagentProvider: "does-not-exist"}, &agent.AgentDef{Name: "worker"}, "model-x")
		if err == nil || !strings.Contains(err.Error(), "unknown execution backend") {
			t.Fatalf("err=%v, want a fail-closed unknown-backend error", err)
		}
	})
}

// --- PR-02: durable Todo/event binding --------------------------------------

// TestProviderBindingRoundTripReplay proves SubagentProvider and a fully
// populated ProviderBinding survive both the event-replay path
// (ReduceToTodoList) and the checkpoint/event-store shadow parity check
// (CompareCanonicalProjection), the way ModelTopology already does
// (task_occurrence_durability_test.go's
// TestTaskOccurrenceModelTopologySurvivesEventReplayAndShadow).
func TestProviderBindingRoundTripReplay(t *testing.T) {
	item := todoItemFromSpec(TodoSpec{
		Agent: "worker", Desc: "provider binding replay", Goal: "provider binding replay",
		SubagentProvider: "codex",
	}, "7")
	// A later phase's session binding (thread/start) is a superset shape;
	// prove the whole struct survives replay, not just Provider.
	item.ProviderBinding = &ProviderBinding{
		Provider: "codex", Protocol: "app-server-v2", SessionID: "session-1", TurnID: "turn-1",
		ProviderVersion: "1.2.3", EffectiveModel: "gpt-5-codex", ExecutionWorldID: "world-1",
		CWD: "/workspace", SandboxMode: "workspace-write", ResumeSupported: true,
	}
	payload, err := json.Marshal(taskTransitionPayload(item))
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}})
	if len(replayed) != 1 {
		t.Fatalf("replayed = %#v, want 1 item", replayed)
	}
	if replayed[0].SubagentProvider != "codex" {
		t.Fatalf("replayed SubagentProvider = %q, want %q", replayed[0].SubagentProvider, "codex")
	}
	if !reflect.DeepEqual(replayed[0].ProviderBinding, item.ProviderBinding) {
		t.Fatalf("replayed ProviderBinding = %#v, want %#v", replayed[0].ProviderBinding, item.ProviderBinding)
	}
	if got := taskDefFromTodoItem(replayed[0]).SubagentProvider; got != "codex" {
		t.Fatalf("task definition from replayed item SubagentProvider = %q, want %q", got, "codex")
	}

	if err := CompareCanonicalProjection(&SessionData{Tasks: []*TodoItem{item}}, []RunEvent{
		{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload},
	}); err != nil {
		t.Fatalf("checkpoint/event shadow rejected provider binding parity: %v", err)
	}
}

// TestProviderSessionBindingIsIdempotent proves the reducer applies the same
// task_created event's provider fields idempotently: replaying it twice must
// not duplicate, corrupt, or change the resulting binding (spec.md §7.4).
func TestProviderSessionBindingIsIdempotent(t *testing.T) {
	item := todoItemFromSpec(TodoSpec{
		Agent: "worker", Desc: "idempotent binding", Goal: "idempotent binding",
		SubagentProvider: "codex",
	}, "9")
	item.ProviderBinding.SessionID = "session-1"
	payload, err := json.Marshal(taskTransitionPayload(item))
	if err != nil {
		t.Fatal(err)
	}
	event := RunEvent{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}

	replayed := ReduceToTodoList([]RunEvent{event, event})
	if len(replayed) != 1 {
		t.Fatalf("replayed task count = %d, want 1 (idempotent by occurrence identity)", len(replayed))
	}
	if replayed[0].SubagentProvider != "codex" || replayed[0].ProviderBinding == nil || replayed[0].ProviderBinding.SessionID != "session-1" {
		t.Fatalf("replayed provider state = %#v, want a stable codex/session-1 binding", replayed[0])
	}
}

// TestProviderBindingDoesNotRetargetAfterConfigChange proves INV-04/§6.4's
// durability half for provider identity, mirroring
// TestRestoredTodoOccurrenceKeepsCanonicalModelWithoutJournal's proof for
// model identity: a durable occurrence's frozen provider survives a
// subsequent team/agent default change.
func TestProviderBindingDoesNotRetargetAfterConfigChange(t *testing.T) {
	c, worker, _ := newSubagentBaselineCoordinator(t, "provider-freeze-model")
	item := admitSubagentBaselineTask(t, c, worker, nil)
	if item.SubagentProvider != localSubagentProviderName {
		t.Fatalf("admitted provider = %q, want %q", item.SubagentProvider, localSubagentProviderName)
	}

	// Configuration changes after admission: a different provider becomes the
	// team default, and the agent itself opts into it too.
	c.session.Config.SubagentProviderDefault = "codex"
	worker.SubagentProvider = "codex"
	c.SetSubagentRegistry(newPrecedenceRegistry("codex"))

	canonical, err := c.canonicalWorkerAttemptContextForID(item.ID)
	if err != nil {
		t.Fatalf("canonicalWorkerAttemptContextForID: %v", err)
	}
	if canonical.Task.SubagentProvider != localSubagentProviderName {
		t.Fatalf("re-derived provider = %q, want the frozen %q despite the config change", canonical.Task.SubagentProvider, localSubagentProviderName)
	}
	if got := c.todoItemByID(item.ID).SubagentProvider; got != localSubagentProviderName {
		t.Fatalf("durable Todo provider = %q, want %q", got, localSubagentProviderName)
	}
}

// TestProviderBindingAppendFailureDoesNotAdvanceProjection proves INV-11: a
// failed task_created append leaves no visible Todo carrying a provider
// binding — no durable event without a matching projection entry.
func TestProviderBindingAppendFailureDoesNotAdvanceProjection(t *testing.T) {
	c, worker, _ := newSubagentBaselineCoordinator(t, "provider-append-failure-model")
	configureEventStoreSyncFailureForEventType(t, c.eventStore, string(EventTaskCreated), 1, errors.New("injected task_created append failure"))

	ids := c.taskTracker.TodoList().ReserveIDs(1)
	resolvedModel := c.resolveAgentModel(worker, "")
	spec := TodoSpec{
		Agent: worker.Name, Desc: "append failure task", Goal: "append failure task",
		Model: resolvedModel, ModelTopology: initialTaskModelTopology(worker, resolvedModel),
		Source: TaskSourceCoordinator, Execution: ExecutionContract{RequiresResult: true},
		SubagentProvider: localSubagentProviderName,
	}
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitTaskOccurrence(context.Background(), projection, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	if _, err := c.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids); err == nil {
		t.Fatal("expected CommitTaskCreationResolved to fail on the injected append failure")
	}

	if items := c.taskTracker.TodoList().Items(); len(items) != 0 {
		t.Fatalf("todo items = %#v, want none visible after a failed task_created append", items)
	}
}

// --- PR-03: coordinator resolves the durable provider -----------------------

// directParityProvider is a minimal SubagentProvider that returns a valid
// canonical success result — enough for executeTask to reach TaskDone
// without a real model backend, so a direct-path escalation test can assert
// on the actual completion outcome (status, durable ProviderBinding,
// receipt fields), not merely that a call happened.
type directParityProvider struct {
	name  string
	calls int
}

func (p *directParityProvider) Name() string { return p.name }
func (p *directParityProvider) Capabilities() SubagentCapabilities {
	return SubagentCapabilities{}
}
func (p *directParityProvider) RunAttempt(_ context.Context, req AttemptRequest) (AttemptResult, error) {
	p.calls++
	agentName := ""
	if req.Agent != nil {
		agentName = req.Agent.Name
	}
	return AttemptResult{
		CanonicalResult: &TaskResult{
			TaskID: req.TaskID, Attempt: req.Attempt, Agent: agentName,
			Status: TaskResultStatusSuccess, Summary: "direct parity provider success",
			Source: ExternalProviderProposalSource,
		},
		Output: "direct parity provider success",
	}, nil
}

// TestDirectFastPathEscalatesNonLocalProviderToCoordinatedDispatch proves
// §28/§28.1 together: a non-hufu-local resolved provider is never executed
// through the direct path's own inline Fantasy agent (which has no
// provider binding, execution-world, or canonicalization parity of its
// own) — but, now that PR-15 has landed, it is also never simply rejected.
// It is escalated to the exact same executeTask dispatch a coordinated task
// uses, so the provider genuinely runs and the task reaches the same
// terminal state coordinated dispatch would produce. Before PR-15 this test
// asserted the opposite (a fail-closed rejection, zero provider calls); the
// mandatory-matrix requirement it satisfies — "direct path cannot silently
// bypass provider binding" — is unchanged, only how that is achieved.
// TestDirectCodexWorkerPreservesNormalTaskSemantics below is the fuller,
// real-Codex-fixture version of the same proof.
func TestDirectFastPathEscalatesNonLocalProviderToCoordinatedDispatch(t *testing.T) {
	workspace := t.TempDir()
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "shadow",
		Generation: agent.GenerationParams{Model: "direct-guard-model"},
	}
	session := &TeamSession{
		Workspace: workspace,
		Config:    agent.TeamConfig{Name: "direct-guard-test"},
		Agents:    map[string]*agent.AgentDef{"worker": worker},
	}
	c := &Coordinator{
		session: session, projectDir: workspace,
		taskTracker: NewTaskTracker(), sessionData: NewSession(), sessionTime: time.Now(),
		reportStatus: func(StatusEvent) {},
	}
	shadow := &directParityProvider{name: "shadow"}
	registry := NewSubagentRegistry(NewHufuLocalSubagentProvider(c))
	_ = registry.Register(shadow)
	c.SetSubagentRegistry(registry)

	result, err := c.RunDirectAgent(context.Background(), "worker", "do the work")
	if err != nil {
		t.Fatalf("RunDirectAgent: %v", err)
	}
	if result == nil || result.AgentName != "worker" {
		t.Fatalf("RunDirectAgent result = %#v", result)
	}
	if shadow.calls != 1 {
		t.Fatalf("shadow provider RunAttempt calls = %d, want exactly 1 (the direct path must genuinely dispatch through the resolved provider)", shadow.calls)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("todo items = %#v, want exactly one durable task occurrence created by the direct invocation", items)
	}
	if items[0].Status != TaskDone {
		t.Fatalf("task status = %s, want done (same terminal state coordinated dispatch would reach)", items[0].Status)
	}
	typedRes := c.GetTaskResult(items[0].ID)
	if typedRes == nil || typedRes.Source != ExternalProviderProposalSource || typedRes.Status != TaskResultStatusSuccess {
		t.Fatalf("TaskResult = %#v, want the provider's canonicalized result (same convergence coordinated dispatch uses, §9.3)", typedRes)
	}
}
