package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
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

func controlNoul(probability float64) string {
	return fmt.Sprintf(`{"answers":{"decision":{"type":"noul","noul":%v}}}`, probability)
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
	service, err := control.New(control.Config{Endpoint: endpoint, Model: "nimble",
		Points: map[control.Point]control.PointConfig{point: {Mode: mode}}}, utils.RedactSecrets)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(t.TempDir(), "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	t.Cleanup(func() { _ = store.Close() })
	legacy := &controlSidecarAgent{text: sidecarText}
	side := sidecar.NewSidecarForTest("sidecar-model", legacy)
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{Name: "control"}, Agents: map[string]*agent.AgentDef{
			"reviewer": {Name: "reviewer", Role: "worker", Description: "Reviews changes."},
			"coder":    {Name: "coder", Role: "worker", Description: "Writes code."},
		}},
		agentPool:        &mockAgentPool{sidecar: side, guardSidecar: side},
		controlDecisions: service,
		eventStore:       store,
		eventJournal:     eventStoreJournal{store: store},
		sessionTime:      time.Now(),
	}
	return c, legacy
}

var askUserTestOptions = []tools.AskUserTUIOption{{Label: "Yes", Value: "yes"}, {Label: "No", Value: "no"}}

func invokeControlPoint(ctx context.Context, c *Coordinator, point control.Point, qtype string) (string, error) {
	switch point {
	case control.GuardReviewer:
		approved, _, err := c.reviewGuardCall(ctx, "bash", `{"command":"rm -rf /srv/data"}`, []string{"never delete files under /srv"})
		return fmt.Sprint(approved), err
	case control.PathReviewer:
		isFileAccess, err := c.reviewPathAccess(ctx, "cat /srv/secret-notes.txt", "/srv/secret-notes.txt")
		return fmt.Sprint(isFileAccess), err
	case control.AskUser:
		response, err := c.chooseAskUserResponse(ctx, "Deploy the release now?", qtype, askUserTestOptions, false)
		return strings.Join(response.Answers, ","), err
	default:
		return c.selectAgentForGoal(ctx, "fix the flaky parser test")
	}
}

func controlObservations(t *testing.T, c *Coordinator) []controlDecisionPayload {
	t.Helper()
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var payloads []controlDecisionPayload
	for _, event := range events {
		if event.Type != string(EventControlDecisionObserved) {
			continue
		}
		if err := ValidateEventPayload(event); err != nil {
			t.Fatalf("persisted observation is invalid: %v", err)
		}
		for _, leaked := range []string{"/srv", "rm -rf", "Deploy the release", "flaky parser", "never delete", "Writes code", "nimble-secret"} {
			if strings.Contains(string(event.Payload), leaked) {
				t.Fatalf("observation leaks %q: %s", leaked, event.Payload)
			}
		}
		var payload controlDecisionPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func TestControlDecisionPointsApplyModes(t *testing.T) {
	agree, disagree := true, false
	const (
		guardApprove = `{"approved": true, "reason": ""}`
		guardDeny    = `{"approved": false, "reason": "violates rule 1"}`
		pathYes      = `{"is_file_access": true, "reason": "reads it"}`
		pathNo       = `{"is_file_access": false, "reason": "pattern"}`
		askNo        = `{"answers":["no"],"free_text":""}`
		agentPick    = `{"agent":"reviewer","confidence":0.9,"reason":"reviews"}`
	)
	tests := []struct {
		name        string
		point       control.Point
		mode        control.Mode
		sidecar     string
		status      int
		model       string
		qtype       string
		want        string
		wantErr     string
		applied     string
		agree       *bool
		legacyCalls int32
		modelCalls  int32
	}{
		{name: "guard off", point: control.GuardReviewer, mode: control.ModeOff, sidecar: guardApprove, status: 200, model: controlNoul(0.01), want: "true", legacyCalls: 1},
		{name: "guard shadow keeps legacy on disagreement", point: control.GuardReviewer, mode: control.ModeShadow, sidecar: guardDeny, status: 200, model: controlNoul(0.97), want: "false", applied: "legacy", agree: &disagree, legacyCalls: 1, modelCalls: 1},
		{name: "guard shadow agreement", point: control.GuardReviewer, mode: control.ModeShadow, sidecar: guardApprove, status: 200, model: controlNoul(0.97), want: "true", applied: "legacy", agree: &agree, legacyCalls: 1, modelCalls: 1},
		{name: "guard active approves", point: control.GuardReviewer, mode: control.ModeActive, sidecar: guardDeny, status: 200, model: controlNoul(0.97), want: "true", applied: "primitive", modelCalls: 1},
		{name: "guard active denies", point: control.GuardReviewer, mode: control.ModeActive, sidecar: guardApprove, status: 200, model: controlNoul(0.02), want: "false", applied: "primitive", modelCalls: 1},
		{name: "guard active abstention denies", point: control.GuardReviewer, mode: control.ModeActive, sidecar: guardApprove, status: 200, model: controlNoul(0.7), want: "false", applied: "safe_default", modelCalls: 1},
		{name: "guard active outage runs legacy", point: control.GuardReviewer, mode: control.ModeActive, sidecar: guardApprove, status: 500, model: `{}`, want: "true", applied: "legacy", legacyCalls: 1, modelCalls: 1},

		{name: "path off", point: control.PathReviewer, mode: control.ModeOff, sidecar: pathNo, status: 200, model: controlNoul(0.99), want: "false", legacyCalls: 1},
		{name: "path shadow keeps legacy", point: control.PathReviewer, mode: control.ModeShadow, sidecar: pathNo, status: 200, model: controlNoul(0.97), want: "false", applied: "legacy", agree: &disagree, legacyCalls: 1, modelCalls: 1},
		{name: "path active drops a non-access", point: control.PathReviewer, mode: control.ModeActive, sidecar: pathYes, status: 200, model: controlNoul(0.02), want: "false", applied: "primitive", modelCalls: 1},
		{name: "path active abstention keeps the path", point: control.PathReviewer, mode: control.ModeActive, sidecar: pathNo, status: 200, model: controlNoul(0.3), want: "true", applied: "safe_default", modelCalls: 1},
		{name: "path active unknown model runs legacy", point: control.PathReviewer, mode: control.ModeActive, sidecar: pathNo, status: 404, model: `{}`, want: "false", applied: "legacy", legacyCalls: 1, modelCalls: 1},

		{name: "ask-user shadow agreement", point: control.AskUser, mode: control.ModeShadow, sidecar: askNo, status: 200, model: controlChoice("o", 1, 0.95), qtype: "single_choice", want: "no", applied: "legacy", agree: &agree, legacyCalls: 1, modelCalls: 1},
		{name: "ask-user active choice", point: control.AskUser, mode: control.ModeActive, sidecar: askNo, status: 200, model: controlChoice("o", 0, 0.95), qtype: "single_choice", want: "yes", applied: "primitive", modelCalls: 1},
		{name: "ask-user active abstention", point: control.AskUser, mode: control.ModeActive, sidecar: askNo, status: 200, model: controlChoice("o", 0, 0.55), qtype: "single_choice", wantErr: tools.ErrAskUserAbstained.Error(), applied: "safe_default", modelCalls: 1},
		{name: "ask-user active invalid output runs legacy", point: control.AskUser, mode: control.ModeActive, sidecar: askNo, status: 200, model: `{"answers":{}}`, qtype: "single_choice", want: "no", applied: "legacy", legacyCalls: 1, modelCalls: 1},
		{name: "ask-user multiple choice stays legacy", point: control.AskUser, mode: control.ModeActive, sidecar: askNo, status: 200, model: controlChoice("o", 0, 0.95), qtype: "multiple_choice", want: "no", legacyCalls: 1},

		{name: "agent shadow keeps legacy", point: control.AgentMatcher, mode: control.ModeShadow, sidecar: agentPick, status: 200, model: controlChoice("o", 0, 0.9), want: "reviewer", applied: "legacy", agree: &disagree, legacyCalls: 1, modelCalls: 1},
		{name: "agent active choice", point: control.AgentMatcher, mode: control.ModeActive, sidecar: agentPick, status: 200, model: controlChoice("o", 0, 0.9), want: "coder", applied: "primitive", modelCalls: 1},
		{name: "agent active abstention fails closed", point: control.AgentMatcher, mode: control.ModeActive, sidecar: agentPick, status: 200, model: controlChoice("o", 1, 0.5), wantErr: "ambiguous", applied: "safe_default", modelCalls: 1},
		{name: "agent active outage runs legacy", point: control.AgentMatcher, mode: control.ModeActive, sidecar: agentPick, status: 503, model: `{}`, want: "reviewer", applied: "legacy", legacyCalls: 1, modelCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newControlModelServer(t, test.status, test.model)
			c, legacy := newControlTestCoordinator(t, test.point, test.mode, server.URL+"/v1/systemone", test.sidecar)
			ctx := withInvocationMetadata(t.Context(), InvocationMetadata{RunID: "run-test", TaskID: "1", AgentName: "coder", Attempt: 1})
			got, err := invokeControlPoint(ctx, c, test.point, test.qtype)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err = %v, want %q", err, test.wantErr)
				}
			} else if err != nil || got != test.want {
				t.Fatalf("result = %q, %v; want %q", got, err, test.want)
			}
			if legacy.calls.Load() != test.legacyCalls || server.calls.Load() != test.modelCalls {
				t.Fatalf("legacy calls = %d, model calls = %d; want %d, %d", legacy.calls.Load(), server.calls.Load(), test.legacyCalls, test.modelCalls)
			}
			observations := controlObservations(t, c)
			if test.applied == "" {
				if len(observations) != 0 {
					t.Fatalf("observations = %#v, want none", observations)
				}
				return
			}
			if len(observations) != 1 {
				t.Fatalf("observations = %d, want 1", len(observations))
			}
			observed := observations[0]
			if observed.Point != string(test.point) || observed.Mode != string(test.mode) || observed.Applied != test.applied || observed.Model != "nimble" {
				t.Fatalf("observation = %#v", observed)
			}
			if (observed.Agree == nil) != (test.agree == nil) || observed.Agree != nil && *observed.Agree != *test.agree {
				t.Fatalf("agree = %v, want %v", observed.Agree, test.agree)
			}
		})
	}
}

func TestControlDecisionShadowRunsBothSidesConcurrently(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(writer, controlNoul(0.9))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	c, legacy := newControlTestCoordinator(t, control.PathReviewer, control.ModeShadow, server.URL+"/v1/systemone", `{"is_file_access": true, "reason": "x"}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.reviewPathAccess(t.Context(), "cat /srv/a", "/srv/a")
	}()
	deadline := time.After(5 * time.Second)
	for legacy.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the existing path did not start while the decision model was still running")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(release)
	<-done
	if observations := controlObservations(t, c); len(observations) != 1 || observations[0].Status != "decided" {
		t.Fatalf("observations = %#v", observations)
	}
}

func TestControlDecisionRedactsRegisteredSecrets(t *testing.T) {
	server := newControlModelServer(t, http.StatusOK, controlNoul(0.9))
	c, _ := newControlTestCoordinator(t, control.PathReviewer, control.ModeShadow, server.URL+"/v1/systemone", `{"is_file_access": true, "reason": "x"}`)
	registry := tools.NewSecretRegistry()
	_ = registry.Register(tools.SecretRef{Name: "test.secret", Source: "test", ExactValue: "nimble-secret-value-0123456789"})
	// Process redactors cannot be unregistered; this one only matches its own
	// test value.
	utils.RegisterSecretRedactor(registry)
	if _, err := c.reviewPathAccess(t.Context(), "curl -H 'X-Token: nimble-secret-value-0123456789' -o /srv/out", "/srv/out"); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.body) != 1 || strings.Contains(server.body[0], "nimble-secret-value") {
		t.Fatalf("decision request leaked a registered secret: %v", server.body)
	}
}

func TestControlDecisionEventValidation(t *testing.T) {
	valid := controlDecisionPayload{Version: 1, Point: "path-reviewer", Mode: "shadow", Applied: "legacy", Status: "decided", Value: "true", Confidence: 0.9, Threshold: 0.9, Legacy: "false", Agree: new(false)}
	mutations := map[string]func(*controlDecisionPayload){
		"unknown point":             func(p *controlDecisionPayload) { p.Point = "similar-task" },
		"shadow applying primitive": func(p *controlDecisionPayload) { p.Applied = "primitive" },
		"value on error":            func(p *controlDecisionPayload) { p.Status, p.ErrorCode = "error", "timeout" },
		"free text value":           func(p *controlDecisionPayload) { p.Value = "/etc/passwd" },
		"confidence above one":      func(p *controlDecisionPayload) { p.Confidence = 1.5 },
		"free text legacy":          func(p *controlDecisionPayload) { p.Legacy = "rm -rf" },
		"agree without comparison":  func(p *controlDecisionPayload) { p.Legacy = "error" },
		"error without code":        func(p *controlDecisionPayload) { p.Status, p.Value, p.Agree = "error", "", nil },
	}
	encode := func(payload controlDecisionPayload) RunEvent {
		data, _ := json.Marshal(payload)
		return RunEvent{Type: string(EventControlDecisionObserved), Actor: "coder", Payload: data}
	}
	if err := validateControlDecisionEvent(encode(valid)); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	for name, mutate := range mutations {
		payload := valid
		mutate(&payload)
		if err := validateControlDecisionEvent(encode(payload)); err == nil {
			t.Errorf("%s: invalid payload accepted", name)
		}
	}
	unknown := encode(valid)
	unknown.Payload = json.RawMessage(strings.Replace(string(unknown.Payload), `"version":1`, `"version":1,"command":"cat /etc/passwd"`, 1))
	if err := validateControlDecisionEvent(unknown); err == nil {
		t.Fatal("unknown payload field accepted")
	}
}
