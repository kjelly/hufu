package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

func TestPreflightExecutionTargetsWorkerCodexChecksExecutableWithoutLaunching(t *testing.T) {
	calls := 0
	err := preflightExecutionTargets(&team.TeamSession{Config: agent.TeamConfig{
		WorkerModel: "codex/gpt-5.6-luna",
	}}, &config.Config{}, team.RoleModels{}, func(binary string) (string, error) {
		calls++
		if binary != "codex" {
			t.Fatalf("LookPath binary = %q, want codex", binary)
		}
		return "/test/codex", nil
	})
	if err != nil {
		t.Fatalf("preflightExecutionTargets() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("LookPath calls = %d, want 1", calls)
	}
}

func TestPreflightExecutionTargetsRejectsMissingCodexBeforeRuntime(t *testing.T) {
	err := preflightExecutionTargets(&team.TeamSession{Config: agent.TeamConfig{
		WorkerModel: "codex/gpt-5.6-luna",
	}}, &config.Config{}, team.RoleModels{}, func(string) (string, error) {
		return "", errors.New("not found")
	})
	if err == nil || !strings.Contains(err.Error(), "codex executable") {
		t.Fatalf("preflightExecutionTargets() error = %v, want missing codex executable", err)
	}
}

func TestPreflightExecutionTargetsChecksAgentGenerationAndExtraModels(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{WorkerModel: "local/default"},
		Agents: map[string]*agent.AgentDef{
			"builder": {
				Name:        "builder",
				Role:        "worker",
				Generation:  agent.GenerationParams{Model: "codex/primary"},
				ExtraModels: []string{"codex/extra"},
			},
		},
	}
	var binaries []string
	err := preflightExecutionTargets(session, &config.Config{}, team.RoleModels{}, func(binary string) (string, error) {
		binaries = append(binaries, binary)
		return "/test/" + binary, nil
	})
	if err != nil {
		t.Fatalf("preflightExecutionTargets() error = %v", err)
	}
	if len(binaries) != 2 || binaries[0] != "codex" || binaries[1] != "codex" {
		t.Fatalf("LookPath binaries = %v, want one check per agent target leaf", binaries)
	}
}

func TestPreflightExecutionTargetsRejectsMissingAgentExtraModel(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{WorkerModel: "local/default"},
		Agents: map[string]*agent.AgentDef{
			"builder": {Name: "builder", Role: "worker", ExtraModels: []string{"codex/extra"}},
		},
	}
	err := preflightExecutionTargets(session, &config.Config{}, team.RoleModels{}, func(string) (string, error) {
		return "", errors.New("not found")
	})
	if err == nil || !strings.Contains(err.Error(), "agent builder extra model 1") || !strings.Contains(err.Error(), "codex executable") {
		t.Fatalf("preflightExecutionTargets() error = %v, want missing agent extra-model executable", err)
	}
}

func TestPreflightExecutionTargetsQualifiedLocalOverridesLegacyCodex(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{
			WorkerModel:             "local/qwen3",
			SubagentProviderDefault: "codex",
		},
		Agents: map[string]*agent.AgentDef{
			"builder": {
				Name:             "builder",
				Role:             "worker",
				SubagentProvider: "codex",
				Generation:       agent.GenerationParams{Model: "local/qwen3"},
			},
		},
	}
	calls := 0
	err := preflightExecutionTargets(session, &config.Config{}, team.RoleModels{}, func(string) (string, error) {
		calls++
		return "", errors.New("codex is unavailable")
	})
	if err != nil {
		t.Fatalf("preflightExecutionTargets() error = %v, want qualified local target to bypass Codex", err)
	}
	if calls != 0 {
		t.Fatalf("agent backend executable lookups = %d, want 0 for explicit local targets", calls)
	}
}

func TestPreflightExecutionTargetsChecksCustomAgentExecutable(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{
		WorkerModel: "remote/worker-model",
		SubagentProviders: map[string]agent.SubagentProviderConfig{
			"remote": {Type: "codex-app-server", Command: []string{"missing-agent"}},
		},
	}}
	err := preflightExecutionTargets(session, &config.Config{}, team.RoleModels{}, func(binary string) (string, error) {
		if binary != "missing-agent" {
			t.Fatalf("LookPath binary = %q, want missing-agent", binary)
		}
		return "", errors.New("not found")
	})
	if err == nil || !strings.Contains(err.Error(), "remote executable") || !strings.Contains(err.Error(), "missing-agent") {
		t.Fatalf("preflightExecutionTargets() error = %v, want custom agent executable failure", err)
	}
}

func TestPreflightExecutionTargetsRejectsUnsupportedLegacyAgentTypeBeforeLifecycle(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	session := &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{
		WorkerModel: "remote/worker-model",
		SubagentProviders: map[string]agent.SubagentProviderConfig{
			"remote": {Type: "unsupported", Command: []string{"true"}},
		},
	}}
	lookups := 0
	err := preflightExecutionTargets(session, &config.Config{}, team.RoleModels{}, func(string) (string, error) {
		lookups++
		return "/bin/true", nil
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("preflightExecutionTargets() error = %v, want unsupported legacy agent type", err)
	}
	if lookups != 0 {
		t.Fatalf("unsupported legacy agent type reached executable lookup %d times", lookups)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "session.json")); !os.IsNotExist(statErr) {
		t.Fatalf("preflight created session.json at %q: %v", workspace, statErr)
	}
}

func TestApplyConfiguredBackendsRejectsUnsupportedLegacyAgentTypeWithoutMutation(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{
		SubagentProviders: map[string]agent.SubagentProviderConfig{
			"remote": {Type: "unsupported", Command: []string{"true"}},
		},
	}}
	err := applyConfiguredBackends(session, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("applyConfiguredBackends() error = %v, want unsupported legacy agent type", err)
	}
	if len(session.Config.SubagentProviders) != 1 || session.Config.SubagentProviders["remote"].Type != "unsupported" {
		t.Fatalf("legacy provider config mutated after rejection: %#v", session.Config.SubagentProviders)
	}
}

func TestPreflightExecutionTargetsRejectsReservedLocalAgentBackend(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{SubagentProviders: map[string]agent.SubagentProviderConfig{
		"local": {Type: "codex-app-server", Command: []string{"codex", "app-server"}},
	}}}
	err := preflightExecutionTargets(session, &config.Config{}, team.RoleModels{}, nil)
	if err == nil || !strings.Contains(err.Error(), "reserved backend name") {
		t.Fatalf("preflightExecutionTargets() error = %v, want reserved local rejection", err)
	}
}

func TestPreflightExecutionTargetsRejectsAgentBackendForLLMRoles(t *testing.T) {
	err := preflightExecutionTargets(&team.TeamSession{Config: agent.TeamConfig{
		WorkerModel:      "local/qwen3",
		CoordinatorModel: "codex/gpt-5.6-luna",
	}}, &config.Config{}, team.RoleModels{}, nil)
	if err == nil || !strings.Contains(err.Error(), "coordinator target") || !strings.Contains(err.Error(), "direct language model") {
		t.Fatalf("preflightExecutionTargets() error = %v, want coordinator LLM-kind error", err)
	}
}

func TestPreflightExecutionTargetsPreservesSelectorModelRemainder(t *testing.T) {
	err := preflightExecutionTargets(&team.TeamSession{Config: agent.TeamConfig{
		WorkerModel: "openrouter/meta/foo",
		Providers:   map[string]config.ProviderConfig{"openrouter": {}},
	}}, &config.Config{}, team.RoleModels{}, nil)
	if err != nil {
		t.Fatalf("preflightExecutionTargets() error = %v", err)
	}
}

func TestPreflightExecutionTargetsRejectsAgentDefaultForBareSelector(t *testing.T) {
	err := preflightExecutionTargets(&team.TeamSession{Config: agent.TeamConfig{
		WorkerModel:       "qwen3",
		DefaultLLMBackend: "codex",
	}}, &config.Config{}, team.RoleModels{}, nil)
	if err == nil || !strings.Contains(err.Error(), "default LLM backend") {
		t.Fatalf("preflightExecutionTargets() error = %v, want invalid default LLM backend", err)
	}
}

func TestPreflightSidecarTargetRejectsAgentBackend(t *testing.T) {
	err := preflightSidecarTarget(&team.TeamSession{}, &config.Config{}, "codex/gpt-5.6-luna", nil)
	if err == nil || !strings.Contains(err.Error(), "sidecar target") || !strings.Contains(err.Error(), "direct language model") {
		t.Fatalf("preflightSidecarTarget() error = %v, want LLM-kind error", err)
	}
}

func TestPreflightRestoredExecutionTargetsUsesFrozenTarget(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{WorkerModel: "local/current"}}
	err := preflightRestoredExecutionTargets(session, &config.Config{}, []*team.TodoItem{{
		ID:     "resume-1",
		Status: team.TaskInProgress,
		ExecutionTarget: execution.ExecutionTarget{
			Backend: "codex",
			Model:   "gpt-5.6-luna",
		},
	}}, func(binary string) (string, error) {
		if binary != "codex" {
			t.Fatalf("LookPath binary = %q, want frozen Codex target", binary)
		}
		return "/test/codex", nil
	})
	if err != nil {
		t.Fatalf("preflightRestoredExecutionTargets() error = %v", err)
	}
	if session.Config.WorkerModel != "local/current" {
		t.Fatalf("session worker target mutated to %q", session.Config.WorkerModel)
	}
}

func TestPreflightRestoredExecutionTargetsMigratesLegacyCodexReadOnly(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{WorkerModel: "local/current"}}
	item := &team.TodoItem{
		ID:               "legacy-resume-codex",
		Status:           team.TaskInProgress,
		Model:            "gpt-5.6-luna",
		SubagentProvider: "codex",
	}
	lookups := 0
	err := preflightRestoredExecutionTargets(session, &config.Config{}, []*team.TodoItem{item}, func(binary string) (string, error) {
		lookups++
		if binary != "codex" {
			t.Fatalf("LookPath binary = %q, want codex", binary)
		}
		return "", errors.New("codex unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "legacy-resume-codex") || !strings.Contains(err.Error(), "codex executable") {
		t.Fatalf("preflightRestoredExecutionTargets() error = %v, want legacy Codex rejection", err)
	}
	if lookups != 1 {
		t.Fatalf("agent executable lookups = %d, want one read-only legacy-target check", lookups)
	}
	if item.ExecutionTarget != (execution.ExecutionTarget{}) {
		t.Fatalf("legacy checkpoint target mutated during preflight: %#v", item.ExecutionTarget)
	}
	if session.Config.WorkerModel != "local/current" {
		t.Fatalf("session worker target mutated to %q", session.Config.WorkerModel)
	}
}

func TestPreflightRestoredExecutionTargetsRejectsAmbiguousLegacyQualifiedModel(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{WorkerModel: "local/current"}}
	item := &team.TodoItem{
		ID:               "legacy-ambiguous",
		Status:           team.TaskInProgress,
		Model:            "openrouter/meta/foo",
		SubagentProvider: "hufu-local",
	}
	lookups := 0
	err := preflightRestoredExecutionTargets(session, &config.Config{}, []*team.TodoItem{item}, func(string) (string, error) {
		lookups++
		return "/unexpected/backend", nil
	})
	if err == nil || !strings.Contains(err.Error(), "legacy execution target ambiguous") {
		t.Fatalf("preflightRestoredExecutionTargets() error = %v, want ambiguous legacy-target rejection", err)
	}
	if lookups != 0 {
		t.Fatalf("backend executable lookups = %d, want no lookup for ambiguous identity", lookups)
	}
	if item.ExecutionTarget != (execution.ExecutionTarget{}) {
		t.Fatalf("ambiguous legacy checkpoint target mutated during preflight: %#v", item.ExecutionTarget)
	}
}

func TestPreflightRestoredExecutionTargetsUsesCheckedEventTargetForStaleCheckpoint(t *testing.T) {
	target := execution.ExecutionTarget{Backend: "openrouter", Model: "meta/foo"}
	session := &team.TeamSession{Config: agent.TeamConfig{
		WorkerModel: "local/current",
		Providers:   map[string]config.ProviderConfig{"openrouter": {}},
	}}
	checkpoint := &team.TodoItem{
		ID:               "stale-checkpoint",
		Status:           team.TaskInProgress,
		Model:            "openrouter/meta/foo",
		SubagentProvider: "hufu-local",
	}
	replayed := &team.TodoItem{
		ID:                checkpoint.ID,
		Status:            team.TaskInProgress,
		ExecutionTarget:   target,
		ExecutionTopology: []execution.ExecutionTarget{target},
	}
	lookups := 0
	err := preflightRestoredExecutionTargetsWithEvidence(session, &config.Config{}, []*team.TodoItem{checkpoint}, nil, []*team.TodoItem{replayed}, func(string) (string, error) {
		lookups++
		return "/unexpected/backend", nil
	})
	if err != nil {
		t.Fatalf("preflightRestoredExecutionTargetsWithEvidence() error = %v", err)
	}
	if lookups != 0 {
		t.Fatalf("agent executable lookups = %d, want no lookup for checked LLM event target", lookups)
	}
	if !checkpoint.ExecutionTarget.IsZero() {
		t.Fatalf("stale checkpoint target mutated during preflight: %#v", checkpoint.ExecutionTarget)
	}
}

func TestPreflightRestoredExecutionTargetsChecksEveryTopologyLeaf(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{WorkerModel: "local/current"}}
	item := &team.TodoItem{
		ID:              "resume-fanout",
		Status:          team.TaskInProgress,
		ExecutionTarget: execution.ExecutionTarget{Backend: "local", Model: "primary"},
		ExecutionTopology: []execution.ExecutionTarget{
			{Backend: "local", Model: "primary"},
			{Backend: "codex", Model: "extra"},
		},
	}
	lookups := 0
	err := preflightRestoredExecutionTargets(session, &config.Config{}, []*team.TodoItem{item}, func(binary string) (string, error) {
		lookups++
		if binary != "codex" {
			t.Fatalf("LookPath binary = %q, want codex", binary)
		}
		return "", errors.New("codex unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "topology leaf 1") || !strings.Contains(err.Error(), "codex executable") {
		t.Fatalf("preflightRestoredExecutionTargets() error = %v, want extra Codex leaf rejection", err)
	}
	if lookups != 1 {
		t.Fatalf("agent executable lookups = %d, want one frozen extra-leaf check", lookups)
	}
	if session.Config.WorkerModel != "local/current" {
		t.Fatalf("session worker target mutated to %q", session.Config.WorkerModel)
	}
}

func TestPreflightRestoredExecutionTargetsRejectsTopologyMissingPrimaryWithoutMutation(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{WorkerModel: "local/current"}}
	primary := execution.ExecutionTarget{Backend: "local", Model: "primary"}
	item := &team.TodoItem{
		ID:              "resume-missing-primary",
		Status:          team.TaskInProgress,
		ExecutionTarget: primary,
		ExecutionTopology: []execution.ExecutionTarget{
			{Backend: "local", Model: "other"},
		},
	}
	lookups := 0
	err := preflightRestoredExecutionTargets(session, &config.Config{}, []*team.TodoItem{item}, func(string) (string, error) {
		lookups++
		return "/unexpected/backend", nil
	})
	if err == nil || !strings.Contains(err.Error(), "absent from its frozen topology") {
		t.Fatalf("preflightRestoredExecutionTargets() error = %v, want missing-primary topology rejection", err)
	}
	if lookups != 0 {
		t.Fatalf("agent executable lookups = %d, want no lookup after identity rejection", lookups)
	}
	if item.ExecutionTarget != primary || len(item.ExecutionTopology) != 1 || item.ExecutionTopology[0].Model != "other" {
		t.Fatalf("restored canonical identity mutated during preflight: %#v", item)
	}
	if session.Config.WorkerModel != "local/current" {
		t.Fatalf("session worker target mutated to %q", session.Config.WorkerModel)
	}
}

func TestPreflightRestoredEventLineageRejectsCrashWindowBeforeCheckpointMutation(t *testing.T) {
	workspace := t.TempDir()
	target := execution.ExecutionTarget{Backend: "codex", Model: "gpt-5.6-luna"}
	payload, err := json.Marshal(map[string]any{
		"id":                 "crash-window-task",
		"status":             string(team.TaskInProgress),
		"model":              target.Model,
		"model_topology":     []string{target.Model},
		"subagent_provider":  "codex",
		"execution_target":   target,
		"execution_topology": []execution.ExecutionTarget{target},
	})
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	store, err := team.NewEventStore(workspace, "run-crash-window", "session-crash-window")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	if err := store.Append(team.RunEvent{Type: string(team.EventTaskCreated), TaskID: "crash-window-task", Payload: payload}); err != nil {
		_ = store.Close()
		t.Fatalf("append task event: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close event store: %v", err)
	}

	session := &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{WorkerModel: "local/current"}}
	lookups := 0
	err = preflightRestoredEventLineage(session, &config.Config{}, func(binary string) (string, error) {
		lookups++
		if binary != "codex" {
			t.Fatalf("LookPath binary = %q, want codex", binary)
		}
		return "", errors.New("codex unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "crash-window-task") || !strings.Contains(err.Error(), "codex executable") {
		t.Fatalf("preflightRestoredEventLineage() error = %v, want crash-window target rejection", err)
	}
	if lookups != 1 {
		t.Fatalf("agent executable lookups = %d, want one read-only target check", lookups)
	}
	if session.Config.WorkerModel != "local/current" {
		t.Fatalf("session worker target mutated to %q", session.Config.WorkerModel)
	}
	if _, err := os.Stat(filepath.Join(workspace, "session.json")); !os.IsNotExist(err) {
		t.Fatalf("crash-window preflight created session checkpoint: %v", err)
	}
}

func TestPreflightRestoredEventLineageMigratesLegacyCodexBeforeCheckpointMutation(t *testing.T) {
	workspace := t.TempDir()
	payload, err := json.Marshal(map[string]any{
		"id":                "legacy-crash-window",
		"status":            string(team.TaskInProgress),
		"model":             "gpt-5.6-luna",
		"model_topology":    []string{"gpt-5.6-luna"},
		"subagent_provider": "codex",
	})
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	store, err := team.NewEventStore(workspace, "run-legacy-crash-window", "session-legacy-crash-window")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	if err := store.Append(team.RunEvent{Type: string(team.EventTaskCreated), TaskID: "legacy-crash-window", Payload: payload}); err != nil {
		_ = store.Close()
		t.Fatalf("append task event: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close event store: %v", err)
	}

	session := &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{WorkerModel: "local/current"}}
	lookups := 0
	err = preflightRestoredEventLineage(session, &config.Config{}, func(binary string) (string, error) {
		lookups++
		if binary != "codex" {
			t.Fatalf("LookPath binary = %q, want codex", binary)
		}
		return "", errors.New("codex unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "legacy-crash-window") || !strings.Contains(err.Error(), "codex executable") {
		t.Fatalf("preflightRestoredEventLineage() error = %v, want legacy Codex rejection", err)
	}
	if lookups != 1 {
		t.Fatalf("agent executable lookups = %d, want one read-only legacy-target check", lookups)
	}
	if session.Config.WorkerModel != "local/current" {
		t.Fatalf("session worker target mutated to %q", session.Config.WorkerModel)
	}
	if _, err := os.Stat(filepath.Join(workspace, "session.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy crash-window preflight created session checkpoint: %v", err)
	}
}

func TestPreflightRestoredEventLineageRejectsAmbiguousLegacyQualifiedModel(t *testing.T) {
	workspace := t.TempDir()
	payload, err := json.Marshal(map[string]any{
		"id":                "ambiguous-crash-window",
		"status":            string(team.TaskInProgress),
		"model":             "openrouter/meta/foo",
		"model_topology":    []string{"openrouter/meta/foo"},
		"subagent_provider": "hufu-local",
	})
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	store, err := team.NewEventStore(workspace, "run-ambiguous-crash-window", "session-ambiguous-crash-window")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	if err := store.Append(team.RunEvent{Type: string(team.EventTaskCreated), TaskID: "ambiguous-crash-window", Payload: payload}); err != nil {
		_ = store.Close()
		t.Fatalf("append task event: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close event store: %v", err)
	}

	session := &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{WorkerModel: "local/current"}}
	lookups := 0
	err = preflightRestoredEventLineage(session, &config.Config{}, func(string) (string, error) {
		lookups++
		return "/unexpected/backend", nil
	})
	if err == nil || !strings.Contains(err.Error(), "legacy execution target ambiguous") {
		t.Fatalf("preflightRestoredEventLineage() error = %v, want ambiguous legacy-target rejection", err)
	}
	if lookups != 0 {
		t.Fatalf("backend executable lookups = %d, want no lookup for ambiguous identity", lookups)
	}
	if _, err := os.Stat(filepath.Join(workspace, "session.json")); !os.IsNotExist(err) {
		t.Fatalf("ambiguous crash-window preflight created session checkpoint: %v", err)
	}
}
