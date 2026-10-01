package control_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"gopkg.in/yaml.v3"
)

func identity(text string) string { return text }

func threshold(value float64) *float64 { return &value }

type systemOneServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	status int
	body   string
}

func newSystemOneServer(t *testing.T, status int, body string) *systemOneServer {
	t.Helper()
	server := &systemOneServer{status: status, body: body}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		var decoded map[string]any
		_ = json.Unmarshal(data, &decoded)
		server.mu.Lock()
		server.bodies = append(server.bodies, decoded)
		server.mu.Unlock()
		writer.WriteHeader(server.status)
		_, _ = io.WriteString(writer, server.body)
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *systemOneServer) endpoint() string { return s.URL + "/v1/systemone" }

func (s *systemOneServer) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

func noulBody(probability string) string {
	return `{"answers":{"decision":{"type":"noul","noul":` + probability + `}}}`
}

func TestConfigDecodesFromYAML(t *testing.T) {
	var config control.Config
	err := yaml.Unmarshal([]byte(`
endpoint: http://gpu:11434/v1/systemone
model: nimble
api-key-env: SYSTEMONE_KEY
timeout: 7s
mode: shadow
min-confidence: 0.8
points:
  path-reviewer: {mode: active, min-confidence: 0.95}
`), &config)
	if err != nil {
		t.Fatal(err)
	}
	if config.Timeout != 7*time.Second || config.Mode != control.ModeShadow || *config.MinConfidence != 0.8 ||
		config.Points[control.PathReviewer].Mode != control.ModeActive || *config.Points[control.PathReviewer].MinConfidence != 0.95 {
		t.Fatalf("config = %#v", config)
	}
}

func TestMergeResolvesEachFieldTeamFirst(t *testing.T) {
	base := control.Config{
		Endpoint: "http://home/v1/systemone", Model: "nimble", APIKeyEnv: "HOME_KEY", Timeout: 3 * time.Second,
		Mode: control.ModeShadow, MinConfidence: threshold(0.7),
		Points: map[control.Point]control.PointConfig{
			control.PathReviewer:  {Mode: control.ModeActive, MinConfidence: threshold(0.99)},
			control.GuardReviewer: {Mode: control.ModeOff},
		},
	}
	override := control.Config{
		Model: "nimble-2", Mode: control.ModeActive,
		Points: map[control.Point]control.PointConfig{
			control.PathReviewer: {MinConfidence: threshold(0.5)},
			control.AskUser:      {Mode: control.ModeOff},
		},
	}
	merged := control.Merge(base, override)
	if merged.Endpoint != "http://home/v1/systemone" || merged.Model != "nimble-2" || merged.APIKeyEnv != "HOME_KEY" ||
		merged.Timeout != 3*time.Second || merged.Mode != control.ModeActive || *merged.MinConfidence != 0.7 {
		t.Fatalf("merged scalars = %#v", merged)
	}
	path := merged.Points[control.PathReviewer]
	if path.Mode != control.ModeActive || *path.MinConfidence != 0.5 {
		t.Fatalf("path-reviewer = %#v", path)
	}
	if merged.Points[control.GuardReviewer].Mode != control.ModeOff || merged.Points[control.AskUser].Mode != control.ModeOff {
		t.Fatalf("points = %#v", merged.Points)
	}
	*merged.MinConfidence = 0
	if *base.MinConfidence != 0.7 || *base.Points[control.PathReviewer].MinConfidence != 0.99 {
		t.Fatal("Merge aliased its input")
	}
}

func TestNewResolvesModesAndThresholds(t *testing.T) {
	server := newSystemOneServer(t, http.StatusOK, noulBody("0.9"))
	service, err := control.New(control.Config{
		Endpoint: server.endpoint(), Model: "nimble", Mode: control.ModeShadow,
		Points: map[control.Point]control.PointConfig{
			control.GuardReviewer: {Mode: control.ModeOff},
			control.PathReviewer:  {Mode: control.ModeActive, MinConfidence: threshold(0.95)},
		},
	}, identity)
	if err != nil {
		t.Fatal(err)
	}
	want := map[control.Point]struct {
		mode      control.Mode
		threshold float64
	}{
		control.AgentMatcher:  {control.ModeShadow, 0.60},
		control.AskUser:       {control.ModeShadow, 0.60},
		control.PathReviewer:  {control.ModeActive, 0.95},
		control.GuardReviewer: {control.ModeOff, 0.90},
	}
	for point, expected := range want {
		if service.Mode(point) != expected.mode || service.MinConfidence(point) != expected.threshold {
			t.Errorf("%s = %s %v, want %s %v", point, service.Mode(point), service.MinConfidence(point), expected.mode, expected.threshold)
		}
	}
	if len(server.requests()) != 0 {
		t.Fatal("New contacted the backend")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		config control.Config
		want   string
	}{
		{name: "unknown mode", config: control.Config{Mode: "on"}, want: "mode"},
		{name: "unknown point", config: control.Config{Points: map[control.Point]control.PointConfig{"similar-task": {Mode: control.ModeShadow}}}, want: "unknown point"},
		{name: "threshold above one", config: control.Config{MinConfidence: threshold(1.5)}, want: "min-confidence"},
		{name: "point threshold below zero", config: control.Config{Points: map[control.Point]control.PointConfig{control.AskUser: {MinConfidence: threshold(-0.1)}}}, want: "min-confidence"},
		{name: "missing model", config: control.Config{Mode: control.ModeShadow}, want: "model"},
		{name: "timeout above maximum", config: control.Config{Mode: control.ModeShadow, Model: "nimble", Timeout: 31 * time.Second}, want: "timeout"},
		{name: "negative timeout", config: control.Config{Mode: control.ModeShadow, Model: "nimble", Timeout: -time.Second}, want: "timeout"},
		{name: "endpoint with query", config: control.Config{Mode: control.ModeShadow, Model: "nimble", Endpoint: "http://gpu/v1/systemone?x=1"}, want: "endpoint"},
		{name: "invalid key variable", config: control.Config{Mode: control.ModeShadow, Model: "nimble", APIKeyEnv: "1BAD"}, want: "api-key-env"},
		{name: "unset key variable", config: control.Config{Mode: control.ModeShadow, Model: "nimble", APIKeyEnv: "HUFU_CONTROL_TEST_UNSET_KEY"}, want: "api-key-env"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := control.New(test.config, identity)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("err = %v, want mention of %q", err, test.want)
			}
		})
	}
	if _, err := control.New(control.Config{}, nil); err == nil {
		t.Fatal("New accepted a nil redactor")
	}
}

func TestOffConfigIgnoresTransport(t *testing.T) {
	service, err := control.New(control.Config{Model: " bad model ", APIKeyEnv: "HUFU_CONTROL_TEST_UNSET_KEY", Timeout: time.Hour}, identity)
	if err != nil {
		t.Fatalf("off config must not validate transport: %v", err)
	}
	if service.Enabled() || service.Hash() != "" || len(service.EnabledPoints()) != 0 {
		t.Fatalf("service = %#v", service)
	}
	var nilService *control.Service
	if nilService.Mode(control.PathReviewer) != control.ModeOff || nilService.Enabled() || nilService.Hash() != "" {
		t.Fatal("nil service must be off")
	}
}

func TestHashPinsActivePolicyOnly(t *testing.T) {
	hash := func(t *testing.T, config control.Config) string {
		t.Helper()
		config.Model = "nimble"
		service, err := control.New(config, identity)
		if err != nil {
			t.Fatal(err)
		}
		return service.Hash()
	}
	shadow := hash(t, control.Config{Mode: control.ModeShadow})
	if shadow != "" {
		t.Fatalf("shadow-only hash = %q, want empty", shadow)
	}
	active := hash(t, control.Config{Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeActive}}})
	if len(active) != 64 {
		t.Fatalf("active hash = %q", active)
	}
	plusShadow := hash(t, control.Config{Mode: control.ModeShadow, Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeActive}}})
	if plusShadow != active {
		t.Fatal("shadow points changed the active hash")
	}
	stricter := hash(t, control.Config{Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeActive, MinConfidence: threshold(0.99)}}})
	if stricter == active {
		t.Fatal("threshold change did not change the hash")
	}
	t.Setenv("HUFU_CONTROL_TEST_KEY", "sk-one")
	keyed := hash(t, control.Config{APIKeyEnv: "HUFU_CONTROL_TEST_KEY", Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeActive}}})
	t.Setenv("HUFU_CONTROL_TEST_KEY", "sk-two")
	rotated := hash(t, control.Config{APIKeyEnv: "HUFU_CONTROL_TEST_KEY", Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeActive}}})
	if keyed == rotated || strings.Contains(keyed, "sk-") {
		t.Fatal("credential revision is not pinned without its value")
	}
}

func TestPointRequestsHaveFixedContracts(t *testing.T) {
	tests := []struct {
		name    string
		build   func() (decisionrt.Request, bool)
		id      string
		kind    decisionrt.Kind
		options []string
		context []string
	}{
		{
			name: "agent matcher", id: "hufu.agent-matcher", kind: decisionrt.KindChoice,
			build: func() (decisionrt.Request, bool) {
				return control.AgentMatcherRequest("fix the parser", []control.Worker{{Name: "coder", Description: "Writes code"}, {Name: "reviewer"}})
			},
			options: []string{"w01=coder: Writes code", "w02=reviewer"}, context: []string{"task"},
		},
		{
			name: "ask user", id: "hufu.ask-user", kind: decisionrt.KindChoice,
			build: func() (decisionrt.Request, bool) {
				return control.AskUserRequest("Proceed?", []control.Option{{Label: "Yes", Value: "yes"}, {Label: "No"}})
			},
			options: []string{"option-01=Yes (value: yes)", "option-02=No"}, context: []string{"question"},
		},
		{
			name: "path reviewer", id: "hufu.path-reviewer", kind: decisionrt.KindBoolean,
			build:   func() (decisionrt.Request, bool) { return control.PathReviewerRequest("cat /etc/hosts", "/etc/hosts") },
			context: []string{"command", "path"},
		},
		{
			name: "guard reviewer", id: "hufu.guard-reviewer", kind: decisionrt.KindBoolean,
			build: func() (decisionrt.Request, bool) {
				return control.GuardReviewerRequest("coder", "bash", `{"command":"ls"}`, []string{"never delete files"})
			},
			context: []string{"agent", "arguments", "rules", "tool"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, ok := test.build()
			if !ok {
				t.Fatal("point not applicable")
			}
			if request.Purpose != test.id || request.Spec.ID != test.id || request.Spec.Version != "v1" || request.Spec.Kind != test.kind {
				t.Fatalf("request = %#v", request)
			}
			var options []string
			for _, option := range request.Spec.Options {
				options = append(options, option.ID+"="+option.Description)
			}
			if strings.Join(options, ",") != strings.Join(test.options, ",") {
				t.Fatalf("options = %v, want %v", options, test.options)
			}
			var keys []string
			for key := range request.Context {
				keys = append(keys, key)
			}
			if len(keys) != len(test.context) {
				t.Fatalf("context keys = %v, want %v", keys, test.context)
			}
			for _, key := range test.context {
				if _, ok := request.Context[key]; !ok {
					t.Fatalf("context missing %q: %v", key, keys)
				}
			}
		})
	}
	guard, _ := control.GuardReviewerRequest("coder", "bash", "{}", []string{"rule one", "rule two"})
	if guard.Context["rules"] != "1. rule one\n2. rule two\n" {
		t.Fatalf("rules = %q", guard.Context["rules"])
	}
}

func TestPointsOutsideTheirDomainAreNotApplicable(t *testing.T) {
	many := func(count int) []control.Worker {
		workers := make([]control.Worker, count)
		for index := range workers {
			workers[index] = control.Worker{Name: "w" + string(rune('a'+index))}
		}
		return workers
	}
	options := func(count int) []control.Option {
		values := make([]control.Option, count)
		for index := range values {
			values[index] = control.Option{Label: "option " + string(rune('a'+index))}
		}
		return values
	}
	tests := []struct {
		name string
		ok   bool
	}{
		{name: "one worker", ok: second(control.AgentMatcherRequest("task", many(1)))},
		{name: "twenty-two workers", ok: second(control.AgentMatcherRequest("task", many(22)))},
		{name: "one option", ok: second(control.AskUserRequest("q", options(1)))},
		{name: "twenty-two options", ok: second(control.AskUserRequest("q", options(22)))},
		{name: "empty path", ok: second(control.PathReviewerRequest("ls", " "))},
		{name: "no guard rules", ok: second(control.GuardReviewerRequest("a", "bash", "{}", nil))},
		{name: "guard rules over budget", ok: second(control.GuardReviewerRequest("a", "bash", "{}", []string{strings.Repeat("r", 4000)}))},
	}
	for _, test := range tests {
		if test.ok {
			t.Errorf("%s: point applied, want not applicable", test.name)
		}
	}
	if !second(control.AgentMatcherRequest("task", many(21))) || !second(control.AskUserRequest("q", options(21))) {
		t.Fatal("21 candidates must apply")
	}
}

func second(_ decisionrt.Request, ok bool) bool { return ok }

func TestLongTextIsTruncatedToValidLimits(t *testing.T) {
	long := strings.Repeat("界", 5000)
	request, ok := control.PathReviewerRequest(long+"\xff", "/etc/hosts")
	if !ok {
		t.Fatal("long command must still apply")
	}
	command := request.Context["command"].(string)
	if !utf8.ValidString(command) || len(command) > 3584 || utf8.RuneCountInString(command) > 3000 {
		t.Fatalf("command = %d bytes, %d runes", len(command), utf8.RuneCountInString(command))
	}
	agent, ok := control.AgentMatcherRequest(long, []control.Worker{{Name: "a", Description: long}, {Name: "b"}})
	if !ok || utf8.RuneCountInString(agent.Spec.Options[0].Description) > 300 || len(agent.Context["task"].(string)) > 3584 {
		t.Fatalf("agent request = %v %#v", ok, agent.Spec.Options[0])
	}
}

func TestDecideMapsOutcomesAndAppliesThreshold(t *testing.T) {
	choice := func(selected string, probability float64) string {
		other := 1 - probability
		probabilities := map[string]float64{"o00": other, "o01": probability}
		if selected == "o00" {
			probabilities = map[string]float64{"o00": probability, "o01": other}
		}
		encoded, _ := json.Marshal(map[string]any{"answers": map[string]any{"decision": map[string]any{"type": "choice", "choice": selected, "probabilities": probabilities, "confidence": 0.1}}})
		return string(encoded)
	}
	agentRequest, _ := control.AgentMatcherRequest("task", []control.Worker{{Name: "a"}, {Name: "b"}})
	pathRequest, _ := control.PathReviewerRequest("cat /etc/hosts", "/etc/hosts")
	tests := []struct {
		name       string
		point      control.Point
		request    decisionrt.Request
		status     int
		body       string
		want       control.Status
		value      string
		accepted   bool
		errorCode  string
		confidence float64
	}{
		{name: "choice accepted", point: control.AgentMatcher, request: agentRequest, status: 200, body: choice("o01", 0.8), want: control.StatusDecided, value: "1", accepted: true, confidence: 0.8},
		{name: "choice below threshold keeps confidence", point: control.AgentMatcher, request: agentRequest, status: 200, body: choice("o00", 0.55), want: control.StatusDecided, value: "0", confidence: 0.55},
		{name: "boolean false accepted", point: control.PathReviewer, request: pathRequest, status: 200, body: noulBody("0.02"), want: control.StatusDecided, value: "false", accepted: true, confidence: 0.98},
		{name: "boolean true below threshold", point: control.PathReviewer, request: pathRequest, status: 200, body: noulBody("0.7"), want: control.StatusDecided, value: "true", confidence: 0.7},
		{name: "server error", point: control.PathReviewer, request: pathRequest, status: 500, body: `{}`, want: control.StatusError, errorCode: "backend_failure"},
		{name: "unknown model", point: control.PathReviewer, request: pathRequest, status: 404, body: `{}`, want: control.StatusError, errorCode: "backend_unavailable"},
		{name: "invalid output", point: control.PathReviewer, request: pathRequest, status: 200, body: `{"answers":{}}`, want: control.StatusError, errorCode: "invalid_backend_output"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newSystemOneServer(t, test.status, test.body)
			service, err := control.New(control.Config{Endpoint: server.endpoint(), Model: "nimble", Mode: control.ModeShadow}, identity)
			if err != nil {
				t.Fatal(err)
			}
			outcome := service.Decide(t.Context(), test.point, test.request)
			if outcome.Status != test.want || outcome.Value != test.value || outcome.Accepted != test.accepted || outcome.ErrorCode != test.errorCode {
				t.Fatalf("outcome = %#v", outcome)
			}
			if test.want == control.StatusDecided && (outcome.Confidence < test.confidence-1e-9 || outcome.Confidence > test.confidence+1e-9 || outcome.Model != "nimble") {
				t.Fatalf("outcome = %#v, want confidence %v", outcome, test.confidence)
			}
		})
	}
}

func TestDecideRedactsContextAndSkipsOffPoints(t *testing.T) {
	server := newSystemOneServer(t, http.StatusOK, noulBody("0.9"))
	service, err := control.New(control.Config{
		Endpoint: server.endpoint(), Model: "nimble",
		Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeShadow}},
	}, func(text string) string { return strings.ReplaceAll(text, "sk-live-secret", "[REDACTED]") })
	if err != nil {
		t.Fatal(err)
	}
	request, _ := control.PathReviewerRequest("curl -H 'Authorization: sk-live-secret' -o /tmp/out", "/tmp/out")
	if outcome := service.Decide(t.Context(), control.PathReviewer, request); outcome.Status != control.StatusDecided {
		t.Fatalf("outcome = %#v", outcome)
	}
	sent := server.requests()
	if len(sent) != 1 {
		t.Fatalf("requests = %d", len(sent))
	}
	state, _ := json.Marshal(sent[0]["state"])
	if strings.Contains(string(state), "sk-live-secret") || !strings.Contains(string(state), "[REDACTED]") {
		t.Fatalf("state = %s", state)
	}
	if request.Context["command"] == "curl -H 'Authorization: [REDACTED]' -o /tmp/out" {
		t.Fatal("Decide modified the caller's request")
	}
	guard, _ := control.GuardReviewerRequest("a", "bash", "{}", []string{"rule"})
	if outcome := service.Decide(t.Context(), control.GuardReviewer, guard); outcome.Status != control.StatusError || outcome.ErrorCode != "point_off" {
		t.Fatalf("off point outcome = %#v", outcome)
	}
	if len(server.requests()) != 1 {
		t.Fatal("an off point contacted the backend")
	}
}

func TestControlDependencyBoundary(t *testing.T) {
	output, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for dependency := range strings.Lines(string(output)) {
		dependency = strings.TrimSpace(dependency)
		for _, forbidden := range []string{
			"github.com/kjelly/hufu/internal/team",
			"github.com/kjelly/hufu/internal/agent",
			"github.com/kjelly/hufu/internal/sidecar",
			"github.com/kjelly/hufu/cmd/hufu",
		} {
			if dependency == forbidden {
				t.Errorf("control depends on %s", forbidden)
			}
		}
	}
}
