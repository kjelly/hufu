package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func coordinatorMetadataFixture(t *testing.T, endpoint string, factory agent.ProviderBoundaryFactory) (*Coordinator, context.Context, agent.ProviderRequest) {
	t.Helper()
	c := newMediaPolicyCoordinatorWithTransport(t, endpoint, factory, providerintrospection.CapabilityNo, func(session *TeamSession) {
		configureNativeMediaCost(t, session)
		coordinator := cloneAgentDef(session.Agents["worker"])
		coordinator.Name, coordinator.Role, coordinator.Tools = "coordinator", "coordinator", ""
		coordinator.Generation.Model = "fixture/text"
		session.Agents["coordinator"] = coordinator
		session.Config.CoordinatorModel = "fixture/text"
	})
	ctx := openMediaInvocation(t, c)
	_ = nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
	ctx = context.WithValue(ctx, todoIDKey{}, CoordTodoID)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: CoordTodoID, AgentName: "coordinator", AgentRole: "coordinator", Attempt: 1, Purpose: "coordinator"})
	ctx = withProviderCostContext(ctx, providerCostContext{Agent: "coordinator", Role: "coordinator", Purpose: cost.PurposeCoordinator})
	ctx, invocation, err := c.resolveProviderBoundInvocationContext(ctx, "fixture/text", c.GetOrchestratorDef())
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = c.withCoordinatorReferenceMetadata(ctx, invocation.AdmissionContext)
	if err != nil {
		t.Fatal(err)
	}
	return c, ctx, agent.ProviderRequest{ModelID: "fixture/text", AdmissionContext: invocation.AdmissionContext, Call: &fantasy.Call{Prompt: cloneMessages(c.conversationHistory)}}
}

func TestCoordinatorReferenceMetadataCountsAndCompilesWithoutImageAccess(t *testing.T) {
	for _, shape := range []string{"call", "object", "messages"} {
		t.Run(shape, func(t *testing.T) {
			c, ctx, request := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
			if request.AdmissionContext.ImageInputCapability != providerintrospection.CapabilityNo {
				t.Fatal("fixture must use a model that cannot inspect images")
			}
			messages := request.Call.Prompt
			if shape == "object" {
				request.Call, request.ObjectCall = nil, &fantasy.ObjectCall{Prompt: messages}
			} else if shape == "messages" {
				request.Call, request.Messages = nil, messages
			}
			before, err := agent.EncodeMediaHistory(messages, "metadata-fixture")
			if err != nil {
				t.Fatal(err)
			}
			store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			// Removing the blob proves that neither admission, counting nor this
			// compiler acquires image bytes. Canonical metadata remains intact.
			if err := os.Remove(store.mediaPath("data", historyReferences(messages)[0].Reference.Ref.ID)); err != nil {
				t.Fatal(err)
			}
			admission := providerRequestAdmission{c: c}
			if err := admission.AdmitProviderRequest(ctx, request); err != nil {
				t.Fatal(err)
			}
			wire, release, err := admission.CompileProviderRequest(ctx, request)
			if err != nil || release != nil || agent.ProviderRequestHasMedia(wire) {
				t.Fatal("metadata compiler acquired image buffers or retained media", err)
			}
			count, err := defaultCounter.CountProviderRequest(ctx, request.ModelID, request)
			if err != nil {
				t.Fatal(err)
			}
			textCount, err := defaultCounter.CountProviderRequest(ctx, request.ModelID, wire)
			if err != nil || count != textCount || count <= 0 || count >= 8192 {
				t.Fatalf("metadata used image tokens or discarded text: metadata=%d text=%d err=%v", count, textCount, err)
			}
			request.InvocationID = "metadata-fixture-committed"
			if err := admission.CommitProviderInvocation(ctx, request); err != nil {
				t.Fatal(err)
			}
			if err := admission.AuthorizeProviderTransport(ctx, request); err != nil {
				t.Fatal(err)
			}
			after, err := agent.EncodeMediaHistory(messages, "metadata-fixture")
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("metadata compiler mutated canonical history")
			}
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == string(EventMediaProviderAdmitted) {
					t.Fatal("metadata-only consumption minted image egress permission")
				}
			}
		})
	}
}

func TestCoordinatorReferenceMetadataRejectsChangedAuthority(t *testing.T) {
	for _, scenario := range []string{"no scope", "worker", "auxiliary", "expired invocation", "cancelled", "model", "capacity", "provider", "source", "order", "role", "raw bytes", "uncommitted", "different invocation", "revoked during journal read"} {
		t.Run(scenario, func(t *testing.T) {
			c, ctx, request := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
			admission := providerRequestAdmission{c: c}
			request.InvocationID = "metadata-transport"
			if scenario != "uncommitted" {
				if err := admission.CommitProviderInvocation(ctx, request); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "no scope":
				ctx = context.WithValue(ctx, coordinatorReferenceMetadataKey{}, (*coordinatorReferenceMetadata)(nil))
			case "worker":
				ctx = context.WithValue(ctx, todoIDKey{}, "worker-task")
			case "auxiliary":
				ctx = withProviderCostContext(ctx, providerCostContext{Purpose: cost.PurposeJudge})
			case "expired invocation":
				c.activeInvocationLease().release()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "model":
				request.ModelID, request.AdmissionContext.ModelID = "fixture/vision", "fixture/vision"
			case "capacity":
				request.AdmissionContext.ContextWindow++
			case "provider":
				request.AdmissionContext.ProviderConfigurationHash = "changed"
			case "source":
				part := request.Call.Prompt[0].Content[1].(agent.MediaReferencePart)
				part.Reference.Source.SnapshotID = "unseen-source"
				request.Call.Prompt[0].Content[1] = part
			case "order":
				parts := request.Call.Prompt[0].Content
				parts[1], parts[2] = parts[2], parts[1]
			case "role":
				request.Call.Prompt[0].Role = fantasy.MessageRoleAssistant
			case "raw bytes":
				request.Call.Prompt[0].Content[1] = fantasy.FilePart{MediaType: "image/png", Data: mediaStorePNG(t)}
			case "different invocation":
				request.InvocationID = "uncommitted-other"
			case "revoked during journal read":
				c.SetEventJournal(&mediaTransportReadHook{AtomicEventJournal: c.EventJournal().(AtomicEventJournal), afterRead: func() { c.activeInvocationLease().release() }})
			}
			if err := admission.AuthorizeProviderTransport(ctx, request); err == nil {
				t.Fatal("changed metadata authority reached the transport boundary")
			}
		})
	}
}

func TestCoordinatorReferenceMetadataDoesNotDowngradeNativeWorker(t *testing.T) {
	c, ctx, _, request := nativeMediaProviderFixture(t)
	ctx, err := c.withCoordinatorReferenceMetadata(ctx, request.AdmissionContext)
	if err != nil {
		t.Fatal(err)
	}
	admission := providerRequestAdmission{c: c}
	metadata, err := admission.AdmitProviderReferenceMetadata(ctx, request)
	if err != nil || metadata {
		t.Fatal("worker inherited its parent's metadata consumer authority", err)
	}
	if err := admission.AdmitProviderRequest(ctx, request); err != nil {
		t.Fatal("native worker lost its independent image admission", err)
	}
	wire, release, err := admission.CompileProviderRequest(ctx, request)
	if release != nil {
		t.Cleanup(release)
	}
	if err != nil || release == nil || !agent.ProviderRequestHasMedia(wire) {
		t.Fatal("native worker was silently downgraded to metadata", err)
	}
}

type revocableMetadataContext struct {
	context.Context
	revoked atomic.Bool
}

func (c *revocableMetadataContext) Value(key any) any {
	if _, ok := key.(providerCostContextKey); ok && c.revoked.Load() {
		return providerCostContext{Purpose: cost.PurposeJudge}
	}
	return c.Context.Value(key)
}

func TestCoordinatorReferenceMetadataTransportRechecksConsumerAfterJournalRead(t *testing.T) {
	c, ctx, request := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
	request.InvocationID = "metadata-consumer-recheck"
	admission := providerRequestAdmission{c: c}
	if err := admission.CommitProviderInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	revocable := &revocableMetadataContext{Context: ctx}
	c.SetEventJournal(&mediaTransportReadHook{AtomicEventJournal: c.EventJournal().(AtomicEventJournal), afterRead: func() { revocable.revoked.Store(true) }})
	requireMediaCode(t, admission.AuthorizeProviderTransport(revocable, request), media.AccessDenied)
}

func TestCoordinatorReferenceMetadataRejectsMissingRunIdentity(t *testing.T) {
	c, ctx, request := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
	original := c.executionRunID
	t.Cleanup(func() {
		c.executionEventsMu.Lock()
		c.executionRunID = original
		c.executionEventsMu.Unlock()
	})
	c.executionEventsMu.Lock()
	c.executionRunID = ""
	c.executionEventsMu.Unlock()
	_, err := (providerRequestAdmission{c: c}).AdmitProviderReferenceMetadata(ctx, request)
	requireMediaCode(t, err, media.AccessDenied)
}

func TestCoordinatorReferenceMetadataReachesTextModelSDKWithoutPixels(t *testing.T) {
	requests := make(chan []mediaWorkerWireMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                   `json:"model"`
			Messages []mediaWorkerWireMessage `json:"messages"`
		}
		if json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil || request.Model != "text" {
			http.Error(w, "invalid metadata request", http.StatusBadRequest)
			return
		}
		select {
		case requests <- request.Messages:
		default:
			http.Error(w, "unexpected extra metadata invocation", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"metadata","object":"chat.completion","created":1,"model":"text","choices":[{"index":0,"message":{"role":"assistant","content":"Metadata received; image inspection requires a worker."},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(server.Close)
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	c, ctx, request := coordinatorMetadataFixture(t, server.URL+"/v1", factory)
	before := cloneMessages(c.conversationHistory)
	backend, target, err := c.gatedAgentBackendForModel(request.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := backend.AgentProvider(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := c.createGatedAgent(ctx, provider, agent.AgentConfig{AdmissionContext: request.AdmissionContext, Def: c.GetOrchestratorDef(), TeamConfig: &c.session.Config, WorkDir: c.projectDir, MaxSteps: 1, InvocationModelID: request.ModelID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Generate(ctx, fantasy.AgentCall{Messages: request.Call.Prompt}); err != nil {
		t.Fatal(err)
	}
	select {
	case messages := <-requests:
		wire, err := json.Marshal(messages)
		if err != nil || bytes.Contains(wire, []byte("image_url")) || bytes.Contains(wire, []byte("base64")) || strings.Count(string(wire), "Image input metadata") != 2 {
			t.Fatal("text model received pixels or lost repeated metadata occurrences")
		}
		for _, location := range historyReferences(before) {
			if bytes.Contains(wire, []byte(location.Reference.Ref.ID)) || bytes.Contains(wire, []byte(location.Reference.Ref.SHA256)) {
				t.Fatal("text metadata exposed a CAS identifier or digest")
			}
		}
	default:
		t.Fatal("metadata never reached the real SDK HTTP boundary")
	}
	if !reflect.DeepEqual(before, c.conversationHistory) || len(c.TaskTracker().TodoList().Items()) != 0 {
		t.Fatal("metadata consumption mutated history or manufactured a worker completion")
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == string(EventMediaProviderAdmitted) {
			t.Fatal("text model invocation acquired image egress permission")
		}
	}
	assertNativeMediaCost(t, events, 1)
}
