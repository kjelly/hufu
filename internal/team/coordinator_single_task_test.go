package team

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/providerproxy"
	"github.com/kjelly/hufu/internal/scripted"
	"github.com/kjelly/hufu/internal/tools"
)

func TestRunTaskStructuredOfflineLifecycle(t *testing.T) {
	for _, category := range []string{"allowed", "denied"} {
		t.Run(category, func(t *testing.T) {
			root := t.TempDir()
			teamDir := filepath.Join(root, "team")
			control := filepath.Join(root, "control")
			subject := filepath.Join(root, "subject")
			for _, dir := range []string{teamDir, control, subject} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			const manifest = `name: single_task
worker-memory: {mode: off}
memory-learning: {mode: off}
semantic-retrieval: {mode: off}
agents:
  worker:
    preset: readonly
    tools:
      allowed: [submit_result]
      denied: [view, grep, glob, ls]
    instructions: Return the requested structured result.
`
			if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			session, err := LoadTeamWithOptions(teamDir, nil, nil, nil, TeamLoadOptions{SkillSearchPaths: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			session.Workspace = control
			if err := session.SetCompatibilityWorkspaceScope(subject); err != nil {
				t.Fatal(err)
			}
			generation := agent.GenerationParams{Model: "local/scripted", ContextWindow: 32768, MaxTokens: "1024", Temperature: "0", TopP: "1"}
			session.Config.Generation = generation
			session.Config.WorkerModel = generation.Model
			worker := session.Agents["worker"]
			worker.Generation = generation
			worker.MaxRetries = 0
			session.Config.MaxRetries = 0
			worker.Memory.Mode = agent.WorkerMemoryOff
			schema, err := CompileResultContractDocument("result.json", []byte(`{"type":"object","required":["category"],"properties":{"category":{"type":"string","enum":["allowed","denied"]}},"additionalProperties":false}`))
			if err != nil {
				t.Fatal(err)
			}
			session.ResultContracts = map[string]*CompiledResultContract{schema.ID: schema}
			session.AgentResultContracts = map[string]ResultContractRef{"worker": schema.ref(true)}
			p := scripted.NewProvider("scripted", []scripted.Step{{
				ToolCall: &scripted.ToolCall{Name: "submit_result", Arguments: `{"status":"success","summary":"test result","structured_payload":{"category":"` + category + `"}}`},
				Usage:    &scripted.Usage{InputTokens: 100, OutputTokens: 20},
			}})
			var bound atomic.Bool
			var starts atomic.Int64
			factory := func(ctx context.Context, _ string, _ providerproxy.Config) (providerproxy.Boundary, error) {
				if !bound.Load() {
					t.Error("provider started before identity was committed")
				}
				starts.Add(1)
				return scripted.NewBoundary(ctx, p)
			}
			c, err := NewCoordinator(session, scripted.Endpoint, "", nil, nil, RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false, WithProviderBoundaryFactory(factory))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			c.SetSessionData(NewSession())
			c.SetBudget(30, 12000)
			if err := c.FreezeExecutionPolicyAtStartup(); err != nil {
				t.Fatal(err)
			}
			var identity InvocationIdentity
			ctx := WithInvocationBinding(context.WithValue(t.Context(), tools.UnattendedKey, true), func(ctx context.Context, id InvocationIdentity) error {
				identity = id
				if err := c.verifyInvocationBindingIdentity(ctx, id); err != nil {
					return err
				}
				bound.Store(true)
				return nil
			})
			task := TaskDef{
				Agent: "worker", Goal: "Classify the input.", SideEffect: SideEffectNone,
				Execution: ExecutionContract{Kind: ExecutionKindInline, RequiresResult: true, RequiresGroundedResult: true, RequiresVerification: true,
					ToolSequence: []string{"submit_result"}},
				VerifySpec: &VerificationSpec{Type: "task_result_assert", Mode: "success",
					TaskResultAssertions: []TaskResultAssertion{{Pointer: "/structured_payload/value/category", Op: "equals", Value: "allowed"}}},
			}
			_, runErr := c.RunTask(ctx, task)
			if starts.Load() != 1 || len(p.Issues()) != 0 {
				t.Fatalf("starts=%d script=%+v err=%v", starts.Load(), p.Issues(), runErr)
			}
			result := c.LastRunResult()
			items := c.TaskTracker().TodoList().Items()
			if result == nil || result.RunID != identity.RunID || len(items) != 1 || !c.TerminalLifecycleConfirmed() {
				t.Fatalf("missing canonical terminal evidence: result=%+v tasks=%+v err=%v", result, items, runErr)
			}
			if category == "allowed" {
				if runErr == nil || result.Outcome != RunOutcomeUnverified || result.StopReason != StopReasonAcceptanceNotSet || items[0].Status != TaskDone {
					for _, receipt := range items[0].ExecutionReceipts {
						store, err := OpenFileArtifactStoreReadOnly(control, control)
						if err != nil {
							t.Fatal(err)
						}
						file, err := store.Open(context.WithoutCancel(ctx), receipt.TranscriptRef)
						if err == nil {
							raw, _ := io.ReadAll(file)
							_ = file.Close()
							t.Log(string(raw))
						} else {
							t.Log(err)
						}
					}
					t.Fatalf("valid result not accepted: status=%s detail=%s err=%v", items[0].Status, items[0].Detail, runErr)
				}
			} else if runErr == nil || items[0].Status == TaskDone {
				t.Fatalf("failed assertion accepted: task=%+v err=%v", items[0], runErr)
			}
			if _, err := c.RunTask(t.Context(), task); err == nil || !strings.Contains(err.Error(), "fresh task scope") {
				t.Fatalf("replayed completed scope: %v", err)
			}
			if c.DualWriteFailures() != 0 {
				t.Fatal("canonical persistence failed")
			}
		})
	}
}
