package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func TestNativeImageBackendCapabilityIsExplicit(t *testing.T) {
	if (execution.BackendCapabilities{}).NativeImageInput || new(AgentExecutionBackend).Capabilities().NativeImageInput {
		t.Fatal("unknown or external backend claimed native image transport")
	}
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := NewLLMExecutionBackend("fixture", manager, new(capturingAttemptRunner), admitExecutionPolicyForTest)
	if err != nil {
		t.Fatal(err)
	}
	if !backend.Capabilities().NativeImageInput {
		t.Fatal("local LLM backend did not declare its adapter transport capability")
	}
}
