package team

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	sidecarbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/utils"
)

// countingGenerator is an LLM generator that always picks candidate A1.
type countingGenerator struct{ calls *atomic.Int32 }

func (g countingGenerator) Execute(context.Context, string) (string, error) {
	g.calls.Add(1)
	return `{"token":"A1"}`, nil
}

func (g countingGenerator) ModelID() string { return "test-sidecar" }

func sidecarPrimitiveCoordinator(t *testing.T, entry catalog.Entry, calls *atomic.Int32) *Coordinator {
	t.Helper()
	service, err := catalog.New(map[string]catalog.Entry{"classify": entry}, catalog.WithSidecarGenerator(func(string) (sidecarbackend.Generator, error) {
		return countingGenerator{calls: calls}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(t.TempDir(), "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.AppendPersisted(RunEvent{Type: string(EventTaskCreated), TaskID: "1", BranchID: "main", Actor: "helper", Payload: json.RawMessage(`{"id":"1","status":"pending"}`)}); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "test", DecisionPrimitives: map[string]catalog.Entry{"classify": entry}}, Agents: map[string]*agent.AgentDef{"helper": {Name: "helper", Role: "worker", Tools: decisionPrimitiveToolName}}}, decisionPrimitives: service, decisionPrimitiveGate: make(chan struct{}, 1), eventStore: store, eventJournal: eventStoreJournal{store: store}, sessionTime: time.Now()}
	c.coreTools = []fantasy.AgentTool{&decisionPrimitiveTool{coordinator: c}}
	return c
}

// TestDecisionPrimitiveSidecarBackend runs the decision_primitive tool on a
// language-model backend: the decision is published without confidence,
// replays from the journal, and still drives block-on.
func TestDecisionPrimitiveSidecarBackend(t *testing.T) {
	entry := primitiveTestEntry("")
	entry.Backend, entry.Endpoint, entry.Model = "sidecar", "", ""
	entry.BlockOn = []string{"b"}
	var calls atomic.Int32
	c := sidecarPrimitiveCoordinator(t, entry, &calls)
	ctx := primitiveTestContext(t, "helper")

	first, err := c.coreTools[0].Run(ctx, primitiveTestCall("call", "text"))
	if err != nil || first.IsError || !strings.Contains(first.Content, `"choice":"b"`) || !strings.Contains(first.Content, `"backend":"sidecar"`) || strings.Contains(first.Content, `"candidates"`) {
		t.Fatalf("decision = %v %q, want choice b from the sidecar backend without candidates", err, first.Content)
	}
	if decision, blocked := c.decisionBlock(ctx); !blocked || decision != "classify=b" {
		t.Fatalf("block = %q %v, want the sidecar decision to stop the attempt", decision, blocked)
	}
	replayed, err := c.coreTools[0].Run(ctx, primitiveTestCall("call", "text"))
	if err != nil || replayed.Content != first.Content || calls.Load() != 1 {
		t.Fatalf("replay = %v %q after %d model calls, want the journaled decision", err, replayed.Content, calls.Load())
	}
}

func TestDecisionSidecarResolver(t *testing.T) {
	withDefault := &decisionSidecarResolver{defaultModel: "team-sidecar"}
	generator, err := withDefault.generator("")
	if err != nil || generator.ModelID() != "team-sidecar" {
		t.Fatalf("default generator = %v, %v; want the team sidecar model", generator, err)
	}
	if _, err := generator.Execute(t.Context(), "prompt"); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbound execute error = %v, want a not-bound error", err)
	}
	if explicit, err := withDefault.generator("ollama/minimax-m3:cloud"); err != nil || explicit.ModelID() != "ollama/minimax-m3:cloud" {
		t.Fatalf("explicit generator = %v, %v", explicit, err)
	}
	if _, err := (&decisionSidecarResolver{}).generator(""); err == nil {
		t.Fatal("a sidecar entry without any model was accepted")
	}
}

// TestDecisionPromptBypassesAuxiliaryContext pins decision-primitive.md §7.2
// for backend sidecar: the coordinator's prompt preparer must hand the
// decision prompt to the model unchanged, never wrapped in run context.
func TestDecisionPromptBypassesAuxiliaryContext(t *testing.T) {
	raw := "Choose exactly one candidate token from this JSON input.\nInput:\n{\"candidates\":[{\"token\":\"A0\"}]}"
	got, err := (&Coordinator{}).prepareAuxiliaryPrompt(t.Context(), decisionPrimitivePurpose, raw)
	if err != nil || got != raw {
		t.Fatalf("prepared prompt = %q, %v; want the raw decision prompt", got, err)
	}
}

// TestControlPointOnSidecarBackend runs path-reviewer with backend sidecar:
// the language model's answer has no confidence, so active mode applies it
// without a threshold, and every observation names the backend.
func TestControlPointOnSidecarBackend(t *testing.T) {
	for _, test := range []struct {
		mode        control.Mode
		wantAccess  string
		wantApplied string
	}{
		{mode: control.ModeActive, wantAccess: "false", wantApplied: controlAppliedPrimitive},
		{mode: control.ModeShadow, wantAccess: "true", wantApplied: controlAppliedLegacy},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			// The existing reviewer calls it a file access; the decision model,
			// through candidate A0 (false), does not.
			c, _ := newControlTestCoordinator(t, control.PathReviewer, control.ModeOff, "http://127.0.0.1:1/v1/systemone", `{"is_file_access": true, "reason": "reads a file"}`)
			var calls atomic.Int32
			service, err := control.New(control.Config{Backend: control.BackendSidecar, Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: test.mode}}}, utils.RedactSecrets,
				control.WithSidecarGenerator(func(string) (sidecarbackend.Generator, error) { return tokenZeroGenerator{calls: &calls}, nil }))
			if err != nil {
				t.Fatal(err)
			}
			c.controlDecisions = service
			got, err := invokeControlPoint(t.Context(), c, control.PathReviewer, "")
			if err != nil || got != test.wantAccess {
				t.Fatalf("path review = %q, %v; want %q", got, err, test.wantAccess)
			}
			observations := controlObservations(t, c)
			if len(observations) != 1 {
				t.Fatalf("observations = %d, want 1", len(observations))
			}
			if o := observations[0]; o.Backend != control.BackendSidecar || !o.Accepted || o.Threshold != 0 || o.Applied != test.wantApplied || o.Model != "test-sidecar" {
				t.Fatalf("observation = %+v, want an accepted sidecar decision applied as %s", o, test.wantApplied)
			}
		})
	}
}

// tokenZeroGenerator always picks candidate A0.
type tokenZeroGenerator struct{ calls *atomic.Int32 }

func (g tokenZeroGenerator) Execute(context.Context, string) (string, error) {
	g.calls.Add(1)
	return `{"token":"A0"}`, nil
}

func (g tokenZeroGenerator) ModelID() string { return "test-sidecar" }
