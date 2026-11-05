package team

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelprofile"
)

func protocolPolicyCoordinator(t *testing.T, protocol string, selected bool) *Coordinator {
	t.Helper()
	c := newTestCoordinator(t)
	pm, err := agent.NewProviderManager("http://127.0.0.1:1/v1", "", map[string]config.ProviderConfig{"local": {APIProtocol: protocol}})
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = pm
	if selected {
		session := dependencyViewSession()
		c.session.ContractTasks = session.ContractTasks
		c.session.Config.Delegation = session.Config.Delegation
		c.session.Agents = session.Agents
	}
	for _, def := range c.session.Agents {
		def.Generation.Model = "test"
	}
	return c
}

func TestInvocationProtocolPersistedBeforeGeneration(t *testing.T) {
	for _, raw := range []string{"", "chat-completions"} {
		t.Run(raw, func(t *testing.T) {
			c := protocolPolicyCoordinator(t, raw, false)
			store, err := NewEventStore(c.session.Workspace, "run-protocol", "session-protocol")
			if err != nil {
				t.Fatal(err)
			}
			c.eventStore = store
			state, err := newExecutionPolicyState(c)
			if err != nil {
				t.Fatal(err)
			}
			c.executionPolicy = state
			runtime := NewModelProfileRuntimeWithCatalog(c.providerManager, true, nil)
			resolved := runtime.ResolveAdmission(t.Context(), "dynamic-model", 8192, 512, 0)
			request := agent.ProviderRequest{ModelID: "dynamic-model", InvocationID: "invocation-protocol", AdmissionContext: resolved.Admission}
			if err := (providerRequestAdmission{c: c}).CommitProviderInvocation(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			events, err := c.EventStore().ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				if event.Type != string(EventModelProfileResolved) {
					continue
				}
				var projection modelprofile.TelemetryProjection
				if err := json.Unmarshal(event.Payload, &projection); err != nil {
					t.Fatal(err)
				}
				if projection.InvocationID != request.InvocationID || projection.APIProtocol != request.AdmissionContext.APIProtocol {
					t.Fatal("invocation protocol lost in durable event")
				}
				if raw == "" {
					if projection.SchemaVersion != 1 || strings.Contains(string(event.Payload), "api_protocol") {
						t.Fatal("legacy event shape changed")
					}
				} else if projection.SchemaVersion != 2 {
					t.Fatal("explicit event is not versioned")
				}
				found = true
			}
			if !found {
				t.Fatal("invocation binding was not committed")
			}
			request.InvocationID = "changed-protocol"
			request.AdmissionContext.APIProtocol.Version++
			if err := (providerRequestAdmission{c: c}).CommitProviderInvocation(t.Context(), request); err == nil {
				t.Fatal("changed protocol reached durable commit")
			}
			after, err := c.EventStore().ReadEvents()
			if err != nil || len(after) != len(events) {
				t.Fatal("rejected binding wrote an event")
			}
		})
	}
}

func TestExecutionPolicyProtocolVersionsAndResume(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "selected"}[selected], func(t *testing.T) {
			legacy := protocolPolicyCoordinator(t, "", selected)
			explicit := protocolPolicyCoordinator(t, " CHAT-COMPLETIONS ", selected)
			old, err := newExecutionPolicyState(legacy)
			if err != nil {
				t.Fatal(err)
			}
			current, err := newExecutionPolicyState(explicit)
			if err != nil {
				t.Fatal(err)
			}
			want := explicitProtocolFullPolicyVersion
			if selected {
				want = explicitProtocolSelectedPolicyVersion
			}
			if current.snapshot.Version != want || current.snapshot.ConfigurationHash == old.snapshot.ConfigurationHash {
				t.Fatal("explicit protocol was not versioned")
			}
			if err := ValidateExecutionPolicySnapshot(current.snapshot); err != nil {
				t.Fatal(err)
			}
			if same, err := explicit.executionPolicySnapshotMatchesCurrent(current.snapshot); err != nil || !same {
				t.Fatalf("unchanged explicit binding: %v", err)
			}
			if same, err := legacy.executionPolicySnapshotMatchesCurrent(current.snapshot); err == nil && same {
				t.Fatal("explicit to legacy drift accepted")
			}
			if same, err := explicit.executionPolicySnapshotMatchesCurrent(old.snapshot); err == nil && same {
				t.Fatal("legacy to explicit drift accepted")
			}
			encoded, err := json.Marshal(old.snapshot)
			if err != nil || strings.Contains(string(encoded), "api_protocol") {
				t.Fatalf("legacy shape changed: %v", err)
			}
			for _, version := range []int{3, 4, 5, 6} {
				if selected != isSelectedDependencyPolicyVersion(version) {
					continue
				}
				state, err := newExecutionPolicyStateForVersion(legacy, version)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateExecutionPolicySnapshot(state.snapshot); err != nil {
					t.Fatal(err)
				}
				if _, err := newExecutionPolicyStateForVersion(explicit, version); err == nil {
					t.Fatalf("v%d admitted explicit configuration", version)
				}
			}
			explicit.executionPolicy = current
			binding, _ := explicit.providerManager.ResolveAPIProtocol("test")
			for _, model := range []string{"test", "dynamic-new-model", "unknown/fallback-model"} {
				request := agent.ProviderRequest{ModelID: model, AdmissionContext: agent.ProviderAdmissionContext{APIProtocol: binding}}
				if err := explicit.validateInvocationProtocol(request); err != nil {
					t.Fatal(err)
				}
				request.AdmissionContext.APIProtocol.Version++
				if err := explicit.validateInvocationProtocol(request); err == nil {
					t.Fatal("changed invocation binding accepted")
				}
			}
		})
	}
}

func TestExecutionPolicyRejectsProtocolBindingTampering(t *testing.T) {
	c := protocolPolicyCoordinator(t, "chat-completions", false)
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ExecutionPolicySnapshot){
		func(s *ExecutionPolicySnapshot) { s.Version = 5 },
		func(s *ExecutionPolicySnapshot) { s.Backends[0].APIProtocol.Version++ },
		func(s *ExecutionPolicySnapshot) { s.ModelRoutes[0].APIProtocol = agent.APIProtocolResolution{} },
	} {
		snapshot := cloneExecutionPolicySnapshot(state.snapshot)
		mutate(snapshot)
		snapshot.ConfigurationHash, err = executionPolicyConfigurationHash(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateExecutionPolicySnapshot(snapshot); err == nil {
			t.Fatal("tampered binding accepted")
		}
	}
}
