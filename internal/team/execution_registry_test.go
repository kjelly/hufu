package team

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
)

type fakeExecutionBackend struct {
	name string
	kind execution.BackendKind
	caps execution.BackendCapabilities
}

func (b fakeExecutionBackend) Name() string                                { return b.name }
func (b fakeExecutionBackend) Kind() execution.BackendKind                 { return b.kind }
func (b fakeExecutionBackend) Capabilities() execution.BackendCapabilities { return b.caps }
func (b fakeExecutionBackend) ValidateTarget(_ context.Context, target execution.ExecutionTarget) error {
	if !execution.BackendNamesEqual(target.Backend, b.name) {
		return fmt.Errorf("wrong backend")
	}
	return target.Validate()
}
func (fakeExecutionBackend) RunAttempt(context.Context, AttemptRequest) (AttemptResult, error) {
	return AttemptResult{}, nil
}

type fakeLanguageModelBackend struct{ fakeExecutionBackend }

func (fakeLanguageModelBackend) LanguageModel(context.Context, execution.ExecutionTarget) (fantasy.LanguageModel, error) {
	return nil, nil
}

type capturingAttemptRunner struct {
	request AttemptRequest
}

func (r *capturingAttemptRunner) RunAttempt(_ context.Context, request AttemptRequest) (AttemptResult, error) {
	r.request = request
	return AttemptResult{}, nil
}

func TestLLMExecutionBackendUsesCanonicalTargetForTransportModel(t *testing.T) {
	manager, err := agent.NewProviderManager("http://localhost:11434/v1", "", map[string]config.ProviderConfig{})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}
	runner := &capturingAttemptRunner{}
	backend, err := NewLLMExecutionBackend("openrouter", manager, runner)
	if err != nil {
		t.Fatalf("NewLLMExecutionBackend: %v", err)
	}
	target := execution.ExecutionTarget{Backend: "openrouter", Model: "meta/foo"}
	if _, err := backend.RunAttempt(t.Context(), AttemptRequest{ModelID: "wrong-model", ExecutionTarget: target}); err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if runner.request.ModelID != "openrouter/meta/foo" {
		t.Fatalf("transport model = %q, want canonical provider/model", runner.request.ModelID)
	}
	if runner.request.ExecutionTarget != target {
		t.Fatalf("transport target = %#v, want %#v", runner.request.ExecutionTarget, target)
	}
}

func TestOllamaExecutionBackendRoutesCloudModelToOllamaLeaf(t *testing.T) {
	manager, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}
	runner := &capturingAttemptRunner{}
	backend, err := NewLLMExecutionBackend(execution.OllamaBackendName, manager, runner)
	if err != nil {
		t.Fatalf("NewLLMExecutionBackend: %v", err)
	}
	target := execution.ExecutionTarget{Backend: execution.OllamaBackendName, Model: "glm-5.3-flash:cloud"}
	if _, err := backend.RunAttempt(t.Context(), AttemptRequest{ModelID: "wrong", ExecutionTarget: target}); err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if backend.Name() != execution.OllamaBackendName {
		t.Fatalf("backend name = %q, want %q", backend.Name(), execution.OllamaBackendName)
	}
	if runner.request.ExecutionTarget != target {
		t.Fatalf("execution target = %#v, want %#v", runner.request.ExecutionTarget, target)
	}
	if runner.request.ModelID != "glm-5.3-flash:cloud" {
		t.Fatalf("transport model = %q, want Ollama leaf model", runner.request.ModelID)
	}
}

func TestLLMExecutionBackendRejectsLegacyModelOnlyAttempt(t *testing.T) {
	manager, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}
	runner := &capturingAttemptRunner{}
	backend, err := NewLLMExecutionBackend("local", manager, runner)
	if err != nil {
		t.Fatalf("NewLLMExecutionBackend: %v", err)
	}
	if _, err := backend.RunAttempt(t.Context(), AttemptRequest{ModelID: "qwen3"}); err == nil {
		t.Fatal("RunAttempt accepted a model-only request without an admitted target")
	}
	if runner.request.ModelID != "" {
		t.Fatalf("runner was called with model %q after target validation failed", runner.request.ModelID)
	}
}

func TestExecutionRegistryResolvesCanonicalTargetsFailClosed(t *testing.T) {
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeLanguageModelBackend{fakeExecutionBackend{name: "local", kind: execution.BackendKindLLM, caps: execution.BackendCapabilities{DirectLanguageModel: true}}}); err != nil {
		t.Fatalf("register local: %v", err)
	}
	if err := registry.Register(fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent}); err != nil {
		t.Fatalf("register codex: %v", err)
	}

	selector, err := execution.ParseExecutionSelector("ollama/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	target, backend, err := registry.ResolveTarget(selector, execution.TargetDefaults{DefaultLLMBackend: "local"})
	if err != nil {
		t.Fatalf("resolve alias: %v", err)
	}
	if target.String() != "ollama/qwen3:8b" || backend.Name() != "local" {
		t.Fatalf("resolved target=%q backend=%q", target, backend.Name())
	}

	unknown, err := execution.ParseExecutionSelector("unknown/model")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ResolveTarget(unknown, execution.TargetDefaults{DefaultLLMBackend: "local"}); err == nil {
		t.Fatal("unknown qualified backend resolved")
	}
}

func TestExecutionRegistryBareSelectorRequiresLLMDefault(t *testing.T) {
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent}); err != nil {
		t.Fatal(err)
	}
	selector, err := execution.ParseExecutionSelector("model")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ResolveTarget(selector, execution.TargetDefaults{DefaultLLMBackend: "codex"}); err == nil {
		t.Fatal("agent backend accepted as bare-selector default")
	}
}

func TestExecutionRegistryLanguageModelCapability(t *testing.T) {
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.LanguageModelBackend(execution.ExecutionTarget{Backend: "codex", Model: "gpt-5"}); err == nil {
		t.Fatal("agent backend exposed language model capability")
	}
}

func TestGatedAgentBackendUsesNamedLLMAndRejectsAgentTarget(t *testing.T) {
	manager, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}
	backend, err := NewLLMExecutionBackend("named", manager, &capturingAttemptRunner{})
	if err != nil {
		t.Fatalf("NewLLMExecutionBackend: %v", err)
	}
	registry := NewExecutionRegistry()
	if err := registry.Register(backend); err != nil {
		t.Fatalf("register named LLM: %v", err)
	}
	if err := registry.Register(fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent}); err != nil {
		t.Fatalf("register codex: %v", err)
	}
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{DefaultLLMBackend: "named"}}}
	c.SetExecutionRegistry(registry)
	gated, target, err := c.gatedAgentBackendForModel("named/qwen3:8b")
	if err != nil {
		t.Fatalf("gatedAgentBackendForModel(named/qwen3:8b): %v", err)
	}
	if target != (execution.ExecutionTarget{Backend: "named", Model: "qwen3:8b"}) || gated.Name() != "named" {
		t.Fatalf("named LLM resolution = backend %q target %#v", gated.Name(), target)
	}
	if _, _, err := c.gatedAgentBackendForModel("codex/gpt-5"); err == nil {
		t.Fatal("agent execution target was accepted for an LLM-only gated role")
	}
}

func TestStateChangingExtraModelFanoutRejectsMixedLocalCodex(t *testing.T) {
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeLanguageModelBackend{fakeExecutionBackend{name: "local", kind: execution.BackendKindLLM, caps: execution.BackendCapabilities{DirectLanguageModel: true}}}); err != nil {
		t.Fatalf("register local: %v", err)
	}
	if err := registry.Register(fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent}); err != nil {
		t.Fatalf("register codex: %v", err)
	}
	c := &Coordinator{}
	c.SetExecutionRegistry(registry)
	err := c.validateExtraModelExecutionTopology(TaskDef{
		SideEffect:        SideEffectWorkspaceWrite,
		ModelTopology:     []string{"local/qwen3", "codex/gpt-5"},
		ExecutionTopology: []execution.ExecutionTarget{{Backend: "local", Model: "qwen3"}, {Backend: "codex", Model: "gpt-5"}},
	})
	if err == nil {
		t.Fatal("mixed local+Codex state-changing fanout was admitted")
	}
	if !strings.Contains(err.Error(), "external agent backend") {
		t.Fatalf("mixed fanout error = %q, want external-backend rejection", err)
	}
}

func TestExecutionRegistryRejectsDuplicateCanonicalAlias(t *testing.T) {
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeExecutionBackend{name: "local", kind: execution.BackendKindLLM}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(fakeExecutionBackend{name: "ollama", kind: execution.BackendKindLLM}); err == nil {
		t.Fatal("canonical alias duplicate registered")
	}
}

func TestAdmittedExecutionTargetForExtraModelUsesFrozenMixedBackendLeaf(t *testing.T) {
	task := TaskDef{
		ModelTopology: []string{"local/primary", "codex/extra"},
		ExecutionTopology: []execution.ExecutionTarget{
			{Backend: "local", Model: "primary"},
			{Backend: "codex", Model: "extra"},
		},
	}
	target, err := admittedExecutionTargetForModel(task, "codex/extra")
	if err != nil {
		t.Fatalf("admittedExecutionTargetForModel: %v", err)
	}
	want := execution.ExecutionTarget{Backend: "codex", Model: "extra"}
	if target != want {
		t.Fatalf("target = %#v, want %#v", target, want)
	}
}
