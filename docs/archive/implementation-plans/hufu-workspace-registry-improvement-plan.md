# Hufu Workspace Registry 與 State Root 實作計畫

> Status: historical — implemented
>
> Target: coding agent / staged pull requests
>
> Verified against `main` commit: `bb0ae5c8742a6f13be7294f8b36601ec4b72730c` (2026-09-17)
>
> Scope owner: `internal/workspace`（registry 與 lifecycle）、`internal/team`（runtime scope）、`cmd/hufu`（CLI integration）
>
> Completed by: `752a155`, `36fd208`, `06d20eb`, `5d56941`, `a9bd89e`, `fc13a08`, `728c154`, `095474b`

---

## 1. 執行摘要

Hufu 的 durable control state 不應繼續預設寫入 `<SubjectRoot>/workspace/<team>`。完成本計畫後：

```text
SubjectRoot
  = agent 操作的 repository/worktree root，或非 Git 目錄的啟動目錄

ControlRoot
  = session、context、event store、logs、task state、artifacts
  = 預設位於 OS state directory

Linux default
  = $XDG_STATE_HOME/hufu
  = fallback ~/.local/state/hufu
```

核心 invariant：

```text
ProjectID != filesystem path
SubjectRoot != managed ControlRoot
session.Workspace == ControlRoot          # compatibility alias
worker/tool WorkDir == SubjectRoot
context.sqlite remains per-team canonical context store
event_store.jsonl remains canonical execution history
```

本文件只保留可由 coding agent 在 repository 內完成並以自動化測試驗證的工作。它不包含 release 排程、真實使用者資料搬移、人工 usability study、修改使用者 shell rc、或 production telemetry 觀察。

---

## 2. 已固定的產品與架構決策

以下決策不得在實作中重新留白或交由 coding agent 猜測：

1. Managed project 使用隨機、不可由 path 推導的 ProjectID。
2. ProjectID 格式為 `prj_` 加 32 個 lowercase hex；WorkspaceID、TrashID、OperationID 分別使用 `ws_`、`tr_`、`op_` 加 32 個 lowercase hex。使用 `crypto/rand` 產生 128 bits，不新增 ID dependency。
3. 一個 registration 只對應一個 SubjectRoot；clone 與 worktree 各自註冊成不同 project。
4. Project rename/move 只透過 explicit `workspace rebind`，不做 fingerprint、自動配對或靜默 rebind。
5. Managed ControlRoot 只位於 StateRoot 下；explicit `--workspace`、`--workspace-root` 與 `--temp` 都是 unmanaged，不寫入 registry，也不能由 workspace management command 刪除。
6. `workspace delete` 永遠只移到 trash；永久刪除只有 `workspace purge` 與 `workspace gc --apply --yes`。
7. Legacy migration 永遠 copy，不 rename、不刪除、不修改 legacy source canonical files；SQLite driver 允許的 transient `-shm` metadata 例外依第 14 節驗證。成功後舊目錄仍保留，managed resolver 以 registry destination 為準。
8. Managed runtime 對同一 team workspace 採單一 writer：run/chat/resume/retry/reconcile 取得 OS-level exclusive lock；鎖忙即拒絕，不等待。
9. Registry lifecycle event 不寫入 Coordinator execution event store；registry SQLite 自己保存 content-free operation records。
10. 不新增 `runtime.sqlite`，不把 artifacts 或 event store 移入 SQLite。
11. 不移除 `--workspace`、`--workspace-root`、`--temp`，也不改變 explicit exact/root path semantics。

---

## 3. Current State 與 canonical owner

目前：

- `internal/operator.ResolveWorkspacePath()` 擁有 `exact`、`root`、`legacy_base` 與舊 `default` path semantics。
- `team.TeamSession.Workspace` 是 control state root。
- `team.Coordinator.projectDir` 來自 `os.Getwd()`，worker tools 使用它作 WorkDir。
- `team.WorkspaceScope` 已存在，但尚未放入 `TeamSession`，也不是 runtime 的單一來源。
- canonical context scope 目前把 `c.projectDir` 寫入 `context.Scope.ProjectID`。
- `cmd/hufu` 仍有多個 command 自行使用 `getWorkspace()` 或字面 `workspace`。

新 canonical owner：

```text
internal/workspace
  StateRoot、SubjectRoot discovery、registry、selector、managed resolver、
  ownership marker、lock、migration、delete/restore/purge、doctor、gc

internal/operator
  只保留 unmanaged explicit exact/root/legacy_base path semantics

internal/team
  接收已解析的 WorkspaceScope；不自行查 registry 或重新讀取 cwd

cmd/hufu
  在任何 team/session/coordinator 建立前解析 scope，並明確關閉 registry/lock
```

禁止新增第二套 registry、package-global `*sql.DB`，或讓 Coordinator 直接打開 global registry。

---

## 4. Terminology 與 deterministic SubjectRoot discovery

### 4.1 名詞

- **Project**：registry 中的一筆 logical registration。
- **ProjectID**：隨 registration 產生的 opaque stable ID。
- **SubjectRoot**：worker 與 filesystem tools 的 WorkDir。
- **ControlRoot**：某 team 的 durable Hufu state root。
- **ProjectRoot**：現有 `WorkspaceScope` compatibility field；本計畫中固定等於 SubjectRoot，不建立第二種 root semantic。
- **ContextScopeID**：context.sqlite 使用的 immutable compatibility scope key。
- **Team Workspace**：一個 `(ProjectID, normalized TeamName)` 的 managed ControlRoot。
- **StateRoot**：global registry、managed projects、locks 與 trash 的共同上層。

### 4.2 SubjectRoot discovery

新增 pure helper：

```go
func DiscoverSubjectRoot(start string) (string, error)
```

演算法固定如下：

1. 對 `start` 執行 `Abs`、`Clean`，並在可解析時 `EvalSymlinks`。
2. 從該目錄向上尋找第一個包含 `.git` entry 的祖先；`.git` 必須是 regular file 或 directory，不 follow `.git` symlink。
3. 找到時回傳該祖先，讓 nested repository 使用最近的 repository root，linked worktree 的 `.git` file 也能辨識。
4. 找不到時回傳 canonicalized `start`。
5. bare repository 不特判；若沒有 `.git` entry，就使用啟動目錄。

所有 default run、chat、workspace management current-project selector 都使用此 helper。不得在下游重新呼叫 `os.Getwd()` 當作 project identity；CLI boundary 只讀一次 cwd，再把解析結果傳下去。

### 4.3 ContextScopeID compatibility

本計畫不重寫既有 context.sqlite 中所有 path-shaped project scope。為保持 canonical context semantics：

- `ContextScopeID` 屬於 team workspace，不屬於 project；不同 legacy team workspace 可能曾從不同 cwd 建立。
- 新建立（非 migration）的 workspace 將 `ContextScopeID` 固定為建立當下的 canonical SubjectRoot。
- 它寫入 `workspaces.context_scope_id`；delete 帶入 trash row，restore 帶回 workspace row，rebind 永不改變。
- legacy migration 依第 14 節從 source DB 保留既有唯一 scope；沒有既有 scope 時使用該 legacy workspace 的推導 subject root。
- managed runtime 的 context reads/writes 使用 `ContextScopeID`，registry/deletion/selector 使用 ProjectID。
- unmanaged explicit/temp runtime 的 `ContextScopeID` 等於當次 canonical SubjectRoot。

因此 registry identity 不依賴 path，各 team 的既有 context row 不需要 destructive rewrite；rebind、delete/restore 後仍使用相同 immutable compatibility key。

---

## 5. StateRoot

新增：

```go
func DefaultStateRoot() (string, error)
```

precedence：

```text
1. HUFU_STATE_HOME                 # exact StateRoot，不再 append hufu
2. OS-native state location
3. documented OS fallback
```

平台規則：

```text
Linux:
  $XDG_STATE_HOME/hufu             # 僅接受 absolute XDG_STATE_HOME
  ~/.local/state/hufu

macOS:
  ~/Library/Application Support/hufu/state

Windows:
  %LOCALAPPDATA%\hufu\state
```

`HUFU_STATE_HOME` 必須是 absolute path；空值視為未設定，relative value 是 hard error。回傳 path 必須 canonicalize nearest existing ancestor，建立時使用 `0700`（Windows 忽略 Unix mode）。Registry DB 與 lock file 使用 `0600`。

為了可測試性，OS-specific default 分到 build-tag files；home/env lookup 使用 injectable seam，不修改 process-global environment 的 parallel tests。

---

## 6. Managed directory layout

```text
$STATE_ROOT/
├── registry.sqlite
├── locks/
│   ├── ws_<id>.lock
│   └── legacy_<sha256-canonical-source>.lock
├── projects/
│   └── hufu--a1b2c3d4e5f60708/
│       └── teams/
│           ├── default/
│           │   ├── workspace.json
│           │   ├── context.sqlite
│           │   ├── session.json
│           │   ├── tasks/
│           │   ├── shared/
│           │   ├── status/
│           │   ├── history/
│           │   └── logs/
│           └── dev/
├── staging/
│   └── op_<id>/
└── trash/
    └── tr_<id>/
```

Project state directory basename 在 registration 時產生並存入 DB：

```text
<sanitized-basename>--<first-16-project-id-hex>
```

slug normalization：lowercase；非 `[a-z0-9._-]` 轉成 `-`；壓縮連續 `-`；trim punctuation；空結果使用 `project`。若 state directory 已存在，重新產生 ProjectID，最多重試 8 次後 fail closed。

TeamName 沿用既有 validation：不可為 absolute、`.`、`..`，不可含 `/` 或 `\`；registry key 使用 lowercase normalized team name。

---

## 7. Registry SQLite schema

新增 `$STATE_ROOT/registry.sqlite`。Registry 不沿用 context store 的 WAL：固定使用 `journal_mode=DELETE`、`synchronous=FULL`、`busy_timeout=5000`、`foreign_keys=ON`、單 handle writer serialization、immutable migrations 與 checksum verification。這讓 read-only command 可取得 SQLite read lock而不必建立 `-wal`/`-shm`；`context.sqlite` 仍維持既有 WAL policy。

Registry 必須提供分離的 open modes：

- `OpenReadWrite` 可建立 StateRoot/DB並套用 migration，只能用於 ensure 與 management mutation。
- `OpenReadOnly` 使用 SQLite URI `mode=ro` 並設定 `PRAGMA query_only=ON`；不得使用 `immutable=1`，以免繞過跨 process locking。DB 不存在時回 typed not-found；不得建立 StateRoot、DB、journal 或 migration。
- preview/list/show/path、read-only commands 與 dynamic completion 只能使用 `OpenReadOnly`；若 registry 不存在，list 回空集合，其餘回 typed not-found。

第一版 schema 固定為：

```sql
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at INTEGER NOT NULL,
    checksum TEXT NOT NULL
);

CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    slug TEXT NOT NULL,
    alias TEXT UNIQUE,
    subject_root TEXT NOT NULL UNIQUE,
    state_dir TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('active')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_used_at INTEGER
);

CREATE TABLE workspaces (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    team_name TEXT NOT NULL,
    context_scope_id TEXT NOT NULL,
    control_root TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('creating','active','deleting','restoring')),
    operation_id TEXT,
    pending_path TEXT,
    requires_fresh_session INTEGER NOT NULL DEFAULT 0 CHECK (requires_fresh_session IN (0,1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_used_at INTEGER,
    UNIQUE(project_id, team_name)
);

CREATE TABLE trash_workspaces (
    trash_id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    team_name TEXT NOT NULL,
    context_scope_id TEXT NOT NULL,
    original_control_root TEXT NOT NULL,
    trash_path TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('trashed','restoring','purging')),
    operation_id TEXT,
    requires_fresh_session INTEGER NOT NULL DEFAULT 0 CHECK (requires_fresh_session IN (0,1)),
    deleted_at INTEGER NOT NULL,
    purge_after INTEGER
);

CREATE TABLE registry_operations (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('create','migrate','delete','restore','purge','repair')),
    project_id TEXT,
    workspace_id TEXT,
    state TEXT NOT NULL CHECK (state IN ('started','completed','failed')),
    detail_code TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    finished_at INTEGER
);

CREATE INDEX idx_projects_slug ON projects(slug);
CREATE INDEX idx_workspaces_project ON workspaces(project_id, team_name);
CREATE INDEX idx_trash_purge_after ON trash_workspaces(state, purge_after);
CREATE INDEX idx_operations_state ON registry_operations(state, started_at);
```

Alias 規則：lowercase，必須符合 `[a-z0-9][a-z0-9._-]{0,63}`。Slug 可重複；alias、SubjectRoot、ControlRoot 不可重複。

所有 list query 必須有 deterministic `ORDER BY`。所有 filesystem operation 都不能包在長時間 SQLite transaction 內；使用 operation state 協調 DB/FS crash recovery。

---

## 8. Ownership markers

Managed workspace root 必須有 atomic-created `workspace.json`：

```json
{
  "schema_version": 1,
  "workspace_id": "ws_0123...",
  "project_id": "prj_0123...",
  "context_scope_id": "/immutable/scope/for-this-team-workspace",
  "team": "dev",
  "managed_by": "hufu",
  "created_at": "2026-09-17T13:00:00Z"
}
```

Staging directory 必須有 atomic-created `operation.json`：

```json
{
  "schema_version": 1,
  "operation_id": "op_0123...",
  "kind": "create",
  "workspace_id": "ws_0123...",
  "final_control_root": "/canonical/final/path"
}
```

`kind` 只能是 `create` 或 `migrate`，而且必須與 registry operation row一致。Marker 是 ownership metadata，不是 runtime truth。Delete、restore、purge、repair 必須同時驗證 DB identity、marker identity、canonical containment；任何 mismatch 都 fail closed。

Marker 與 operation marker 使用既有 atomic-create/write discipline，不 overwrite 既有檔案。

---

## 9. Go API

新增 `internal/workspace`：

```text
state_root.go
subject_root.go
identity.go
registry.go
registry_schema.go
selector.go
resolver.go
ownership.go
lock.go + lock_unix.go + lock_windows.go
migrate.go
delete.go
restore.go
purge.go
doctor.go
gc.go
```

核心型別：

```go
type ResolveMode string

const (
    ResolveExisting ResolveMode = "existing"
    ResolveEnsure   ResolveMode = "ensure"
    ResolvePreview  ResolveMode = "preview"
)

type ResolveRequest struct {
    StartDir       string
    TeamName       string
    ExplicitExact string
    ExplicitRoot  string
    TemporaryRoot string
    Mode           ResolveMode
}

type Resolution struct {
    ProjectID      string
    WorkspaceID    string
    ContextScopeID string
    TeamName       string
    SubjectRoot    string
    ControlRoot    string
    Managed        bool
    WouldCreate    bool
}

type Registry interface {
    RegisterProject(context.Context, string) (Project, error)
    ResolveProject(context.Context, string) (Project, error)
    ResolveProjectByRoot(context.Context, string) (Project, error)
    ListProjects(context.Context, ListOptions) ([]Project, error)
    SetAlias(context.Context, string, string) error
    ClearAlias(context.Context, string) error
    RebindProject(context.Context, string, string) error
    GetWorkspace(context.Context, string, string) (Workspace, error)
    ListWorkspaces(context.Context, string) ([]Workspace, error)
    Close() error
}

type Manager interface {
    Resolve(context.Context, ResolveRequest) (Resolution, error)
    Migrate(context.Context, MigrateRequest) (MigrateResult, error)
    Delete(context.Context, DeleteRequest) (DeleteResult, error)
    Restore(context.Context, RestoreRequest) (RestoreResult, error)
    Purge(context.Context, PurgeRequest) (PurgeResult, error)
    Doctor(context.Context, DoctorRequest) (DoctorReport, error)
    GC(context.Context, GCRequest) (GCReport, error)
    Close() error
}
```

Resolution precedence：

```text
1. TemporaryRoot       -> unmanaged exact, never registry
2. ExplicitExact       -> unmanaged exact
3. ExplicitRoot        -> unmanaged root + team once
4. registered SubjectRoot + team
5. Mode=ensure         -> register project and create managed workspace
6. Mode=preview        -> no writes; return WouldCreate=true without IDs/path creation
7. Mode=existing       -> typed not-found error
```

Unmanaged resolution 的 ProjectID/WorkspaceID 可為空，但 SubjectRoot、ControlRoot、ContextScopeID 不得為空。Preview 若 target 尚未註冊，ProjectID、WorkspaceID、ContextScopeID、ControlRoot 都可為空，必須以 `WouldCreate=true` 表達；不得產生稍後不會沿用的 provisional identity。若 project 已註冊但 team workspace 尚未建立，preview 可回傳由既有 state directory 算出的 planned ControlRoot，ContextScopeID 可確定為 SubjectRoot，但 WorkspaceID 仍為空。

Selector precedence：

```text
1. exact full ProjectID
2. unique ProjectID prefix，至少 8 hex chars
3. exact alias
4. exact canonical SubjectRoot
5. unambiguous slug
6. omitted selector -> DiscoverSubjectRoot(cwd)
```

任何 ambiguity 都回傳列出候選 ProjectID/SubjectRoot 的 deterministic error，不猜測。

---

## 10. Runtime scope integration

擴充現有型別並放入 `TeamSession`：

```go
type WorkspaceScope struct {
    ProjectID      string `json:"project_id,omitempty" yaml:"project-id,omitempty"`
    ContextScopeID string `json:"context_scope_id" yaml:"context-scope-id"`
    ControlRoot    string `json:"control_root" yaml:"control-root"`
    SubjectRoot    string `json:"subject_root" yaml:"subject-root"`
    ProjectRoot    string `json:"project_root" yaml:"project-root"`
    Managed        bool   `json:"managed" yaml:"managed"`
}

type TeamSession struct {
    // existing fields...
    Workspace string         // compatibility alias; always == Scope.ControlRoot
    Scope     WorkspaceScope
}
```

Runtime rules：

1. CLI 必須在 `LoadTeam` 後、任何 workspace I/O 或 provider/model call 前設定 Scope。
2. `NewCoordinator` 不再呼叫 `os.Getwd()`；`c.projectDir = Scope.SubjectRoot` 作 compatibility bridge。
3. `Scope.ProjectRoot = Scope.SubjectRoot`；`session.Workspace = Scope.ControlRoot`，現有 persistence/artifact/event paths 保持不變。
4. worker、direct agent、extra model、plan reviewer、judge/skeptic 可執行 filesystem 的 path 都使用 SubjectRoot。
5. coordinator protocol、context DB、event store、receipts、session、logs 都使用 ControlRoot。
6. canonical context `Scope.ProjectID` 改用 `WorkspaceScope.ContextScopeID`，不得再使用 `c.projectDir`。
7. execution policy 的 project-root hash、repository receipt 與 verifier WorkDir 使用 SubjectRoot。
8. Scope validation 在任何 directory creation 前執行。Managed scope 一律呼叫 `ValidateWorkspaceScope` 並拒絕任一方向的 overlap；unmanaged explicit/temp scope 不呼叫該 helper，只沿用現有 `ValidateWorkspaceIsolationPaths`，因此只有 `RequireWorkspaceIsolation` profile 拒絕 overlap。
9. registry/lock failure 必須發生在 LLM、MCP、action provider、session mutation之前。

必須 trace 並測試所有路徑：normal worker、direct agent、extra model、sidecar/reviewer、fast path、chat、unattended、dry-run、resume、targeted retry/reconcile、runtime action。

### 10.1 Rebind 與 session safety

`workspace rebind <selector> <new-root>`：

1. canonicalize 且要求 new root 是 existing directory。
2. 收集該 project 所有 active 與 trash WorkspaceID，去重後依 lexical order取得 locks；任一 lock busy 即全部釋放並失敗，避免與 restore/purge 競爭。
3. 以第 14 節 candidate roots 檢查 old SubjectRoot；若仍有 valid legacy team directory，而該 team 尚無 active managed workspace，拒絕 rebind並要求先 migrate；避免 rebind 後失去 deterministic legacy source location。
4. transactionally 更新 `projects.subject_root`；若 new root 已屬於其他 project則 fail。
5. 所有 active workspaces 與 trash rows 設 `requires_fresh_session=1`；restore 必須把該 flag帶回 workspace row。
6. 不改任何 active/trash workspace 的 ContextScopeID、不搬 ControlRoot。

下一次 execution：

- flag 為 1 且沒有 `--new`：preflight hard error，提示重新執行 `--new`。
- flag 為 1 且有 `--new`：完成既有 archive/reset/fresh policy checkpoint 後才清除 flag。
- fresh startup 中途失敗：flag 保持 1。

---

## 11. CLI integration matrix

不得只修改 `hufu run`。新增 shared CLI seam，例如：

```go
func resolveCommandWorkspace(ctx context.Context, req commandWorkspaceRequest) (workspace.Resolution, io.Closer, error)
```

command 分類：

| 類別 | Commands | Resolver mode |
|---|---|---|
| 建立/執行 | root legacy invocation、`run`、`chat` | ensure；dry-run 改 preview |
| 恢復/變更既有 session | `resume`、`retry`、`reconcile`、`session resume/retry/reconcile` | existing；explicit path 仍 exact |
| 讀取既有 state | `session *`、`context *`、`inspect *`、`audit *`、`debug`、`history`、`status`、`improve *`、`terminal *`、dynamic completion | existing；not-found 不建立任何檔案 |
| 暫存執行 | 任一 execution command + `--temp` | unmanaged temporary exact |
| 管理 | `workspace *` | registry/manager direct |

實作時以以下 search 作 inventory gate：

```bash
rg -n 'getWorkspace\(|getSessionWorkspace\(|ResolveWorkspacePath\(|OpenSQLite\(' cmd/hufu internal
rg -n '"workspace"|<cwd>/workspace' cmd/hufu --glob '*.go'
```

完成後，非 migration fixture、explicit compatibility path、help/deprecation text 不得再有 command-local default `<cwd>/workspace` 推導。

### 11.1 Dry-run

`--dry-run` 不得建立 StateRoot、registry row、workspace directory、marker、lock 或 session file。

- registered project/workspace：回傳 existing managed resolution。
- unregistered/missing workspace：preview 回傳 `WouldCreate=true`，dry-run renderer 只顯示確定的 SubjectRoot/team與「將建立 managed workspace」；不得虛構 ProjectID/WorkspaceID，也不得建構會開啟 context.sqlite 的 Coordinator。
- dry-run tests 必須 snapshot filesystem before/after。

### 11.2 Temporary workspace

`--temp` 在 CLI boundary 建立 temp root，resolver 視為 `TemporaryRoot` exact path；不註冊、不留下 lock/marker，並沿用現有 cleanup。`--temp` 與 explicit workspace flags 維持既有 mutual-exclusion validation。

---

## 12. Workspace management CLI

新增 top-level `hufu workspace`：

```text
register [path] [--output text|json]
list [--all] [--output text|json]
show [selector] [--output text|json]
path [selector] [--team name]
subject-path [selector]
alias set <selector> <alias> [--output text|json]
alias clear <selector> [--output text|json]
rebind <selector> <new-root> [--output text|json]
migrate [selector] (--team name | --all-teams) [--legacy-root path] [--output text|json]
delete [selector] (--team name | --all-teams) [--yes] [--output text|json]
restore <trash-id> [--yes] [--output text|json]
purge <trash-id> [--yes] [--output text|json]
gc [--dry-run | --apply --yes] [--trash-older-than duration] [--output text|json]
doctor [selector] [--repair] [--output text|json]
shell-init <bash|zsh|fish|powershell>
```

Defaults：

- omitted selector 使用 current discovered SubjectRoot。
- omitted `--team` 使用 `default`。
- `register` 是唯一 standalone registration command；`show/path/subject-path` 永不隱式建立。
- `list` 預設只顯示 active projects；`--all` 另外包含 trash summary 與 incomplete operations。

### 12.1 Machine output contract

`workspace path` stdout 僅為 canonical ControlRoot 加 newline；`subject-path` 同理。不得有 ANSI、label、spinner 或 log。

JSON commands 使用 envelope：

```json
{
  "schema_version": 1,
  "data": {},
  "warnings": []
}
```

規則：

- stdout 只有 result；diagnostics 到 stderr。
- JSON arrays 按 ProjectID、TeamName、TrashID 排序。
- timestamps 為 UTC RFC3339Nano。
- path 一律 absolute canonical path。
- read-only/preflight failure exit code 非 0，stdout 不輸出 partial JSON；error 到 stderr。
- filesystem-spanning lifecycle transition（create/migrate/delete/restore/purge/repair）必須建立 operation row；純 DB mutation（register/alias/rebind）不得建立 operation row。
- `hufu workspace` mutation command 一旦建立 operation row，就必須在 stdout 回傳帶有 `outcome: complete|partial|failed` 的 selected-format result；JSON 使用上述 envelope，text 固定以 `<command> outcome=<value>` 開頭，後續欄位按文件列出的 identity key lexical order輸出。partial/failed 同時使用非 0 exit code。尚未建立 operation row 的 preflight failure不輸出 stdout。Runtime ensure/create 沿用 execution command既有 final-result contract，不插入 workspace-management result；失敗細節寫入 operation row並由既有 command error channel回報。
- golden tests 固定 text 與 JSON schema。

### 12.2 Shell integration

`shell-init` 只輸出 static function source，不修改 shell rc：

```bash
hcd() {
    local p
    p="$(command hufu workspace path "$@")" || return
    cd -- "$p"
}

hproj() {
    local p
    p="$(command hufu workspace subject-path "$@")" || return
    cd -- "$p"
}
```

Fish/PowerShell 使用各自等價 quoting。禁止把 alias/path 插入生成的 shell source；runtime data 只能作 quoted command output。使用 golden tests 驗證 spaces、unicode、malicious alias 與 command failure。

---

## 13. Lock contract

沿用 repository 已有 `x/sys/unix` / `x/sys/windows` file-lock pattern，不新增第三方 lock package。

Managed lock path：

```text
$STATE_ROOT/locks/<WorkspaceID>.lock
```

規則：

1. OS lock 才是 truth；lock file 存在不代表 busy，process exit 自動釋放。
2. 不提供 stale PID 清除與 `--force` bypass。
3. lock file 內容固定為空；busy diagnostics 只依 non-blocking OS lock attempt，不讀 PID 或猜測 stale owner。
4. execution mutation 持有 exclusive lock，從第一次 state read 前直到 coordinator/session close 後。
5. delete、restore、rebind、migration finalization 取得相同 WorkspaceID lock。
6. 多 workspace 操作按 WorkspaceID lexical order 取得，避免 deadlock；失敗立即釋放已取得 locks。
7. list/show/path/subject-path 不取得 workspace lock；doctor read-only 報告 busy 狀態，`--repair` 才需 lock。
8. lock file 位於 StateRoot/locks，不放在會被 rename 的 ControlRoot。

---

## 14. Legacy migration protocol

Legacy base candidate roots 固定為下列 canonical、去重後的集合：

```text
<StartDir>/workspace
<SubjectRoot>/workspace
```

`--legacy-root` 若提供，必須是 existing directory，並取代上述候選集合；它指向 team directories 的直接 parent，不是單一 team directory。單一 team migration 將 `<candidate>/<normalized-team>` 視為 source。零個 valid source 是 typed not-found；超過一個 valid source 是 `legacy_source_ambiguous` hard error，列出排序後的 canonical paths並要求 `--legacy-root`。這保留從 nested cwd 建立的舊 default workspace；其他歷史 cwd 可由 explicit `--legacy-root` 指定，不做 recursive guessing。

Managed resolver 行為：

- registry 已有 active workspace：使用 managed workspace；legacy source 即使仍存在也不取代它。
- registry 沒有 workspace，但 current StartDir/SubjectRoot candidate 中有 legacy source：normal execution 在任何 registry/filesystem write 前 hard error，輸出精確 `hufu workspace migrate --team <team>` 指令；不得註冊 project或建立空 managed workspace。
- legacy source 不存在：ensure 可建立 managed workspace。

`workspace migrate` 是 non-destructive import：

1. 依 candidate rules resolve 唯一 source；驗證是 directory、不是 top-level symlink，且與 StateRoot/managed destination 不 overlap。所有這些是無寫入 preflight。
2. 決定 ContextScopeID：若 source 沒有 `context.sqlite`，使用 canonical `filepath.Dir(legacyRoot)`。若存在，read-only inspect 每個 application table 中名為 `project_id` 的欄位，並解析 `context_events.scope_json.project_id`；忽略空值、排序去重。零個值使用 `filepath.Dir(legacyRoot)`，一個值原樣保留，超過一個值以 `legacy_context_scope_ambiguous` hard error結束。不得改寫 source或destination scope rows。
3. register/resolve project；project registration 不保存 ContextScopeID。
4. 取得 `legacy_<sha256-source>.lock` 與 reserved WorkspaceID lock。
5. transaction 建立 `workspaces(state='creating', context_scope_id=<resolved>, operation_id=..., pending_path=<staging>)` 與 operation row。
6. 在 `$STATE_ROOT/staging/<OperationID>` 建立 operation marker。
7. 建立 source inventory：relative path、entry type、mode、size、symlink target、regular-file SHA-256。walker 不 follow symlink；SQLite `-shm` 視為 transient，不納入 parity，`-wal` 納入 mutation detection。
8. copy source 到 staging；一般檔案逐檔 atomic create，symlink 只複製 link text、不 follow。`context.sqlite`、`context.sqlite-wal`、`context.sqlite-shm` 不走一般 file copy。
9. 若 source 有 `context.sqlite`，透過 `database/sql.Conn.Raw` 與 modernc SQLite connection 的 `NewBackup`/`Backup.Step` online-backup API，從 read-only source connection 建立 staging `context.sqlite` consistent snapshot。helper 放在 `internal/context`；每次 `Step(128)` pages，遇到 `SQLITE_BUSY`/`SQLITE_LOCKED` 每 50ms重試，總等待上限 5 seconds，並以更早發生者接受 context cancellation。不得對 source 執行 checkpoint、VACUUM 或 migration。若 source 沒有 context DB，使用現有 `OpenSQLite` 在 staging 建立空 canonical store並在結果加入 `context_store_created` warning。
10. 再建立 source inventory；與步驟 7 不同表示 concurrent mutation，刪除 owned staging、移除 creating row並失敗。Hufu 不 rename、delete 或寫入 source canonical files；SQLite driver 可能建立/更新 transient `-shm`，因此 source-preservation assertion排除 `-shm`。
11. 驗證非 SQLite staging inventory與 source parity；對 destination context DB 執行 `PRAGMA quick_check`、migration checksum verification。source DB存在時再比較 source/destination canonical table row counts與 repository revision。session JSON與event store若存在，分別驗證 readability與現有 chain verifier。
12. atomic-create 含 resolved ContextScopeID 的 `workspace.json`，再於同 filesystem rename staging 到 final ControlRoot。
13. transaction 將 workspace 設 active、operation completed。
14. source canonical files 不修改、不 rename、不刪除；允許 SQLite driver 的 transient `-shm` lock metadata變化。

`--all-teams` 以 team name 排序逐一執行。任一失敗後停止、exit nonzero；已完成 team 保持 active，尚未開始的 team 不變，輸出 machine-readable completed/failed/pending lists。

`--all-teams` 對 resolved 唯一 legacy root 的 immediate child directories建立候選集合；多個 candidate roots 都含 valid child 時先回 `legacy_source_ambiguous`，要求 `--legacy-root`。CLI 對 basename 套用既有 team-name validation、lowercase normalization、排序與去重。invalid child 造成 preflight hard error，不能被靜默略過。

不提供 `--remove-source` 或 migration cleanup command。

---

## 15. Create、delete、restore、purge protocol

### 15.1 Create

```text
DB reserve creating + ContextScopeID + OperationID + pending staging path
and insert registry_operations(kind='create', state='started')
→ create staging + operation marker
→ create workspace contents + workspace marker
→ rename staging to final
→ DB active + operation completed
```

不得直接在 final path 漸進建立。若 final path 已存在且不是 exact matching marker，fail closed。

### 15.2 Delete to trash

只接受 registry 中 active managed workspace：

1. resolve exact ProjectID/team，取得 WorkspaceID lock。
2. `Lstat` root，拒絕 top-level symlink。
3. canonical verify：target 必須是 `$STATE_ROOT/projects` descendant，且不得等於 `/`、home、StateRoot、projects root、SubjectRoot、ProjectRoot 或其 ancestor。
4. 驗證 registry、workspace marker 的 WorkspaceID/ProjectID/team 全部一致。
5. 產生 TrashID 與 `$STATE_ROOT/trash/<TrashID>`。
6. transaction 將 workspace 設 deleting，保存 operation_id/pending_path，並 insert `registry_operations(kind='delete', state='started')`。
7. same-filesystem rename ControlRoot 到 trash path。
8. transaction insert `trash_workspaces(state='trashed')`，保留 `context_scope_id` 與 `requires_fresh_session`，delete active workspace row，並將 operation設 completed。

`delete --all-teams` 先顯示完整 target list並按 WorkspaceID 排序 lock。`--yes` 只略過 prompt，不能略過任何 validation。Delete/restore/purge 在 stdin 非 TTY 且未提供 `--yes` 時必須 preflight hard error，不得等待輸入。

### 15.3 Restore

1. resolve TrashID，取得 WorkspaceID lock。
2. 驗證 trash marker 與 row；原 ControlRoot 必須不存在，且 `(ProjectID, TeamName)` 不得有 active workspace。
3. transaction insert workspace restoring，從 trash row帶回 `context_scope_id` 與 `requires_fresh_session`，trash row設 restoring，並 insert `registry_operations(kind='restore', state='started')`。
4. rename trash path 回 original ControlRoot。
5. transaction workspace active，delete trash row，並將 operation設 completed。

不得 overwrite 或選 alternate path；遇到 conflict fail closed。

### 15.4 Purge

1. resolve TrashID，取得 WorkspaceID lock。
2. 驗證 trash path 是 `$STATE_ROOT/trash` direct descendant、不是 top-level symlink，marker identity 一致。
3. transaction 設 purging，並 insert `registry_operations(kind='purge', state='started')`。
4. `RemoveAll` trash directory；`RemoveAll` 不 follow directory 中的 symlink entries。
5. transaction delete trash row，並將 operation設 completed。

只有 `purge --yes` 執行 permanent deletion；沒有 `delete --purge` alias。

---

## 16. Doctor 與 GC

### 16.1 Doctor issue codes

Doctor 必須回傳 typed、deterministic codes：

```text
registry_integrity_failed
migration_checksum_mismatch
project_root_missing
workspace_missing
workspace_marker_missing
workspace_marker_mismatch
scope_overlap
creating_incomplete
deleting_incomplete
restoring_incomplete
purging_incomplete
orphan_staging
orphan_managed_workspace
trash_missing
legacy_source_present
context_quick_check_failed
event_chain_invalid
workspace_busy
```

Read-only doctor 不修改 filesystem/DB。`doctor --repair` 先取得相關 lock，對每個實際 repair insert `registry_operations(kind='repair', state='started')`，完成時設 completed、可回滾的 validation failure設 failed；只做以下 deterministic repair：

- creating：staging/final 有 exact markers時完成；兩者都不存在時移除 creating row；其他狀態拒絕。
- deleting：original 存在且 trash 不存在時回 active；original 不存在且 exact trash marker 存在時完成 trash transaction；其他狀態拒絕。
- restoring：trash 存在且 original 不存在時回復 trashed；original 存在且 exact marker matches時完成 active transaction；其他狀態拒絕。
- purging：trash 不存在時刪 row；trash 仍存在時重新執行已授權 purge。
- orphan staging：只有 operation marker 能對上 failed/incomplete operation 時刪除。
- refresh `last_used_at/updated_at` derived metadata。

Doctor 不重寫 event history、不猜 marker identity、不收養無 DB operation 的 directory。

### 16.2 GC

```text
hufu workspace gc --dry-run
hufu workspace gc --apply --yes --trash-older-than 720h
```

預設等同 `--dry-run`。Apply 只 purge `state='trashed' AND purge_after <= now`，逐筆重用 Purge protocol；不處理 active project/workspace，不自動 repair。Retention 預設 `720h`，常數定義在 `cmd/hufu` 並只由 `--trash-older-than` 覆寫；storage layer 一律接收已解析的 duration。

---

## 17. Crash recovery matrix

| Operation | Observable crash state | `doctor --repair` action |
|---|---|---|
| create/migrate | creating row，無 staging/final | remove row，mark operation failed |
| create/migrate | exact staging marker | 全部 verification 成功則 rename/finalize；否則刪 owned staging、移除 creating row、mark operation failed |
| create/migrate | exact final marker | finalize active |
| delete | deleting，original exists，trash missing | restore active |
| delete | deleting，original missing，exact trash exists | finalize trash |
| restore | restoring，trash exists，original missing | return to trashed |
| restore | restoring，original exists，exact marker | finalize active |
| purge | purging，trash missing | delete trash row |
| purge | purging，trash exists | re-run validated purge |

兩個候選 path 同時存在、marker mismatch、path escape、registry identity ambiguity 一律只報告，不 repair。

---

## 18. Implementation phases / PR boundaries

每個 phase 必須單獨可 merge，不能混入 unrelated persistence refactor。

### PR-1 — Characterization 與 scope plumbing

- 補 current exact/root/default/temp/dry-run tests。
- `TeamSession.Scope` 與 expanded `WorkspaceScope`。
- CLI 先以現有 canonical cwd與現有 workspace path填入 compatibility Scope；Coordinator 改由 Scope 取得 SubjectRoot/ControlRoot/ContextScopeID，不改 default path或subdirectory WorkDir行為。
- trace 所有 execution paths。

### PR-2 — StateRoot、identity、registry core

- StateRoot、SubjectRoot discovery、ID、schema/migrations。
- project register/resolve/selector/alias。
- workspace create state machine、markers、operation records。
- registry concurrency/crash fixture tests。

### PR-3 — Read-only與 registration CLI

- `workspace register/list/show/path/subject-path/alias`。
- text/JSON golden tests。
- shell completion metadata。
- 此階段 runtime default 仍不切換。

### PR-4 — Runtime resolver、locks、全部 CLI consumer

- shared command resolver。
- default selector 開始使用 `DiscoverSubjectRoot`，但 managed default 尚未啟用；legacy default path 改以 discovered SubjectRoot 推導。
- managed/exact/temp/preview modes。
- execution exclusive lock。
- run/chat/resume/recovery/read-only commands/dynamic completion integration。
- `rebind` + `requires_fresh_session`。
- default 仍保持 legacy，先完成 compatibility tests。

### PR-5 — Non-destructive legacy migration + doctor

- migration staging/copy/inventory/online-backup/verification。
- doctor read-only與 deterministic repair。
- interrupted migration tests。

### PR-6 — Managed default switch

- no flags 時使用 managed resolver。
- legacy source without active registry workspace 時 hard error + migrate instruction。
- explicit exact/root/temp compatibility。
- workspace snapshot 不再需要把 managed ControlRoot 視為 SubjectRoot child。

### PR-7 — Trash lifecycle

- delete/restore/purge state machines。
- all protected-path、marker mismatch、busy lock、crash-window tests。

### PR-8 — GC 與 shell integration

- GC dry-run/apply。
- bash/zsh/fish/PowerShell static shell-init + golden tests。
- README、CLI reference、migration guide 更新。

不開始下一個 PR，直到前一個 PR 的 required validation 全綠。

---

## 19. Test plan

### 19.1 Path 與 StateRoot

- Linux XDG set/unset/relative。
- macOS/Windows helper tests與 cross-build。
- HUFU_STATE_HOME absolute/relative/spaces/symlink ancestor。
- Git directory、linked worktree `.git` file、nested repo、non-Git cwd。
- invalid team、slug collision、path canonicalization。

### 19.2 Registry

- registration idempotency。
- random ID entropy failure fail closed。
- alias normalization/uniqueness。
- short ID ambiguity。
- SubjectRoot/ControlRoot uniqueness。
- concurrent readers/writers與 busy timeout。
- immutable migration checksum mismatch。
- create crash states與doctor repair。

### 19.3 Runtime scope

- tools/direct/extra model/reviewer WorkDir 是 SubjectRoot。
- session/context/event/artifacts 寫入 ControlRoot。
- context reads/writes 使用 ContextScopeID。
- 不同 team workspace 可持有不同 ContextScopeID；create、delete/restore、rebind 不會錯置 scope。
- rebind 後 ContextScopeID 不變。
- rebind requires `--new`，成功 fresh checkpoint 後才 clear flag。
- rebind 後 delete/restore 的 workspace 仍保留 `requires_fresh_session`，restore 後沒有 `--new` 仍拒絕 execution。
- validation 發生在 provider/MCP/action/filesystem mutation 前。
- strict isolation profile 保持現有拒絕語意。

### 19.4 Resolver 與 CLI consumers

- exact、root、managed existing、managed create、preview、temp。
- dry-run filesystem/registry byte-for-byte 無變化。
- run、chat、resume、retry/reconcile、session、context、inspect、audit、debug、history、status、improve、terminal、completion。
- missing read-only workspace 不建立檔案。
- multi-team prompt 每 team 各自 ControlRoot/lock。

### 19.5 Migration

- source missing/not-directory/top-level symlink。
- StartDir 與 SubjectRoot legacy candidates：零個、一個、兩個；兩個時要求 `--legacy-root`。
- nested-cwd legacy root與 explicit `--legacy-root`。
- context scope inventory：零個值 fallback legacy subject、一個值保留、多個值 fail closed。
- destination/staging collision。
- SQLite online-backup open/step/cancel/busy failure。
- spaces、unicode、symlink entry、不 follow symlink。
- concurrent source mutation使前後 inventory 不同。
- context quick_check failure。
- event chain failure。
- crash at each DB/FS boundary。
- any failure leaves source canonical files byte-for-byte unchanged（排除 transient `-shm`）。
- success leaves source canonical files unchanged且managed destination可 resume。

### 19.6 Delete/restore/purge

- refuse `/`、home、StateRoot、projects root、SubjectRoot、ancestor、path escape。
- refuse unmanaged/external path、missing/mismatched marker、top-level symlink。
- busy workspace refusal。
- delete to trash、restore、restore conflict、purge。
- `--yes` cannot bypass validation。
- crash matrix every state。
- child symlink never deletes target。

### 19.7 Output 與 shell

- path/subject-path exact stdout bytes、no ANSI、diagnostics on stderr。
- deterministic JSON ordering/schema/timestamps。
- Bash/Zsh/Fish/PowerShell golden source。
- spaces、unicode、malicious alias 不造成 code injection。
- resolver failure 不執行 `cd`。

---

## 20. Required validation

每個修改 Go source/tests 的 PR 都要分別執行並成功：

```bash
go test ./...
go vet ./...
golangci-lint run
```

涉及 platform build-tag files 時另外執行：

```bash
GOOS=windows go test -c -o /tmp/hufu-workspace-windows.test ./internal/workspace
GOOS=windows go test -c -o /tmp/hufu-cmd-windows.test ./cmd/hufu
GOOS=darwin go test -c -o /tmp/hufu-workspace-darwin.test ./internal/workspace
GOOS=darwin go test -c -o /tmp/hufu-cmd-darwin.test ./cmd/hufu
```

這些命令只證明 cross-build/test compilation，不得宣稱已做 Windows/macOS runtime verification。

Documentation-only PR 不需要 lint，但必須檢查 links、CLI examples 與 `rg` inventory。

---

## 21. Acceptance criteria

- [x] 未指定 workspace flags 的 real execution 使用 registry-managed ControlRoot。
- [x] SubjectRoot discovery deterministic，且 nested cwd 不產生同 repo 的重複 project。
- [x] ProjectID 不由 path 推導；ContextScopeID 是 per-workspace immutable key；rebind 不改 ProjectID/ContextScopeID/ControlRoot。
- [x] `TeamSession.Scope` 是 runtime 唯一 scope source，Coordinator 不重新讀 cwd。
- [x] worker filesystem WorkDir 是 SubjectRoot，durable state 全部位於 ControlRoot。
- [x] managed context scope 在 rebind 後仍可讀既有資料。
- [x] dry-run、read-only commands 與 completion 不建立 registry/workspace state。
- [x] `--temp` 不寫 registry且結束後清理。
- [x] current StartDir/SubjectRoot candidate 中有 legacy source 時不會靜默建立空 managed workspace；其他舊 cwd 可用 `--legacy-root` deterministic migration。
- [x] migration 成功或失敗都不修改 legacy source canonical files（SQLite transient `-shm` 除外）。
- [x] explicit `--workspace` / `--workspace-root` path semantics 保持相容。
- [x] managed execution lock 能阻止 delete/rebind/migrate race。
- [x] delete 只移到 trash，永不刪 SubjectRoot或 unmanaged path。
- [x] restore 不 overwrite；purge 需 explicit `--yes`。
- [x] doctor 能分類並 deterministic repair 所有列出的 crash states。
- [x] stdout/JSON/shell contracts 有 golden tests。
- [x] context.sqlite 與 event-store canonical semantics 未被替換。
- [x] 全部 required validation 通過。

---

## 22. Explicitly out of scope

以下項目已從本實作計畫移除：

- 根據 Git remote/fingerprint 自動辨識或合併 clone/worktree。
- 自動或靜默 rebind。
- 由 Hufu 刪除 legacy source/legacy backup。
- 將 context/event/artifacts 搬進 global registry SQLite。
- 新增 `runtime.sqlite`。
- 修改使用者的 `.bashrc`、`.zshrc`、fish config 或 PowerShell profile。
- release/deprecation 時程、production rollout、真實使用者資料 migration。
- 需要人工判斷的 doctor repair 或 orphan adoption。
- 外部系統、provider、MCP server 或 live model 驗證。

這些項目若未來需要，必須另開獨立計畫，不得在本計畫的 PR 中順手加入。

---

## 23. Final target UX

```bash
cd ~/github/hufu

# explicit registration（也可省略，首次 real run 會執行相同 ensure）
hufu workspace register

# legacy state exists時先做non-destructive import
hufu workspace migrate --team dev

# normal execution
hufu run --team dev "implement feature"

# inspect and navigate
hufu workspace show
hufu workspace path --team dev
cd "$(hufu workspace path --team dev)"

# lifecycle
hufu workspace delete --team dev
hufu workspace restore tr_0123... --yes
hufu workspace purge tr_0123... --yes
hufu workspace gc --dry-run
hufu workspace doctor
```

Resulting scope：

```text
ProjectID:    prj_0123...
SubjectRoot:  ~/github/hufu
ControlRoot:  ~/.local/state/hufu/projects/hufu--0123.../teams/dev
ContextScope: immutable per-team-workspace compatibility key
```
