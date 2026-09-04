package team

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

func TestProviderAdmissionBoundZeroContextDoesNotUseGlobalRegistry(t *testing.T) {
	modelID := "bound-zero-admission-model"
	preserveRegisteredModelSpec(t, GlobalModelSpecRegistry(), modelID)
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	})

	request := agent.ProviderRequest{
		ModelID: modelID,
		AdmissionContext: agent.ProviderAdmissionContext{
			ModelID:          modelID,
			ProviderIdentity: "remote",
			ProviderBaseURL:  "https://provider.example/v1",
			Bound:            true,
		},
	}
	err := (providerRequestAdmission{}).AdmitProviderRequest(t.Context(), request)
	var metadataErr *ContextWindowMetadataUnavailableError
	if !errors.As(err, &metadataErr) {
		t.Fatalf("admission error = %v, want bound metadata-unavailable error", err)
	}
}

type boundZeroCountingCounter struct {
	counts int
}

func (c *boundZeroCountingCounter) CountText(context.Context, string, string) (int, error) {
	c.counts++
	return 0, nil
}

func (c *boundZeroCountingCounter) CountMessages(context.Context, string, []fantasy.Message) (int, error) {
	c.counts++
	return 0, nil
}

func (c *boundZeroCountingCounter) CountTools(context.Context, string, []fantasy.AgentTool) (int, error) {
	c.counts++
	return 0, nil
}

func TestContextWindowManagerBoundZeroFailsBeforeRegistryAndCounting(t *testing.T) {
	modelID := "bound-zero-context-window-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	})
	counter := &boundZeroCountingCounter{}
	manager := NewContextWindowManager(counter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		t.Fatal("bound-zero admission unexpectedly compacted")
		return nil, nil
	})
	admission, err := manager.Admit(t.Context(), ContextWindowRequest{
		ModelID:  modelID,
		Messages: []fantasy.Message{fantasy.NewUserMessage("request")},
		AdmissionContext: agent.ProviderAdmissionContext{
			ModelID:          modelID,
			ProviderIdentity: "remote",
			ProviderBaseURL:  "https://provider.example/v1",
			Bound:            true,
		},
	})
	if metadataErr, ok := errors.AsType[*ContextWindowMetadataUnavailableError](err); !ok || metadataErr == nil {
		t.Fatalf("manager error = %v, want metadata-unavailable", err)
	}
	if admission.Decision != ContextWindowCannotFit || admission.RejectionReason != contextWindowReasonMetadataUnavailable {
		t.Fatalf("admission = %#v, want rejected metadata-unavailable", admission)
	}
	if counter.counts != 0 {
		t.Fatalf("token counter calls = %d, want zero", counter.counts)
	}
}

func TestPrepareStepBoundZeroDoesNotFallBackToGlobalCapacity(t *testing.T) {
	modelID := "bound-zero-prepare-step-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	})
	coordinator := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "bound-zero"}}}
	prepare := coordinator.prepareAgentModelRequest(agent.AgentConfig{
		Def:        &agent.AgentDef{Name: "worker", Generation: agent.GenerationParams{Model: modelID}},
		TeamConfig: &agent.TeamConfig{Name: "bound-zero"},
		AdmissionContext: agent.ProviderAdmissionContext{
			ModelID:          modelID,
			ProviderIdentity: "remote",
			ProviderBaseURL:  "https://provider.example/v1",
			Bound:            true,
		},
	}, nil)
	_, _, err := prepare(t.Context(), fantasy.PrepareStepFunctionOptions{
		Messages: []fantasy.Message{fantasy.NewUserMessage("request")},
	})
	if metadataErr, ok := errors.AsType[*ContextWindowMetadataUnavailableError](err); !ok || metadataErr == nil {
		t.Fatalf("PrepareStep error = %v, want metadata-unavailable", err)
	}
}

func TestContextWindowManagerUnboundZeroRetainsRegistryCompatibility(t *testing.T) {
	modelID := "unbound-zero-context-window-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	})
	manager := NewContextWindowManager(&boundZeroCountingCounter{}, nil)
	if _, err := manager.Admit(t.Context(), ContextWindowRequest{
		ModelID: modelID, Messages: []fantasy.Message{fantasy.NewUserMessage("request")},
	}); err != nil {
		t.Fatalf("unbound registry-compatible admission failed: %v", err)
	}
}
