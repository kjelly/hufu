package improver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/providerproxy"
	"github.com/kjelly/hufu/internal/scripted"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
)

func TestRuntimeFactoryOwnsCopiesAndComparesPolicies(t *testing.T) {
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewRuntimeFactory(prepared, RuntimeSettings{ProviderURL: scripted.Endpoint})
	if err != nil {
		t.Fatal(err)
	}
	models, err := factory.Preflight()
	if err != nil {
		t.Fatal(err)
	}
	// Mutating caller-owned state after construction cannot alter a run.
	prepared.Config.Worker = "other"
	prepared.Schema[0] = '!'
	prepared.Variants["baseline"].Tree.files["team.yaml"][0] = '!'
	root := t.TempDir()
	objects, err := OpenObjects(root, "evidence", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	var digest string
	for _, slot := range []string{"baseline", "candidate_1"} {
		var calls atomic.Int64
		p := scripted.NewProvider("scripted", []scripted.Step{{
			ToolCall: &scripted.ToolCall{Name: "submit_result", Arguments: `{"status":"success","summary":"classified","structured_payload":{"category":"allowed"}}`},
			Usage:    &scripted.Usage{InputTokens: 100, OutputTokens: 20},
		}})
		bound := false
		runtime, err := factory.open(filepath.Join(root, slot), slot, "executor", func(ctx context.Context, name string, _ providerproxy.Config) (providerproxy.Boundary, error) {
			calls.Add(1)
			if !bound || name != "local" {
				t.Error("transport opened before parent binding or for an unexpected provider")
			}
			return scripted.NewBoundary(ctx, p)
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = runtime.close() })
		if calls.Load() != 0 {
			t.Fatal("preflight started a provider")
		}
		if digest == "" {
			digest = runtime.policy.ComparisonDigest
		} else if digest != runtime.policy.ComparisonDigest {
			t.Fatalf("paired policies differ: %s != %s", digest, runtime.policy.ComparisonDigest)
		}
		actualPolicy, err := runtimePolicyDigest(runtime.policy, runtime.overlay)
		if err != nil || actualPolicy != models["executor"].PolicyDigest {
			t.Fatalf("runtime policy drifted from preflight: %v", err)
		}
		if runtime.session.Workspace == runtime.session.Scope.SubjectRoot || !strings.HasPrefix(runtime.session.Workspace, runtime.root) {
			t.Fatal("control workspace is not independently owned")
		}
		var identity paired.RuntimeIdentity
		ctx := team.WithInvocationBinding(context.WithValue(t.Context(), tools.UnattendedKey, true), func(_ context.Context, id team.InvocationIdentity) error {
			if id.InvocationID == "" || id.RunID == "" || id.BranchID == "" {
				t.Fatal("missing canonical child identity")
			}
			bound = true
			identity = paired.RuntimeIdentity{InvocationID: id.InvocationID, RunID: id.RunID, BranchID: id.BranchID}
			return nil
		})
		task := runtime.task("extractor", "Classify the supplied item.", prepared.Fixture.Cases[0].Verify)
		_, runErr := runtime.coordinator.RunTask(ctx, task)
		result := runtime.coordinator.LastRunResult()
		items := runtime.coordinator.TaskTracker().TodoList().Items()
		if result == nil || result.Outcome != team.RunOutcomeUnverified || result.StopReason != team.StopReasonAcceptanceNotSet ||
			len(items) != 1 || items[0].Status != team.TaskDone || items[0].VerifyResult == nil || items[0].VerifyResult.ExitCode != 0 || items[0].VerifyResult.TimedOut {
			t.Fatalf("canonical task verification failed: result=%+v items=%+v err=%v", result, items, runErr)
		}
		if calls.Load() != 1 || len(p.Issues()) != 0 {
			t.Fatalf("provider startup/cursor mismatch: calls=%d issues=%+v", calls.Load(), p.Issues())
		}
		if _, err := importChildEvidence(t.Context(), objects, runtime, identity); err == nil {
			t.Fatal("import accepted an undrained runtime")
		}
		if err := runtime.close(); err != nil {
			t.Fatal(err)
		}
		evidence, err := importChildEvidence(t.Context(), objects, runtime, identity)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := inspectChildEvidence(t.Context(), objects, evidence)
		if err != nil {
			t.Fatal(err)
		}
		if !observation.usageKnown || observation.tokens != 120 || observation.calls != 1 {
			t.Fatalf("usage presence or accounting drift: %+v", observation)
		}
		// Imported evidence is self-contained and still checked after the
		// disposable child tree is gone.
		if err := os.RemoveAll(runtime.root); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectChildEvidence(t.Context(), objects, evidence); err != nil {
			t.Fatal(err)
		}
		tampered := evidence
		tampered.Identity.RunID = "foreign"
		if _, err := inspectChildEvidence(t.Context(), objects, tampered); err == nil {
			t.Fatal("import accepted a foreign runtime identity")
		}
	}
}

func TestRuntimeFactoryRejectsReuseAndLiveFallback(t *testing.T) {
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewRuntimeFactory(prepared, RuntimeSettings{ProviderURL: scripted.Endpoint})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	path := filepath.Join(parent, "child")
	if _, err := factory.open(path, "baseline", "executor", nil); err == nil {
		t.Fatal("fixture accepted ordinary live provider")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid transport created a child")
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	_, err = factory.open(path, "baseline", "executor", func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
		calls.Add(1)
		return nil, nil
	})
	if err == nil || calls.Load() != 0 {
		t.Fatal("existing child reused")
	}
}
