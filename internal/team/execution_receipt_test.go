package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestExecutionReceipt_JSONSerialization(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	exitCode := 0
	receipt := ExecutionReceipt{
		RunID:      "run-123",
		TaskID:     "task-456",
		Attempt:    2,
		StartedAt:  now,
		FinishedAt: now.Add(2 * time.Second),
		ExitCode:   &exitCode,
		ProducerID: "worker-agent",
	}

	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var restored ExecutionReceipt
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if restored.RunID != receipt.RunID || restored.TaskID != receipt.TaskID || restored.Attempt != receipt.Attempt || restored.ProducerID != receipt.ProducerID {
		t.Errorf("Restored receipt mismatch: got %#v, want %#v", restored, receipt)
	}
	if restored.ExitCode == nil || *restored.ExitCode != 0 {
		t.Errorf("Restored ExitCode = %v, want 0", restored.ExitCode)
	}
}

func TestArtifactExpectation_AllSerializationVariants(t *testing.T) {
	// 1. JSON with kebab-case and explicit required: false
	jsonKebab := `{"name":"opt_build", "locator":"dist/opt", "must-be-fresh":true, "required":false, "verification-mode":"file"}`
	var exp1 ArtifactExpectation
	if err := json.Unmarshal([]byte(jsonKebab), &exp1); err != nil {
		t.Fatalf("Unmarshal json kebab failed: %v", err)
	}
	if !exp1.MustBeFresh || exp1.Required || exp1.VerificationMode != "file" {
		t.Errorf("Unexpected exp1 fields: %#v", exp1)
	}

	// 2. JSON with snake_case and default required (omitted -> true)
	jsonSnake := `{"name":"req_build", "locator":"dist/req", "must_be_fresh":true, "verification_mode":"exists"}`
	var exp2 ArtifactExpectation
	if err := json.Unmarshal([]byte(jsonSnake), &exp2); err != nil {
		t.Fatalf("Unmarshal json snake failed: %v", err)
	}
	if !exp2.MustBeFresh || !exp2.Required || exp2.VerificationMode != "exists" {
		t.Errorf("Unexpected exp2 fields: %#v", exp2)
	}

	// 3. YAML with kebab-case
	yamlKebab := `
name: report_kebab
locator: reports/final.pdf
must-be-fresh: true
required: false
verification-mode: file
`
	var exp3 ArtifactExpectation
	if err := yaml.Unmarshal([]byte(yamlKebab), &exp3); err != nil {
		t.Fatalf("Unmarshal yaml kebab failed: %v", err)
	}
	if !exp3.MustBeFresh || exp3.Required || exp3.VerificationMode != "file" {
		t.Errorf("Unexpected exp3 fields: %#v", exp3)
	}

	// 4. YAML with snake_case
	yamlSnake := `
name: report_snake
locator: reports/final.pdf
must_be_fresh: true
required: true
verification_mode: file
`
	var exp4 ArtifactExpectation
	if err := yaml.Unmarshal([]byte(yamlSnake), &exp4); err != nil {
		t.Fatalf("Unmarshal yaml snake failed: %v", err)
	}
	if !exp4.MustBeFresh || !exp4.Required || exp4.VerificationMode != "file" {
		t.Errorf("Unexpected exp4 fields: %#v", exp4)
	}
}

func TestArtifactVerifierRegistry_DispatchAndDefault(t *testing.T) {
	dir := t.TempDir()
	reg := NewArtifactVerifierRegistry(dir)

	receipt := ExecutionReceipt{
		RunID:     "run-1",
		TaskID:    "task-1",
		Attempt:   1,
		StartedAt: time.Now().Add(-10 * time.Second),
	}
	expMissing := ArtifactExpectation{
		Name:             "missing",
		Locator:          "non_existent.txt",
		Required:         true,
		VerificationMode: "file",
	}

	res := reg.Verify(context.Background(), receipt, expMissing)
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "does not exist") {
		t.Errorf("Expected missing file error, got result: %#v", res)
	}

	// Unregistered verification mode
	expUnknown := ArtifactExpectation{
		Name:             "unknown",
		Locator:          "http://example.com",
		VerificationMode: "custom_http",
	}
	resUnknown := reg.Verify(context.Background(), receipt, expUnknown)
	if resUnknown.ExitCode == 0 || !strings.Contains(resUnknown.Stderr, "unregistered verification mode") {
		t.Errorf("Expected unregistered mode error, got result: %#v", resUnknown)
	}

	// Custom verifier registration
	customV := &mockVerifier{exitCode: 0, stdout: "custom check passed"}
	reg.Register("custom_http", customV)
	resCustom := reg.Verify(context.Background(), receipt, expUnknown)
	if resCustom.ExitCode != 0 || resCustom.Stdout != "custom check passed" {
		t.Errorf("Expected custom verifier success, got result: %#v", resCustom)
	}
}

type mockVerifier struct {
	exitCode int
	stdout   string
}

func (m *mockVerifier) Verify(ctx context.Context, receipt ExecutionReceipt, expectation ArtifactExpectation) VerificationResult {
	return VerificationResult{
		Command:  "mock_verifier:" + expectation.VerificationMode,
		ExitCode: m.exitCode,
		Stdout:   m.stdout,
	}
}

func TestFileArtifactVerifier_FreshnessValidation(t *testing.T) {
	dir := t.TempDir()
	fv := NewFileArtifactVerifier(dir)

	artPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(artPath, []byte("pre-existing data"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Receipt starts AFTER artifact was created -> Freshness check MUST FAIL
	startTimeAfter := time.Now().Add(5 * time.Second)
	receipt1 := ExecutionReceipt{
		RunID:     "run-1",
		TaskID:    "task-1",
		Attempt:   1,
		StartedAt: startTimeAfter,
	}

	exp := ArtifactExpectation{
		Name:        "output",
		Locator:     "output.txt",
		MustBeFresh: true,
		Required:    true,
	}

	res1 := fv.Verify(context.Background(), receipt1, exp)
	if res1.ExitCode == 0 {
		t.Fatal("Expected freshness check to FAIL for pre-existing artifact, but got success")
	}
	if !strings.Contains(res1.Stderr, "not after receipt start time") {
		t.Errorf("Unexpected stderr for freshness failure: %q", res1.Stderr)
	}

	// 2. Receipt starts BEFORE artifact modification -> Freshness check MUST PASS
	startTimeBefore := time.Now().Add(-5 * time.Second)
	receipt2 := ExecutionReceipt{
		RunID:     "run-1",
		TaskID:    "task-1",
		Attempt:   1,
		StartedAt: startTimeBefore,
	}

	res2 := fv.Verify(context.Background(), receipt2, exp)
	if res2.ExitCode != 0 {
		t.Fatalf("Expected freshness check to PASS for newly updated artifact, got stderr: %q", res2.Stderr)
	}
}

func TestFileArtifactVerifier_FreshnessEquality(t *testing.T) {
	dir := t.TempDir()
	fv := NewFileArtifactVerifier(dir)

	artPath := filepath.Join(dir, "equal_time.txt")
	if err := os.WriteFile(artPath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(artPath)
	if err != nil {
		t.Fatal(err)
	}
	modTime := info.ModTime()

	// receipt.StartedAt equals modTime -> conservative provenance rejects modtime equal to receipt start time
	receipt := ExecutionReceipt{
		RunID:     "run-1",
		TaskID:    "task-1",
		Attempt:   1,
		StartedAt: modTime,
	}

	exp := ArtifactExpectation{
		Name:        "equal_time",
		Locator:     "equal_time.txt",
		MustBeFresh: true,
	}

	res := fv.Verify(context.Background(), receipt, exp)
	if res.ExitCode == 0 {
		t.Fatal("Expected conservative freshness check to FAIL when modTime equals receipt.StartedAt")
	}
	if !strings.Contains(res.Stderr, "not after receipt start time") {
		t.Errorf("Unexpected stderr for equality timestamp failure: %s", res.Stderr)
	}
}

func TestFileArtifactVerifier_RetryAttemptRejection_MustBeFreshFalse(t *testing.T) {
	dir := t.TempDir()
	fv := NewFileArtifactVerifier(dir)

	artPath := filepath.Join(dir, "retry_no_fresh.txt")
	if err := os.WriteFile(artPath, []byte("attempt 1 data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Companion metadata indicating attempt 1
	attempt1 := 1
	meta1 := ArtifactProducerMeta{
		RunID:   "run-100",
		TaskID:  "task-200",
		Attempt: &attempt1,
	}
	metaBytes, _ := json.Marshal(meta1)
	if err := os.WriteFile(artPath+".json", metaBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// Attempt 2 receipt with MustBeFresh: false
	receiptAttempt2 := ExecutionReceipt{
		RunID:     "run-100",
		TaskID:    "task-200",
		Attempt:   2,
		StartedAt: time.Now().Add(-10 * time.Second),
	}

	expNoFresh := ArtifactExpectation{
		Name:        "retry_no_fresh",
		Locator:     "retry_no_fresh.txt",
		MustBeFresh: false, // MustBeFresh is false!
		Required:    true,
	}

	res := fv.Verify(context.Background(), receiptAttempt2, expNoFresh)
	if res.ExitCode == 0 {
		t.Fatal("Expected retry attempt 2 to FAIL even when MustBeFresh is false due to companion metadata attempt mismatch")
	}
	if !strings.Contains(res.Stderr, "artifact producer attempt 1 does not match receipt attempt 2") {
		t.Errorf("Unexpected stderr for attempt mismatch when MustBeFresh=false: %s", res.Stderr)
	}
}

func TestFileArtifactVerifier_AttemptZeroIdentityComparison(t *testing.T) {
	dir := t.TempDir()
	fv := NewFileArtifactVerifier(dir)

	artPath := filepath.Join(dir, "attempt_zero.txt")
	if err := os.WriteFile(artPath, []byte("attempt 0 data"), 0644); err != nil {
		t.Fatal(err)
	}

	attemptZero := 0
	meta0 := ArtifactProducerMeta{
		RunID:   "run-000",
		TaskID:  "task-000",
		Attempt: &attemptZero,
	}
	metaBytes, _ := json.Marshal(meta0)
	if err := os.WriteFile(artPath+".json", metaBytes, 0644); err != nil {
		t.Fatal(err)
	}

	exp := ArtifactExpectation{
		Name:    "attempt_zero",
		Locator: "attempt_zero.txt",
	}

	// 1. Receipt attempt is 1 vs companion metadata attempt 0 -> MUST FAIL
	resMismatch := fv.Verify(context.Background(), ExecutionReceipt{
		RunID:     "run-000",
		TaskID:    "task-000",
		Attempt:   1,
		StartedAt: time.Now().Add(-5 * time.Second),
	}, exp)
	if resMismatch.ExitCode == 0 || !strings.Contains(resMismatch.Stderr, "attempt 0 does not match receipt attempt 1") {
		t.Fatalf("Expected failure for attempt 0 vs attempt 1 mismatch, got: %#v", resMismatch)
	}

	// 2. Receipt attempt is 0 vs companion metadata attempt 0 -> MUST PASS
	resMatch := fv.Verify(context.Background(), ExecutionReceipt{
		RunID:     "run-000",
		TaskID:    "task-000",
		Attempt:   0,
		StartedAt: time.Now().Add(-5 * time.Second),
	}, exp)
	if resMatch.ExitCode != 0 {
		t.Fatalf("Expected success for matching attempt 0, got: %#v", resMatch)
	}
}

func TestFileArtifactVerifier_OptionalArtifactMissing(t *testing.T) {
	dir := t.TempDir()
	fv := NewFileArtifactVerifier(dir)

	receipt := ExecutionReceipt{
		RunID:     "run-1",
		TaskID:    "task-1",
		Attempt:   1,
		StartedAt: time.Now().Add(-5 * time.Second),
	}

	// 1. Missing artifact with Required: true -> fails with exit code 1
	expRequired := ArtifactExpectation{
		Name:     "missing_req",
		Locator:  "absent_req.txt",
		Required: true,
	}
	resReq := fv.Verify(context.Background(), receipt, expRequired)
	if resReq.ExitCode == 0 || !strings.Contains(resReq.Stderr, "does not exist") {
		t.Errorf("Expected failure for required missing artifact, got: %#v", resReq)
	}

	// 2. Missing artifact with Required: false -> succeeds with exit code 0
	expOptional := ArtifactExpectation{
		Name:     "missing_opt",
		Locator:  "absent_opt.txt",
		Required: false,
	}
	resOpt := fv.Verify(context.Background(), receipt, expOptional)
	if resOpt.ExitCode != 0 || !strings.Contains(resOpt.Stdout, "optional artifact file") {
		t.Errorf("Expected success (ExitCode 0) for optional missing artifact, got: %#v", resOpt)
	}
}

func TestFileArtifactVerifier_ProducerMetadataMismatch(t *testing.T) {
	dir := t.TempDir()
	fv := NewFileArtifactVerifier(dir)

	artPath := filepath.Join(dir, "meta_test.txt")
	if err := os.WriteFile(artPath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	attempt1 := 1
	meta := ArtifactProducerMeta{
		RunID:      "run-A",
		TaskID:     "task-A",
		Attempt:    &attempt1,
		ProducerID: "agent-alpha",
	}
	metaBytes, _ := json.Marshal(meta)
	_ = os.WriteFile(artPath+".json", metaBytes, 0644)

	exp := ArtifactExpectation{
		Name:        "meta_test",
		Locator:     "meta_test.txt",
		MustBeFresh: true,
	}

	// RunID mismatch
	resRunID := fv.Verify(context.Background(), ExecutionReceipt{RunID: "run-B", TaskID: "task-A", Attempt: 1, ProducerID: "agent-alpha", StartedAt: time.Now().Add(-5 * time.Second)}, exp)
	if resRunID.ExitCode == 0 || !strings.Contains(resRunID.Stderr, "run_id") {
		t.Errorf("Expected run_id mismatch error, got: %#v", resRunID)
	}

	// TaskID mismatch
	resTaskID := fv.Verify(context.Background(), ExecutionReceipt{RunID: "run-A", TaskID: "task-B", Attempt: 1, ProducerID: "agent-alpha", StartedAt: time.Now().Add(-5 * time.Second)}, exp)
	if resTaskID.ExitCode == 0 || !strings.Contains(resTaskID.Stderr, "task_id") {
		t.Errorf("Expected task_id mismatch error, got: %#v", resTaskID)
	}

	// ProducerID mismatch
	resProducer := fv.Verify(context.Background(), ExecutionReceipt{RunID: "run-A", TaskID: "task-A", Attempt: 1, ProducerID: "agent-beta", StartedAt: time.Now().Add(-5 * time.Second)}, exp)
	if resProducer.ExitCode == 0 || !strings.Contains(resProducer.Stderr, "producer_id") {
		t.Errorf("Expected producer_id mismatch error, got: %#v", resProducer)
	}
}

func TestVerificationResult_EventStoreAppendAndReplayIntegration(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-test-es", "session-test-es")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}

	c := &Coordinator{
		session:                &TeamSession{Workspace: dir},
		eventStore:             es,
		emittedTaskTransitions: make(map[string]bool),
	}

	expectedResult := &VerificationResult{
		Command:  "make test-artifact",
		WorkDir:  "/workspace/project",
		ExitCode: 0,
		Stdout:   "all 12 verification checks passed",
		Stderr:   "warning: unused variable",
		Duration: 1250 * time.Millisecond,
		TimedOut: false,
	}

	todoItem := &TodoItem{
		ID:           "task-verify-1",
		Desc:         "Build and verify deliverable",
		Status:       TaskDone,
		Agent:        "builder",
		Verify:       "make test-artifact",
		VerifyMode:   "file",
		VerifyResult: expectedResult,
	}

	// Emit task events to the disk event store
	c.emitTaskEventsFromCheckpoint([]*TodoItem{todoItem})

	if err := es.Close(); err != nil {
		t.Fatalf("Close event store failed: %v", err)
	}

	// Re-open event store and read raw events
	readerES, err := OpenEventStore(dir)
	if err != nil {
		t.Fatalf("OpenEventStore failed: %v", err)
	}
	defer readerES.Close()

	events, err := readerES.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents failed: %v", err)
	}

	// Replay events through ReduceToTodoList
	reducedTasks := ReduceToTodoList(events)
	if len(reducedTasks) != 1 {
		t.Fatalf("ReduceToTodoList len = %d, want 1", len(reducedTasks))
	}

	reducedItem := reducedTasks[0]
	if reducedItem.VerifyResult == nil {
		t.Fatal("VerifyResult was NOT reconstructed from event store replay")
	}

	vr := reducedItem.VerifyResult
	if vr.Command != expectedResult.Command {
		t.Errorf("Command = %q, want %q", vr.Command, expectedResult.Command)
	}
	if vr.WorkDir != expectedResult.WorkDir {
		t.Errorf("WorkDir = %q, want %q", vr.WorkDir, expectedResult.WorkDir)
	}
	if vr.ExitCode != expectedResult.ExitCode {
		t.Errorf("ExitCode = %d, want %d", vr.ExitCode, expectedResult.ExitCode)
	}
	if vr.Stdout != expectedResult.Stdout {
		t.Errorf("Stdout = %q, want %q", vr.Stdout, expectedResult.Stdout)
	}
	if vr.Stderr != expectedResult.Stderr {
		t.Errorf("Stderr = %q, want %q", vr.Stderr, expectedResult.Stderr)
	}
	if vr.Duration != expectedResult.Duration {
		t.Errorf("Duration = %v, want %v", vr.Duration, expectedResult.Duration)
	}
	if vr.TimedOut != expectedResult.TimedOut {
		t.Errorf("TimedOut = %v, want %v", vr.TimedOut, expectedResult.TimedOut)
	}
}
