# Direct Agent Tool Allowlist Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make direct and fast-path worker runs receive the same effective tool allowlist as coordinated tasks.

**Architecture:** Centralize the merge of `TeamConfig.ToolsAllowed` and an agent definition's comma-separated `Tools` field in `Coordinator`. Apply it to the per-task context before calling `runAgentWithStatusAndHistory`. `executeTask`, `ExecuteSubAgent`, and `RunDirectAgent` consume the same helper so future policy changes cannot split the execution paths again.

**Tech Stack:** Go 1.26, `context.Context`, Go standard `testing` package.

## Global Constraints

- Preserve deny-by-default behavior when neither the team nor agent declares any tools.
- Preserve existing direct-agent timeout, identity, guard, network, and path context values.
- Do not modify unrelated, pre-existing working-tree changes.
- Verify a default Helper with `bash` also retains its implied `wait_for` permission.

---

## File Structure

- Modify `internal/team/coordinator_task_run.go`: add a Coordinator helper that derives and attaches the effective runtime tool allowlist.
- Modify `internal/team/coordinator_run.go`: attach that allowlist in `RunDirectAgent`, the path used by the one-worker fast path.
- Modify `internal/team/coordinator_tools_delegate.go`: replace duplicate sub-agent allowlist construction with the shared helper.
- Modify `internal/team/*_test.go`: add a regression test that observes the effective context used by a direct Helper run without calling an external LLM.

### Task 1: Add a failing direct-agent allowlist regression test

**Files:**

- Modify: `internal/team/coordinator_run_test.go` (create if the package has no focused direct-run test file)

**Interfaces:**

- Consumes: `LoadDefaultTeam(workspace, nil, "bash")` and `tools.GetToolsAllowed(ctx)`.
- Produces: a test proving the direct-run context has `view`, `bash`, and `wait_for` before any LLM tool call occurs.

- [ ] **Step 1: Write the failing test**

Create a test-only `Coordinator` setup that uses `LoadDefaultTeam(t.TempDir(), nil, "bash")`, invokes the direct-run context setup through the smallest exposed/internal seam, and asserts the literal expected allowlist members:

```go
for _, want := range []string{"view", "bash", "wait_for"} {

    if !slices.Contains(tools.GetToolsAllowed(taskCtx), want) {
        t.Fatalf("direct Helper allowlist = %v, missing %q", tools.GetToolsAllowed(taskCtx), want)
    }
}
```

The test must fail if the direct path omits `AgentToolsAllowedKey`; it must not inspect source text or assert a mock call count.

- [ ] **Step 2: Run the focused test to verify it fails**

Run:

```bash
go test ./internal/team -run '^TestRunDirectAgent_HelperToolsPopulateRuntimeAllowlist$' -count=1
```

Expected: FAIL because the direct execution context has no tools allowlist.

### Task 2: Centralize effective tool allowlist propagation

**Files:**

- Modify: `internal/team/coordinator_task_run.go:350-367,1237-1305`
- Modify: `internal/team/coordinator_run.go:100-135`
- Modify: `internal/team/coordinator_tools_delegate.go:243-253`

**Interfaces:**

- Produces: `func (c *Coordinator) withEffectiveToolsAllowed(ctx context.Context, def *agent.AgentDef) context.Context`.
- Consumes: `c.session.Config.ToolsAllowed`, `def.Tools`, and `agent.ExpandImpliedTools`-normalized agent tool strings.
- Guarantees: a non-empty effective list is attached under `tools.AgentToolsAllowedKey`; an empty policy leaves the context unchanged.

- [ ] **Step 1: Write the minimal implementation**

Implement the helper using the existing semantics, then replace each duplicated merge block and call it in `RunDirectAgent` after its task-specific context values are attached:

```go
func (c *Coordinator) withEffectiveToolsAllowed(ctx context.Context, def *agent.AgentDef) context.Context {
    allowed := append([]string(nil), c.session.Config.ToolsAllowed...)
    if def != nil {
        for _, name := range strings.Split(def.Tools, ",") {
            if name = strings.TrimSpace(name); name != "" {
                allowed = append(allowed, name)
            }
        }
    }
    if len(allowed) == 0 {
        return ctx
    }
    return context.WithValue(ctx, tools.AgentToolsAllowedKey, allowed)
}
```

Use `taskCtx = c.withEffectiveToolsAllowed(taskCtx, agentDef)` in the direct run and equivalent shared calls in normal-task/sub-agent paths. Do not change the special coordinator-only allowlist.

- [ ] **Step 2: Run the focused test to verify it passes**

Run:

```bash
go test ./internal/team -run '^TestRunDirectAgent_HelperToolsPopulateRuntimeAllowlist$' -count=1
```

Expected: PASS; the observed direct context contains `view`, `bash`, and `wait_for`.

### Task 3: Verify policy parity and project health

**Files:**

- Modify: only files from Tasks 1-2 if test refinements are needed.

**Interfaces:**

- Consumes: the shared `withEffectiveToolsAllowed` helper.
- Produces: evidence that existing normal-task, default-team, and permission behavior remains intact.

- [ ] **Step 1: Run focused policy and default-team tests**

Run:

```bash
go test ./internal/team ./internal/tools -run 'Test(LoadDefaultTeam_HelperTools|RunDirectAgent_HelperToolsPopulateRuntimeAllowlist|CheckToolPermission)' -count=1
```

Expected: PASS.

- [ ] **Step 2: Run package tests and build**

Run:

```bash
go test ./internal/team ./internal/tools ./cmd/hufu && go build ./cmd/hufu
```

Expected: every command exits 0.

- [ ] **Step 3: Inspect the scoped diff**

Run:

```bash
git diff --check -- internal/team
git diff -- internal/team/coordinator_task_run.go internal/team/coordinator_run.go internal/team/coordinator_tools_delegate.go internal/team/coordinator_run_test.go
```

Expected: no whitespace errors; only the shared helper, its three call sites, and regression test are present.

## Self-Review

- Spec coverage: Tasks 1-2 cover the default Helper's direct/fast path and centralize all three worker allowlist call sites; Task 3 checks baseline behavior and the full scoped build.
- Placeholder scan: no unresolved implementation or test placeholders are present.
- Type consistency: every task uses `withEffectiveToolsAllowed(ctx context.Context, def *agent.AgentDef) context.Context` and the existing `tools.AgentToolsAllowedKey` context value.
