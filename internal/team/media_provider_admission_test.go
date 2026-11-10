package team

import (
	"context"
	"os"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

func nativeMediaProviderFixture(t *testing.T, configure ...func(*TeamSession)) (*Coordinator, context.Context, *TodoItem, agent.ProviderRequest) {
	t.Helper()
	c := newMediaPolicyCoordinatorWithTransport(t, "http://fixture.invalid/v1", nil, providerintrospection.CapabilityUnknown, func(session *TeamSession) {
		for _, apply := range configure {
			apply(session)
		}
	})
	ctx := openMediaInvocation(t, c)
	task := nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
	if def := c.session.Agents["worker"]; len(def.ExtraModels) > 0 {
		task.ModelTopology = initialTaskModelTopology(def, def.Generation.Model)
	}
	item := createMediaOccurrence(t, c, ctx, task)
	if err := c.CommitTaskTransition(ctx, item.ID, TaskPending, TaskInProgress, "running", "", nil); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	identity, found := c.activeTaskResultOccurrence(item.ID)
	if !found {
		t.Fatal("fixture did not open its canonical attempt lease")
	}
	item = c.todoItemByID(item.ID)
	ctx = withSubmitResultRuntimeIdentity(ctx, identity)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: item.ID, AgentName: item.Agent, AgentRole: "worker", Attempt: 1, Purpose: "task_execution"})
	message := fantasy.NewUserMessage(item.Goal)
	for index, selection := range item.MediaBinding.Selections {
		message.Content = append(message.Content, agent.MediaReferencePart{Reference: media.HistoryReference{Version: media.Version,
			Source: selection, Ref: item.MediaBinding.Refs[index], TaskID: item.ID, BindingHash: item.MediaBinding.Hash, PolicyHash: item.MediaBinding.PolicyHash}})
	}
	return c, ctx, item, nativeMediaProviderRequest(t, c, []fantasy.Message{message})
}

func nativeMediaProviderRequest(t *testing.T, c *Coordinator, messages []fantasy.Message, windows ...int) agent.ProviderRequest {
	t.Helper()
	window := 32768
	if len(windows) > 0 {
		window = windows[0]
	}
	const modelID = "fixture/vision"
	ref, err := c.providerManager.ResolveProviderRef(modelID)
	if err != nil {
		t.Fatal(err)
	}
	profile := modelprofile.ResolveModelProfile(modelprofile.ModelProfileInput{ModelID: modelID, Provider: ref.Provider,
		Context:      modelprofile.ContextResolutionInput{OperatorContext: window},
		Capabilities: modelprofile.CapabilityResolutionInput{ImageInput: modelprofile.CapabilityEvidence{ProviderMetadata: modelprofile.CapabilityYes}}})
	bound := admissionContextFromProfile(modelID, ref, profile, 64)
	transport, err := c.providerManager.ResolveProviderExecutionIdentity(modelID)
	if err != nil {
		t.Fatal(err)
	}
	bound.ProviderConfigurationHash = transport.ConfigurationHash
	return agent.ProviderRequest{ModelID: modelID, Call: &fantasy.Call{Prompt: messages}, AdmissionContext: bound}
}

func TestNativeProviderMediaValidatesCanonicalMetadataWithoutReadingImages(t *testing.T) {
	for _, shape := range []string{"call", "object", "messages"} {
		t.Run(shape, func(t *testing.T) {
			c, ctx, item, request := nativeMediaProviderFixture(t)
			messages := request.Call.Prompt
			if shape == "object" {
				request.Call, request.ObjectCall = nil, &fantasy.ObjectCall{Prompt: messages}
			} else if shape == "messages" {
				request.Call, request.Messages = nil, messages
			}
			store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			// Metadata admission must not decode or hydrate private image bytes.
			// The compiler's subsequent verified read must reject this corruption.
			if err := os.WriteFile(store.mediaPath("data", item.MediaBinding.Refs[0].ID), []byte("corrupt fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			admission, err := c.resolveNativeProviderMedia(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if admission.Identity.TaskID != item.ID || admission.Binding.Hash != item.MediaBinding.Hash || admission.Target != item.ExecutionTarget || len(admission.Locations) != 2 || admission.ImageTokens != 2*8192 {
				t.Fatal("provider metadata lost its owner, ordered occurrences, target or image budget")
			}
			admission.Binding.Selections[0].PartIndex++
			if admission.Binding.Selections[0] == c.todoItemByID(item.ID).MediaBinding.Selections[0] {
				t.Fatal("metadata proof shared mutable binding storage with the canonical task")
			}
		})
	}
}

func TestNativeProviderMediaRejectsUnboundOrChangedRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(context.Context, *agent.ProviderRequest) context.Context
		code   media.Code
	}{
		{"missing lease", func(ctx context.Context, _ *agent.ProviderRequest) context.Context {
			return context.WithValue(ctx, submitResultRuntimeIdentityKey{}, submitResultRuntimeIdentity{})
		}, media.AccessDenied},
		{"stale lease", func(ctx context.Context, _ *agent.ProviderRequest) context.Context {
			identity := ctx.Value(submitResultRuntimeIdentityKey{}).(submitResultRuntimeIdentity)
			identity.OccurrenceRevision++
			return withSubmitResultRuntimeIdentity(ctx, identity)
		}, media.AccessDenied},
		{"auxiliary role", func(ctx context.Context, _ *agent.ProviderRequest) context.Context {
			return withProviderCostContext(ctx, providerCostContext{Purpose: cost.PurposeJudge})
		}, media.AccessDenied},
		{"result repair", func(ctx context.Context, _ *agent.ProviderRequest) context.Context {
			return context.WithValue(ctx, protocolRepairExecutionKey{}, true)
		}, media.AccessDenied},
		{"wrong target", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			request.ModelID, request.AdmissionContext.ModelID = "fixture/other", "fixture/other"
			return ctx
		}, media.EgressDenied},
		{"changed configuration", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			request.AdmissionContext.ProviderConfigurationHash = "changed"
			return ctx
		}, media.EgressDenied},
		{"model unknown", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			request.AdmissionContext.ImageInputCapability = providerintrospection.CapabilityUnknown
			return ctx
		}, media.Unsupported},
		{"model denied", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			request.AdmissionContext.ImageInputCapability = providerintrospection.CapabilityNo
			return ctx
		}, media.Unsupported},
		{"missing capability source", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			request.AdmissionContext.ImageInputSource = ""
			return ctx
		}, media.Unsupported},
		{"missing image", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			request.Call.Prompt[0].Content = request.Call.Prompt[0].Content[:2]
			return ctx
		}, media.BindingConflict},
		{"reordered occurrences", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			parts := request.Call.Prompt[0].Content
			parts[1], parts[2] = parts[2], parts[1]
			return ctx
		}, media.AccessDenied},
		{"another owner", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			part := request.Call.Prompt[0].Content[1].(agent.MediaReferencePart)
			part.Reference.TaskID = "another-task"
			request.Call.Prompt[0].Content[1] = part
			return ctx
		}, media.AccessDenied},
		{"wrong role", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			request.Call.Prompt[0].Role = fantasy.MessageRoleAssistant
			return ctx
		}, media.BindingConflict},
		{"ambiguous message sources", func(ctx context.Context, request *agent.ProviderRequest) context.Context {
			request.Messages = request.Call.Prompt
			return ctx
		}, media.BindingConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, ctx, _, request := nativeMediaProviderFixture(t)
			ctx = test.mutate(ctx, &request)
			_, err := c.resolveNativeProviderMedia(ctx, request)
			requireMediaCode(t, err, test.code)
		})
	}
}

type imageCapabilityBackend struct {
	fakeLanguageModelBackend
	provider *agent.OpenAICompatibleProvider
}

func (b imageCapabilityBackend) AgentProvider(context.Context, execution.ExecutionTarget) (*agent.OpenAICompatibleProvider, error) {
	return b.provider, nil
}

func TestNativeProviderMediaRequiresBackendAndAdapterCapabilities(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "backend missing capability"
		if native {
			name = "adapter unavailable"
		}
		t.Run(name, func(t *testing.T) {
			c, ctx, _, request := nativeMediaProviderFixture(t)
			backend := imageCapabilityBackend{fakeLanguageModelBackend: fakeLanguageModelBackend{fakeExecutionBackend{
				name: "fixture", kind: execution.BackendKindLLM, caps: execution.BackendCapabilities{DirectLanguageModel: true, NativeImageInput: native},
			}}}
			registry := c.ExecutionRegistry()
			registry.mu.Lock()
			registry.backends["fixture"] = backend
			registry.mu.Unlock()
			_, err := c.resolveNativeProviderMedia(ctx, request)
			requireMediaCode(t, err, media.Unsupported)
		})
	}
}
