package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

// controlSidecarAgent answers every sidecar prompt with fixed text and counts
// calls, standing in for the existing path.
type controlSidecarAgent struct {
	text  string
	calls atomic.Int32
}

func (a *controlSidecarAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	a.calls.Add(1)
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: a.text}}}}, nil
}

func (a *controlSidecarAgent) Stream(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.Generate(ctx, fantasy.AgentCall{})
}

type controlModelServer struct {
	*httptest.Server
	calls atomic.Int32
	mu    sync.Mutex
	body  []string
}

func newControlModelServer(t *testing.T, status int, response string) *controlModelServer {
	t.Helper()
	server := &controlModelServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		server.calls.Add(1)
		data, _ := io.ReadAll(request.Body)
		server.mu.Lock()
		server.body = append(server.body, string(data))
		server.mu.Unlock()
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, response)
	}))
	t.Cleanup(server.Close)
	return server
}

func controlChoice(prefix string, selected int, probability float64) string {
	probabilities := map[string]float64{}
	for index := range 2 {
		probabilities[fmt.Sprintf("%s%02d", prefix, index)] = 1 - probability
	}
	key := fmt.Sprintf("%s%02d", prefix, selected)
	probabilities[key] = probability
	encoded, _ := json.Marshal(map[string]any{"answers": map[string]any{"decision": map[string]any{"type": "choice", "choice": key, "probabilities": probabilities, "confidence": 0.1}}})
	return string(encoded)
}

func newControlTestCoordinator(t *testing.T, point control.Point, mode control.Mode, endpoint, sidecarText string) (*Coordinator, *controlSidecarAgent) {
	t.Helper()
	cfg := control.Config{Points: map[control.Point]control.PointConfig{point: {Mode: mode}}}
	if mode == control.ModeActive {
		cfg.Backend = "systemone"
		cfg.Endpoint = endpoint
		cfg.Model = "nimble"
	}
	service, err := control.New(cfg, utils.RedactSecrets)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	t.Cleanup(func() { _ = store.Close() })
	generator := &controlSidecarAgent{text: sidecarText}
	side := sidecar.NewSidecarForTest("sidecar-model", generator)
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "control", ControlDecisions: cfg}, Agents: map[string]*agent.AgentDef{"reviewer": {Name: "reviewer", Role: "worker"}, "coder": {Name: "coder", Role: "worker"}}}, agentPool: &mockAgentPool{sidecar: side, guardSidecar: side}, controlDecisions: service, eventStore: store, eventJournal: eventStoreJournal{store: store}, sessionTime: time.Now(), executionRunID: "run-test", decisionPrimitiveGate: make(chan struct{}, 1)}
	side.SetDecisionExecutor(func(ctx context.Context, in sidecar.DecisionInvocation) (decisionrt.Result, error) {
		return c.runRuntimeChoice(ctx, side, in)
	})
	return c, generator
}

func TestRuntimeControlUsesPrimitiveAndSettledReplay(t *testing.T) {
	c, generator := newControlTestCoordinator(t, control.PathReviewer, control.ModeOff, "", `{"token":"A1"}`)
	for range 2 {
		access, err := c.reviewPathAccess(t.Context(), "sed 's|/a|/b|' file", "/a")
		if err != nil || access {
			t.Fatalf("access=%v error=%v", access, err)
		}
	}
	if generator.calls.Load() != 1 {
		t.Fatalf("calls=%d", generator.calls.Load())
	}
	records, err := c.DecisionPrimitiveResults(t.Context())
	if err != nil || len(records) != 2 || !records[1].Reused || records[0].Backend != "sidecar" || records[0].Transport == nil {
		t.Fatalf("records=%#v error=%v", records, err)
	}
	c.SetBudget(1, 0)
	c.sessionTime = time.Now().Add(-time.Hour)
	if _, err := c.reviewPathAccess(t.Context(), "sed 's|/a|/b|' file", "/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.reviewPathAccess(t.Context(), "cat /a", "/a"); err == nil {
		t.Fatal("changed snapshot bypassed budget")
	}
}

func TestRuntimeControlProviderFailureDoesNotRunLegacy(t *testing.T) {
	server := newControlModelServer(t, 503, "unavailable")
	c, generator := newControlTestCoordinator(t, control.GuardReviewer, control.ModeActive, server.URL+"/v1/systemone", `{"token":"A0"}`)
	for range 2 {
		approved, _, err := c.reviewGuardCall(t.Context(), "bash", "arguments", []string{"no writes"})
		if err == nil || approved {
			t.Fatalf("approved=%v error=%v", approved, err)
		}
	}
	if generator.calls.Load() != 0 || server.calls.Load() != 1 {
		t.Fatalf("sidecar=%d systemone=%d", generator.calls.Load(), server.calls.Load())
	}
}

func TestRuntimeControlSystemOneDecisionAndNoNet(t *testing.T) {
	server := newControlModelServer(t, 200, controlChoice("o", 0, 0.99))
	c, generator := newControlTestCoordinator(t, control.GuardReviewer, control.ModeActive, server.URL+"/v1/systemone", `{"token":"A1"}`)
	c.noNet = true
	approved, _, err := c.reviewGuardCall(t.Context(), "view", "arguments", []string{"read only"})
	if err != nil || !approved || generator.calls.Load() != 0 || server.calls.Load() != 1 {
		t.Fatalf("approved=%v error=%v calls=%d", approved, err, server.calls.Load())
	}
	c.forceMCP = true
	if approved, _, err := c.reviewGuardCall(t.Context(), "view", "arguments", []string{"read only"}); err == nil || approved {
		t.Fatal("force-mcp bypassed")
	}
}

func TestClosedQuestionAndMixedQuestionUseSeparatePaths(t *testing.T) {
	c, generator := newControlTestCoordinator(t, control.AskUser, control.ModeOff, "", `{"token":"A1"}`)
	opts := []tools.AskUserTUIOption{{Label: "Yes", Value: "yes"}, {Label: "No", Value: "no"}}
	response, err := c.chooseAskUserResponse(t.Context(), "proceed?", "single_choice", opts, false)
	if err != nil || len(response.Answers) != 1 || response.Answers[0] != "yes" {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	generator.text = `{"answers":[],"free_text":"custom answer"}`
	response, err = c.chooseAskUserResponse(t.Context(), "proceed?", "mixed", opts, true)
	if err != nil || response.Free != "custom answer" {
		t.Fatalf("mixed response=%#v error=%v", response, err)
	}
	records, err := c.DecisionPrimitiveResults(t.Context())
	if err != nil || len(records) != 1 || generator.calls.Load() != 2 {
		t.Fatalf("records=%d calls=%d error=%v", len(records), generator.calls.Load(), err)
	}
}
