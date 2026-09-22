package sidecar_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	decisionbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	hufusidecar "github.com/kjelly/hufu/internal/sidecar"
)

var _ decisionbackend.Generator = (*hufusidecar.Sidecar)(nil)

type fakeGenerator struct {
	modelID  string
	response string
	err      error
	execute  func(context.Context, string) (string, error)
}

func (g *fakeGenerator) Execute(ctx context.Context, prompt string) (string, error) {
	if g.execute != nil {
		return g.execute(ctx, prompt)
	}
	return g.response, g.err
}

func (g *fakeGenerator) ModelID() string { return g.modelID }

func TestChoiceTokenMapsToTypedValue(t *testing.T) {
	generator := &fakeGenerator{modelID: "test-model", response: `{"token":"A1"}`}
	backend := mustBackend(t, generator)
	result, err := backend.Decide(t.Context(), choiceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != decisionrt.StatusDecided || result.Value.Choice != "large" || result.Model != "test-model" {
		t.Fatalf("result = %#v", result)
	}
	if result.Confidence != 0 || result.ConfidenceSemantics != decisionrt.ConfidenceNone || len(result.Candidates) != 0 {
		t.Fatalf("unexpected confidence data: %#v", result)
	}
}

func TestPromptUsesEscapedBoundedInput(t *testing.T) {
	request := choiceRequest()
	request.Spec.Question = "Choose \"carefully\".\nNow."
	request.Spec.Options[0].Description = "Line one\nline two"
	request.Context = map[string]any{"z": json.Number("3.0"), "a": "quoted \"value\""}
	generator := &fakeGenerator{modelID: "test-model"}
	generator.execute = func(_ context.Context, prompt string) (string, error) {
		const prefix = "Choose exactly one candidate token from this JSON input.\nInput:\n"
		const suffix = "\n\nReturn only one JSON object. TOKEN must be replaced by exactly one candidate token:\n{\"token\":\"TOKEN\"}"
		if !strings.HasPrefix(prompt, prefix) || !strings.HasSuffix(prompt, suffix) {
			t.Fatalf("unexpected prompt framing: %q", prompt)
		}
		encoded := strings.TrimSuffix(strings.TrimPrefix(prompt, prefix), suffix)
		var input struct {
			Purpose     string         `json:"purpose"`
			SpecID      string         `json:"spec_id"`
			SpecVersion string         `json:"spec_version"`
			Question    string         `json:"question"`
			Context     map[string]any `json:"context"`
			Candidates  []struct {
				Token       string `json:"token"`
				Value       string `json:"value"`
				Description string `json:"description"`
			} `json:"candidates"`
		}
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&input); err != nil {
			t.Fatalf("decode prompt input: %v", err)
		}
		if input.Purpose != request.Purpose || input.SpecID != request.Spec.ID || input.SpecVersion != request.Spec.Version || input.Question != request.Spec.Question {
			t.Fatalf("input = %#v", input)
		}
		if input.Candidates[0].Token != "A0" || input.Candidates[0].Value != "small" || input.Candidates[0].Description != request.Spec.Options[0].Description {
			t.Fatalf("candidates = %#v", input.Candidates)
		}
		return `{"token":"A0"}`, nil
	}
	backend := mustBackend(t, generator)
	if _, err := backend.Decide(t.Context(), request); err != nil {
		t.Fatal(err)
	}
}

func TestPromptCanonicalizesEquivalentContextNumbers(t *testing.T) {
	contextValues := []any{int(3), int64(3), float64(3), json.Number("3.0")}
	prompts := make([]string, 0, len(contextValues))
	for _, contextValue := range contextValues {
		generator := &fakeGenerator{modelID: "test-model"}
		generator.execute = func(_ context.Context, prompt string) (string, error) {
			prompts = append(prompts, prompt)
			return `{"token":"A0"}`, nil
		}
		request := choiceRequest()
		request.Context = map[string]any{"number": contextValue}
		backend := mustBackend(t, generator)
		if _, err := backend.Decide(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	for index := 1; index < len(prompts); index++ {
		if prompts[index] != prompts[0] {
			t.Fatalf("prompt %d differs for equivalent number", index)
		}
	}
}

func TestBooleanAndIntegerMappings(t *testing.T) {
	tests := []struct {
		name     string
		request  decisionrt.Request
		response string
		check    func(decisionrt.Value) bool
	}{
		{
			name: "boolean false", request: booleanRequest(), response: `{"token":"A0"}`,
			check: func(value decisionrt.Value) bool { return value.Boolean != nil && !*value.Boolean },
		},
		{
			name: "boolean true", request: booleanRequest(), response: `{"token":"A1"}`,
			check: func(value decisionrt.Value) bool { return value.Boolean != nil && *value.Boolean },
		},
		{
			name: "integer", request: integerRequest(), response: `{"token":"A2"}`,
			check: func(value decisionrt.Value) bool { return value.Integer != nil && *value.Integer == math.MinInt64+2 },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := mustBackend(t, &fakeGenerator{modelID: "test-model", response: test.response})
			result, err := backend.Decide(t.Context(), test.request)
			if err != nil || !test.check(result.Value) {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
		})
	}
}

func TestStrictResponseParsing(t *testing.T) {
	tests := map[string]string{
		"unknown token":   `{"token":"small"}`,
		"prose prefix":    `answer: {"token":"A0"}`,
		"prose suffix":    `{"token":"A0"} done`,
		"malformed":       `{"token":`,
		"duplicate key":   `{"token":"A0","token":"A1"}`,
		"unknown key":     `{"token":"A0","reason":"x"}`,
		"trailing object": `{"token":"A0"}{"token":"A1"}`,
		"markdown fence":  "```json\n{\"token\":\"A0\"}\n```",
		"empty":           "",
		"non-string":      `{"token":0}`,
		"missing token":   `{}`,
		"empty token":     `{"token":""}`,
		"oversized":       strings.Repeat("x", 4097),
		"invalid UTF-8":   string([]byte{0xff}),
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			backend := mustBackend(t, &fakeGenerator{modelID: "test-model", response: response})
			_, err := backend.Decide(t.Context(), choiceRequest())
			assertErrorKind(t, err, decisionrt.ErrorInvalidBackendOutput)
		})
	}
}

func TestGeneratorFailureAndCancellation(t *testing.T) {
	sentinel := errors.New("generator unavailable")
	backend := mustBackend(t, &fakeGenerator{modelID: "test-model", err: sentinel})
	_, err := backend.Decide(t.Context(), choiceRequest())
	assertErrorKind(t, err, decisionrt.ErrorBackendFailure)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped generator error", err)
	}

	generator := &fakeGenerator{modelID: "test-model"}
	generator.execute = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	backend = mustBackend(t, generator)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = backend.Decide(ctx, choiceRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
}

func TestPromptLimitDoesNotInvokeGenerator(t *testing.T) {
	var calls int
	generator := &fakeGenerator{modelID: "test-model", execute: func(context.Context, string) (string, error) {
		calls++
		return `{"token":"A0"}`, nil
	}}
	request := choiceRequest()
	request.Spec.Question = strings.Repeat("q", 4096)
	for index := range 64 {
		request.Context["key"+strings.Repeat("x", index)] = strings.Repeat("界", 60)
	}
	backend := mustBackend(t, generator)
	_, err := backend.Decide(t.Context(), request)
	assertErrorKind(t, err, decisionrt.ErrorBackendFailure)
	if calls != 0 {
		t.Fatalf("generator calls = %d", calls)
	}
}

func TestNewRejectsInvalidGeneratorOrModel(t *testing.T) {
	var nilGenerator *fakeGenerator
	invalid := []decisionbackend.Generator{
		nil,
		nilGenerator,
		&fakeGenerator{modelID: ""},
		&fakeGenerator{modelID: " model"},
		&fakeGenerator{modelID: "model "},
		&fakeGenerator{modelID: strings.Repeat("x", 257)},
		&fakeGenerator{modelID: string([]byte{0xff})},
		&fakeGenerator{modelID: "model\nname"},
	}
	for _, generator := range invalid {
		backend, err := decisionbackend.New(generator)
		assertErrorKind(t, err, decisionrt.ErrorConfiguration)
		if backend != nil {
			t.Fatalf("backend = %#v", backend)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	backend := mustBackend(t, &fakeGenerator{modelID: "test-model", response: `{"token":"A0"}`})
	errorsChannel := make(chan error, 32)
	var wait sync.WaitGroup
	for range 32 {
		wait.Go(func() {
			result, err := backend.Decide(t.Context(), choiceRequest())
			if err == nil && result.Value.Choice != "small" {
				err = errors.New("unexpected decision")
			}
			errorsChannel <- err
		})
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func mustBackend(t *testing.T, generator decisionbackend.Generator) decisionrt.Backend {
	t.Helper()
	backend, err := decisionbackend.New(generator)
	if err != nil {
		t.Fatal(err)
	}
	if backend.Name() != "sidecar" {
		t.Fatalf("name = %q", backend.Name())
	}
	return backend
}

func choiceRequest() decisionrt.Request {
	return decisionrt.Request{
		Purpose: "size-policy@v1",
		Spec: decisionrt.Spec{
			ID: "size-policy", Version: "v1", Kind: decisionrt.KindChoice, Question: "Select a size.",
			Options: []decisionrt.Option{{ID: "small", Description: "Small"}, {ID: "large", Description: "Large"}},
		},
		Context: map[string]any{},
	}
}

func booleanRequest() decisionrt.Request {
	return decisionrt.Request{
		Purpose: "boolean-policy@v1",
		Spec:    decisionrt.Spec{ID: "boolean-policy", Version: "v1", Kind: decisionrt.KindBoolean, Question: "Enable?"},
	}
}

func integerRequest() decisionrt.Request {
	return decisionrt.Request{
		Purpose: "integer-policy@v1",
		Spec: decisionrt.Spec{
			ID: "integer-policy", Version: "v1", Kind: decisionrt.KindIntegerRange, Question: "Select an integer.",
			Range: &decisionrt.IntegerRange{Min: math.MinInt64, Max: math.MinInt64 + 2},
		},
	}
}

func assertErrorKind(t *testing.T, err error, want decisionrt.ErrorKind) {
	t.Helper()
	typed, ok := errors.AsType[*decisionrt.RuntimeError](err)
	if !ok || typed.Kind != want {
		t.Fatalf("error = %#v, want kind %s", err, want)
	}
}
