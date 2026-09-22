package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/decisionrt"
)

type decisionRTFakeBackend struct {
	name   string
	decide func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error)
}

func (b decisionRTFakeBackend) Name() string { return b.name }

func (b decisionRTFakeBackend) Decide(ctx context.Context, request decisionrt.Request) (decisionrt.BackendResult, error) {
	return b.decide(ctx, request)
}

type decisionRTFakeRegistry struct {
	backend      decisionrt.Backend
	resolveError error
	infos        []BackendInfo
	resolveCalls int
}

func (r *decisionRTFakeRegistry) Resolve(context.Context, string) (decisionrt.Backend, error) {
	r.resolveCalls++
	return r.backend, r.resolveError
}

func (r *decisionRTFakeRegistry) List() []BackendInfo {
	return r.infos
}

type decisionRTCommandFixture struct {
	stdin         *strings.Reader
	stdout        bytes.Buffer
	stderr        bytes.Buffer
	registry      *decisionRTFakeRegistry
	factoryCalls  int
	registryOpts  RegistryOptions
	terminalStdin bool
	environment   map[string]string
}

func newDecisionRTCommandFixture(backend decisionrt.Backend) *decisionRTCommandFixture {
	return &decisionRTCommandFixture{
		stdin:         strings.NewReader(""),
		registry:      &decisionRTFakeRegistry{backend: backend},
		terminalStdin: true,
		environment:   make(map[string]string),
	}
}

func (f *decisionRTCommandFixture) deps() decisionRTDeps {
	return decisionRTDeps{
		registryFactory: func(options RegistryOptions) BackendRegistry {
			f.factoryCalls++
			f.registryOpts = options
			return f.registry
		},
		stdin:           f.stdin,
		stdout:          &f.stdout,
		stderr:          &f.stderr,
		stdinIsTerminal: func() bool { return f.terminalStdin },
		getenv:          func(key string) string { return f.environment[key] },
	}
}

func (f *decisionRTCommandFixture) execute(args ...string) error {
	deps := f.deps()
	command := newDecisionRTCommand(deps)
	command.SetArgs(args)
	return command.Execute()
}

func TestDecisionRTChoiceJSONAndContextPrecedence(t *testing.T) {
	var captured decisionrt.Request
	backend := decisionRTFakeBackend{name: "sidecar", decide: func(_ context.Context, request decisionrt.Request) (decisionrt.BackendResult, error) {
		captured = request
		return decidedDecisionRTChoice("large"), nil
	}}
	fixture := newDecisionRTCommandFixture(backend)
	contextFile := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(contextFile, []byte(`{"winner":"file","file_only":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := fixture.execute(
		"choice", "--id", "size", "--version", "v1", "--purpose", "size@v1", "--question", "Choose.",
		"--option", "small=Small", "--option", "large=Large", "--context-file", contextFile,
		"--context-json", `{"winner":"json","json_only":true}`, "--context", "winner=flag",
		"--context", "number=3", "--context", "code=\"001\"", "--backend", "sidecar", "--json",
	)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.stderr.Len() != 0 {
		t.Fatalf("stderr = %q", fixture.stderr.String())
	}
	var result decisionrt.Result
	if err := json.Unmarshal(fixture.stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout = %q: %v", fixture.stdout.String(), err)
	}
	if result.Value.Choice != "large" || result.Backend != "sidecar" {
		t.Fatalf("result = %#v", result)
	}
	if captured.Context["winner"] != "flag" || captured.Context["file_only"] != json.Number("1") || captured.Context["json_only"] != true || captured.Context["number"] != json.Number("3") || captured.Context["code"] != "001" {
		t.Fatalf("context = %#v", captured.Context)
	}
}

func TestDecisionRTChoiceValidationErrors(t *testing.T) {
	tests := map[string][]string{
		"duplicate option": append(validDecisionRTChoiceArgs(), "--option", "small=Again"),
		"one option": {
			"choice", "--id", "size", "--version", "v1", "--purpose", "size@v1", "--question", "Choose.", "--option", "small=Small",
		},
		"invalid option syntax": {
			"choice", "--id", "size", "--version", "v1", "--purpose", "size@v1", "--question", "Choose.", "--option", "small", "--option", "large=Large",
		},
		"duplicate context flag": append(validDecisionRTChoiceArgs(), "--context", "key=one", "--context", "key=two"),
		"context object":         append(validDecisionRTChoiceArgs(), "--context", `key={"nested":true}`),
		"context null":           append(validDecisionRTChoiceArgs(), "--context", "key=null"),
		"duplicate context JSON": append(validDecisionRTChoiceArgs(), "--context-json", `{"key":1,"key":2}`),
		"empty context JSON":     append(validDecisionRTChoiceArgs(), "--context-json", ""),
		"empty context file":     append(validDecisionRTChoiceArgs(), "--context-file", ""),
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newDecisionRTCommandFixture(decidedDecisionRTBackend("rule", decisionrt.Value{Choice: "small"}))
			err := fixture.execute(args...)
			assertDecisionRTExitCode(t, err, 2)
			if fixture.stdout.Len() != 0 || fixture.stderr.Len() == 0 {
				t.Fatalf("stdout=%q stderr=%q", fixture.stdout.String(), fixture.stderr.String())
			}
		})
	}
}

func TestDecisionRTBooleanTrueAndFalse(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(fmt.Sprintf("%t", selected), func(t *testing.T) {
			value := selected
			backend := decidedDecisionRTBackend("sidecar", decisionrt.Value{Boolean: &value})
			fixture := newDecisionRTCommandFixture(backend)
			err := fixture.execute("boolean", "--id", "review", "--version", "v1", "--purpose", "review@v1", "--question", "Review?", "--backend", "sidecar")
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(fixture.stdout.String()) != fmt.Sprintf("%t", selected) || fixture.stderr.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q", fixture.stdout.String(), fixture.stderr.String())
			}
		})
	}

	fixture := newDecisionRTCommandFixture(decidedDecisionRTBackend("sidecar", decisionrt.Value{Boolean: new(true)}))
	err := fixture.execute("boolean", "--option", "yes=Yes")
	assertDecisionRTExitCode(t, err, 2)
}

func TestDecisionRTIntegerValidationAndOutput(t *testing.T) {
	value := int64(0)
	fixture := newDecisionRTCommandFixture(decidedDecisionRTBackend("sidecar", decisionrt.Value{Integer: &value}))
	err := fixture.execute("integer", "--id", "score", "--version", "v1", "--purpose", "score@v1", "--question", "Score?", "--min", "-1", "--max", "1", "--backend", "sidecar")
	if err != nil || strings.TrimSpace(fixture.stdout.String()) != "0" {
		t.Fatalf("err=%v stdout=%q", err, fixture.stdout.String())
	}

	tests := map[string][]string{
		"missing min":          {"integer", "--id", "score", "--version", "v1", "--purpose", "score@v1", "--question", "Score?", "--max", "1"},
		"min greater than max": {"integer", "--id", "score", "--version", "v1", "--purpose", "score@v1", "--question", "Score?", "--min", "2", "--max", "1"},
		"range too wide":       {"integer", "--id", "score", "--version", "v1", "--purpose", "score@v1", "--question", "Score?", "--min", "0", "--max", "21"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newDecisionRTCommandFixture(decidedDecisionRTBackend("rule", decisionrt.Value{Integer: new(int64(0))}))
			assertDecisionRTExitCode(t, fixture.execute(args...), 2)
		})
	}

	outOfRange := int64(2)
	fixture = newDecisionRTCommandFixture(decidedDecisionRTBackend("sidecar", decisionrt.Value{Integer: &outOfRange}))
	err = fixture.execute("integer", "--id", "score", "--version", "v1", "--purpose", "score@v1", "--question", "Score?", "--min", "-1", "--max", "1", "--backend", "sidecar", "--no-fallback")
	assertDecisionRTExitCode(t, err, 4)
}

func TestDecisionRTRunInputModesAndStrictJSON(t *testing.T) {
	requestJSON := validDecisionRTRequestJSON()
	requestFile := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestFile, []byte(requestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		args     []string
		stdin    string
		terminal bool
	}{
		{name: "file", args: []string{"run", "--file", requestFile, "--backend", "sidecar"}, terminal: true},
		{name: "explicit stdin", args: []string{"run", "--stdin", "--backend", "sidecar"}, stdin: requestJSON, terminal: true},
		{name: "implicit stdin", args: []string{"run", "--backend", "sidecar"}, stdin: requestJSON, terminal: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDecisionRTCommandFixture(decidedDecisionRTBackend("sidecar", decisionrt.Value{Choice: "small"}))
			fixture.stdin = strings.NewReader(test.stdin)
			fixture.terminalStdin = test.terminal
			if err := fixture.execute(test.args...); err != nil {
				t.Fatal(err)
			}
		})
	}

	invalid := []struct {
		name     string
		args     []string
		stdin    string
		terminal bool
	}{
		{name: "missing tty input", args: []string{"run"}, terminal: true},
		{name: "empty file path", args: []string{"run", "--file", ""}, terminal: false, stdin: requestJSON},
		{name: "file and stdin", args: []string{"run", "--file", requestFile, "--stdin"}, stdin: requestJSON},
		{name: "malformed", args: []string{"run", "--stdin"}, stdin: `{"purpose":`},
		{name: "duplicate key", args: []string{"run", "--stdin"}, stdin: strings.Replace(requestJSON, `"purpose":"size@v1"`, `"purpose":"size@v1","purpose":"other"`, 1)},
		{name: "trailing value", args: []string{"run", "--stdin"}, stdin: requestJSON + `{}`},
		{name: "unknown kind", args: []string{"run", "--stdin"}, stdin: strings.Replace(requestJSON, `"kind":"choice"`, `"kind":"unknown"`, 1)},
		{name: "unknown field", args: []string{"run", "--stdin"}, stdin: strings.Replace(requestJSON, `"purpose":"size@v1"`, `"purpose":"size@v1","extra":true`, 1)},
		{name: "over limit", args: []string{"run", "--stdin"}, stdin: strings.Repeat("x", decisionRTMaximumInputBytes+1)},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDecisionRTCommandFixture(decidedDecisionRTBackend("rule", decisionrt.Value{Choice: "small"}))
			fixture.stdin = strings.NewReader(test.stdin)
			fixture.terminalStdin = test.terminal
			assertDecisionRTExitCode(t, fixture.execute(test.args...), 2)
		})
	}
}

func TestDecisionRTValidateDoesNotConstructRegistry(t *testing.T) {
	requestFile := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestFile, []byte(validDecisionRTRequestJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newDecisionRTCommandFixture(nil)
	if err := fixture.execute("validate", "--file", requestFile, "--json"); err != nil {
		t.Fatal(err)
	}
	if fixture.factoryCalls != 0 {
		t.Fatalf("registry factory calls = %d", fixture.factoryCalls)
	}
	var output decisionRTValidationOutput
	if err := json.Unmarshal(fixture.stdout.Bytes(), &output); err != nil || !output.Valid || !strings.HasPrefix(output.RequestDigest, "sha256:") {
		t.Fatalf("output=%#v err=%v", output, err)
	}

	fixture = newDecisionRTCommandFixture(nil)
	fixture.stdin = strings.NewReader(`{"purpose":`)
	assertDecisionRTExitCode(t, fixture.execute("validate", "--stdin"), 2)
	if fixture.factoryCalls != 0 {
		t.Fatalf("invalid validate registry factory calls = %d", fixture.factoryCalls)
	}
}

func TestDecisionRTBackendsStableOutputWithoutResolve(t *testing.T) {
	fixture := newDecisionRTCommandFixture(nil)
	fixture.registry.infos = []BackendInfo{
		{Name: "rule", Available: true, Type: "deterministic"},
		{Name: "sidecar", Available: false, Type: "generative", Reason: "missing_sidecar_model"},
	}
	if err := fixture.execute("backends", "--json"); err != nil {
		t.Fatal(err)
	}
	if fixture.registry.resolveCalls != 0 || fixture.factoryCalls != 1 {
		t.Fatalf("factory=%d resolve=%d", fixture.factoryCalls, fixture.registry.resolveCalls)
	}
	var output decisionRTBackendsOutput
	if err := json.Unmarshal(fixture.stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(output.Backends, fixture.registry.infos) {
		t.Fatalf("backends = %#v", output.Backends)
	}
}

func TestDecisionRTReceiptAlwaysUsesExactEnvelope(t *testing.T) {
	fixture := newDecisionRTCommandFixture(decidedDecisionRTBackend("sidecar", decisionrt.Value{Choice: "small"}))
	err := fixture.execute(append(validDecisionRTChoiceArgs(), "--backend", "sidecar", "--receipt")...)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(fixture.stdout.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 || raw["result"] == nil || raw["receipt"] == nil {
		t.Fatalf("envelope = %#v", raw)
	}
	var receipt decisionrt.Receipt
	if err := json.Unmarshal(raw["receipt"], &receipt); err != nil || receipt.SchemaVersion != 1 || !strings.HasPrefix(receipt.RequestDigest, "sha256:") {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

func TestDecisionRTExitCodesFallbackAndSanitizedStreams(t *testing.T) {
	backendFailure := &decisionrt.RuntimeError{Kind: decisionrt.ErrorBackendFailure, Err: errors.New("secret-provider-error")}
	tests := []struct {
		name       string
		backend    decisionrt.Backend
		resolveErr error
		args       []string
		wantCode   int
		wantStdout bool
		wantStderr bool
	}{
		{name: "decided", backend: decidedDecisionRTBackend("sidecar", decisionrt.Value{Choice: "small"}), args: append(validDecisionRTChoiceArgs(), "--backend", "sidecar"), wantCode: 0, wantStdout: true},
		{name: "invalid", backend: decidedDecisionRTBackend("rule", decisionrt.Value{Choice: "small"}), args: []string{"choice"}, wantCode: 2, wantStderr: true},
		{name: "abstained", backend: abstainedDecisionRTBackend("rule"), args: validDecisionRTChoiceArgs(), wantCode: 3, wantStdout: true},
		{name: "failure no fallback", backend: errorDecisionRTBackend("sidecar", backendFailure), args: append(validDecisionRTChoiceArgs(), "--backend", "sidecar", "--no-fallback"), wantCode: 4, wantStderr: true},
		{name: "unavailable", resolveErr: &decisionrt.RuntimeError{Kind: decisionrt.ErrorBackendUnavailable, Err: errors.New("secret-provider-error")}, args: append(validDecisionRTChoiceArgs(), "--backend", "unknown"), wantCode: 5, wantStderr: true},
		{name: "failure falls back", backend: errorDecisionRTBackend("sidecar", backendFailure), args: append(validDecisionRTChoiceArgs(), "--backend", "sidecar"), wantCode: 3, wantStdout: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDecisionRTCommandFixture(test.backend)
			fixture.registry.resolveError = test.resolveErr
			fixture.environment["HUFU_PROVIDER_API_KEY"] = "top-secret-api-key"
			err := fixture.execute(test.args...)
			assertDecisionRTExitCode(t, err, test.wantCode)
			if (fixture.stdout.Len() > 0) != test.wantStdout || (fixture.stderr.Len() > 0) != test.wantStderr {
				t.Fatalf("stdout=%q stderr=%q", fixture.stdout.String(), fixture.stderr.String())
			}
			combined := fixture.stdout.String() + fixture.stderr.String()
			if strings.Contains(combined, "top-secret-api-key") || strings.Contains(combined, "secret-provider-error") {
				t.Fatalf("secret leaked: %q", combined)
			}
		})
	}
}

func TestDecisionRTOutputFailureIsTechnicalError(t *testing.T) {
	var stderr bytes.Buffer
	registry := &decisionRTFakeRegistry{backend: decidedDecisionRTBackend("sidecar", decisionrt.Value{Choice: "small"})}
	deps := decisionRTDeps{
		registryFactory: func(RegistryOptions) BackendRegistry { return registry },
		stdin:           strings.NewReader(""), stdout: failingDecisionRTWriter{}, stderr: &stderr,
		stdinIsTerminal: func() bool { return true }, getenv: func(string) string { return "" },
	}
	command := newDecisionRTCommand(deps)
	command.SetArgs(append(validDecisionRTChoiceArgs(), "--backend", "sidecar"))
	assertDecisionRTExitCode(t, command.Execute(), 4)
	if stderr.Len() == 0 {
		t.Fatal("expected sanitized stderr diagnostic")
	}
}

func TestDecisionRTDebugOutputDoesNotContaminateStdout(t *testing.T) {
	fixture := newDecisionRTCommandFixture(nil)
	fixture.registry.backend = decisionRTFakeBackend{name: "sidecar", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		_, _ = fmt.Fprintln(&fixture.stderr, "debug: fake backend")
		return decidedDecisionRTChoice("small"), nil
	}}
	if err := fixture.execute(append(validDecisionRTChoiceArgs(), "--backend", "sidecar")...); err != nil {
		t.Fatal(err)
	}
	if fixture.stdout.String() != "small\n" || !strings.Contains(fixture.stderr.String(), "debug: fake backend") {
		t.Fatalf("stdout=%q stderr=%q", fixture.stdout.String(), fixture.stderr.String())
	}
}

func TestDecisionRTSidecarMissingModelDoesNotFallback(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := decisionRTDeps{
		registryFactory: NewDefaultRegistry,
		stdin:           strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		stdinIsTerminal: func() bool { return true }, getenv: func(string) string { return "" },
	}
	command := newDecisionRTCommand(deps)
	command.SetArgs(append(validDecisionRTChoiceArgs(), "--backend", "sidecar"))
	assertDecisionRTExitCode(t, command.Execute(), 5)
	if stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDecisionRTInheritedFlagsSubcommandsAndHelp(t *testing.T) {
	for _, flag := range []string{"workspace", "decision-profile", "profile", "execution-profile", "goal-mode", "no-color"} {
		t.Run(flag, func(t *testing.T) {
			fixture := newDecisionRTCommandFixture(abstainedDecisionRTBackend("rule"))
			root := &cobra.Command{Use: "hufu", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().String("workspace", "", "")
			root.PersistentFlags().String("decision-profile", "", "")
			root.PersistentFlags().String("profile", "", "")
			root.PersistentFlags().String("execution-profile", "", "")
			root.PersistentFlags().String("goal-mode", "", "")
			root.PersistentFlags().Bool("no-color", false, "")
			root.AddCommand(newDecisionRTCommand(fixture.deps()))
			args := append([]string{"decisionrt"}, validDecisionRTChoiceArgs()...)
			if flag == "no-color" {
				args = append(args, "--no-color")
			} else {
				args = append(args, "--"+flag, "value")
			}
			root.SetArgs(args)
			assertDecisionRTExitCode(t, root.Execute(), 2)
		})
	}

	fixture := newDecisionRTCommandFixture(nil)
	assertDecisionRTExitCode(t, fixture.execute(), 2)
	fixture = newDecisionRTCommandFixture(nil)
	assertDecisionRTExitCode(t, fixture.execute("unknown"), 2)
	fixture = newDecisionRTCommandFixture(nil)
	if err := fixture.execute("--help"); err != nil {
		t.Fatalf("help: %v", err)
	}
}

func TestDefaultDecisionRTRegistryAvailability(t *testing.T) {
	tests := []struct {
		options RegistryOptions
		reason  string
	}{
		{options: RegistryOptions{ProviderURL: decisionRTDefaultProviderURL}, reason: "missing_sidecar_model"},
		{options: RegistryOptions{SidecarModel: " bad", ProviderURL: "bad"}, reason: "invalid_sidecar_model"},
		{options: RegistryOptions{SidecarModel: "model", ProviderURL: "ftp://host"}, reason: "invalid_provider_url"},
		{options: RegistryOptions{SidecarModel: "model", ProviderURL: decisionRTDefaultProviderURL}},
	}
	for _, test := range tests {
		infos := NewDefaultRegistry(test.options).List()
		if len(infos) != 2 || infos[0].Name != "rule" || infos[1].Name != "sidecar" || infos[1].Reason != test.reason || infos[1].Available != (test.reason == "") {
			t.Fatalf("options=%#v infos=%#v", test.options, infos)
		}
	}
	for _, name := range []string{"sidecar", "unknown"} {
		_, err := NewDefaultRegistry(RegistryOptions{ProviderURL: decisionRTDefaultProviderURL}).Resolve(t.Context(), name)
		if typed, ok := errors.AsType[*decisionrt.RuntimeError](err); !ok || typed.Kind != decisionrt.ErrorBackendUnavailable {
			t.Fatalf("Resolve(%q) error = %#v", name, err)
		}
	}
}

func TestDecisionRTProcessExitCodes(t *testing.T) {
	requestFile := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestFile, []byte(validDecisionRTRequestJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "decided validate", args: []string{"decisionrt", "validate", "--file", requestFile}, code: 0},
		{name: "invalid", args: []string{"decisionrt", "choice"}, code: 2},
		{name: "abstained", args: append([]string{"decisionrt"}, validDecisionRTChoiceArgs()...), code: 3},
		{name: "backend failure", args: append(append([]string{"decisionrt"}, validDecisionRTChoiceArgs()...), "--backend", "sidecar", "--sidecar-model", "model", "--provider-url", "http://127.0.0.1:1/v1", "--no-fallback"), code: 4},
		{name: "unavailable", args: append(append([]string{"decisionrt"}, validDecisionRTChoiceArgs()...), "--backend", "unknown"), code: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.args)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestDecisionRTProcessHelper")
			command.Env = append(os.Environ(), "HUFU_DECISIONRT_PROCESS_HELPER="+string(encoded))
			output, err := command.CombinedOutput()
			code := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatalf("run helper: %v", err)
				}
				code = exitError.ExitCode()
			}
			if code != test.code {
				t.Fatalf("exit=%d want=%d output=%q", code, test.code, output)
			}
		})
	}
}

func TestDecisionRTProcessHelper(t *testing.T) {
	raw := os.Getenv("HUFU_DECISIONRT_PROCESS_HELPER")
	if raw == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		panic(err)
	}
	os.Args = append([]string{"hufu"}, args...)
	main()
}

func TestDecisionRTIgnoresInvalidTeamAndHufuConfig(t *testing.T) {
	workingDirectory := t.TempDir()
	for _, name := range []string{"hufu.yaml", "team.yaml"} {
		if err := os.WriteFile(filepath.Join(workingDirectory, name), []byte(": invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := append([]string{"decisionrt"}, validDecisionRTChoiceArgs()...)
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestDecisionRTProcessHelper")
	command.Dir = workingDirectory
	command.Env = append(os.Environ(), "HUFU_DECISIONRT_PROCESS_HELPER="+string(encoded))
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 3 {
		t.Fatalf("err=%v output=%q", err, output)
	}
}

func validDecisionRTChoiceArgs() []string {
	return []string{
		"choice", "--id", "size", "--version", "v1", "--purpose", "size@v1", "--question", "Choose.",
		"--option", "small=Small", "--option", "large=Large",
	}
}

func validDecisionRTRequestJSON() string {
	return `{"purpose":"size@v1","spec":{"id":"size","version":"v1","kind":"choice","question":"Choose.","options":[{"id":"small","description":"Small"},{"id":"large","description":"Large"}]},"context":{"workload":"small"}}`
}

func decidedDecisionRTChoice(choice string) decisionrt.BackendResult {
	return decisionrt.BackendResult{
		Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: choice}, ConfidenceSemantics: decisionrt.ConfidenceNone, Model: "test-model",
	}
}

func decidedDecisionRTBackend(name string, value decisionrt.Value) decisionrt.Backend {
	return decisionRTFakeBackend{name: name, decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		return decisionrt.BackendResult{Status: decisionrt.StatusDecided, Value: value, ConfidenceSemantics: decisionrt.ConfidenceNone}, nil
	}}
}

func abstainedDecisionRTBackend(name string) decisionrt.Backend {
	return decisionRTFakeBackend{name: name, decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		return decisionrt.BackendResult{Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone}, nil
	}}
}

func errorDecisionRTBackend(name string, err error) decisionrt.Backend {
	return decisionRTFakeBackend{name: name, decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		return decisionrt.BackendResult{}, err
	}}
}

func assertDecisionRTExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if want == 0 {
		if err != nil {
			t.Fatalf("err = %v, want success", err)
		}
		return
	}
	var exitError interface{ ProcessExitCode() int }
	if !errors.As(err, &exitError) || exitError.ProcessExitCode() != want {
		t.Fatalf("err = %#v, want exit code %d", err, want)
	}
}

type failingDecisionRTWriter struct{}

func (failingDecisionRTWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
