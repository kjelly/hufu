package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/tools"
)

func withExecutionRootForTest(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, tools.AgentExecutionRootKey, tools.AgentExecutionRoot{Root: root})
}

func TestAttemptProjectRootFollowsExecutionRoot(t *testing.T) {
	c := &Coordinator{projectDir: "/canonical"}
	if got := c.attemptProjectRoot(context.Background()); got != "/canonical" {
		t.Fatalf("attemptProjectRoot without a root = %q", got)
	}
	if got := c.attemptProjectRoot(withExecutionRootForTest(context.Background(), "/attempt/root")); got != "/attempt/root" {
		t.Fatalf("attemptProjectRoot with a root = %q", got)
	}
}

// Verification runs where the attempt's tools run, while its fingerprint
// stays on the canonical project so repeated-failure detection still sees
// the same failure across attempts with different roots.
func TestVerificationRunsInAttemptRootWithCanonicalFingerprint(t *testing.T) {
	canonical, attemptRoot := t.TempDir(), t.TempDir()
	c := &Coordinator{projectDir: canonical}
	result, err := c.verifyTaskDeliverableWithMode(withExecutionRootForTest(context.Background(), attemptRoot), nil, "pwd", "success")
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !strings.Contains(result.Stdout, attemptRoot) {
		t.Fatalf("verification stdout = %#v, want it to run in %s", result, attemptRoot)
	}
	spec := VerificationSpec{Type: VerifyCommandExit, Mode: "success", Command: "pwd"}
	if want := ComputeVerificationFingerprint(spec, result, canonical); result.Fingerprint != want {
		t.Fatalf("fingerprint = %q, want the canonical-root fingerprint %q", result.Fingerprint, want)
	}
	other, err := c.verifyTaskDeliverableWithMode(withExecutionRootForTest(context.Background(), t.TempDir()), nil, "true", "success")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.verifyTaskDeliverableWithMode(withExecutionRootForTest(context.Background(), t.TempDir()), nil, "true", "success")
	if err != nil {
		t.Fatal(err)
	}
	if other.Fingerprint != again.Fingerprint {
		t.Fatalf("fingerprints differ across attempt roots: %q vs %q", other.Fingerprint, again.Fingerprint)
	}
}

func TestWorkerPromptNamesAttemptRoot(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	def := c.session.Agents["worker"]
	prompt := c.injectWorkerContext(withExecutionRootForTest(context.Background(), "/attempt/root"), def).System
	if !strings.Contains(prompt, "Project root (CWD): /attempt/root") {
		t.Fatalf("worker prompt does not name the attempt root: %q", prompt)
	}
	canonical := c.injectWorkerContext(context.Background(), def).System
	if !strings.Contains(canonical, "Project root (CWD): "+c.projectDir) {
		t.Fatalf("worker prompt without a root = %q", canonical)
	}
}
