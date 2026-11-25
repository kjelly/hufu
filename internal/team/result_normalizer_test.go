package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/runtimestore"
)

type mockNormalizerProvider struct {
	fn func(context.Context, []byte) ([]byte, error)
}

type normalizationTestResultSink func()

func (sink normalizationTestResultSink) Submit(context.Context, string, TaskResult) error {
	sink()
	return nil
}

func (m *mockNormalizerProvider) Validate(Action) error { return nil }
func (m *mockNormalizerProvider) Execute(context.Context, Action) (any, error) {
	return nil, errors.New("unexpected ordinary action execution")
}
func (m *mockNormalizerProvider) normalizeResult(ctx context.Context, raw []byte) ([]byte, error) {
	return m.fn(ctx, raw)
}

func bindTestNormalizer(t *testing.T, c *Coordinator, item *TodoItem, provider *mockNormalizerProvider) {
	t.Helper()
	if c.session.Workspace == "" {
		c.session.Workspace = t.TempDir()
	}
	item.ResultContract.Normalizer, item.ResultContract.NormalizerSHA256 = registerTestNormalizer(t, c, provider)
}

func registerTestNormalizer(t *testing.T, c *Coordinator, provider *mockNormalizerProvider) (string, string) {
	t.Helper()
	if !golangruntime.NormalizerSupported() {
		t.Skip("bounded normalizer runtime requires Linux")
	}
	const capability = "normalize-result"
	c.session.Config.ActionProviders = map[string]agent.ActionProviderConfig{
		capability: {Runtime: "golang", Source: "./normalize", Mode: golangruntime.TrustedStaticMode},
	}
	c.session.ProviderRegistry = NewProviderRegistry()
	c.session.ProviderRegistry.Register(capability, provider)
	return capability, actionProviderIdentityHash(c.session, capability)
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
	if _, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
		t.Fatal("unregistered normalizer was accepted")
	}
	if !golangruntime.NormalizerSupported() {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("action-providers:\n  ast-snap:\n    runtime: golang\n    source: ./normalize\n    mode: trusted-static\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "normalize")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\nimport (\"context\"; \"io\")\nfunc Run(_ context.Context, in io.Reader, out io.Writer) error { _, err := io.Copy(out, in); return err }"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := session.AgentResultContracts["worker"]
	if !ok || ref.Normalizer != "ast-snap" || ref.NormalizerSHA256 == "" {
		t.Fatalf("expected compiled contract ref to have normalizer 'ast-snap', got %#v", ref)
	}
}

func TestNormalizerPipelineDualReceiptAndSchemaConvergence(t *testing.T) {
	t.Parallel()
	c, item := resultContractCoordinator(t, true)
	calls := 0
	bindTestNormalizer(t, c, item, &mockNormalizerProvider{fn: func(ctx context.Context, raw []byte) ([]byte, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("normalizer context lacks deadline")
		}
		return []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), "  needs formatting  ", "needs formatting"), "APPROVE", "approve")), nil
	}})

	rawPayloadJSON := `{"findings":[{"summary":"  needs formatting  "}],"verdict":"APPROVE"}`
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

	if calls != 1 {
		t.Fatalf("normalizer calls = %d", calls)
	}
	claims := SubmitResultInput{Status: "success", Summary: "reviewed", StructuredPayload: []byte(rawPayloadJSON)}
	admission := localResultAdmission{coordinator: c, todoID: item.ID}
	ctx := occurrenceTestContext(c, item.ID, 1)
	if _, err := admission.validateClaims(ctx, claims, localResultSubmissionContract(c, item.ID), nil, false); localResultRejectionReason(err) != RepairFailureSource || calls != 1 {
		t.Fatalf("read-only validation executed or accepted unstaged claims: calls=%d, err=%v", calls, err)
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

func TestResultNormalizerRejectsModelDeliveryReceiptOverrides(t *testing.T) {
	compiled := modelInputTestContract(t)
	const valid = `{"findings":[{"sources":[{}]}]}`
	const override = `{"findings":[{"sources":[{"delivery":{"status":"observed"}}]}]}`
	for _, tc := range []struct {
		name, input, output string
		reason              RepairFailureReason
		calls               int
	}{
		{"model draft", override, valid, RepairFailureInvalidSchema, 0},
		{"normalizer output", valid, override, RepairFailureAction, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			ref := compiled.ref(true)
			calls := 0
			bindTestNormalizer(t, c, &TodoItem{ResultContract: &ref}, &mockNormalizerProvider{
				fn: func(context.Context, []byte) ([]byte, error) {
					calls++
					return []byte(tc.output), nil
				},
			})
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verify delivery evidence", ResultContract: &ref}})[0]
			ctx := occurrenceTestContext(c, item.ID, 1)
			stage, err := c.stageResultNormalization(ctx, item.ID, []byte(tc.input), nil)
			if stage != nil || localResultRejectionReason(err) != tc.reason || calls != tc.calls || !strings.Contains(err.Error(), "/delivery") {
				t.Fatalf("stage=%+v, normalizer calls=%d, error=%v", stage, calls, err)
			}
		})
	}
}

func TestNormalizerPipelineActionFailureFailsClosedWithoutRepair(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		err   error
	}{
		{name: "failed", err: errors.New("execution failed")},
		{name: "empty"},
		{name: "null", value: `null`},
		{name: "duplicate", value: `{"verdict":"approve","verdict":"reject"}`},
		{name: "secret", value: `{"token":"sk-proj-` + strings.Repeat("a", 24) + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, item := resultContractCoordinator(t, true)
			bindTestNormalizer(t, c, item, &mockNormalizerProvider{fn: func(context.Context, []byte) ([]byte, error) {
				return []byte(test.value), test.err
			}})
			tool := &submitResultTool{coordinator: c, todoID: item.ID}
			resp, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
				Name: "submit_result", Input: `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"approve"}}`,
			})
			if err != nil || !resp.IsError || !resp.StopTurn || submissionFailureMetadata(resp.Metadata) != RepairFailureAction || c.GetTaskResult(item.ID) != nil {
				t.Fatalf("normalizer rejection = %#v, %v", resp, err)
			}
		})
	}
}

func TestNormalizerPipelineStreamAdmission(t *testing.T) {
	const original = `{"z":"\u0061",  "verdict" : "checked"}`
	const normalized = `{"verdict":"approve","findings":[]}`
	for _, tc := range []struct {
		name            string
		want            RepairFailureReason
		wantRepairCalls int
		repairOmitsTool bool
	}{
		{name: "gated_enum_casing_preserves_original"},
		{name: "action_failure_with_sibling", want: RepairFailureAction},
		{name: "schema_error_gets_one_correction", want: RepairFailureInvalidSchema, wantRepairCalls: 1},
		{name: "schema_correction_cannot_restart_read_only_work", want: RepairFailureInvalidSchema, wantRepairCalls: 1, repairOmitsTool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoordinator(t)
			compiled, ref := compiledReviewContract(t, true)
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "review", ResultContract: &ref}})[0]
			actionCalls := 0
			bindTestNormalizer(t, c, item, &mockNormalizerProvider{fn: func(_ context.Context, raw []byte) ([]byte, error) {
				actionCalls++
				if tc.want == "" {
					if string(raw) != original {
						t.Fatalf("normalizer draft = %q, want %q", raw, original)
					}
					return []byte(normalized), nil
				}
				if tc.want == RepairFailureAction && strings.Contains(string(raw), "reject-action") {
					return nil, errors.New("normalizer process failed")
				}
				return raw, nil
			}})
			tool := &submitResultTool{coordinator: c, todoID: item.ID}
			worker := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls,
				input: func(int) string {
					if tc.want == "" {
						return `{"status":"SUCCESS","summary":"reviewed","structured_payload":` + original + `}`
					}
					return `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"reject-action"}}`
				},
				sibling: `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"approve","findings":[]}}`,
			}
			repair := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls,
				input: func(attempt int) string {
					return fmt.Sprintf(`{"status":"success","summary":"reviewed","structured_payload":{"verdict":"wrong-%d","findings":[]}}`, attempt)
				},
			}
			effect := SideEffectExternalWrite
			wantReason := tc.want
			wantActionCalls := 1 + tc.wantRepairCalls
			if tc.repairOmitsTool {
				repair.pauseAt = 1
				effect = SideEffectNone
				wantReason = RepairFailureNoToolCall
				wantActionCalls = 1
			}
			gated := c.gatePolicyTools([]fantasy.AgentTool{tool})
			c.workerAgentOverride = fantasy.NewAgent(worker, fantasy.WithTools(gated...), fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(4)))
			c.repairAgentOverride = fantasy.NewAgent(repair, fantasy.WithTools(tool), fantasy.WithMaxRetries(0))
			_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), TaskDef{
				Agent: "worker", Goal: "review", Recovery: RecoveryManual, SideEffect: effect,
				Execution: ExecutionContract{RequiresResult: true},
			}, item.ID)
			receipt := c.todoItemByID(item.ID).ExecutionReceipt
			if tc.want == "" {
				if err != nil || receipt == nil || receipt.ResultRawPayloadSHA256 != sha256HexBytes([]byte(original)) ||
					receipt.ResultNormalizerOutputSHA256 != sha256HexBytes([]byte(normalized)) || actionCalls != 1 || repair.calls != 0 {
					t.Fatalf("gated submission outcome=%v action=%d repair=%d receipt=%+v", err, actionCalls, repair.calls, receipt)
				}
				return
			}
			if receipt == nil || receipt.WorkSubmissionFailure != tc.want || receipt.RepairProvenance == nil || receipt.RepairProvenance.FailureReason != wantReason {
				t.Fatalf("classified receipt = %+v", receipt)
			}
			result := c.GetTaskResult(item.ID)
			if err == nil || worker.calls != 1 || repair.calls != tc.wantRepairCalls || actionCalls != wantActionCalls ||
				(result != nil && isLocalSubmittedResultSource(result.Source)) {
				t.Fatalf("outcome %v, worker/repair/action calls = %d/%d/%d, result = %+v", err, worker.calls, repair.calls, actionCalls, result)
			}
			if tc.want == RepairFailureInvalidSchema {
				err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID))
				if err == nil || !strings.Contains(err.Error(), "already consumed the schema correction") || repair.calls != 1 || worker.calls != 1 {
					t.Fatalf("operator reopened the schema correction budget: %v", err)
				}
			}
		})
	}
}

func TestNormalizerPipelineRejectsDraftBeforePersistingSecrets(t *testing.T) {
	c, item := resultContractCoordinator(t, true)
	bindTestNormalizer(t, c, item, &mockNormalizerProvider{fn: func(context.Context, []byte) ([]byte, error) {
		t.Fatal("secret-like draft reached normalizer")
		return nil, nil
	}})
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	const secretSuffix = "-proj-aaaaaaaaaaaaaaaaaaaaaaaa"
	resp, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name: "submit_result", Input: `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"\u0073k` + secretSuffix + `"}}`,
	})
	if err != nil || submissionFailureMetadata(resp.Metadata) != RepairFailureInvalidSchema || strings.Contains(resp.Content, secretSuffix) {
		t.Fatalf("unexpected rejection: %v (%s)", err, resp.Metadata)
	}
}

func TestNormalizerPipelineSourceRejectionPrecedesMalformedClaims(t *testing.T) {
	const malformed = `{"status":"success","summary":["wrong shape"],"receipt_ids":["forged-receipt"],"structured_payload":[]}`
	const duplicate = `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"approve","verdict":"reject"},"receipt_ids":["forged-receipt"]}`
	const unknownArtifactMember = `{"status":"success","summary":"reviewed","artifacts":[{"path":"missing-readable-artifact.txt","unknown":true}],"structured_payload":{"verdict":"approve"}}`
	provider := &mockNormalizerProvider{fn: func(context.Context, []byte) ([]byte, error) {
		t.Fatal("unverified source reached normalizer")
		return nil, nil
	}}
	for _, input := range []string{malformed, duplicate, unknownArtifactMember} {
		t.Run("direct/"+input, func(t *testing.T) {
			c, item := resultContractCoordinator(t, true)
			bindTestNormalizer(t, c, item, provider)
			tool := &submitResultTool{coordinator: c, todoID: item.ID}
			resp, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{Name: "submit_result", Input: input})
			if err != nil || submissionFailureMetadata(resp.Metadata) != RepairFailureSource {
				t.Fatalf("direct rejection = %s, %v", resp.Metadata, err)
			}
		})
	}
	t.Run("collector", func(t *testing.T) {
		compiled, ref := compiledReviewContract(t, true)
		c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
			spec.ResultContract = &ref
		}, func(c *Coordinator) {
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			bindTestNormalizer(t, c, &TodoItem{ResultContract: &ref}, provider)
		})
		cp, err := c.beginResultCollection(ctx, item.ID, receipt, "completed original work")
		if err == nil {
			cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
		}
		if err == nil {
			cp, err = c.recordCollectionResponse(ctx, cp, []byte(malformed))
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.prepareCollectionClaims(ctx, cp); localResultRejectionReason(err) != RepairFailureSource {
			t.Fatalf("collector rejection = %v", err)
		}
	})
}

func TestNormalizerPipelineRepairsMissingRequiredModelField(t *testing.T) {
	const schema = `{"type":"object","additionalProperties":false,
		"properties":{"scope":{"const":"sealed"},"answer":{"type":"string"}},
		"required":["scope","answer"],"x-hufu-runtime-inputs":[{"target":"/scope","source_kind":"const"}]}`
	const incomplete = `{"status":"success","summary":"reviewed","structured_payload":{}}`
	const corrected = `{"status":"success","summary":"reviewed","structured_payload":{"answer":"fixed"}}`
	const want = `{"answer":"fixed","scope":"sealed"}`

	compiled, err := CompileResultContractDocument("claims.json", []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	c := newTestCoordinator(t)
	ref := compiled.ref(true)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "review", ResultContract: &ref}})[0]
	calls := 0
	bindTestNormalizer(t, c, item, &mockNormalizerProvider{fn: func(_ context.Context, draft []byte) ([]byte, error) {
		calls++
		return draft, nil
	}})
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	worker := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls, input: func(int) string { return incomplete }}
	repair := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls, input: func(int) string { return corrected }}
	c.workerAgentOverride = fantasy.NewAgent(worker, fantasy.WithTools(c.gatePolicyTools([]fantasy.AgentTool{tool})...), fantasy.WithMaxRetries(0))
	c.repairAgentOverride = fantasy.NewAgent(repair, fantasy.WithTools(tool), fantasy.WithMaxRetries(0))
	_, err = c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), TaskDef{
		Agent: "worker", Goal: "review", Recovery: RecoveryManual, SideEffect: SideEffectExternalWrite,
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err != nil || worker.calls != 1 || repair.calls != 1 {
		t.Fatalf("work/repair calls=%d/%d outcome=%v", worker.calls, repair.calls, err)
	}
	result := c.GetTaskResult(item.ID)
	if calls != 2 || result == nil || result.StructuredPayload == nil || string(result.StructuredPayload.Value) != want {
		t.Fatalf("normalizer calls=%d result=%+v", calls, result)
	}
}

func TestNormalizerPipelineRepairsObservedEvidenceSelection(t *testing.T) {
	f := artifactKeysFixtureForSchema(t, observedFilesTestSchema())
	f.readSource(t)
	calls := 0
	bindTestNormalizer(t, f.c, f.item, &mockNormalizerProvider{fn: func(_ context.Context, draft []byte) ([]byte, error) {
		calls++
		return draft, nil
	}})
	tool := &submitResultTool{coordinator: f.c, todoID: f.item.ID}
	for _, key := range []string{"ev_000000000000", f.key} {
		response, err := tool.Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: fmt.Sprintf(
			`{"status":"success","summary":"reviewed","structured_payload":{"claims":[{"evidence":[%q],"note":"preserve this judgment"}]}}`, key,
		)})
		if err != nil {
			t.Fatal(err)
		}
		if key != f.key && (submissionFailureMetadata(response.Metadata) != RepairFailureInvalidSchema ||
			!strings.Contains(response.Content, "evidence_reference_unknown") || !strings.Contains(response.Content, f.key)) {
			t.Fatalf("repairable selection lost its diagnostic or allowed choice: %+v", response)
		}
		if key == f.key && response.IsError {
			t.Fatalf("corrected selection failed: %+v", response)
		}
	}
	result := f.c.GetTaskResult(f.item.ID)
	if calls != 2 || result == nil || result.StructuredPayload == nil ||
		!strings.Contains(string(result.StructuredPayload.Value), f.ref.ID) ||
		!strings.Contains(string(result.StructuredPayload.Value), "preserve this judgment") {
		t.Fatalf("normalizations=%d result=%+v", calls, result)
	}
}

func TestNormalizerRetainsStageWhenObservedEvidenceReadIsInterrupted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission interruption requires a non-root process")
	}
	f := artifactKeysFixtureForSchema(t, observedFilesTestSchema())
	f.readSource(t)
	events, err := f.c.EventJournal().ReadEvents(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var observation decisionArtifactObservation
	for _, event := range events {
		if event.Type == string(EventArtifactEvidenceObserved) {
			if err := json.Unmarshal(event.Payload, &observation); err != nil {
				t.Fatal(err)
			}
		}
	}
	store, err := OpenFileArtifactStoreReadOnly(f.c.artifactStoreRootPath(), f.c.artifactStoreRootPath())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(store.root, "data", observation.Output.ID)
	info, err := os.Stat(file)
	if err != nil || observation.Output.ID == "" {
		t.Fatalf("observed output CAS entry: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, info.Mode()) })

	draft := fmt.Sprintf(`{"claims":[{"evidence":[%q],"note":"preserve this judgment"}]}`, f.key)
	call := fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"reviewed","Structured_Payload":` + draft + `}`}
	calls := 0
	bindTestNormalizer(t, f.c, f.item, &mockNormalizerProvider{fn: func(_ context.Context, draft []byte) ([]byte, error) {
		calls++
		if err := os.Chmod(file, 0); err != nil {
			t.Fatal(err)
		}
		return draft, nil
	}})
	tool := &submitResultTool{coordinator: f.c, todoID: f.item.ID}
	response, err := tool.Run(f.ctx, call)
	failure := decodeSubmissionFailure(response.Metadata)
	if err != nil || !response.IsError || !response.StopTurn || failure.Reason != RepairFailureCommit || f.c.GetTaskResult(f.item.ID) != nil {
		t.Fatalf("interrupted submission = %+v, %v", response, err)
	}
	evidence := failure.Normalization
	ref := *f.item.ResultContract
	hash := sha256HexBytes([]byte(draft))
	if evidence.RawSHA256 != hash || evidence.OutputSHA256 != hash || evidence.ProviderSHA256 != ref.NormalizerSHA256 {
		t.Fatalf("interrupted normalization = %+v", evidence)
	}
	stageEvidence := assertDirectNormalizationSubmission(t, f.c, evidence, call.Input)
	stage, found, err := f.c.readStagedNormalization(ref, evidence.OccurrenceSHA256, hash)
	if err != nil || !found || stage.evidence() != stageEvidence {
		t.Fatalf("sealed stage = %+v, found=%t, err=%v", stage, found, err)
	}
	if err := os.Chmod(file, info.Mode()); err != nil {
		t.Fatal(err)
	}
	response, err = tool.Run(f.ctx, call)
	result := f.c.GetTaskResult(f.item.ID)
	if err != nil || response.IsError || calls != 1 || result == nil || result.StructuredPayload == nil || result.StructuredPayload.Normalization.evidence() != stageEvidence {
		t.Fatalf("retry = %+v, %v, normalizations=%d, result=%+v", response, err, calls, result)
	}
}

func TestResultCollectionNormalizerReusesDurableOutputAcrossFailures(t *testing.T) {
	if !golangruntime.NormalizerSupported() {
		t.Skip("bounded normalizers require Linux")
	}
	compiled, err := CompileResultContractDocument("normalizer.json", []byte(reviewResultSchema))
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	calls := 0
	provider := &mockNormalizerProvider{fn: func(_ context.Context, raw []byte) ([]byte, error) {
		calls++
		return []byte(strings.ReplaceAll(string(raw), "APPROVE", "approve")), nil
	}}
	c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		spec.ResultContract = &ref
	}, func(c *Coordinator) {
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		bindTestNormalizer(t, c, &TodoItem{ResultContract: &ref}, provider)
	})
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "completed original work")
	if err == nil {
		cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
	}
	const draft = `{"findings": [], "verdict": "APPROVE"}`
	if err == nil {
		cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":"reviewed","structured_payload":`+draft+`}`))
	}
	if err != nil {
		t.Fatal(err)
	}
	journal := c.EventJournal()
	c.SetEventJournal(failingJournal{err: errors.New("storage unavailable")})
	if _, err := c.prepareCollectionClaims(ctx, cp); localResultRejectionReason(err) != RepairFailureCommit || calls != 1 {
		t.Fatalf("prepare failure = %v with %d calls", err, calls)
	}
	c.SetEventJournal(journal)
	c = reopenRecoveryCoordinator(t, c)
	ctx = WithSessionResume(t.Context())
	prepared, err := c.prepareCollectionClaims(ctx, cp)
	if err != nil || calls != 1 {
		t.Fatalf("stage retry = %v with %d calls", err, calls)
	}
	work, err := c.loadCollectionWork(ctx, c.todoItemByID(item.ID), prepared)
	if err != nil {
		t.Fatal(err)
	}
	a := &ResultCollectionRecoveryAssessment{Checkpoint: prepared}
	c.assessRecoveryClaims(ctx, c.todoItemByID(item.ID), a, work, compiled, func(_ string, err error) bool { return err == nil })
	if a.Disposition != RecoveryCommitLocal || calls != 1 {
		t.Fatalf("read-only assessment = %s with %d calls", a.Disposition, calls)
	}
	for _, ref := range []ResultCollectionRef{*prepared.Prepared, prepared.Work} {
		store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.artifactStoreRootPath())
		if err != nil {
			t.Fatal(err)
		}
		filename := filepath.Join(store.root, "data", ref.ArtifactID)
		backup := filename + ".saved"
		if err := os.Rename(filename, backup); err != nil {
			t.Fatal(err)
		}
		if err := c.commitPreparedCollection(ctx, prepared); localResultRejectionReason(err) != RepairFailureSource {
			t.Fatalf("missing CAS blob = %v", err)
		}
		if err := os.Mkdir(filename, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := c.commitPreparedCollection(ctx, prepared); localResultRejectionReason(err) != RepairFailureCommit || calls != 1 {
			t.Fatalf("transient CAS read = %v with %d calls", err, calls)
		}
		if err := os.Remove(filename); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, filename); err != nil {
			t.Fatal(err)
		}
	}
	// Introduce faults after the preliminary collection reads have succeeded.
	// The registry and the publishing transaction must keep the same reason
	// and the sealed normalization evidence at their later reads.
	submissionCtx, err := c.restoreCollectionContext(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := c.readCollectionBlobRecord(submissionCtx, prepared, "claims", prepared.Prepared)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := c.decodeCollectionClaims(item.ID, staged.Data)
	if err != nil {
		t.Fatal(err)
	}
	claims.resultNormalization = staged.Normalization
	submissionCtx = context.WithValue(submissionCtx, resultCollectionAdmissionKey{}, prepared)
	for _, tc := range []struct {
		name, artifact string
		registry       bool
		missing        bool
		want           RepairFailureReason
	}{
		{"registry read", prepared.Work.ArtifactID, true, false, RepairFailureCommit},
		{"transaction read", prepared.Work.ArtifactID, false, false, RepairFailureCommit},
		{"transaction missing work", prepared.Work.ArtifactID, false, true, RepairFailureSource},
		{"transaction transcript read", receipt.TranscriptRef, false, false, RepairFailureCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(runtimestore.ArtifactRoot(c.artifactStoreRootPath()), "data", tc.artifact)
			backup := filename + ".saved"
			interrupt := func() {
				if err := os.Rename(filename, backup); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if !tc.missing {
						if err := os.Remove(filename); err != nil {
							t.Error(err)
						}
					}
					if err := os.Rename(backup, filename); err != nil {
						t.Error(err)
					}
				})
				if !tc.missing {
					if err := os.Mkdir(filename, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			input := claims
			admission := localResultAdmission{coordinator: c, todoID: item.ID}
			if tc.registry {
				input.ReceiptIDs = []string{"request a scoped registry read"}
				interrupt()
			} else {
				admission.sink = normalizationTestResultSink(interrupt)
			}
			err := admission.admit(submissionCtx, input, localResultSubmissionContract(c, item.ID), nil)
			if localResultRejectionReason(err) != tc.want || c.GetTaskResult(item.ID) != nil || calls != 1 {
				t.Fatalf("late source rejection = %v, want %s with one normalizer call", err, tc.want)
			}
			if tc.want == RepairFailureCommit {
				interruption, ok := errors.AsType[*normalizedResultCommitError](err)
				if !ok || interruption.evidence != staged.Normalization.evidence() {
					t.Fatalf("late failure lost its sealed normalization evidence: %v", err)
				}
			}
		})
	}
	journal = c.EventJournal()
	c.SetEventJournal(failingJournal{err: errors.New("storage unavailable")})
	if err := c.commitPreparedCollection(ctx, prepared); localResultRejectionReason(err) != RepairFailureCommit || c.GetTaskResult(item.ID) != nil || calls != 1 {
		t.Fatalf("commit failure = %v with %d calls", err, calls)
	}
	c.SetEventJournal(journal)
	c = reopenRecoveryCoordinator(t, c)
	if err := c.commitPreparedCollection(ctx, prepared); err != nil || calls != 1 {
		t.Fatalf("commit retry = %v with %d calls", err, calls)
	}
	c = reopenRecoveryCoordinator(t, c)
	current := c.todoItemByID(item.ID)
	payload := current.TypedResult.StructuredPayload
	if payload == nil || payload.RawSHA256 != sha256HexBytes([]byte(draft)) ||
		string(payload.Value) != `{"findings":[],"verdict":"approve"}` ||
		!receiptMatchesResultPayload(current.ExecutionReceipt, payload) {
		t.Fatalf("replayed payload or receipt differs: %+v", payload)
	}
}

func TestNormalizerPipelineFencesRevocationThroughSeal(t *testing.T) {
	c, item := resultContractCoordinator(t, true)
	started, finish := make(chan struct{}), make(chan struct{})
	var closeFinish sync.Once
	t.Cleanup(func() { closeFinish.Do(func() { close(finish) }) })
	bindTestNormalizer(t, c, item, &mockNormalizerProvider{fn: func(ctx context.Context, raw []byte) ([]byte, error) {
		close(started)
		select {
		case <-finish:
			return raw, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	ctx := occurrenceTestContext(c, item.ID, 1)
	raw := []byte(`{"verdict":"approve","findings":[]}`)
	done := make(chan error, 1)
	go func() {
		_, err := c.stageResultNormalization(ctx, item.ID, raw, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("normalizer did not start")
	}
	controller := c.occurrenceController(item.ID)
	if controller.projectionMu.TryLock() {
		controller.projectionMu.Unlock()
		t.Fatal("lifecycle publication could revoke a running normalizer")
	}
	revoked := make(chan struct{})
	go func() {
		c.revokeTaskOccurrence(item.ID)
		close(revoked)
	}()
	closeFinish.Do(func() { close(finish) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-revoked
	_, err := c.stageResultNormalization(ctx, item.ID, []byte(`{"verdict":"reject"}`), nil)
	if !errors.Is(err, errTaskResultStale) || localResultRejectionReason(err) != RepairFailureSource {
		t.Fatalf("revoked worker could stage new output: %v", err)
	}
}

func installAttemptTerminalEvidence(t *testing.T, c *Coordinator, sessions ...TerminalSession) {
	t.Helper()
	body, err := json.Marshal(sessions)
	if err != nil {
		t.Fatal(err)
	}
	putRuntimeDocument(t, c.session.Workspace, runtimestore.DocumentTerminalSessions, body)
	c.terminalSessionMgr, err = NewTerminalSessionManager(c.session.Workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNormalizedSubmissionIgnoresPriorTerminalAttempt(t *testing.T) {
	c, _, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) { spec.Verify = "true" })
	installAttemptTerminalEvidence(t, c, TerminalSession{
		ID: "prior-attempt", RunID: c.executionRunID, OwnerTaskID: item.ID,
		State: TerminalSessionExited, ExitCode: new(7), Command: []string{"false"}, StartedAt: time.Now().Add(-time.Minute),
	})
	worker, model := normalizationStreamedSubmission(c, item.ID, normalizationWire(approvedNormalizationDraft), nil)
	c.workerAgentOverride = worker
	c.repairAgentOverride = scriptWorker(failTurn(errors.New("unexpected result-only correction")))
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(item), item.ID)
	got := todoByID(t, c, item.ID)
	if err != nil || got.Status != TaskDone || got.VerifyResult == nil || model.calls != 1 {
		t.Fatalf("prior terminal affected a different worker attempt: status=%s calls=%d error=%v", got.Status, model.calls, err)
	}
}

func TestNormalizedCompletionChecksOriginalTerminal(t *testing.T) {
	for _, afterAcknowledgment := range []bool{false, true} {
		name := "verification_checkpoint"
		if afterAcknowledgment {
			name = "submission_checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			c, journal, item := newDirectNormalizationRun(t, echoNormalization, func(spec *TodoSpec) { spec.Verify = "true" })
			wire := normalizationWire(approvedNormalizationDraft)
			if afterAcknowledgment {
				c.workerAgentOverride = stopAfterNormalizedSubmission(t, c, item.ID, wire, nil)
				interruptAfterNormalizedSubmission(t, c, item)
			} else {
				c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, wire, nil)
				stopBeforeObjectiveVerification(t, c, item)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			checkpoint := normalizationResumeProjection(t, c)
			reader, _ := restartDirectNormalizationRun(t, c, checkpoint, item, func(context.Context, []byte) ([]byte, error) {
				t.Error("terminal verification repeated normalization")
				return nil, errors.New("unexpected normalizer call")
			})
			original := todoByID(t, reader, item.ID)
			receipt := original.ExecutionReceipt
			if afterAcknowledgment {
				var err error
				receipt, err = reader.readAcceptedNormalizedSubmission(original)
				if err != nil {
					t.Fatal(err)
				}
			}
			if receipt == nil || receipt.StartedAt.IsZero() || receipt.FinishedAt.IsZero() || receipt.FinishedAt.Before(receipt.StartedAt) {
				t.Fatal("canonical original work is missing its dispatch interval")
			}
			installAttemptTerminalEvidence(t, reader,
				TerminalSession{ID: "original-terminal", RunID: receipt.RunID, OwnerTaskID: item.ID, State: TerminalSessionExited,
					ExitCode: new(7), Command: []string{"false"}, StartedAt: receipt.StartedAt},
				TerminalSession{ID: "later-attempt", RunID: receipt.RunID, OwnerTaskID: item.ID, State: TerminalSessionExited,
					ExitCode: new(0), Command: []string{"true"}, StartedAt: receipt.FinishedAt.Add(time.Second)})
			forbidden := scriptWorker(failTurn(errors.New("unexpected model call")))
			reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
			count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			blocked := todoByID(t, reader, item.ID)
			if count != 1 || err == nil || !strings.Contains(err.Error(), "status 7") || blocked.Status != TaskBlocked ||
				blocked.FailureEvent == nil || blocked.FailureEvent.FailureClass != FailureVerify || blocked.VerifyResult != nil || forbidden.Calls() != 0 {
				t.Fatalf("later terminal masked original evidence: count=%d status=%s failure=%+v model calls=%d error=%v", count, blocked.Status, blocked.FailureEvent, forbidden.Calls(), err)
			}
		})
	}
}
