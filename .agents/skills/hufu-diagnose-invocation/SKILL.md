---
name: hufu-diagnose-invocation
description: Investigate, diagnose, and troubleshoot Hufu invocations or runs (`inv-...` or `run-...`). Use when asked to check if an invocation's progress is normal, diagnose stuck/deadlocked runs, analyze failures, inspect live tmux/processes, and attribute root causes across Hufu runtime, agent team, and repository codebase.
---

# Hufu Diagnose Invocation Skill

Standard Operating Procedure (SOP) for investigating, diagnosing, and attributing root causes of Hufu agent team executions using an Invocation ID (`inv-...`) or Run ID (`run-...`).

## Core Principle

**Never run a blind recursive `grep` across the entire repository or `.git`.**
Hufu keeps its event journal, session checkpoint, and task journal in `<state_dir>/runtime.sqlite`, and audit logs under `<state_dir>/logs/`, in deterministic managed state directories. Follow the phases below to isolate the issue in 2–3 precise operations.

**Never write to `runtime.sqlite` or `context.sqlite`.** Prefer `hufu inspect ...` and `hufu debug <run_id>` bundles; query the database directly only with `sqlite3 -readonly` (SQLite may leave empty `-wal`/`-shm` files next to it, which is harmless). A state directory that still has `session.json` or `logs/event_store.jsonl` but no `runtime.sqlite` is a retired file-based workspace that current Hufu refuses to run; the operator can convert it with `hufu workspace convert-storage --team <team>` (never convert on their behalf during diagnosis).

---

## Diagnostic Workflow

```text
Phase 1: Locate & Map  ──►  Phase 2: Live Status  ──►  Phase 3: Deep-Dive Logs  ──►  Phase 4: Tri-Layer Attribution  ──►  Phase 5: Actionable Report
(Find PID, state dir)       (Process, tmux, step)     (Audit, session, events)       (Runtime vs Team vs Repo)            (Clear synthesis)
```

---

## Phase 1: Fast Location & Identification

When given an Invocation ID (`inv-...`) or Run ID (`run-...`):

### 1. Check for Active Processes
```bash
ps aux | grep -E "hufu.*(inv-|--agent-team)" | grep -v grep
```
- Note the **PID**, **CPU%**, **start time**, and CLI arguments (e.g., `--agent-team`, `--model`, `--new`, prompt).

### 2. Locate the Managed State Directory
Hufu stores team sessions either in the project managed state path or a local `./workspace/`:
- **Default managed state**: `~/.local/state/hufu/projects/<project-slug>/teams/<team-name>/`
  - Find recent project state dirs:
    ```bash
    ls -td ~/.local/state/hufu/projects/* 2>/dev/null | head -n 3
    ```
  - Project slug follows `<repo-name>--<hash>` (e.g., `agent-team-cli--1d1699dc8b9e18e8`).
- **Check active open file descriptors of the PID** (fastest way to locate active workspace):
  ```bash
  ls -l /proc/<PID>/fd 2>/dev/null | grep -E "(runtime\.sqlite|context\.sqlite|audit)"
  ```
- **Map Invocation ID to Run ID & Team** (read-only query of each team's event journal):
  ```bash
  for db in ~/.local/state/hufu/projects/*/teams/*/runtime.sqlite; do
    sqlite3 -readonly "$db" "SELECT DISTINCT run_id FROM events WHERE invocation_id = 'inv-<TIMESTAMP>'" 2>/dev/null | sed "s|^|$db |"
  done
  ```

---

## Phase 2: Live Status & Terminal Observation

Determine whether the execution is **RUNNING**, **STUCK/DEADLOCKED**, or **TERMINATED**.

### 1. If the Process is RUNNING:
Check the interactive terminal (tmux pane) if available:
```bash
tmux capture-pane -pt hufu -S -50 2>/dev/null
```
- **Identify current Step & Actor**: Look for `step <N>`, `hufu-<team>/coordinator [model] › <tool>`.
- **Read Coordinator Thoughts**: Look for `💬 <thought>` to understand what the model is trying to do.
- **Detect Tool Call Loops (Deadlocks)**:
  - Is the model repeatedly calling `finish` and receiving `cannot finish: ...`?
  - Is the model repeatedly calling `agent` and getting `delegation policy violation`?
  - Is `coordinator policy repair budget` being consumed?
  - If CPU% is high without new tools for minutes, the model may be waiting on slow streaming/inference or stuck in a tool loop.

### 2. If the Process is TERMINATED:
- Check exit code and final terminal output in tmux or stderr.
- Note whether the run produced a final `report.md`:
  ```bash
  cat <state_dir>/report.md | head -n 40
  ```

---

## Phase 3: Deep-Dive Artifact & Log Inspection

Drill down into the state directory `<state_dir>`:

### 1. Inspect Task States (session checkpoint in `runtime.sqlite`)
```bash
sqlite3 -readonly <state_dir>/runtime.sqlite "SELECT CAST(body AS TEXT) FROM runtime_documents WHERE name = 'session'" | jq -c '{workflow_state: .workflow_state, tasks: [.tasks[]? | {id: .ID, agent: .Agent, goal: .goal, status: .Status, err: .runtime_error.message}]}'
```
- The checkpoint reflects the active branch's latest run only; for an older run use `hufu inspect run <run_id>`.
- Check `workflow_state`: `PREPARE`, `VERIFY`, `FAILED`, `DONE`.
- Identify which task failed and read its `failure_event` and `runtime_error`.

### 2. Inspect Tool Calls & Errors (`logs/audit/audit-YYYY-MM-DD.jsonl`)
Filter recent calls for the invocation:
```bash
jq -c 'select(.timestamp >= "<run-start-time>") | {time: .timestamp, agent: .agent, tool: .tool, action: .action, err: .error, input: (.input | if . then fromjson? // . else null end)}' <state_dir>/logs/audit/audit-*.jsonl | tail -n 30
```
- Look for `action == "result"` with non-null `error`.
- Check if tool execution failed (e.g., action provider error, gate error, policy violation).

### 3. Inspect Lifecycle Events (event journal in `runtime.sqlite`)
Export the run's events with a debug bundle (redacted; written to the current directory as `hufu-debug-<run_id>.tar.gz`, never overwritten), then read its JSONL export:
```bash
hufu debug <run_id> --workspace <state_dir>
tar -xzOf hufu-debug-<run_id>.tar.gz logs/event_store.jsonl | jq -c '{time: .timestamp, type: .type, actor: .actor, tool: .payload.tool_name, status: .payload.action_status}' | tail -n 25
```
Or query the journal directly (unredacted, newest first):
```bash
sqlite3 -readonly <state_dir>/runtime.sqlite "SELECT timestamp, type, actor, json_extract(CAST(payload AS TEXT), '$.tool_name'), json_extract(CAST(payload AS TEXT), '$.action_status') FROM events WHERE run_id = '<run_id>' ORDER BY sequence DESC LIMIT 25"
```
- Observe the state transition chain: `phase_started` → `tool_observation_failed` → `phase_failed` → `run_finished`.

### 4. Use Native Hufu CLI Diagnostics (Read-Only)
```bash
hufu inspect run <run_id> --workspace <state_dir>
hufu inspect trace <run_id> --workspace <state_dir> | tail -n 20
```

---

## Phase 4: Tri-Layer Root Cause Attribution

Categorize findings into one of three distinct layers:

| Layer | Responsibility | Typical Signatures |
| :--- | :--- | :--- |
| **Layer 1: Hufu Runtime Bug** (`internal/**`) | Core scheduler, state machine, policy gates, token management | • **Policy/Gate Deadlock**: Delegation policy tells coordinator `"If no unfinished work remains, call finish directly"`, but `requireFinished()` returns `"cannot finish: workflow verification gate is not satisfied: current phase is FAILED"`.<br>• `coordinator policy repair budget exhausted`.<br>• Context window overflow / compaction failure (`CannotFit`).<br>• Shadow export parity mismatch warnings. |
| **Layer 2: Agent Team Defect** (`.agent-teams/<team>/`) | Team YAML, worker prompts, action providers (`./reviewprep`, etc.) | • **Overly Strict Workflow Policy**: `policies.fail_fast: true` + `require_phase_success: true` killing the entire run on a minor pre-check.<br>• **Action Provider Grep/Regex Flaws**: Using raw `git grep -F` on qualified Go symbols (`package.Type`), causing false-negative symbol missing errors.<br>• **Archive Deadlinks**: Rigidly failing on superseded/gitignored paths in historical docs.<br>• Missing recovery instructions in coordinator prompt. |
| **Layer 3: Target Repository Defect** (Codebase under review) | The user's actual source code, tests, docs | • **Dangling Documentation**: Readme/docs describe structs/types that don't exist in `*.go` (e.g., `DecisionPrimitive`).<br>• Compilation errors (`go build ./...` fails).<br>• Broken unit tests or lint violations. |

---

## Phase 5: Output Report Structure

When reporting back to the user, present the diagnosis clearly using this structure:

1. **執行狀態與總結 (Execution Status & Verdict)**:
   - 是否正常（Running / Failed / Deadlocked）
   - 耗時、當前 Step、Invocation ID、Run ID
2. **執行歷程時間線 (Timeline of Events)**:
   - 關鍵節點：啟動 → 哪一個 Phase/Task 失敗 → Coordinator 嘗試了什麼 → 最終如何終止
3. **三層歸因分析 (Tri-Layer Attribution)**:
   - **Hufu Runtime 層**
   - **Agent Team 配置與 Action Provider 層**
   - **專案代碼/文件層**
4. **關鍵代碼與日誌證據 (Verifiable Evidence)**:
   - 引用具體的檔案路徑與行號（使用 `file:///...` 格式）及終端日誌
5. **具體處置建議 (Actionable Remediation)**:
   - 如何安全中斷卡死進程（若仍在跑）
   - 如何修復 Team 配置或 Action Provider
   - 如何修復 Codebase 本身的缺陷並重試
