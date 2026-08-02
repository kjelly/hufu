package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

// Minimum Test Matrix Item 5: worker says success while json_assert sees a failed value
func TestVerification_JSONAssert_WorkerSuccessValueFailed(t *testing.T) {
	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "output.json")
	if err := os.WriteFile(jsonFile, []byte(`{"status": "error", "code": 500}`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "output.json",
		Assertions: []JSONAssertion{
			{Path: "status", Equals: "ok"},
		},
	}

	res, err := ExecuteVerificationSpec(context.Background(), "sh", dir, spec)
	if err == nil {
		t.Fatalf("expected json_assert verification error when status is error, got nil")
	}
	if res == nil || res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code for failed json_assert, got %#v", res)
	}
}

func TestVerification_JSONAssertDoesNotCoerceStringToNumber(t *testing.T) {
	if equalJSONValues(float64(42), "42") {
		t.Fatal("JSON scalar equality must not coerce a numeric string to a number")
	}
	if !equalJSONValues(float64(42), 42) {
		t.Fatal("JSON numeric equality should accept equivalent decoded numeric types")
	}
	if !equalJSONValues(json.Number("42"), int8(42)) || !equalJSONValues(json.Number("42"), uint16(42)) {
		t.Fatal("all supported integer scalar widths must compare exactly to JSON numbers")
	}
}

func TestVerification_JSONAssertPreservesLargeIntegerPrecision(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"id":9007199254740993}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Exact scalar equality must not round the JSON value through float64 and
	// accidentally accept the adjacent representable integer.
	wrong := VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "result.json",
		Assertions: []JSONAssertion{{
			Path:   "id",
			Equals: int64(9007199254740992),
		}},
	}
	if _, err := ExecuteVerificationSpec(context.Background(), "sh", dir, wrong); err == nil {
		t.Fatal("json_assert must reject an adjacent integer above float64 precision")
	}

	right := wrong
	right.Assertions = []JSONAssertion{{Path: "id", Equals: int64(9007199254740993)}}
	if _, err := ExecuteVerificationSpec(context.Background(), "sh", dir, right); err != nil {
		t.Fatalf("json_assert must accept the exact large integer: %v", err)
	}
}

func TestVerification_JSONAssertCommandUsesUntruncatedOutput(t *testing.T) {
	dir := t.TempDir()
	const payload = "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"
	if err := os.WriteFile(filepath.Join(dir, "large.json"), []byte(`{"status":"ok","payload":"`+payload+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type:    VerifyJSONAssert,
		Command: "cat large.json",
		Assertions: []JSONAssertion{{
			Path:   "status",
			Equals: "ok",
		}},
	})
	if err != nil {
		t.Fatalf("json_assert must parse complete command output: %v", err)
	}
	if result == nil || len(result.Stdout) > 2000 {
		t.Fatalf("durable command evidence must stay bounded, got %#v", result)
	}
}

func TestVerification_JSONAssertRejectsTrailingJSONData(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"status":"ok"} {"extra":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type:       VerifyJSONAssert,
		Path:       "result.json",
		Assertions: []JSONAssertion{{Path: "status", Equals: "ok"}},
	})
	if err == nil {
		t.Fatal("json_assert must reject input containing more than one JSON document")
	}
}

// Minimum Test Matrix Item 6: legacy shell verifier that prints failure but exits zero retains shell exit semantics and emits weak-verifier warning
func TestVerification_CommandExit_WeakVerifierWarning(t *testing.T) {
	dir := t.TempDir()
	spec := VerificationSpec{
		Type:    VerifyCommandExit,
		Command: "echo 'status: failed' && exit 0",
	}

	res, err := ExecuteVerificationSpec(context.Background(), "sh", dir, spec)
	if err != nil {
		t.Fatalf("expected command exit 0 to pass verification, got error: %v", err)
	}
	if res == nil || res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %#v", res)
	}
	if !res.WeakWarning {
		t.Fatalf("expected weak verifier warning to be set when output contains 'status: failed'")
	}
	if res.WeakReason == "" {
		t.Fatalf("expected non-empty weak verifier reason")
	}
	if res.rawStdout != "" {
		t.Fatal("ordinary command verification must not retain unbounded raw stdout")
	}
}

func TestVerification_CommandExit_UnresolvedPipelineStageIsEnvironmentNotPolarity(t *testing.T) {
	// The literal angle-bracket token mirrors the incident shape recorded in
	// the reliability spec. Its diagnostic would be swallowed by grep if the
	// shell were allowed to run the pipeline.
	const command = "<missing> show 2>&1 | grep -c running"
	res, err := ExecuteVerificationSpec(context.Background(), "sh", t.TempDir(), VerificationSpec{
		Type:    VerifyCommandExit,
		Command: command,
	})
	if err == nil {
		t.Fatal("expected unresolved pipeline executable to fail before shell execution")
	}
	if res == nil || res.ExitCode != 127 {
		t.Fatalf("expected an unresolved executable result with exit 127, got %#v", res)
	}
	combined := strings.ToLower(res.Stderr + "\n" + err.Error())
	if !strings.Contains(combined, "executable unresolved") {
		t.Fatalf("expected environment evidence, got result=%#v err=%v", res, err)
	}
	if !strings.Contains(combined, "explicit relative path") {
		t.Fatalf("expected actionable explicit-path hint, got result=%#v err=%v", res, err)
	}
	if strings.Contains(combined, "polarity") || strings.Contains(combined, "wrong polarity") {
		t.Fatalf("unresolved pipeline must not emit polarity diagnostics: result=%#v err=%v", res, err)
	}
	if got := ClassifyTaskFailureStructured(FailureClassificationInput{
		Err:             err,
		ExitCode:        res.ExitCode,
		ExitCodeSource:  ExitCodeSourceVerify,
		ResolveFindings: environmentFindingsFromVerifyResult(res),
	}); got != FailureEnvironment {
		t.Fatalf("unresolved pipeline classified as %q, want environment", got)
	}
	if isUnfixableVerifyFailure(err) {
		t.Fatal("unresolved pipeline must not take the unfixable-polarity retry path")
	}
}

func TestVerification_UnresolvedPipelineStageNeverAcceptedByNonSuccessModes(t *testing.T) {
	for _, mode := range []string{"expected_failure", "observation"} {
		t.Run(mode, func(t *testing.T) {
			res, err := ExecuteVerificationSpec(context.Background(), "sh", t.TempDir(), VerificationSpec{
				Type:    VerifyCommandExit,
				Mode:    mode,
				Command: "definitely_missing_cmd_99999 show 2>&1 | grep -c running",
			})
			if err == nil {
				t.Fatalf("unresolved verifier in %s mode must remain an error", mode)
			}
			if res == nil || res.ExitCode != 127 {
				t.Fatalf("unresolved verifier in %s mode = %#v, want exit 127", mode, res)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "executable unresolved") {
				t.Fatalf("unresolved verifier in %s mode lost environment evidence: %v", mode, err)
			}
			if got := ClassifyTaskFailureStructured(FailureClassificationInput{
				Err:             err,
				ExitCode:        res.ExitCode,
				ExitCodeSource:  ExitCodeSourceVerify,
				ResolveFindings: environmentFindingsFromVerifyResult(res),
			}); got != FailureEnvironment {
				t.Fatalf("unresolved verifier in %s mode classified as %q, want environment", mode, got)
			}
		})
	}
}

func TestVerification_CaseBodyUnresolvedPipelineStageIsEnvironmentNotPolarity(t *testing.T) {
	const command = "case x in x) definitely_missing_cmd_99999 | grep -c running ;; esac"
	res, err := ExecuteVerificationSpec(context.Background(), "sh", t.TempDir(), VerificationSpec{
		Type:    VerifyCommandExit,
		Command: command,
	})
	if err == nil || res == nil || res.ExitCode != 127 {
		t.Fatalf("case body unresolved verifier = result %#v, err %v; want exit 127 error", res, err)
	}
	combined := strings.ToLower(res.Stderr + "\n" + err.Error())
	if !strings.Contains(combined, "executable unresolved") {
		t.Fatalf("case body lost environment evidence: result=%#v err=%v", res, err)
	}
	if strings.Contains(combined, "polarity") || isUnfixableVerifyFailure(err) {
		t.Fatalf("case body unresolved command must not take polarity path: result=%#v err=%v", res, err)
	}
}

func TestVerification_CaseCommandSubstitutionSelectorIsValid(t *testing.T) {
	res, err := ExecuteVerificationSpec(context.Background(), "sh", t.TempDir(), VerificationSpec{
		Type:    VerifyCommandExit,
		Command: `case "$(printf x)" in x) true ;; *) false ;; esac`,
	})
	if err != nil || res == nil || res.ExitCode != 0 {
		t.Fatalf("valid case command-substitution verifier = result %#v, err %v; want exit 0", res, err)
	}
}

func TestVerification_CaseCommandSubstitutionSelectorStillRejectsUnresolvedBody(t *testing.T) {
	res, err := ExecuteVerificationSpec(context.Background(), "sh", t.TempDir(), VerificationSpec{
		Type:    VerifyCommandExit,
		Command: `case "$(printf x)" in x) definitely_missing_cmd_99999 ;; *) false ;; esac`,
	})
	if err == nil || res == nil || res.ExitCode != 127 {
		t.Fatalf("case selector unresolved body = result %#v, err %v; want exit 127 error", res, err)
	}
	combined := strings.ToLower(res.Stderr + "\n" + err.Error())
	if !strings.Contains(combined, "executable unresolved") {
		t.Fatalf("case selector lost unresolved-body evidence: result=%#v err=%v", res, err)
	}
}

func TestVerification_CaseCommandSubstitutionSelectorMismatchIsNonZero(t *testing.T) {
	res, err := ExecuteVerificationSpec(context.Background(), "sh", t.TempDir(), VerificationSpec{
		Type:    VerifyCommandExit,
		Command: `case "$(printf y)" in x) true ;; *) false ;; esac`,
	})
	if err == nil || res == nil || res.ExitCode == 0 {
		t.Fatalf("unmatched case selector = result %#v, err %v; want non-zero exit", res, err)
	}
}

func TestVerification_CommandExitRejectsExplicitFailureOutput(t *testing.T) {
	dir := t.TempDir()
	res, err := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type:    VerifyCommandExit,
		Command: "echo 'failed -1' && exit 0",
	})
	if err == nil {
		t.Fatal("explicit failed result must fail verification even when wrapper exits 0")
	}
	if res == nil || res.ExitCode == 0 {
		t.Fatalf("expected non-zero verification result, got %#v", res)
	}
}

// Minimum Test Matrix Item 7: observation records stdout and exit status but cannot satisfy a mandatory criterion
func TestVerification_Observation_CannotSatisfyMandatoryCriterion(t *testing.T) {
	dir := t.TempDir()
	spec := VerificationSpec{
		Type:    VerifyCommandExit,
		Mode:    "observation",
		Command: "echo 'hello observation' && exit 0",
	}

	res, err := ExecuteVerificationSpec(context.Background(), "sh", dir, spec)
	if err != nil {
		t.Fatalf("observation mode should record evidence without error on exit 0, got: %v", err)
	}
	if res == nil || res.Stdout != "hello observation" {
		t.Fatalf("unexpected observation result: %#v", res)
	}

	// Acceptance check with observation mode must fail because observation cannot satisfy mandatory acceptance
	c := &Coordinator{
		session: &TeamSession{
			Config: agent.TeamConfig{
				Name: "test",
				AcceptanceSpec: &agent.AcceptanceSpec{
					Verifications: []agent.VerificationSpec{spec},
				},
			},
		},
		acceptanceSpec: &agent.AcceptanceSpec{
			Verifications: []agent.VerificationSpec{spec},
		},
		projectDir:  dir,
		taskTracker: NewTaskTracker(),
	}

	accRes, accErr := c.runAcceptance(context.Background())
	if accErr == nil {
		t.Fatalf("expected acceptance check to fail when only observation verifications exist, got success")
	}
	if accRes == nil || accRes.Passed {
		t.Fatalf("expected acceptance Passed = false for observation-only acceptance spec, got %#v", accRes)
	}
}

func TestVerification_ObservationMayAccompanyMandatoryAcceptance(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		acceptanceSpec: &agent.AcceptanceSpec{Verifications: []agent.VerificationSpec{
			{Type: agent.VerifyCommandExit, Mode: "success", Command: "true"},
			{Type: agent.VerifyCommandExit, Mode: "observation", Command: "echo observed"},
		}},
		projectDir:  dir,
		taskTracker: NewTaskTracker(),
	}

	result, err := c.runAcceptance(context.Background())
	if err != nil {
		t.Fatalf("a passing mandatory acceptance check must not be blocked by an observation: %v", err)
	}
	if result == nil || !result.Passed || result.State != AcceptancePassed {
		t.Fatalf("expected passing acceptance with supplemental observation, got %#v", result)
	}
	if len(result.VerificationEvidence) != 2 {
		t.Fatalf("expected both mandatory and observation evidence, got %#v", result.VerificationEvidence)
	}
}

func TestVerification_MalformedObservationFailsAcceptanceClosed(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		acceptanceSpec: &agent.AcceptanceSpec{Verifications: []agent.VerificationSpec{
			{Type: agent.VerifyCommandExit, Mode: "success", Command: "true"},
			// A malformed observation is a configuration error, not benign
			// observation evidence. The valid mandatory verifier above must not
			// permit the acceptance gate to pass it over.
			{Type: agent.VerifyJSONAssert, Mode: "observation", Path: "result.json"},
		}},
		projectDir:  dir,
		taskTracker: NewTaskTracker(),
	}

	result, err := c.runAcceptance(context.Background())
	if err == nil {
		t.Fatal("malformed observation verifier must fail the acceptance gate closed")
	}
	if result == nil || result.Passed || result.State != AcceptanceFailed {
		t.Fatalf("malformed observation verifier must fail acceptance, got %#v", result)
	}
	if len(result.VerificationEvidence) != 2 {
		t.Fatalf("expected evidence for both valid and malformed verifiers, got %#v", result.VerificationEvidence)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "malformed") {
		t.Fatalf("expected malformed verifier failure detail, got %#v", result.Errors)
	}
}

func TestVerification_AcceptanceExpectedFailureCanSatisfyCriterion(t *testing.T) {
	dir := t.TempDir()
	spec := VerificationSpec{
		Type:    VerifyCommandExit,
		Mode:    "expected_failure",
		Command: "exit 1",
	}

	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		acceptanceSpec: &agent.AcceptanceSpec{
			Verifications: []agent.VerificationSpec{spec},
		},
		projectDir:  dir,
		taskTracker: NewTaskTracker(),
	}

	result, err := c.runAcceptance(context.Background())
	if err != nil {
		t.Fatalf("expected_failure acceptance verification should pass: %v", err)
	}
	if result == nil || !result.Passed || result.State != AcceptancePassed {
		t.Fatalf("expected passed acceptance result, got %#v", result)
	}
	if len(result.VerificationEvidence) != 1 || result.VerificationEvidence[0].ExitCode != 1 {
		t.Fatalf("expected non-zero expected_failure evidence to be retained, got %#v", result.VerificationEvidence)
	}
}

func TestVerification_EmptyAcceptanceSpecIsNotConfigured(t *testing.T) {
	c := &Coordinator{
		session:        &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		acceptanceSpec: &agent.AcceptanceSpec{},
		taskTracker:    NewTaskTracker(),
	}

	result, err := c.runAcceptance(context.Background())
	if err != nil {
		t.Fatalf("empty acceptance spec should be a no-op: %v", err)
	}
	if result == nil || result.State != AcceptanceNotConfigured || result.Passed {
		t.Fatalf("empty acceptance spec must remain not configured, got %#v", result)
	}
}

// Minimum Test Matrix Item 8: file existence and absence assertions handle relative and absolute paths consistently
func TestVerification_FileExistsAndAbsent_RelativeAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	relFile := "reports/summary.md"
	absFile := filepath.Join(dir, relFile)

	if err := os.MkdirAll(filepath.Dir(absFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absFile, []byte("summary content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Relative path file_exists
	resRelExist, errRelExist := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: VerifyFileExists,
		Path: relFile,
	})
	if errRelExist != nil || resRelExist.ExitCode != 0 {
		t.Fatalf("relative file_exists failed: %v, %#v", errRelExist, resRelExist)
	}

	// 2. Absolute path file_exists
	resAbsExist, errAbsExist := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: VerifyFileExists,
		Path: absFile,
	})
	if errAbsExist != nil || resAbsExist.ExitCode != 0 {
		t.Fatalf("absolute file_exists failed: %v, %#v", errAbsExist, resAbsExist)
	}

	// 3. Relative path file_absent (should pass when missing)
	resRelAbsent, errRelAbsent := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: VerifyFileAbsent,
		Path: "reports/missing.md",
	})
	if errRelAbsent != nil || resRelAbsent.ExitCode != 0 {
		t.Fatalf("relative file_absent failed: %v, %#v", errRelAbsent, resRelAbsent)
	}

	// 4. Absolute path file_absent (should pass when missing)
	resAbsAbsent, errAbsAbsent := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: VerifyFileAbsent,
		Path: filepath.Join(dir, "reports/missing.md"),
	})
	if errAbsAbsent != nil || resAbsAbsent.ExitCode != 0 {
		t.Fatalf("absolute file_absent failed: %v, %#v", errAbsAbsent, resAbsAbsent)
	}

	// 5. File absent when file actually exists should fail
	_, errExistAbsent := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: VerifyFileAbsent,
		Path: relFile,
	})
	if errExistAbsent == nil {
		t.Fatalf("expected file_absent to fail when file exists")
	}
}

// Minimum Test Matrix Item 9: malformed typed verification fails closed
func TestVerification_Malformed_FailsClosed(t *testing.T) {
	dir := t.TempDir()

	// 1. Invalid verification type
	res1, err1 := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: "invalid_type",
	})
	if err1 == nil {
		t.Fatalf("expected error for invalid verification type")
	}
	if res1 == nil || res1.ExitCode != -1 {
		t.Fatalf("expected exit code -1 for malformed spec, got %#v", res1)
	}

	// 2. json_assert with no path and no command
	res2, err2 := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: VerifyJSONAssert,
		Assertions: []JSONAssertion{
			{Path: "foo", Equals: "bar"},
		},
	})
	if err2 == nil {
		t.Fatalf("expected error for json_assert missing path and command")
	}
	if res2 == nil || res2.ExitCode != -1 {
		t.Fatalf("expected exit code -1 for malformed json_assert, got %#v", res2)
	}

	// 3. file_exists with empty path
	res3, err3 := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: VerifyFileExists,
		Path: "",
	})
	if err3 == nil {
		t.Fatalf("expected error for file_exists with empty path")
	}
	if res3 == nil || res3.ExitCode != -1 {
		t.Fatalf("expected exit code -1 for file_exists missing path, got %#v", res3)
	}
}

func TestVerification_JSONAssertRejectsNonScalarExpectedValue(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "output.json"), []byte(`{"state":{"ready":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "output.json",
		Assertions: []JSONAssertion{{
			Path:   "state",
			Equals: map[string]any{"ready": true},
		}},
	})
	if err == nil {
		t.Fatal("expected non-scalar json_assert value to fail closed")
	}
	if res == nil || res.ExitCode != -1 {
		t.Fatalf("expected malformed typed verifier result with exit code -1, got %#v", res)
	}
}

func TestVerification_InvalidModeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "must-not-run")
	res, err := ExecuteVerificationSpec(context.Background(), "sh", dir, VerificationSpec{
		Type:    VerifyCommandExit,
		Mode:    "not-a-mode",
		Command: "touch " + marker,
	})
	if err == nil {
		t.Fatal("expected an invalid verification mode to fail closed")
	}
	if res == nil || res.ExitCode != -1 {
		t.Fatalf("expected malformed verifier result with exit code -1, got %#v", res)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("invalid typed verifier must be rejected before its command runs; stat error = %v", statErr)
	}
}

func TestVerification_EvidenceFingerprint(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "data.json")
	if err := os.WriteFile(filePath, []byte(`{"value": 42}`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "data.json",
		Assertions: []JSONAssertion{
			{Path: "value", Equals: 42},
		},
	}

	res, err := ExecuteVerificationSpec(context.Background(), "sh", dir, spec)
	if err != nil {
		t.Fatalf("json_assert failed: %v", err)
	}

	if res.Fingerprint == "" || !strings.HasPrefix(res.Fingerprint, "vfp_") {
		t.Fatalf("invalid verification fingerprint: %q", res.Fingerprint)
	}
}
