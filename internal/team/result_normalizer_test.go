package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type mockNormalizerProvider struct {
	fn func(ctx context.Context, action Action) (any, error)
}

func (m *mockNormalizerProvider) Validate(Action) error { return nil }
func (m *mockNormalizerProvider) Execute(ctx context.Context, action Action) (any, error) {
	if m.fn != nil {
		return m.fn(ctx, action)
	}
	return action.Payload, nil
}

func TestAgentFrontmatterParsesNormalizer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeAgentFile(t, dir, "reviewer.md", "name: reviewer\nresult-contract:\n  schema: schemas/review.json\n  require-structured: true\n  normalizer: test-normalizer", "Review.")
	def, err := parseAgentFile(filepath.Join(dir, "reviewer.md"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if def.ResultContract == nil || def.ResultContract.Normalizer != "test-normalizer" {
		t.Fatalf("expected Normalizer 'test-normalizer', got %#v", def.ResultContract)
	}
}

func TestResultContractCompileBindsNormalizer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "schemas/review.json", reviewResultSchema)
	writeAgentFile(t, dir, "worker.md", "name: worker\nresult-contract:\n  schema: schemas/review.json\n  require-structured: true\n  normalizer: ast-snap", "Worker.")
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := session.AgentResultContracts["worker"]
	if !ok || ref.Normalizer != "ast-snap" {
		t.Fatalf("expected compiled contract ref to have normalizer 'ast-snap', got %#v", ref)
	}
}

func TestNormalizerPipelineDualReceiptAndSchemaConvergence(t *testing.T) {
	t.Parallel()
	c, item := resultContractCoordinator(t, true)
	item.ResultContract.Normalizer = "clean-normalizer"

	// Register normalizer in ProviderRegistry
	c.session.ProviderRegistry = NewProviderRegistry()
	c.session.ProviderRegistry.Register("clean-normalizer", &mockNormalizerProvider{
		fn: func(ctx context.Context, action Action) (any, error) {
			var raw struct {
				Verdict  string `json:"verdict"`
				Findings []struct {
					Summary string `json:"summary"`
				} `json:"findings"`
			}
			if err := json.Unmarshal([]byte(action.Payload), &raw); err != nil {
				return nil, err
			}
			for i := range raw.Findings {
				raw.Findings[i].Summary = strings.TrimSpace(raw.Findings[i].Summary)
			}
			normalized, err := json.Marshal(raw)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"outputs": map[string]any{
					"normalized_payload": string(normalized),
				},
			}, nil
		},
	})

	rawPayloadJSON := `{"findings":[{"summary":"  needs formatting  "}],"verdict":"approve"}`
	sum := sha256.Sum256([]byte(rawPayloadJSON))
	wantRawSHA256 := hex.EncodeToString(sum[:])

	rawSubmission := `{"status":"success","summary":"reviewed","structured_payload":` + rawPayloadJSON + `}`

	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	resp, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name:  "submit_result",
		Input: rawSubmission,
	})
	if err != nil || resp.IsError {
		t.Fatalf("submit_result failed unexpectedly: resp=%#v, err=%v", resp, err)
	}

	stored := c.GetTaskResult(item.ID)
	if stored == nil || stored.StructuredPayload == nil {
		t.Fatalf("stored result missing structured payload: %#v", stored)
	}
	wantCanonicalJSON := `{"findings":[{"summary":"needs formatting"}],"verdict":"approve"}`
	if string(stored.StructuredPayload.Value) != wantCanonicalJSON {
		t.Fatalf("payload value was not normalized: %s, want %s", string(stored.StructuredPayload.Value), wantCanonicalJSON)
	}
	if stored.StructuredPayload.RawSHA256 != wantRawSHA256 {
		t.Fatalf("RawSHA256 = %s, want %s", stored.StructuredPayload.RawSHA256, wantRawSHA256)
	}

	normSum := sha256.Sum256([]byte(wantCanonicalJSON))
	wantNormSHA256 := hex.EncodeToString(normSum[:])
	if stored.StructuredPayload.SHA256 != wantNormSHA256 {
		t.Fatalf("SHA256 = %s, want %s", stored.StructuredPayload.SHA256, wantNormSHA256)
	}

	// Verify ExecutionReceipt records dual hashes
	receipt := &ExecutionReceipt{}
	c.applyResultContractReceipt(item.ID, stored, receipt)
	if receipt.ResultPayloadSHA256 != wantNormSHA256 {
		t.Fatalf("receipt ResultPayloadSHA256 = %s, want %s", receipt.ResultPayloadSHA256, wantNormSHA256)
	}
	if receipt.ResultRawPayloadSHA256 != wantRawSHA256 {
		t.Fatalf("receipt ResultRawPayloadSHA256 = %s, want %s", receipt.ResultRawPayloadSHA256, wantRawSHA256)
	}
	if receipt.ResultValidation != ResultValidationValid {
		t.Fatalf("receipt ResultValidation = %s, want %s", receipt.ResultValidation, ResultValidationValid)
	}
}

func TestNormalizerPipelineActionFailureFailsClosedWithoutRepair(t *testing.T) {
	t.Parallel()
	c, item := resultContractCoordinator(t, true)
	item.ResultContract.Normalizer = "broken-normalizer"

	c.session.ProviderRegistry = NewProviderRegistry()
	c.session.ProviderRegistry.Register("broken-normalizer", &mockNormalizerProvider{
		fn: func(ctx context.Context, action Action) (any, error) {
			return nil, errors.New("normalizer crashed: out of memory")
		},
	})

	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	resp, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"approve"}}`,
	})
	if err != nil {
		t.Fatalf("unexpected tool runner error: %v", err)
	}
	if !resp.IsError {
		t.Fatal("expected tool error response for broken normalizer, got success")
	}
	if !strings.Contains(resp.Content, "normalizer \"broken-normalizer\" failed") {
		t.Fatalf("expected error message mentioning normalizer failure, got: %s", resp.Content)
	}

	// Verify validateStructuredPayloadClaims returns repairable == false
	_, _, repairable := c.validateStructuredPayloadClaims(item.ID, []byte(`{"verdict":"approve"}`), false)
	if repairable {
		t.Fatal("normalizer action error must not be marked repairable (would waste model turns)")
	}
}

func TestNormalizerPipelineMissingProviderFailsClosed(t *testing.T) {
	t.Parallel()
	c, item := resultContractCoordinator(t, true)
	item.ResultContract.Normalizer = "unregistered-normalizer"
	c.session.ProviderRegistry = NewProviderRegistry()

	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	resp, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name:  "submit_result",
		Input: `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"approve"}}`,
	})
	if err != nil {
		t.Fatalf("unexpected runner error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "is not registered") {
		t.Fatalf("expected error mentioning unregistered provider, got: %#v", resp)
	}
}
