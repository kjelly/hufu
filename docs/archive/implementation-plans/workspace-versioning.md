# Hufu CAS + SQLite Snapshot DAG：Workspace Versioning / Session Fork 實作規格

> Status: in implementation — archived 2026-09-23 from local scratch space before implementation began
> Authority: reference
> Baseline: `47a7a1d842875ac5e4812b81a283ed9e81bf3f5e`
> Branch: `feat/workspace-versioning`
> Decisions: 2026-09-23 已由維護者確認（§1.3）；本文件不留待決事項
> Revision: v3 — v2 依 readiness review 修訂（原基準 `5e6decb`）；v3 寫入維護者決策、更正 lock 現況、定案所有 SHOULD / 可選項目
>
> Target repository: `github.com/kjelly/hufu`
>
> Primary goal: 將既有 **Session Tree（工作階段分支樹：保存 event/session lineage）** 擴充為真正的 **Workspace Versioning（工作區版本化）**，使 `session fork` / `session checkout` 同時切換 runtime/session state 與實際 filesystem state。
>
> Storage model: **CAS（Content-Addressable Storage，依內容雜湊定址的不可變物件儲存） + SQLite Snapshot DAG（以 SQLite 保存快照節點、branch head、operation/recovery metadata）**
>
> Explicit non-goal: **不是 Git integration，也不以 Git commit/branch/index 作為 hufu runtime semantic。**


## Implementation record

Landed on branch `feat/workspace-versioning`; one commit per §36 phase.

### Baseline test status（2026-09-23，`47a7a1d`）

| Check | Command | Result |
| --- | --- | --- |
| Tests | `go test ./... -race -short -timeout 20m` | pass — 41 packages with tests, `internal/team` 705.8 s |
| Vet | `go vet ./...` | pass |
| Lint | `golangci-lint run ./...`（v2.12.2） | 0 issues |
| Docs | `bin/check-docs` | pass — 46 active files |

Environment note: the terminal broker tests bind Unix sockets under `t.TempDir()`, so `TMPDIR`
must stay short. With `TMPDIR` under `/home/...` six `internal/team` terminal tests fail with
`bind: invalid argument` (socket path over the 108-byte limit); with the default `/tmp` they pass.
`go test` passes `GOTMPDIR` through to test binaries, so it cannot be used to move only the build
artifacts. This is environmental, not a baseline regression.

---

## 1. Executive decision

Hufu MUST 新增獨立的 `WorkspaceVersionStore`。其核心模型如下：

```text
                         Hufu Session Tree
                     (event/runtime lineage)
                              │
                         branch_id
                              │
                              ▼
                   Workspace Branch Head
                              │
                       snapshot_id
                              │
                              ▼
                    SQLite Snapshot DAG
                              │
                       root_tree_hash
                              │
                              ▼
                         CAS Object Store
                    ┌─────────┼─────────┐
                    │         │         │
                  blobs      trees     links
```

`session fork` 不再只是複製 task/session projection，而是：

```text
parent branch
    │
    └── workspace head = S42
                          │
                          ├──────────────┐
                          │              │
                     parent branch   new branch
                         S42             S43
                                          │
                                root_tree == S42.root_tree
```

其中 `S43` 是一個新的 snapshot metadata node，但 **不複製任何檔案內容**；所有 CAS objects 共用，因此 fork 的 storage cost 接近 O(1)。

### 1.1 Authority boundary

下列權責 MUST 明確分離：

| Concern | Canonical authority |
|---|---|
| Session/event lineage | `session_tree.json` + EventStore |
| Runtime event durability | `event_store.jsonl` |
| Workspace snapshot graph | `workspace_versions.db` |
| Branch → workspace head | `workspace_versions.db` |
| File/tree/link bytes | CAS objects |
| Live filesystem | materialized projection；不是 durable authority |
| Existing `WorkspaceSnapshotter` | execution-world change observation；不是版本儲存 |
| Subject-root binding / mode floor / recovery marker | `workspace_versions.db` 的 `subject_state`（§11.6） |
| Subject-root 互斥 | `<Project.StateDir>/workspace-versions/lock`（§27；不是 `LocalExecutionWorld` 的 process-local lease） |

### 1.2 Git 的角色

現有：

```text
internal/team/workspace_snapshot_git.go
```

只允許繼續做：

```text
git ls-files
git ls-files --others --exclude-standard
```

用途僅限 **candidate discovery（候選檔案發現：快速取得 tracked + untracked-but-not-ignored files）**。

Git MUST NOT 成為：

- snapshot identity
- branch identity
- restore backend
- session fork backend
- CAS format
- runtime source of truth

## 1.3 v2 decision log

v1 review 找出的待決事項，下列決定皆已定案（D2、D4、D5、D8 由維護者於 2026-09-23 確認）。coding agent 不得自行改變這些決定；若實作中發現與某項決定衝突，停下來回報，不要自行選擇替代方案。

| # | 決策 | v2 採用 | 影響章節 |
|---|---|---|---|
| D1 | runtime checkpoint 粒度 | **只在 quiescent run boundary**（run admission、run 結束前、fork/checkout/restore 前）。v1 不做 per-attempt checkpoint；attempt-level 列為 v1.x 延伸，前提是 scheduler 能保證 subject root 獨占 | §15、§22、§36 Phase 4、§44 |
| D2 | 同 project 多 team 共用 subject root | **（維護者確認）** lock key 為 **project / canonical subject root**；另一 team 造成的變更在本 team 看來就是 `external_drift`；`required` 模式的 run 全程持有 subject-root lease，因此同一 subject root 同時只能有一個 `required` run | §26、§27、§39 IT10 |
| D3 | unmanaged workspace | **v1 不支援**，一律回 `ErrVersionedWorkspaceUnresolved`；metadata-only session 操作維持舊行為 | §26 |
| D4 | GC retention 與歷史 fork | **（維護者確認）** retention 可修剪祖先：snapshot row 保留為 `pruned` tombstone（DAG lineage 不斷），CAS objects 可回收；fork/restore 到 `pruned` snapshot 回 `ErrWorkspaceSnapshotUnavailable` | §11.2、§12、§23、§28 |
| D5 | unauthorized mutation 的 tainted snapshot | **（維護者確認）** v1 不建立 tainted snapshot、schema 不加 `accepted`；改為在 `subject_state` 寫 durable recovery marker，下一次 admission fail closed，直到操作者明確 restore 或 adopt | §11.6、§22.3 |
| D6 | 空目錄 | **不保留**（與 Git 一致）；materialize 只移除「因 managed file 刪除而變空」的目錄 | §8.3、§21.3、§38 |
| D7 | 非 Git workspace 的 `required` 模式 | 以 subject root 下存在 `.hufuignore`（可為空檔，代表明確選擇不排除任何路徑）為前置條件；`.hufuignore` 從 Phase 1 延後項目改為 Phase 1 必要項目 | §9.2、§31、§36 |
| D8 | 支援平台 | **（維護者確認）** v1 只在 Linux 啟用；其他 GOOS 必須能編譯，但 effective mode 強制為 `off` | §3 G9 |
| D9 | rollout 與預設 mode | v1 交付時預設 `mode: off`，`observe` / `required` 由使用者在 config 中 opt-in。何時改預設值由維護者另行決定，不是本規格的工作項目 | §31、§43 |

### v3 實作定案（coding agent 不需再判斷）

| 項目 | 定案 | 章節 |
|---|---|---|
| `object_index` | v1 不實作 | §11.5 |
| `stat_cache` | v1 必須實作並使用 | §11.7 |
| capture 重試上限 | 單檔 3 次；整次 capture 3 次 | §14.2 |
| case-insensitive FS | 不做專門偵測，由 §21.5 verification fail closed | §8.3 |
| lock | 沿用既有 team lock，新增 project lock，由 `commandWorkspaceLease` 持有 | §27 |
| `-w` 指定的 control root | 以新增的 `Registry.GetWorkspaceByControlRoot` 判斷是否為 managed | §26 |
| crash / fault injection | 以測試專用 hook 模擬，不做真實 kill | §36 Phase 2 |
| benchmark | 只記錄數字，不作為合併門檻 | §41 |
| 文件 | 指定檔案清單 | §45 |

---

# 2. Current-state baseline

目前 hufu 已存在三個可直接利用的基礎能力。

## 2.1 Workspace observation

`internal/team/workspace_snapshot.go` 已有：

```go
type WorkspaceSnapshotter interface {
    Snapshot(ctx context.Context, root string) (WorkspaceSnapshot, error)
    Diff(ctx context.Context, before, after WorkspaceSnapshot) (WorkspaceDelta, error)
}
```

以及：

```go
type WorkspaceFileState struct {
    Path   string
    SHA256 string
    Bytes  int64
    Mode   uint32
}
```

現況：

- 對實際 bytes 計算 SHA-256
- 支援 Git-assisted discovery
- 非 Git workspace fallback 到 filesystem walk
- 有 symlink containment 防護
- 有 maximum file-count budget
- snapshot manifest 只存於 process memory
- 無 durable file content
- 無 restore
- 無 fork
- 無 persistent snapshot graph

此元件 MUST 保留、不得被 versionstore 取代，因為它屬於 **ExecutionWorld mutation detection（執行世界變更偵測）**。

### 2.1.1 v2 核實的使用範圍與限制

下列事實決定了本規格不能把 `WorkspaceSnapshotter` 當成 versioning 的輸入來源：

1. **只有 Codex provider 使用。** `ExecutionWorld.Prepare` / `Snapshot` / `ValidateExecutionWorldDelta` 的呼叫點全部在 `internal/team/subagent_codex.go`。原生 worker（內建 agent 的 bash / write 工具，工作目錄 `session.Scope.SubjectRoot`）**沒有任何 baseline/final snapshot**。
2. **原生 worker 並行寫同一 root。** 預設 `max-concurrent` 為 8（`cmd/hufu/team_setup.go`）。`LocalExecutionWorld` 的 lease 是 process-local，只讓 ExecutionWorld 使用者彼此互斥，不約束原生 worker。因此在並行 run 中，「一個 attempt 的 workspace 狀態」沒有定義。
3. **排除規則以任一層目錄名比對。** `workspaceInternalDirs` + `isWorkspaceInternalPath` 會在任何深度排除名為 `.git`、`tasks`、`shared`、`status`、`history`、`logs` 的目錄，walk 與 Git 兩條路徑都套用。`docs/history/`、`internal/tasks/` 這類專案目錄因此不會被觀察。
4. **symlink 表示法與本規格的 link object 不同。** 內部 symlink 記錄的是**目標內容**的 hash，`Mode` 只存 `Perm()`（沒有型別位元）；escaping symlink 的 hash 是對 `"workspace-snapshot:escaping-symlink:"+resolved_target` 算的。
5. **已知缺陷（PR-00 修正）：**
   - walk 路徑遇到指向內部**目錄**的 symlink 時，會對目錄做 `io.Copy`，得到 `EISDIR`，整個 snapshot 失敗。
   - walk 路徑遇到 FIFO 時，`os.Open` 會無限期阻塞，且不受 ctx 取消。

結論：`versionstore` MUST 有自己的 inclusion policy（§9）與 entry-type 判斷（§10），不得沿用上述排除規則或 SHA 表示法。

## 2.2 Session Tree

`internal/team/session_tree.go` 已有：

```go
type SessionBranch struct {
    ID          string
    Name        string
    ParentID    string
    ForkEventID string
    CreatedAt   string
    State       BranchState
}
```

已支援：

- `CreateBranch`
- `CreateRootBranch`
- `CheckoutBranch`
- branch event lineage
- `ForkEventID`
- branch-scoped `RunEvent.BranchID`
- branch task/session rebuild
- task / verification / artifact / memory diff

因此本規格 MUST **延伸現有 Session Tree，而非建立第二套 session branch system**。

v2 核實的現況缺陷（PR-05 起必須修正）：

- `cmd/hufu/sessioncmd.go` 的 fork / checkout 以 `es, _ := team.OpenEventStore(ws)` **吞掉開啟錯誤**。`es == nil` 時，fork 會建出沒有 `ForkEventID` lineage 的 branch，`RebuildSessionForBranch` 也會靜默不動 `session.json`。versioning 的 fork / checkout 必須 fail closed。
- coordinator 只在啟動時讀一次 `session_tree.json`，run 進行中被 CLI 改寫時不會察覺。
- `session_tree.go` 中 `CheckoutBranch` 的註解「current coordinator does not tag BranchID yet」已過時（coordinator 已呼叫 `SetBranchID`），PR-06 順手更正。

## 2.3 Durable runtime events

`EventStore` 已有：

- append-only JSONL
- SHA-256 hash chain
- `BranchID`
- `IdempotencyKey`
- fsync durability boundary
- durability-unknown recovery semantics
- event-first projection原則

Workspace Versioning MUST 遵守同樣原則：

> CAS object 可以先寫入，但 **branch head 不得在對應 durable event 確認前成為 canonical head**。

v2 補充兩點：

- EventStore 的 interprocess `flock` 只在**單次 append 期間**持有，不代表整個 run，**不能**拿來偵測「是否有進行中的 run」。可用的是既有的 team 層級 run-long lock（`TeamSession.WorkspaceLease`），本規格再加一把 project 層級的鎖（§27）。
- 既有 restart repair 慣例是**恢復流程不補寫、不重播事件**（見 `reconcilePendingTerminalCommit`）。Workspace Versioning 的自動 recovery 遵守同一慣例（§16.2）。

## 2.4 Workspace topology（v2 新增）

本規格必須涵蓋下列真實部署形態：

| 形態 | 事實 | 本規格的處理 |
|---|---|---|
| 多 team 共用 subject root | `Workspace`（team 層級）有 `ProjectID` / `TeamName` / `ControlRoot`；`Project` 才有 `SubjectRoot` / `StateDir`。同 project 的多個 team 操作同一份檔案系統 | §26、§27（D2） |
| managed workspace | control root 在 `Project.StateDir/teams/<team>`，registry 在建立時檢查它不與 subject root 重疊 | 正常支援 |
| compatibility scope | `SubjectRoot` 為空時，coordinator 以 `session.Workspace`（control root 本身）當 subject root；legacy 佈局為 `SubjectRoot/workspace/<team>`；`ExecutionWorldSpec.ControlWorkspace` 也允許在 Root 內 | 不是 managed workspace，v1 effective mode 為 `off`（D3）；§9.5 的 control-root 排除仍是 `versionstore` 的防禦性保證 |
| unmanaged run | subject root 是 run 當下的 cwd，**沒有持久化**；`SessionData.RuntimeWorkspace` 是 `<control>/runtime`（artifacts/receipts 暫存區），**不是** subject root | v1 不支援（D3，§26） |

## 2.5 Reusable primitives（v2 新增）

| 用途 | 既有元件 | 限制與處理 |
|---|---|---|
| migration 附 checksum | `internal/workspace/registry_schema.go` | 函式未匯出。PR-00 抽成 `internal/sqlmigrate`，registry 改用它（行為不變） |
| CAS 不覆寫發布、fsync 目錄 | `internal/team/atomic_write.go` 的 `AtomicCreateFile`（hard-link，目標已存在即失敗）、`AtomicWriteFile`、`SyncDir` | 位於 `internal/team`，`versionstore` 依 §6 不能 import。PR-00 搬到 `internal/fsutil`，`internal/team` 保留 thin wrapper |
| interprocess lock | `internal/workspace/lock_unix.go` / `lock_windows.go`（`ErrBusy`）；既有 team 層級 run-long lock 經 `resolveCommandWorkspace` 取得、存於 `TeamSession.WorkspaceLease` | flock 實作未匯出，PR-00 抽到 `internal/fsutil`；team lock 直接沿用（§27） |
| root-anchored、不跟隨 symlink 的寫入 | `internal/tools/scoped_file_access_unix.go`（`os.OpenRoot` + atomic write） | 只有 unix 版本；materialize 以 `os.Root`（Go 1.26）實作，Windows 需補 |
| 「fork event 之前最後一個 X」 | `latestCompactionCheckpointThroughEvent`（`internal/team/compaction_state.go`） | §23 的 lineage 掃描照此模式 |
| fault injection | `EventStore.syncFile` 注入點 | IT7～IT9 沿用 |
| 事件 catalog | `IsKnownEventType`（`internal/team/event_types.go`） | 加入 `workspace_snapshot_committed` |
| SQLite driver | `modernc.org/sqlite`（純 Go） | 直接使用 |

---

# 3. Goals

## G1 — Durable workspace snapshots

能將 workspace filesystem state 持久保存，process restart 後仍可：

```text
List
Diff
Materialize
Restore
Fork
GC
Verify
```

## G2 — Full session fork

`hufu session fork` MUST 同時 fork：

```text
event lineage
task/runtime projection
conversation/compaction state
workspace snapshot head
```

## G3 — Full session checkout

`hufu session checkout <branch>` MUST 使：

```text
session tree active branch
session.json projection
workspace filesystem
workspace branch head
```

全部對齊。

## G4 — Content deduplication

不同 snapshot 中相同內容 MUST 共享 CAS object。

## G5 — Git-independent

沒有 `.git` 或系統沒有 `git` binary，Workspace Versioning 仍可運作。

## G6 — Crash recovery

下列時點 crash 後 MUST 可 deterministic recovery（確定性恢復）：

- CAS object 寫入後
- SQLite snapshot insert 後
- event append 前/後
- materialization 中途
- `session.json` rebuild 中
- active branch 更新前/後
- fork 流程中兩次 `SaveSessionTree` 之間（§18.3）

## G7 — Existing runtime compatibility

不得改變：

- ExecutionTarget durability
- EventStore hash-chain semantics
- resource-scope enforcement
- tool authorization
- retry/resume execution identity
- current `WorkspaceSnapshotter` security invariants
- `off` / `observe` 模式下的 worker 並行度與多 team 並行 run 行為（只有 opt-in 的 `required` 模式會以 subject-root lease 序列化 run，§27）

## G8 — Explicit versioning scope（v2 新增）

Workspace Versioning 只版本化 **subject root 內、依 §9 inclusion policy 納入的 regular file / symlink**。下列項目**不在範圍內**，fork / checkout / restore 不會回溯它們：

- 透過 `--allow-path` / `allowed-paths` 寫到 subject root 以外的檔案
- ignored / unmanaged 路徑（§9.3）
- Git metadata（`.git/`）與 Git submodule 內容（§9.1）
- 網路呼叫、外部服務、資料庫等外部副作用
- 空目錄（D6）

CLI 與文件 MUST 以這個範圍描述功能，不得宣稱「完整還原執行狀態」。

## G9 — Platform scope（v3，D8）

- v1 只在 `runtime.GOOS == "linux"` 啟用。判斷集中在 `versionstore.PlatformSupported()`（以可注入的 `goos` 變數實作，方便測試）。
- 其他平台：config 設定任何非 `off` mode 時，effective mode 強制為 `off`，並在 coordinator startup 印一行 warning；`hufu workspace version ...` 指令回 `ErrUnsupportedPlatform`。
- 只在 Linux 使用的程式碼以 build tag 隔離，其他平台提供回傳 `ErrUnsupportedPlatform` 的 stub。
- 編譯要求：整個 repo 必須能以 `GOOS=darwin` 編譯（CI 已檢查）。新 package（`internal/fsutil`、`internal/sqlmigrate`、`internal/workspace/versionstore`）另外必須能以 `GOOS=windows` 編譯。整個 repo 的 `GOOS=windows go build ./...` 目前無法通過，且不在本規格範圍：`internal/tools/artifact_traversal.go` 缺少 `linux || darwin` build tag（補上即可讓 `internal/tools` 編譯），但下一層 `internal/team` 還依賴約 22 個只在 linux/darwin 定義的 tools API，以及 `unix.Flock`（`decision_index.go`）、`syscall.Kill` / `Setpgid`（`terminal_session.go`）。hufu 從未真正支援 Windows（`AllTools` 在 Windows 回傳空集合），維護者已決定不處理。coding agent 不得為了本規格去修 Windows 編譯。

---

# 4. Non-goals

Phase 1～4 MUST NOT 實作：

- Git interoperability
- Git commit import/export
- merge / three-way merge
- remote CAS
- distributed multi-host replication
- CAS encryption key management
- filesystem block-level snapshot
- OverlayFS dependency
- ReFS/Btrfs/ZFS-specific mandatory implementation
- automatic conflict merge
- binary delta compression

未來可以加入，但不可污染 v1 API。

---

# 5. Terminology

### Workspace Root

實際被 agent / ExecutionWorld 操作的 project filesystem root。

通常對 managed project 為：

```text
workspace.Project.SubjectRoot
```

不是 hufu control workspace。

### Control Workspace

hufu 自己的 durable runtime state root，例如：

```text
session.json
session_tree.json
logs/event_store.jsonl
history/
status/
```

不得被 Workspace Version Store 當成 user workspace version 內容。

### Snapshot

一個不可變 workspace state node：

```text
snapshot_id
parent_snapshot_id
root_tree_hash
metadata
```

### Branch Head

某個 session branch 目前綁定的 workspace snapshot。

### CAS

以 SHA-256 content digest 取得 immutable object。

### Materialization

將 snapshot tree 投影回實際 filesystem。

### Snapshot DAG

多個 snapshot 可共享同一 parent / CAS tree：

```text
             S1
              │
             S2
            /  \
           S3  S4
```

---

# 6. Package architecture

新增：

```text
internal/workspace/versionstore/
├── types.go
├── store.go
├── sqlite.go
├── migrations.go
├── cas.go
├── codec.go
├── inclusion.go                 # v2：自有 inclusion policy（§9）
├── hufuignore.go                # v2：.hufuignore parser / matcher（§9.2）
├── capture.go
├── incremental.go               # library-only in v1（§15）
├── materialize.go
├── diff.go
├── verify.go
├── recovery.go
├── subject_state.go             # v2：subject_state table（§11.6）
├── gc.go
├── lock.go
├── paths.go
└── *_test.go
```

Shared primitives（PR-00，v2 新增；見 §2.5）：

```text
internal/fsutil/                 # AtomicCreateFile / AtomicWriteFile / SyncDir / interprocess flock
internal/sqlmigrate/             # checksum-verified SQLite migrations（registry 與 versionstore 共用）
```

Team integration：

```text
internal/team/
├── workspace_version_service.go
├── workspace_version_events.go
├── workspace_version_recovery.go
├── workspace_version_runtime.go
└── session_tree.go              # minimal integration only
```

CLI：

```text
cmd/hufu/
├── sessioncmd.go                # fork/checkout/diff integration
└── workspaceversioncmd.go       # inspect/doctor/gc/manual snapshot
```

### Dependency rule

禁止：

```text
internal/workspace/versionstore -> internal/team
```

允許：

```text
internal/team -> internal/workspace/versionstore
```

`versionstore` MUST 是 runtime-independent storage primitive。

`versionstore` 只可 import 標準函式庫、`modernc.org/sqlite`、`internal/fsutil`、`internal/sqlmigrate`。它**不得** import `internal/team`，也不 import `internal/workspace`（避免日後 registry 需要 versionstore 時形成循環）。凡是 API 需要的 manifest / delta 型別都由 `versionstore` 自己定義（§13），`internal/team` 負責 adapter。

---

# 7. Persistent layout

對 managed project：

```text
<Project.StateDir>/
└── workspace-versions/
    ├── workspace_versions.db
    ├── lock
    ├── objects/
    │   ├── blob/
    │   │   └── sha256/
    │   │       └── ab/
    │   │           └── cdef...
    │   ├── tree/
    │   │   └── sha256/
    │   │       └── 12/
    │   │           └── 34...
    │   └── link/
    │       └── sha256/
    │           └── 98/
    │               └── 76...
    └── tmp/
```

MUST NOT 寫入：

```text
<Project.SubjectRoot>/.hufu-versions
<Project.SubjectRoot>/.git
```

`workspace-versions/` 是 **project 層級**：同一 project 的所有 team 共用同一個 DB、同一份 CAS 與同一個 `lock`（D2）。branch 仍以 `(workspace_id, branch_id)` 區分，因為 session tree 是 team 層級。

### Permissions

Unix：

```text
workspace-versions/      0700
objects/                 0700
workspace_versions.db    0600
CAS files                0600
tmp files                0600
```

v1 只在 Linux 啟用 workspace versioning（D8），因此只需要上述 Unix mode。其他平台見 §3 G9。

---

# 8. CAS object model

## 8.1 Blob object

regular file bytes：

```text
blob_hash = SHA256(raw_file_bytes)
```

這個決策刻意與現有 `WorkspaceFileState.SHA256` 對齊，使 incremental ingestion 可以直接驗證。**只對 regular file 成立**：`WorkspaceFileState` 對 symlink 記錄的是目標內容或 escaping placeholder 的 hash（§2.1.1），不能拿來驗證 link object。

Object path：

```text
objects/blob/sha256/<first-2>/<remaining-62>
```

CAS object MUST：

- immutable
- atomic create
- hash verified before publication
- existing object 必須 verify size/hash 或安全 reuse
- 不得 overwrite 已存在但 hash mismatch 的 object

## 8.2 Link object

symbolic link 的 raw target string：

```text
link_hash = SHA256([]byte(readlink_target))
```

Object path：

```text
objects/link/sha256/...
```

tree entry 必須標記 `kind=symlink`，不能把 link 當成 regular file。

## 8.3 Tree object

Tree 是 Merkle tree（Merkle 樹：每個 directory hash 由其 children identity 決定）。

```go
type TreeEntry struct {
    Name       string
    Kind       EntryKind // blob | tree | symlink
    ObjectHash string
    Mode       uint32
    Size       int64
}

type TreeObject struct {
    Version uint8
    Entries []TreeEntry // mandatory bytewise sorted by Name
}
```

### Canonical encoding

MUST 使用自有 deterministic binary codec，不新增 CBOR/MessagePack dependency。

Format v1：

```text
magic: "HUFUTREE\x00"
version: uint8 = 1
entry_count: uvarint

for each sorted entry:
    name_len: uvarint
    name_bytes: UTF-8
    kind: uint8
    mode: uint32 little-endian
    size: uint64 little-endian
    object_hash: 32 raw bytes
```

Hash：

```text
tree_hash = SHA256(canonical_tree_bytes)
```

### Entry field values（v2 定案）

不同實作必須對同一棵樹算出同一個 hash，因此各 kind 的欄位值固定如下：

| kind | `mode` | `size` | `object_hash` |
|---|---|---|---|
| `blob` | `perm & 0o777`（與 `WorkspaceFileState.Mode` 相同，不保存 setuid/setgid/sticky） | 檔案 byte 數 | `SHA256(raw bytes)` |
| `symlink` | `0` | `len(readlink_target)` | `SHA256([]byte(readlink_target))` |
| `tree` | `0` | `0` | child tree hash |

- kind 編碼：`blob=1`、`tree=2`、`symlink=3`。
- **空目錄不保留（D6）**：capture 不產生沒有任何 entry 的 tree；若某目錄底下所有檔案都被排除，該目錄不出現在 tree 中。
- **檔名**：非合法 UTF-8 的 path component 使 capture 以 `ErrUnsupportedPathName` 失敗（不得跳過或改名）。
- **case-insensitive filesystem**（例如 Linux 上開了 casefold 的目錄）：v1 不做專門偵測。若 tree 內有兩個名稱在 case folding 後相同，materialize 後的 §21.5 verification 會發現其中一個 hash 不符而失敗（fail closed），operation 標記 `failed`。

### Tree validation

Reject：

- duplicate entry names
- name == `.`
- name == `..`
- `/`
- `\x00`
- invalid UTF-8 path component
- unknown kind
- malformed hash
- unsorted entries
- 非 root 的空 tree（D6）
- `tree` / `symlink` entry 的 `mode`、`size` 不符合上表

---

# 9. Filesystem inclusion policy

`versionstore` MUST 有**自己的** inclusion policy（`inclusion.go`），**不得**沿用 `internal/team` 的 `workspaceInternalDirs` / `isWorkspaceInternalPath`。那套規則會在任何深度排除名為 `tasks`、`shared`、`status`、`history`、`logs` 的目錄（§2.1.1），沿用會讓這些專案檔永遠不被版本化，違反 §9.4 的「MUST NOT silently omit」。

v2 inclusion policy 只排除三類路徑：

1. 任何深度、名稱為 `.git` 的 entry（目錄或 gitfile）
2. control root 子樹（§9.5）
3. Git workspace 中被 Git ignore 的路徑（§9.1），或非 Git workspace 中被 `.hufuignore` 排除的路徑（§9.2）

除此之外的 regular file 與 symlink 都是 managed path。

## 9.1 Git workspace

若：

```text
git binary available
AND
root is Git working tree
```

使用既有：

```text
git ls-files -z
git ls-files -z --others --exclude-standard
```

因此：

- tracked files included
- untracked, non-ignored included
- `.gitignore` ignored files excluded
- `.git/` excluded

Git 只決定 candidate set。

所有 bytes 仍由 hufu 自己讀取與 hash。

Candidate 的處理規則（v2 明訂）：

- tracked 但 working tree 中已刪除的路徑：不在 snapshot 中（視同刪除），不是錯誤
- Git submodule（gitlink entry，`git ls-files` 列出的是目錄）：v1 **不**版本化 submodule 內容，該路徑視為 unmanaged，materialize 時保留（G8）
- candidate 是指向目錄的 symlink：以 `symlink` entry 保存 link target，不展開目錄內容

## 9.2 Non-Git workspace

使用 secure directory walk，排除規則見 §9 開頭的三類。

**`.hufuignore`（v2：Phase 1 必要項目，D7）**

非 Git workspace 沒有 `.gitignore` 語意可用，`node_modules`、build 輸出等會被整包 capture，很容易撞到 `MaxFiles`。因此：

- `mode=required` 且 subject root 不是 Git working tree 時，subject root 下 MUST 存在 `.hufuignore`（可以是空檔，代表明確選擇不排除任何路徑）。不存在則 run admission 與 fork / checkout 回 `ErrHufuignoreRequired`。
- `mode=observe` 時缺少 `.hufuignore` 只記錄 warning。
- Git workspace 忽略 `.hufuignore`（Git ignore 語意已足夠，避免兩套規則互相矛盾）。

v1 語法（刻意保持最小，不引入新 dependency）：

- 一行一個 pattern；空行與 `#` 開頭的行忽略
- 以 `/` 開頭：相對 subject root 的錨定路徑前綴
- 不以 `/` 開頭：比對任一層的 basename，使用 `path.Match` glob
- 以 `/` 結尾：只比對目錄
- v1 **不支援** `!` negation 與 `**`；出現時 parse 失敗並回報行號

`.hufuignore` 本身是 managed file，會被版本化。

## 9.3 Ignored/unmanaged paths

Materialization MUST NOT 刪除 snapshot namespace 以外的檔案。

例：

```text
node_modules/       # ignored
.env                # ignored by project .gitignore
.git/
```

branch checkout 不得因 target snapshot 沒有它們就刪除。

## 9.4 Size budget

MUST 有 configurable safety budget：

```go
type CaptureLimits struct {
    MaxFiles        int
    MaxFileBytes    int64
    MaxLogicalBytes int64
}
```

規則：

- 超限 MUST fail capture
- MUST NOT silently omit oversized managed files
- `MaxFiles` 預設 200000（與現有 `workspaceSnapshotMaxFiles` 相同）
- `MaxFileBytes`、`MaxLogicalBytes` 為 0 時代表不限制；非 0 時超過即 fail
- 預設值集中定義在 `versionstore/types.go` 的 `DefaultCaptureLimits()`，config（§31）只能覆寫，不散落 magic number

## 9.5 Control-root exclusion（v2 新增）

compatibility scope 與 legacy 佈局下，control root 可能位於 subject root 內，甚至與 subject root 相同（§2.4）。v1 只對 managed workspace 啟用 versioning（D3），而 registry 保證 managed control root 不與 subject root 重疊，所以正常情況下不會觸發本節；但 `versionstore` 與 service 層仍 MUST 實作這個檢查，作為防禦性保證（v1.x 支援 unmanaged 時會直接用到）。現有 snapshotter 只排除剛好等於 `<control>/session.json` 的路徑，`session_tree.json`、`runtime/`、compaction state 等控制檔都會被 capture，並在 materialize 時被舊版本覆蓋。

規則：

- 解析出 canonical subject root 與 canonical control root 後：
  - control root 在 subject root **內**：整個 control root 子樹從 capture、diff、materialize 中排除，視為 unmanaged（materialize 不讀、不寫、不刪）
  - control root **等於** subject root：回 `ErrVersionedWorkspaceUnresolved`
  - control root 在 subject root **外**（managed workspace 的正常情況）：不需額外排除
- 同 project 其他 team 的 control root 也適用同一規則（它們在 managed 模式下都位於 `Project.StateDir`，實務上不會落在 subject root 內）

---

# 10. Symlink / special-file policy

## 10.1 Symlink

Capture：

1. `Lstat`
2. `Readlink`
3. 保存 link target，不 follow bytes
4. 計算解析後 target 是否仍在 root

Snapshot metadata：

```go
type SymlinkSafety string

const (
    SymlinkInternal SymlinkSafety = "internal"
    SymlinkEscaping SymlinkSafety = "escaping"
)
```

### Materialization rule

`internal` symlink：

- 可 materialize
- materialize 前再次驗證 target 不逃逸 target root

`escaping` symlink：

- 可以作為 forensic identity 存入 snapshot
- v1 MUST NOT 自動 materialize
- snapshot 標記 `Materializable=false`
- checkout/restore 預設 fail closed，除非未來加入 explicit unsafe policy

## 10.2 Special files

Reject：

- socket
- device
- FIFO
- named pipe
- unknown filesystem type

不得將它們轉為空 regular file。

判斷 MUST 在開檔**之前**以 `Lstat` 完成，只有 regular file 才可 `open`。現有 walk 路徑對 FIFO 直接 `os.Open` 會無限期阻塞（§2.1.1），`versionstore` 不得重蹈。開啟 regular file MUST 經由 `os.Root`，並在開啟後以 `fstat` 確認仍是 regular file，避免 `Lstat` 與 `open` 之間被換成 symlink / FIFO。

---

# 11. SQLite schema

新 DB：

```text
workspace_versions.db
```

不得塞入 workspace registry DB，避免 registry lifecycle 與 high-churn snapshot metadata 耦合。

### Connection pragmas（v2 定案）

沿用 registry 的 durability-first 組合（`internal/workspace/registry.go`）：

```sql
PRAGMA busy_timeout=5000;
PRAGMA journal_mode=DELETE;
PRAGMA synchronous=FULL;
PRAGMA foreign_keys=ON;
```

- `foreign_keys=ON` 是本 schema 的 `REFERENCES` 生效的前提，每條連線都必須設定。
- 不使用 WAL：DB 與 CAS 的 durability 論證以「transaction commit 即落盤」為前提，`synchronous=FULL` + rollback journal 最容易推理。
- read-only 連線（list / diff / show）改用 `PRAGMA query_only=ON`。
- 依 `docs/architecture/sqlite-maintenance-policy.md`，任何路徑（含 GC、doctor）都**不得**執行 `VACUUM`、`PRAGMA optimize` 或 checkpoint。

## 11.1 schema_migrations

沿用 hufu registry migration style：

```sql
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at INTEGER NOT NULL,
    checksum TEXT NOT NULL
);
```

每個 migration MUST checksum 驗證。

## 11.2 snapshots

```sql
CREATE TABLE snapshots (
    id TEXT PRIMARY KEY,

    workspace_id TEXT NOT NULL,
    branch_id TEXT NOT NULL,

    parent_snapshot_id TEXT
        REFERENCES snapshots(id),

    root_tree_hash TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,

    file_count INTEGER NOT NULL,
    logical_bytes INTEGER NOT NULL,

    materializable INTEGER NOT NULL
        CHECK (materializable IN (0,1)),

    reason TEXT NOT NULL,

    run_id TEXT,
    task_id TEXT,
    attempt INTEGER,

    anchor_event_id TEXT,
    commit_event_id TEXT UNIQUE,

    state TEXT NOT NULL
        CHECK (state IN ('pending','published','orphaned','pruned')),

    created_at INTEGER NOT NULL,
    published_at INTEGER,
    pruned_at INTEGER
);

CREATE INDEX idx_snapshots_workspace_branch_created
ON snapshots(workspace_id, branch_id, created_at);

CREATE INDEX idx_snapshots_parent
ON snapshots(parent_snapshot_id);
```

`commit_event_id UNIQUE` 本身已建立 index，不另建 `idx_snapshots_commit_event`。

`pruned`（D4）：snapshot row 永遠保留，DAG lineage 不會斷；只有其 root tree 以下、且不被其他保留 snapshot 引用的 CAS objects 會被 GC 回收（§28）。

## 11.3 branch_heads

```sql
CREATE TABLE branch_heads (
    workspace_id TEXT NOT NULL,
    branch_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL
        REFERENCES snapshots(id),

    generation INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    PRIMARY KEY(workspace_id, branch_id)
);
```

`generation` 提供 optimistic CAS update：

```sql
UPDATE branch_heads
SET snapshot_id=?, generation=generation+1, updated_at=?
WHERE workspace_id=? AND branch_id=? AND generation=?;
```

0 affected rows = concurrent head move。

第一次建立 head（v2 定案）：`branch_heads` 尚無該 row 時，呼叫端以 `expectedGeneration = 0` 表示「預期尚無 head」，實作以 `INSERT ... generation = 1` 完成；若 row 已存在（`PRIMARY KEY` 衝突）即回 `ErrBranchHeadConflict`。`expectedGeneration > 0` 一律走上面的 `UPDATE`。

## 11.4 operations

```sql
CREATE TABLE operations (
    id TEXT PRIMARY KEY,

    workspace_id TEXT NOT NULL,
    branch_id TEXT,

    kind TEXT NOT NULL CHECK (
        kind IN (
            'capture',
            'fork',
            'checkout',
            'restore',
            'materialize',
            'adopt',
            'downgrade',
            'gc'
        )
    ),

    state TEXT NOT NULL CHECK (
        state IN (
            'prepared',
            'objects_written',
            'event_committed',
            'materializing',
            'materialized',
            'projection_updated',
            'completed',
            'failed'
        )
    ),

    from_snapshot_id TEXT,
    to_snapshot_id TEXT,

    idempotency_key TEXT,
    target_branch_id TEXT,

    detail_code TEXT NOT NULL DEFAULT '',

    started_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_operations_incomplete
ON operations(state, updated_at);
```

## 11.5 object_index（v1 不實作）

v3 定案：v1 **不**建立此表。GC 以走訪 `objects/` 目錄與 tree 取得所需資訊，storage 統計由 `workspace version status` 走訪 `objects/` 計算。下列 schema 僅保留作為 v1.x 參考，不是 v1 工作項目：

```sql
CREATE TABLE object_index (
    kind TEXT NOT NULL CHECK (kind IN ('blob','tree','link')),
    hash TEXT NOT NULL,
    bytes INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    last_verified_at INTEGER,

    PRIMARY KEY(kind, hash)
);
```

其用途：

- GC
- integrity scan
- storage statistics

但 snapshot correctness MUST NOT 依賴 object_index；CAS filesystem bytes 才是 object authority。

## 11.6 subject_state（v2 新增，Phase 2）

每個 subject root 一列，存放不屬於任何單一 branch 的持久狀態：

```sql
CREATE TABLE subject_state (
    subject_key TEXT PRIMARY KEY,        -- SHA256(canonical subject root)
    subject_root TEXT NOT NULL,          -- canonical absolute path；B6 的持久綁定

    mode_floor TEXT NOT NULL
        CHECK (mode_floor IN ('off','observe','required')),

    materialized_workspace_id TEXT,      -- 目前落在磁碟上的是哪個 branch 的 head（diagnostic projection）
    materialized_branch_id TEXT,
    materialized_snapshot_id TEXT
        REFERENCES snapshots(id),

    recovery_required INTEGER NOT NULL DEFAULT 0
        CHECK (recovery_required IN (0,1)),
    recovery_code TEXT NOT NULL DEFAULT '',
    recovery_detail TEXT NOT NULL DEFAULT '',

    checkpoint_deferred INTEGER NOT NULL DEFAULT 0
        CHECK (checkpoint_deferred IN (0,1)),

    generation INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
```

用途：

- **`subject_root`**：第一次 baseline 時寫入。之後每次開啟都比對 canonical path，不一致（例如 project 被 rebind）即回 `ErrVersionedWorkspaceUnresolved`，由 `workspace version doctor` 處理。
- **`mode_floor`**（G13）：一旦寫入 `required`，之後以 `observe` / `off` 開啟時 MUST 拒絕並提示需要明確 downgrade 指令（`hufu workspace version downgrade`，會記錄 operation row）。
- **`materialized_*`**：只供 doctor 與 CLI 提示使用，不是 authority；authority 仍是 `branch_heads` 與 live filesystem。
- **`recovery_required`**（D5，§22.3）：unauthorized mutation 等狀況的 durable marker。
- **`checkpoint_deferred`**（§22.2）：run 結束前的 checkpoint 失敗時設為 1，下一次 admission 清除。

`subject_state` 的更新同樣使用 `generation` optimistic update。

## 11.7 stat_cache（v2 新增，Phase 2）

run admission 與 checkout 前都要比對 live filesystem 與 head（§22.1、§19.1），沒有快取就得每次重新 hash 全部 bytes。

```sql
CREATE TABLE stat_cache (
    subject_key TEXT NOT NULL,
    path TEXT NOT NULL,
    size INTEGER NOT NULL,
    mtime_ns INTEGER NOT NULL,
    ctime_ns INTEGER NOT NULL,
    inode INTEGER NOT NULL,
    mode INTEGER NOT NULL,
    blob_hash TEXT NOT NULL,
    PRIMARY KEY(subject_key, path)
);
```

規則：

- 只作為「這個檔案可能沒變」的 hint；size / mtime / ctime / inode / mode 任一不同就必須重新 hash
- mtime 與 capture 開始時間的差距小於 filesystem timestamp 精度時（racy-clean，同 Git index 的處理），不得信任 cache
- correctness 不得依賴 stat_cache；刪除整張表只影響效能
- v3 定案：v1 **必須**實作並在 run admission、run checkpoint、fork / checkout 前的 drift 比對中使用

---

# 12. Snapshot identity

Snapshot ID 不等於 root tree hash。

原因：

兩個不同 checkpoint 可能 filesystem 完全相同，但 lineage 不同：

```text
S10(root=A)
  │
S11(root=A)   # no filesystem change, but checkpoint/event identity differs
```

v2 定案：沿用 `internal/workspace.IDGenerator` 的**格式**（`<kind>_<32 hex from crypto/rand>`），但不 import 該 package（§6）：

```text
snapshot:  wsv_<32 hex>
operation: wop_<32 hex>
```

不需要 time-sortable：snapshot 的順序由 `created_at` 與 DAG parent 決定，ID 只負責唯一性。

Snapshot immutable fields 建立後不得 UPDATE：

```text
parent_snapshot_id
root_tree_hash
manifest_digest
workspace_id
branch_id
reason
run/task/attempt identity
```

只允許：

```text
pending   -> published
pending   -> orphaned
published -> pruned      # 只能由 GC apply 執行（D4，§28）
```

`pruned` 是終態；目前被任何 `branch_heads` 指向的 snapshot 不得轉為 `pruned`。

---

# 13. VersionStore API

`internal/workspace/versionstore/types.go`

```go
type SnapshotID string

type SnapshotReason string

const (
    SnapshotBaseline       SnapshotReason = "baseline"
    SnapshotExternalDrift  SnapshotReason = "external_drift"
    SnapshotRunCheckpoint  SnapshotReason = "run_checkpoint" // v2：run 結束前的 quiescent checkpoint（§22.2）
    SnapshotFork           SnapshotReason = "fork"
    SnapshotCheckoutSave   SnapshotReason = "checkout_save"
    SnapshotRestore        SnapshotReason = "restore"        // v2：§20 的 restore node
    SnapshotManual         SnapshotReason = "manual"
    SnapshotAdopt          SnapshotReason = "adopt"          // v2：操作者明確接受 recovery-required 狀態（§22.3）

    // SnapshotAttempt 保留給 v1.x 的 attempt-level checkpoint（D1），v1 不得產生。
    SnapshotAttempt        SnapshotReason = "attempt"
)

type CaptureRequest struct {
    WorkspaceID string
    BranchID    string

    Root string

    // ExcludeSubtrees 是 Root 相對路徑的子樹，整個排除並視為 unmanaged。
    // team 端以此傳入 control root 子樹（§9.5）。
    ExcludeSubtrees []string

    Parent SnapshotID

    Reason SnapshotReason

    RunID  string
    TaskID string
    Attempt int

    AnchorEventID string

    Limits CaptureLimits
}

// ObservedFile / ObservedDelta 是 versionstore 自有型別（§6 依賴規則）。
// internal/team 負責把 WorkspaceSnapshot / WorkspaceDelta 轉成這些型別。
type ObservedFile struct {
    Path   string // Root 相對、forward-slash
    SHA256 string // 只對 regular file 有意義
    Bytes  int64
    Mode   uint32 // perm bits
}

type ObservedDelta struct {
    Added    []ObservedFile
    Modified []ObservedFile
    Deleted  []string
}

type Snapshot struct {
    ID               SnapshotID
    WorkspaceID      string
    BranchID         string
    Parent           SnapshotID
    RootTreeHash     string
    ManifestDigest   string
    FileCount        int
    LogicalBytes     int64
    Materializable   bool
    Reason           SnapshotReason
    CommitEventID    string
    CreatedAt        time.Time
}

type MaterializeRequest struct {
    WorkspaceID string
    Root        string
    SnapshotID  SnapshotID

    CurrentSnapshotID SnapshotID

    // ExcludeSubtrees 與 CaptureRequest 相同；materialize 不得讀、寫、刪這些子樹。
    ExcludeSubtrees []string

    PreserveUnmanaged bool
}

type VersionStore interface {
    Capture(context.Context, CaptureRequest) (Snapshot, error)

    // CaptureFromDelta 在 v1 是 library-only primitive（D1）：
    // runtime 不呼叫它，只有 §15 的測試與 benchmark 使用。
    CaptureFromDelta(
        context.Context,
        CaptureRequest,       // Parent 必須是已 published 的 snapshot，作為 base tree
        ObservedDelta,
    ) (Snapshot, error)

    GetSnapshot(context.Context, SnapshotID) (Snapshot, error)

    GetBranchHead(
        context.Context,
        workspaceID string,
        branchID string,
    ) (Snapshot, bool, error)

    // expectedGeneration = 0 表示預期該 branch 尚無 head（§11.3）。
    PublishSnapshot(
        context.Context,
        SnapshotID,
        commitEventID string,
        expectedGeneration int64,
    ) error

    AttachBranch(
        context.Context,
        workspaceID string,
        branchID string,
        base SnapshotID,
        reason SnapshotReason,
    ) (Snapshot, error)

    Materialize(context.Context, MaterializeRequest) error

    Diff(
        context.Context,
        from SnapshotID,
        to SnapshotID,
    ) (WorkspaceVersionDelta, error)

    Verify(context.Context, SnapshotID) error

    Recover(context.Context) ([]RecoveryAction, error)

    GC(context.Context, GCRequest) (GCResult, error)

    Close() error
}
```

### Important

`VersionStore` MUST NOT expose：

```text
Commit
Branch
CheckoutIndex
GitRef
GitObject
```

Hufu semantic 使用：

```text
Snapshot
BranchHead
Fork
Materialize
Restore
Diff
```

---

# 14. Capture algorithm

## 14.1 Full capture

Pseudo flow：

```text
Acquire workspace version lock
        │
        ▼
Resolve secure candidate set
        │
        ▼
For each entry
  ├─ regular file -> hash + CAS blob
  ├─ directory    -> recursive tree
  ├─ symlink      -> link object
  └─ special      -> fail
        │
        ▼
Build Merkle trees bottom-up
        │
        ▼
fsync CAS objects
        │
        ▼
INSERT snapshot(state=pending)
        │
        ▼
return pending snapshot
```

`Capture()` 本身不得直接 advance branch head。

Branch head publication 要經過 event boundary。

## 14.2 Stable-read requirement

Capture 不可接受「掃到一半 workspace 持續變動」卻宣稱 deterministic snapshot。

對 regular file：

```text
Lstat before
read/hash/store
Lstat after
```

若以下任一變更：

```text
size
mtime
file identity where available
mode
```

則該 file retry，同一 file 最多重試 3 次。

完成 tree 後 MUST 對所有已 capture 的 regular file 再做一次 `Lstat`，比對上列欄位；任一不同即整次 capture 重來，整次 capture 最多重試 2 次（共 3 次）。

全 workspace 超過 retry budget：

```text
ErrWorkspaceUnstable
```

不得產生 published snapshot。

## 14.3 CAS write

Object publication：

```text
tmp/<random>
    ↓ write
hash while write
    ↓ verify expected hash
fsync(file)
    ↓
rename(temp, final)
    ↓
fsync(parent dir)
```

如果 final 已存在：

1. verify existing object
2. match → discard temp
3. mismatch → `ErrCASCorrupt`

不得 overwrite。

---

# 15. Incremental capture

> **v2 修正：** v1 版本宣稱「目前 ExecutionWorld 已能取得 baseline / final WorkspaceSnapshot 與 WorkspaceDelta」，這只對 Codex provider 成立（§2.1.1）。原生 worker 沒有 delta，而且並行寫同一 root，per-attempt delta 在並行 run 中沒有定義。
>
> 因此 v1 的 runtime checkpoint 一律使用 **full capture**（可用 §11.7 stat_cache 加速），`CaptureFromDelta` 只作為 library primitive 實作與測試，供 v1.x 的 attempt-level checkpoint（D1）使用。

`CaptureFromDelta` 的輸入是一次觀察期間的：

```text
Added
Modified
Deleted
```

只要觀察期間 subject root 沒有其他寫入者，persistent version 就不必 full re-scan。

## 15.1 Adapter（v1.x 才需要）

在 `internal/team/workspace_snapshot.go` 新增 read-only export：

```go
func (s WorkspaceSnapshot) Manifest() []WorkspaceFileState
```

規則：

- 回傳 defensive copy
- deterministic path order
- 不暴露 mutable internal map

或新增 internal-only adapter：

```go
type WorkspaceSnapshotManifestReader interface {
    ManifestEntries() []WorkspaceFileState
}
```

不要直接 export `files map`。

adapter 放在 `internal/team`，負責把 `WorkspaceFileState` / `WorkspaceDelta` 轉成 `versionstore.ObservedFile` / `ObservedDelta`（§13）。

## 15.2 Incremental tree rewrite

假設：

```text
parent root
└── src/
    ├── a.go = H1
    └── b.go = H2
```

只修改 `src/b.go`：

```text
new blob H3
new tree src = T2
new root tree = R2
```

不重寫：

```text
a.go blob H1
任何未變 subtree
```

Algorithm：

1. Load parent root tree
2. 對 Added/Modified path：
   - `Lstat` 判斷實際 entry type（不得依賴 `ObservedFile.Mode`，它只有 perm bits）
   - regular file：read actual bytes、compute SHA-256，MUST equal `ObservedFile.SHA256`，寫入 CAS blob
   - symlink：`Readlink` 後寫入 link object；**不**比對 `ObservedFile.SHA256`（那是目標內容或 escaping placeholder 的 hash，§2.1.1）
   - 其他 type：依 §10.2 fail
3. 對 Deleted path 移除 entry
4. 只 rebuild affected ancestor trees
5. 新 root tree hash
6. insert pending snapshot

**type change 的限制：** `WorkspaceDelta` 只以 SHA / bytes / perm 判斷 Modified。regular file 換成內容相同、perm 相同的 symlink（Git candidate 路徑會 follow 內部 symlink 取 `Stat`）時，delta 看不出變化。因此 `CaptureFromDelta` 只能用在 delta 由 `versionstore` 自己的觀察產生、或呼叫端能保證沒有 type change 的情境；v1.x 啟用前 MUST 補上這個缺口（例如在 `WorkspaceFileState` 加入 entry type），否則維持 full capture。

若 bytes 已在 final snapshot 後被外部修改：

```text
actual SHA != observed SHA
```

MUST fail：

```text
ErrWorkspaceChangedAfterObservation
```

不要 silently capture later bytes。

---

# 16. Event-first publication

CAS data 必須先存在，event 才能宣告 snapshot durable。

新增 event type：

```text
workspace_snapshot_committed
```

Payload：

```go
type WorkspaceSnapshotCommittedPayload struct {
    SnapshotID       string `json:"snapshot_id"`
    ParentSnapshotID string `json:"parent_snapshot_id,omitempty"`
    RootTreeHash     string `json:"root_tree_hash"`
    ManifestDigest   string `json:"manifest_digest"`

    FileCount    int   `json:"file_count"`
    LogicalBytes int64 `json:"logical_bytes"`

    // v2：observability 投影來源（§35）
    NewCASBytes       int64 `json:"new_cas_bytes"`
    ReusedCASBytes    int64 `json:"reused_cas_bytes"`
    CaptureDurationMS int64 `json:"capture_duration_ms"`

    Reason string `json:"reason"`

    RunID   string `json:"run_id,omitempty"`
    TaskID  string `json:"task_id,omitempty"`
    Attempt int    `json:"attempt,omitempty"`

    Materializable bool `json:"materializable"`
}
```

Event：

```go
RunEvent{
    BranchID: branchID,
    Type: "workspace_snapshot_committed",
    IdempotencyKey: "workspace-snapshot:"+snapshotID,
}
```

## 16.1 Publication ordering

MUST：

```text
1. CAS objects durable
2. SQLite snapshot = pending
3. EventStore AppendPersisted(workspace_snapshot_committed)
4. SQLite transaction:
     pending -> published
     branch_head -> snapshot
```

v1 沒有其他需要刷新的 projection（`session.json` 不記錄 workspace head）。

禁止：

```text
branch head update
    BEFORE
event durable
```

## 16.2 Crash cases

### Crash after 1

只有 orphan CAS objects。

GC 可清除。

### Crash after 2, before 3

`pending snapshot` 無 durable event。

Recovery（v2 定案）：

- 標記 `orphaned`，對應 operation 標記 `failed`
- **自動 recovery 不補寫 event**

理由：

1. 與既有慣例一致：restart repair 不補寫、不重播事件（`reconcilePendingTerminalCommit`）。
2. 補寫的 event 會落在 event log 的「現在」，但 snapshot 內容是 crash 前的檔案系統；§23 的 historical fork 會因此把舊內容當成較晚時點的狀態。
3. 不補寫也不會遺失資料：live filesystem 仍在，下一次 admission（§22.1）或 fork / checkout 前的 drift capture 會重新 capture。

操作者明確觸發的指令（例如重新執行 fork）屬於新的 operation，可以正常 append event，不受此限。

### Crash after 3, before 4

Event 已 canonical。

Recovery MUST：

- 找到 event payload
- verify snapshot CAS
- 將 snapshot mark published
- repair branch head

這是最重要的 reducer-repair path。

---

# 17. Branch head semantics

`branch_heads` 是 workspace bytes lineage 的 canonical projection。

Branch head MUST satisfy：

```text
head.workspace_id == branch workspace
head.snapshot.branch_id == branch_id
snapshot.state == published
snapshot CAS verifies
```

例：

```text
main -> S10

fork experiment

main       -> S10
experiment -> S11

S11.parent = S10
S11.root_tree_hash = S10.root_tree_hash
```

Fork 不 copy bytes。

---

# 18. Session fork integration

修改：

```text
cmd/hufu/sessioncmd.go
internal/team/session_tree.go
```

但 `SessionTree` 仍負責 event lineage；版本儲存由 `WorkspaceVersionService` 負責。

## 18.1 Fork active branch HEAD

現有：

```bash
hufu session fork --name exp
```

新流程：

```text
0. open EventStore for write；失敗即 fail closed（不得沿用現行 `es, _ :=` 吞錯的行為）
1. resolve control workspace + subject root + workspace ID（§26）
2. 經 `resolveCommandWorkspace`（`ResolveExisting`）取得 team lock，再取得 project lock（§27）；任一取不到回 ErrVersionOperationInProgress
3. 若 live filesystem 與 source head 不同：capture + publish 到 source branch（reason=external_drift）
4. SnapshotBranchState(existing behavior)
5. CreateBranch(existing behavior)；create fork operation row（target_branch_id = child）
6. SaveSessionTree with new branch INACTIVE
7. AttachBranch:
       parent snapshot = source head
       new snapshot metadata node
       same root_tree_hash
8. append workspace_snapshot_committed on child branch
9. publish child branch head
10. MaterializeCompactionBranch(existing)
11. RebuildSessionForBranch(existing)
12. set ActiveBranch = child
13. SaveSessionTree；operation → completed
14. release lease
```

Fork from current HEAD MUST NOT materialize：第 3 步之後 live filesystem 已等於 source head，child 又共用同一個 root tree。

## 18.2 Fork historical branch/event

```bash
hufu session fork evt-xxx --name experiment
```

必須：

1. §18.1 的第 0～2 步（EventStore fail closed、resolve、lease）
2. resolve parent event lineage
3. find latest **published workspace snapshot visible at or before fork event**（§23）
4. if none，或找到的 snapshot 是 `pruned`：
   - return `ErrWorkspaceSnapshotUnavailable`（`pruned` 時 detail 註明已被 GC 修剪）
   - MUST NOT silently use current workspace
5. verify selected snapshot CAS
6. **先保存目前 active branch 的 live 狀態（v2 新增）：** 若 live filesystem 與 active branch head 不同，capture + publish 到 active branch（reason=`checkout_save`）。historical fork 會把較舊的 snapshot materialize 到 live filesystem，少了這一步，未 capture 的修改會被覆蓋
7. create fork operation row
8. SaveSessionTree with new branch INACTIVE
9. AttachBranch：child metadata snapshot reusing selected root tree
10. append workspace_snapshot_committed on child branch；publish child head
11. materialize selected snapshot；operation → materialized
12. MaterializeCompactionBranch / rebuild session projection to event fork point
13. activate child；SaveSessionTree；operation → completed
14. release lease

### Legacy events

Feature introduction以前的 event 沒有 workspace checkpoint。

Default behavior：

```text
fail closed
```

可提供 explicit compatibility flag：

```bash
hufu session fork <legacy-event> --metadata-only
```

但輸出 MUST 清楚標示：

```text
workspace_state: unavailable
```

不能假裝是完整 fork。

## 18.3 Fork crash recovery（v2 新增）

fork 流程在兩次 `SaveSessionTree` 之間 crash，會留下「session tree 裡有 child branch、但沒有 workspace head」的狀態；不處理的話，它會被誤判成 legacy branch。recovery 依 fork operation row 與 child branch 的 event 決定：

| crash 時點 | 判斷依據 | recovery 動作 |
|---|---|---|
| operation row 建立後、child 寫入 session tree 前 | session tree 沒有 `target_branch_id` | operation → `failed` |
| child 已寫入（INACTIVE）、`workspace_snapshot_committed` 尚未 durable | child 仍 INACTIVE、event log 中沒有任何帶 child `BranchID` 的事件、child snapshot 為 `pending` | snapshot → `orphaned`；從 session tree **移除**該 child branch；operation → `failed`。這個移除是確定性的：session tree 的 branch 建立本身不是 event，child 也還沒有任何 lineage |
| event 已 durable、head 未 publish 或後續步驟未完成 | event log 有 child 的 `workspace_snapshot_committed` | **complete forward**：依 §16.2「crash after 3」修復 head，接著完成 materialize、compaction、session rebuild、activate。與 checkout 一樣只往前完成，不回滾 |

不論哪一種，recovery 都不補寫 event（§16.2）。使用者若仍要該 fork，重新執行 `hufu session fork` 即可（新的 operation）。

---

# 19. Session checkout integration

現有：

```bash
hufu session checkout target
```

目前只 rebuild `session.json`。

新 semantic：

> checkout = session projection + workspace materialization

## 19.1 Checkout flow

```text
source branch A
target branch B
```

MUST：

```text
0. open EventStore for write；失敗即 fail closed
1. 經 `resolveCommandWorkspace`（`ResolveExisting`）取得 team lock，再取得 project lock（§27）；任一取不到回 ErrVersionOperationInProgress
2. verify no active mutating run：materializing checkout 只在 subject root 的 effective mode
   為 required 時執行（observe 模式下 checkout 維持舊行為，§43），而 effective mode 為 required 時
   同 subject root 所有 team 的 run 都必須全程持有 lease（§27）。因此 lease 取得成功
   即代表沒有進行中的 run
3. detect live workspace drift from A.head
4. if drift:
       capture + publish A.new_head（reason=checkout_save）
5. resolve B.head；B.head 為 pruned 時回 ErrWorkspaceSnapshotUnavailable
6. verify B.head CAS
7. create checkout operation row
8. materialize B.head
9. mark operation materialized
10. RebuildSessionForBranch(B)
11. rebuild compaction state as existing logic requires
12. set session tree ActiveBranch=B
13. SaveSessionTree
14. mark operation completed
```

## 19.2 Crash after filesystem materialized but before ActiveBranch save

Operation row：

```text
state=materialized
target_branch_id=B
to_snapshot_id=B.head
```

下一次 hufu startup MUST complete-forward：

```text
rebuild session B
set active branch B
mark completed
```

「hufu startup」在 v2 明確定義為下列任一入口，且 MUST 在執行本身的工作**之前**完成 recovery：

- coordinator 啟動（`coordinator_eventstore.go` 的 startup 路徑，與 `reconcilePendingTerminalCommit` 同一階段）
- 任何會變更狀態的 `hufu session ...` 或 `hufu workspace version ...` 指令

唯讀指令（`list`、`show`、`diff`、`status`）只回報未完成的 operation，不執行 recovery。

不得猜測要 rollback A。

理由：

filesystem 已切換，forward completion 最容易保持 idempotent。

---

# 20. Restore semantics

新增：

```bash
hufu workspace version restore <snapshot>
```

Restore 與 checkout 不同：

- checkout 切 session branch
- restore 只把 **active branch** workspace head 移到指定 snapshot，並建立新的 snapshot node 表示 restore action

不能直接把 branch head 指回舊 node，否則 lineage 被改寫。

例：

```text
S1 -> S2 -> S3

restore S1

S1 -> S2 -> S3 -> S4
                    |
                    root_tree = S1.root_tree
                    reason = restore
```

這保留 immutable history。

restore 同樣會覆蓋 live filesystem，因此流程與 checkout 相同：先取得 subject-root lease，若 live filesystem 與 active head 不同，先 capture + publish（reason=`checkout_save`），再建立 restore node 並 materialize。目標 snapshot 為 `pruned` 時回 `ErrWorkspaceSnapshotUnavailable`。

---

# 21. Materialization algorithm

Materialization MUST 是可重入、可 crash-recover 的 deterministic operation。

## 21.1 Managed namespace only

要計算：

```text
current managed tree
target managed tree
```

然後：

```text
delete = current - target
write  = target added/modified
chmod  = metadata differences
links  = symlink differences
```

不得刪除 unmanaged/ignored path。`ExcludeSubtrees`（control root 子樹，§9.5）與 Git submodule 路徑一律視為 unmanaged，不讀、不寫、不刪。

所有檔案操作 MUST 經由 `os.OpenRoot(subjectRoot)` 取得的 `*os.Root` 進行（Go 1.26 標準庫；repo 已有 `internal/tools/scoped_file_access_unix.go` 的前例），以免中途被換成 symlink 的目錄把寫入導到 root 外。可用的方法包含 `OpenFile`、`Rename`、`Remove`、`Mkdir`、`Chmod`、`Symlink`、`Readlink`、`Lstat`（Go 1.26）。

## 21.2 Atomic regular-file replace

每個 file：

```text
target-dir/.hufu-materialize-<random>
    ↓
write CAS bytes
fsync
chmod
rename over final path
```

temp 檔、`fsync`、`chmod`、`rename` 都經由同一個 `*os.Root` 完成；rename 後 fsync 所在目錄。

## 21.3 Delete ordering

先：

1. write/create target files
2. verify
3. delete managed paths no longer in target
4. remove empty managed directories bottom-up：只移除「因第 3 步刪除 managed file 而變空」的目錄；原本就空、或仍含 unmanaged 檔案的目錄保留（D6）

避免先刪除再因 write failure 留下大面積資料缺失。

## 21.4 Collision

如果 target 要建立：

```text
foo/bar.txt
```

但 live filesystem 有 unmanaged：

```text
foo -> symlink outside root
```

MUST fail：

```text
ErrMaterializationCollision
```

不得 follow。

## 21.5 Verification

完成後 MUST 計算 lightweight tree verification。

至少驗證：

```text
all target managed paths exist
hash matches
mode matches
no forbidden escaping symlink
```

只有 verify 成功才能：

```text
operation -> materialized
```

---

# 22. Runtime integration

目標不是每個 event 都 full snapshot，而是在 **filesystem state 有 semantic boundary 時** checkpoint。

v2（D1）：v1 的 semantic boundary 只有 **quiescent run boundary**，也就是整個 subject root 沒有 hufu 寫入者的時點。原生 worker 在 run 中並行寫同一 root（§2.1.1），run 中途沒有這樣的時點，因此 v1 不做 per-attempt checkpoint。

## 22.1 Run admission

在每個 run 開始時執行：同一個 coordinator process 可能多次呼叫 `Coordinator.Run`（`cmd/hufu/chat.go`、`run.go`、`segments.go`），每次都要 admission。呼叫點：`beginInvocationExecutionRunWithLease`（`internal/team/execution_events.go`）emit `run_started` **之前**呼叫 `AdmitRun`；`AdmitRun` 回傳 error 時不 emit `run_started`，run 以該 error 結束。

```text
1. resolve subject root / workspace ID（§26）；unmanaged → effective mode 視為 off
2. config mode 低於 subject_state.mode_floor → fail（§11.6）；否則 effective mode = config mode
3. effective mode = off → 結束，完全維持舊行為
4. required：確認 `WorkspaceVersionContext.HoldsProjectLock == true`（project lock 已由 cmd 層在 team 載入時取得，§27）；否則 fail
   observe：不持有長期 lock；只在第 5、8 步短暫取得 project lock，取不到就跳過並記錄 warning
5. 完成未完成 operation 的 recovery（§18.3、§19.2）
6. subject_state.recovery_required = 1：
     required → fail closed（§22.3）
     observe  → 記錄 warning 並跳過 checkpoint
7. 非 Git workspace 且沒有 .hufuignore（§9.2）：
     required → ErrHufuignoreRequired
     observe  → 記錄 warning
8. active branch head exists?
     no  → capture baseline（reason=baseline）+ publish
     yes → 比較 live filesystem 與 head；不同 → capture（reason=external_drift）+ publish
9. 清除 subject_state.checkpoint_deferred；更新 materialized_*
```

因此 user 在兩次 hufu run 之間手動修改檔案（或同 project 另一個 team 的 run 改了檔案，D2），不會被下一次 run 覆蓋，而是成為本 branch 的 `external_drift` snapshot。

## 22.2 Run checkpoint（取代 v1 的 attempt checkpoint）

run 結束時、所有 worker 都已停止之後，在 `run_finished` terminal event **之前**。呼叫點：`finalizeRunPrepared`（`internal/team/run_finalizer.go`）中 `c.drainAsyncTasks()` 之後、`commitTerminalLifecycle` 之前呼叫 `CheckpointRun`。中斷路徑 `EmergencyFinalizeRun` **不** capture，只設定 `subject_state.checkpoint_deferred = 1`。

```text
all workers stopped（dag scheduler drained）
    ↓
full capture（可用 stat_cache，§11.7）
    ↓
delta empty     → 不建立新 snapshot，head 不變
delta non-empty → pending snapshot（reason=run_checkpoint）
    ↓
workspace_snapshot_committed durable
    ↓
publish head
    ↓
run_finished（既有 terminal commit protocol，PendingTerminalCommit 等不變）
```

如此 `run_finished` 的 visible lineage 前方已經有對應 workspace state。

**checkpoint 失敗時**（例如 `ErrWorkspaceUnstable`：使用者在 run 結束時正在改檔案）：

- 不 advance head，也不宣稱 workspace 等於任何 snapshot
- 設定 `subject_state.checkpoint_deferred = 1`，在 status 與 `workspace version status` 顯示
- `run_finished` 照常提交：task 結果是真實發生的，不能因 checkpoint 失敗而改寫
- 下一次 admission 的 drift capture 會補上這段變更（reason=`external_drift`），並清除 deferred 標記

這不違反 I5 / I6：historical fork 到這個 run 的事件時，拿到的是 run 開始時的 snapshot（較舊，但不是未來），不會拿到 live filesystem。

**Codex attempt：** 既有 `ExecutionWorld` 的 baseline / final snapshot 與 `ValidateExecutionWorldDelta` 維持不變，繼續負責 effect verification；v1 不把它的 delta 寫成 snapshot。

**v1.x 延伸（attempt-level checkpoint）：** 只有在 scheduler 能保證某個 attempt 執行期間 subject root 沒有其他寫入者時（例如 task 對 workspace root 持有 `exclusive` ResourceClaim，或 effective 並行度為 1），才可在該 attempt 結束後以 `CaptureFromDelta`（reason=`attempt`）checkpoint。啟用前 MUST 先解決 §15.2 的 type change 限制。

## 22.3 Unauthorized mutation（D5）

若 `ValidateExecutionWorldDelta` fail（目前只發生在 Codex provider 路徑）。呼叫點：`internal/team/subagent_codex.go` 中兩處失敗分支（repair 前 frozen delta 的檢查，以及 `finalizeCodexTurn`），經 `p.coordinator` 呼叫 `MarkUnauthorizedMutation`；coordinator 未啟用 versioning 時為 no-op：

- 不可把 mutation 當 accepted workspace version
- 但 filesystem 已可能被改動

v1 行為（不建立 tainted snapshot，schema 不加 `accepted`）：

1. 設定 `subject_state.recovery_required = 1`、`recovery_code = unauthorized_mutation`；`recovery_detail` 記錄 run / task / attempt 與違規路徑（只存路徑，不存內容）
2. 既有 coordinator recovery-required 流程照舊
3. 該 run 結束時跳過 run checkpoint，不得把違規狀態寫成 `run_checkpoint`
4. 下一次 admission 在 required 模式下 **fail closed**，不得把違規狀態當成 `external_drift` 自動接受。操作者必須明確擇一：
   - `hufu workspace version restore <head>`：materialize 最後一個 accepted head，清除 marker
   - `hufu workspace version adopt`：capture 目前狀態（reason=`adopt`）並 publish，清除 marker

   兩者都寫 operation row 與 `workspace_snapshot_committed`。

observe 模式只記錄 marker 與 warning，不擋 admission。

---

# 23. Event ↔ snapshot temporal semantics

任意 event fork 不可能憑空重建「事件當下每一奈秒」filesystem。

Canonical rule：

> 對 event `E` fork 時，workspace state = `E` 所在 branch visible lineage 中，**最後一個位於 `E` 或 `E` 之前的 `workspace_snapshot_committed`**。

如果沒有：

```text
ErrWorkspaceSnapshotUnavailable
```

不得使用：

```text
current live workspace
parent branch current head
latest global snapshot
```

因為那些會造成 semantic time travel error（語意時間旅行錯誤：session state 在舊時間點，但 filesystem 卻來自未來）。

v2 補充：

- 找到的 snapshot 為 `pruned`（D4）時，同樣回 `ErrWorkspaceSnapshotUnavailable`（detail 註明已被 GC 修剪），不得往更早或更晚找替代品。
- lineage 掃描沿用 `latestCompactionCheckpointThroughEvent`（`internal/team/compaction_state.go`）的模式：以 `FilterEventsForBranch` 取得 visible lineage，由 fork event 往回找。
- run checkpoint 失敗而 deferred 時（§22.2），該 run 的事件對應到 run 開始時的 snapshot，符合本規則。

---

# 24. Workspace diff

新增：

```go
type WorkspaceVersionDelta struct {
    Added    []PathChange
    Modified []PathChange
    Deleted  []PathChange
    TypeChanged []PathChange
}
```

Merkle diff：

```text
if tree hash equal:
    skip subtree O(1)

if different:
    recursively compare entries
```

因此大 workspace 但局部修改時 diff 不需掃全部 blobs。

整合現有：

```bash
hufu session diff A B
```

輸出新增：

```text
Workspace:
  + internal/foo/new.go
  ~ internal/bar/service.go
  - docs/old.md
```

現有 `ArtifactDiffs` 仍保留，因為 artifact semantic 與 filesystem diff 不同。

任一端為 `pruned`（D4）時回 `ErrWorkspaceSnapshotUnavailable`；branch 沒有 workspace head（legacy）時，`session diff` 的 Workspace 區塊顯示 `unavailable`，其餘 diff 照常輸出。

---

# 25. Version service integration in Coordinator

`internal/team/services.go` 新增 abstraction：

```go
type WorkspaceVersionService interface {
    // AdmitRun 執行 §22.1 的 admission：recovery、marker 檢查、baseline / drift capture。
    AdmitRun(
        context.Context,
        WorkspaceVersionContext,
    ) (versionstore.Snapshot, error)

    // CheckpointRun 執行 §22.2：run_finished 之前的 full capture。
    // 失敗時設定 checkpoint_deferred，回傳 error 只供記錄，不阻止 run_finished。
    CheckpointRun(
        context.Context,
        WorkspaceVersionContext,
    ) (versionstore.Snapshot, bool, error)

    // MarkUnauthorizedMutation 執行 §22.3 的 recovery marker。
    MarkUnauthorizedMutation(
        context.Context,
        WorkspaceVersionContext,
        WorkspaceDelta,
    ) error

    ForkBranch(
        context.Context,
        WorkspaceForkRequest,
    ) (versionstore.Snapshot, error)

    CheckoutBranch(
        context.Context,
        WorkspaceCheckoutRequest,
    ) error

    BranchHead(
        context.Context,
        workspaceID, branchID string,
    ) (versionstore.Snapshot, bool, error)
}
```

Coordinator 不得直接操作：

```text
SQLite
CAS filesystem paths
tree encoding
```

### Context 與接線（v3）

```go
type WorkspaceVersionContext struct {
    Mode             versionstore.Mode // effective mode（§31、§3 G9 之後的結果）
    WorkspaceID      string            // Resolution.WorkspaceID
    ProjectID        string
    TeamName         string
    SubjectRoot      string            // canonical
    ControlRoot      string            // canonical；用於 §9.5 ExcludeSubtrees
    StateDir         string            // Project.StateDir
    HoldsProjectLock bool              // cmd 層已取得 project lock（§27）
    Limits           versionstore.CaptureLimits
}
```

- 由 `cmd/hufu` 在 team 載入時建立：`resolveCommandWorkspace` 的 `Resolution` 提供 `WorkspaceID` / `ProjectID` / `SubjectRoot` / `ControlRoot`；`Project.StateDir` 以 `Registry.ResolveProject(ctx, resolution.ProjectID)`（完整 `prj_` ID 精確比對）取得。
- 經 `RuntimeServices`（`internal/team/services.go`）傳入 coordinator；未 managed、`Mode == off` 或平台不支援時，注入 no-op 實作，coordinator 端不需要判斷。
- `WorkspaceVersionService` 的實作放在 `internal/team/workspace_version_service.go`，自行開啟與關閉 `versionstore.VersionStore`。

---

# 26. Workspace identity resolution

Versioning 需要兩種 root：

```text
ControlRoot
SubjectRoot
```

Resolution priority：

## Managed workspace

使用 `internal/workspace.Registry`：

```text
control_root -> Workspace
Workspace.ProjectID -> Project.SubjectRoot / Project.StateDir
```

解析路徑（v3 定案）：

- **run（team 載入）**：沿用 `resolveCommandWorkspace`，它已回傳 managed `Resolution` 並取得 team lock。
- **`hufu session ...` / `hufu workspace version ...`，未帶 `-w`**：改為呼叫 `resolveCommandWorkspace`（`Mode: ResolveExisting`），取代目前只拿路徑的 `resolveExistingManagedWorkspacePath`，以便同時取得 team lock。唯讀子指令（`list`、`tree`、`diff`、`status`、`show`）不取得 lock，只解析。
- **帶 `-w <path>`**：新增 `Registry.GetWorkspaceByControlRoot(ctx, canonicalPath)`（`workspaces.control_root` 已是 `UNIQUE` 欄位），找到即走 managed 路徑並取得 team lock；找不到即視為 unmanaged（D3）。

Version namespace：

```text
workspace_id = managed workspace ID
storage root = Project.StateDir/workspace-versions
filesystem root = Project.SubjectRoot
```

額外檢查（v2）：

- 第一次 baseline 時把 canonical subject root 寫入 `subject_state.subject_root`（§11.6）；之後每次解析都比對，不一致回 `ErrVersionedWorkspaceUnresolved`，由 `workspace version doctor` 處理（例如 project 被 `workspace rebind` 到新路徑）
- 依 §9.5 檢查 control root 與 subject root 的相對位置；`SubjectRoot == ControlRoot` 回 `ErrVersionedWorkspaceUnresolved`
- 同 project 的多個 team 各有自己的 `workspace_id` 與 branch heads，但共用 storage root、`subject_state` 與 lock（D2）

## Legacy / unmanaged workspace（v2：v1 不支援，D3）

v1 版本建議從 `SessionData.RuntimeWorkspace` 取得 subject root，這是錯的：

- `SessionData.RuntimeWorkspace` 是 `<control>/runtime`，是 artifacts / receipts 的暫存區，不是 subject root
- unmanaged run 的 subject root 是 run 當下的 cwd，沒有持久化在 `session.json`，CLI 事後無從得知

因此 v1：

- control root 無法經 registry 解析為 managed workspace 時，effective mode 一律視為 `off`
- `hufu session fork` / `checkout` 在 unmanaged workspace 上維持舊的 metadata-only 行為；帶有 workspace 語意的選項（或 `mode=required` 設定）回 `ErrVersionedWorkspaceUnresolved`
- `hufu workspace version ...` 指令回 `ErrVersionedWorkspaceUnresolved`，並提示先 `hufu workspace register`

v1.x 若要支援 unmanaged workspace，必須先在 run 開始時把 subject root 綁定持久化（例如寫入以 control root hash 命名的 global state namespace），不得在事後推測。

---

# 27. Concurrency

Workspace Version Store 必須有：

```text
process mutex
+
interprocess file lock
```

Lock scope（v2 修正，D2）：

```text
project / canonical subject root
```

v1 版本寫的是 `workspace_id + subject root`。但 `workspace_id` 是 team 層級，同 project 的多個 team 共用同一個 subject root（§2.4），以 `workspace_id` 分鎖會讓 team A 的 checkout 在 team B 的 run 進行中改寫檔案。lock 檔即 `<Project.StateDir>/workspace-versions/lock`，一個 project 一把。

實作沿用 PR-00 抽出的 `internal/fsutil` flock（來源：`internal/workspace/lock_unix.go` / `lock_windows.go`），non-blocking 取得，取不到回 `ErrVersionOperationInProgress`，並在錯誤中附上持有者資訊（寫在 lock 檔旁的 owner 檔：pid、workspace_id、operation kind、開始時間；只作診斷，不作 authority）。

### Run-long lease（v2 新增，B3）

用語：本規格其他章節所稱的「subject-root lease」，指的是下表的 **project lock**；需要與同 team 的 run 互斥的操作，另外還要先取得 **team lock**。

現況（v3 更正）：

- **team 層級已有 run-long lock**：managed run 經 `resolveCommandWorkspace`（`cmd/hufu/workspace_resolver.go`）呼叫 `workspace.AcquireWorkspaceLocks(stateRoot, [workspaceID])`，鎖檔為 `<stateRoot>/locks/<workspace_id>.lock`，存在 `TeamSession.WorkspaceLease`，持有到 coordinator 結束（`internal/team/parse.go` 的註解：「holds the managed runtime lock for the lifetime of the coordinator」）。`hufu session retry/reconcile` 也經同一路徑取得它。
- **但 `hufu session fork/checkout/list/diff` 沒有取得它**：它們用 `resolveExistingManagedWorkspacePath` 只拿路徑，因此可以在同 team 的 run 進行中執行。
- 沒有 **project 層級**的鎖：不同 team 各有自己的 team lock，互不阻擋。
- EventStore 的 flock 只在單次 append 期間持有，不能代表 run。

因此 v1 使用兩層鎖：

| 層級 | 鎖檔 | 誰持有 | 目的 |
|---|---|---|---|
| team | `<stateRoot>/locks/<workspace_id>.lock`（既有） | 所有 managed run（既有行為）；v1 起 `session fork/checkout`、`workspace version` 的變更類指令也必須經 `resolveCommandWorkspace` 取得 | 同 team 的 run 與版本操作互斥 |
| project | `<Project.StateDir>/workspace-versions/lock`（新增） | effective mode 為 `required` 的 coordinator（全程）與所有版本操作 | 跨 team 互斥（D2） |

規則：

- 取得順序固定為 **team lock → project lock**；兩者都是 non-blocking，取不到即失敗，不等待，因此不會死鎖
- project lock 由 `commandWorkspaceLease` 一併持有與釋放（擴充該 struct 增加一個 closer），沿用既有的 `closeSessionWorkspaceLease` 釋放路徑
- subject root 的 effective mode 為 `required` 時，**同 subject root 每個 team 的 coordinator** 都必須在 startup 取得 project lock，並持有到 coordinator process 結束（不是每個 run 結束就釋放，因為 coordinator 只在啟動時讀 session tree，run 之間被 checkout 會讓記憶體狀態過時）
- 結果：`required` 模式下，同一 subject root 同時只能有一個 coordinator。這是 opt-in 的行為改變，`off` / `observe` 模式不受影響（G7）
- `observe` 模式的 coordinator 不持有長期 lease，只在 capture + publish 期間短暫取得；取不到就跳過該次 checkpoint 並記錄 warning

Mutating operations：

- Capture publication
- Materialize
- Restore
- Checkout
- Fork activation
- GC sweep

都需要 lock。

Read-only：

- list
- diff
- verify snapshot metadata

不需獨占 filesystem lock，但 SQLite read transaction仍要一致。

### Run conflict

當同 subject root 有 coordinator 持有 run-long lease（不論哪個 team）時：

```bash
hufu session checkout ...
hufu session fork ...
hufu workspace version restore ...
hufu workspace version gc --apply
```

MUST return `ErrVersionOperationInProgress`：

```text
workspace version operation conflicts with active execution
```

不得 live checkout。

反過來，fork / checkout / restore / GC sweep 進行中，coordinator startup 取不到 lease 時也 MUST 失敗，不得等待後在舊的 session tree 上繼續。

---

# 28. GC

CAS 是 immutable，所以 GC 採 mark-and-sweep。

## 28.1 Roots

Mark roots：

```text
all branch_heads
snapshots referenced by incomplete operations
snapshots referenced by subject_state.materialized_snapshot_id
snapshots pinned by explicit labels/checkpoints
retention-window snapshots（每個 branch 最近 keep_recent_snapshots 個 published snapshot）
```

v2 修正（D4）：mark 階段**只**沿

```text
root tree -> child trees/blobs/links
```

標記 reachable objects，**不**沿 `snapshot.parent_snapshot_id` 往上走。v1 版本同時沿 parent chain 標記，會讓每個 head 的所有祖先永久保留，`keep_recent_snapshots` 因此失效。

不在任何 root 中的 published snapshot，在 GC apply 時轉為 `pruned`（§12）：

- snapshot row 保留，DAG lineage 不斷，`list` / `show` 仍可看到（標示 pruned）
- 其 tree 以下、不被任何 root 引用的 CAS objects 可回收
- fork / restore / historical fork 指向 `pruned` snapshot 時回 `ErrWorkspaceSnapshotUnavailable`（I6：不 fallback）

## 28.2 Sweep

只刪：

```text
unreachable
AND
older than grace period
AND
not in tmp/in-flight operation
```

Default `hufu workspace version gc` MUST dry-run。

Apply：

```bash
hufu workspace version gc --apply
```

輸出：

```text
snapshots_prunable
objects_prunable
bytes_reclaimable
```

GC apply 需要 subject-root lease（§27）；同 project 的所有 team 共用 CAS，因此 roots 必須涵蓋 DB 中**所有** `workspace_id` 的 branch heads，不能只看發出指令的 team。

## 28.3 Pending objects

CAS object-first protocol 會產生 crash orphan。

GC 必須處理，但不可將剛建立的 object 立即掃除。

需要 grace period。

---

# 29. Integrity / doctor

新增：

```bash
hufu workspace version doctor
```

檢查：

1. SQLite migration checksum
2. branch head snapshot exists
3. published snapshot有 durable commit event
4. snapshot parent exists
5. no parent cycle
6. root tree exists
7. all reachable tree objects hash valid
8. all referenced blob/link objects exist
9. object filename hash == content hash
10. incomplete operation recovery
11. session-tree branch ↔ branch_heads consistency
12. active branch live workspace drift
13. `subject_state.subject_root` 與 registry 目前的 `Project.SubjectRoot` 一致（v2）
14. 沒有 branch head 指向 `pruned` snapshot（v2）
15. `recovery_required` / `checkpoint_deferred` 標記與其原因（v2，只回報）
16. lock owner 檔指向的 process 是否仍存在（v2，只回報）

DB 是 project 層級，但 event store 與 session tree 是 team 層級：第 3、11 項必須對 DB 中每個 `workspace_id`，經 registry 找到對應 control root 的 event store 與 session tree 逐一檢查。

`--repair` 只允許 deterministic repairs：

- rebuild branch_heads from canonical events
- complete published snapshot projection
- mark abandoned pending snapshots orphaned
- remove stale temp files

不可自動：

- invent missing blob
- silently choose newer snapshot
- bind legacy branch to arbitrary live workspace

---

# 30. Security / privacy

CAS 會持久保存實際 source bytes，風險高於現有只存 hash 的 WorkspaceSnapshot。

MUST：

- state root owner-only permissions
- no world-readable CAS
- no CAS path under project root
- ignore `.git/`
- respect `.gitignore` candidate semantics when Git可用
- never redact source bytes（會改變內容）
- never log blob contents
- event payload只存 hashes/IDs/count，不存 file contents
- error message不得包含 secret-bearing file bytes
- verify symlink containment
- reject path traversal
- all materialization paths root-anchored

Future：

```text
CAS encryption-at-rest
```

不屬於 v1，但 API不得阻止未來加密 backend。

---

# 31. Configuration

新增：

```yaml
workspace-versioning:
  mode: off            # off | observe | required；預設 off

  capture:
    max-files: 200000
    max-file-bytes: 0
    max-logical-bytes: 0

  retention:
    keep-recent-snapshots: 50
    orphan-grace: 24h
```

materialize 後的 verification（§21.5）一律執行，不提供關閉選項。

v2 修正：

- 鍵名改用 kebab-case，與 hufu 既有 config 一致（`provider-url`、`max-concurrent` 等，`internal/config/config.go`）。
- 移除 `enabled`：與 `mode: off` 重複。
- 放在全域 hufu config（`internal/config.Config`），**不**放 `team.yaml`：mode 是 subject root 層級的性質，同 project 的 team 不應各自設定不同 mode。
- 欄位定義：

```go
// internal/config/config.go
WorkspaceVersioning WorkspaceVersioningConfig `yaml:"workspace-versioning"`

type WorkspaceVersioningConfig struct {
    Mode      string `yaml:"mode"` // "", "off", "observe", "required"；"" 等同 "off"
    Capture   struct {
        MaxFiles        int   `yaml:"max-files"`
        MaxFileBytes    int64 `yaml:"max-file-bytes"`
        MaxLogicalBytes int64 `yaml:"max-logical-bytes"`
    } `yaml:"capture"`
    Retention struct {
        KeepRecentSnapshots int    `yaml:"keep-recent-snapshots"` // 0 = 預設 50
        OrphanGrace         string `yaml:"orphan-grace"`          // time.ParseDuration；"" = 預設 24h
    } `yaml:"retention"`
}
```

- 未知的 `mode` 字串在 config 載入時即回錯誤；`OrphanGrace` 解析失敗同樣回錯誤。

`0` 表示由 implementation default / unlimited policy 決定，避免在 config schema 中硬塞不合理通用上限。

Mode：

```text
off
observe
required
```

### off

完全維持舊行為。

### observe

建立 snapshot，但 session fork/checkout 不依賴它做 correctness guarantee。

用於 rollout。

### required

完整 workspace-aware fork/checkout。非 Git workspace 需要 `.hufuignore`（§9.2）。

一旦某 subject root 以 `required` 建立 canonical branch heads，`subject_state.mode_floor` 記為 `required`（§11.6）。之後 config mode 低於 mode_floor 時 MUST 失敗並提示使用 `hufu workspace version downgrade`，不得無聲 downgrade。

---

# 32. CLI specification

## 32.1 Existing commands

### `hufu session fork`

新增完整 workspace fork。開啟 EventStore 失敗時 MUST fail closed（現行實作會吞掉錯誤，§2.2）。

新增 `--json` 輸出。現行 fork / checkout 會忽略 persistent `--json` flag、只印文字，因此這是全新輸出，不是擴充既有 schema：

```json
{
  "branch_id": "exp",
  "parent_branch_id": "main",
  "fork_event_id": "...",
  "workspace_snapshot_id": "wsv-...",
  "workspace_root_tree_hash": "..."
}
```

### `hufu session checkout`

成功的定義改為：

```text
session projection switched
AND
workspace materialized + verified
```

不能 filesystem 失敗卻仍印：

```text
Checked out branch
```

`--json` 輸出（v3）：

```json
{
  "branch_id": "main",
  "previous_branch_id": "exp",
  "checkout_save_snapshot_id": "wsv_...",
  "workspace_snapshot_id": "wsv_...",
  "workspace_root_tree_hash": "...",
  "files_written": 3,
  "files_deleted": 1,
  "metadata_only": false
}
```

`checkout_save_snapshot_id` 在沒有 drift 時省略；`--metadata-only` 時 `metadata_only` 為 `true`，workspace 欄位省略。

### `hufu session diff`

加入 workspace diff（§24）。`--json` 時在既有 `SessionDiff` 加上 `workspace_diff` 欄位（`added` / `modified` / `deleted` / `type_changed` 四個路徑陣列），或在任一端無 head / 為 pruned 時加上 `workspace_diff_unavailable: "<reason>"`。

### `hufu session list`

`--json` 時每個 branch 物件加上 `workspace_snapshot_id`（無 head 時省略）與 `workspace_state`（`available` / `unavailable_legacy` / `unavailable_pruned`）。

## 32.2 New commands

`hufu workspace version status [--json]` 輸出欄位（v3 定案）：

```text
subject_root, effective_mode, mode_floor, platform_supported
active_workspace_id, active_branch_id, active_head_snapshot_id
materialized_workspace_id, materialized_branch_id, materialized_snapshot_id
live_drift            # true / false，以 stat_cache + hash 判斷
recovery_required, recovery_code, recovery_detail
checkpoint_deferred
incomplete_operations # id、kind、state、updated_at
snapshot_counts       # by state
cas_objects, cas_bytes  # 走訪 objects/ 取得
```

`list` 輸出每個 snapshot 的 `id`、`branch_id`、`parent`、`reason`、`state`、`file_count`、`logical_bytes`、`created_at`；`show <snapshot>` 另加 `root_tree_hash`、`commit_event_id`、`materializable`。

```text
hufu workspace version status
hufu workspace version list
hufu workspace version show <snapshot>
hufu workspace version diff <a> <b>
hufu workspace version snapshot
hufu workspace version restore <snapshot>
hufu workspace version adopt              # v2：接受 recovery-required 的目前狀態（§22.3）
hufu workspace version downgrade <mode>   # v2：明確降低 mode_floor（§31）
hufu workspace version doctor [--repair]
hufu workspace version gc [--apply]
```

命名注意：`hufu workspace` 已有 `gc`、`doctor`、`restore <trash-id>`（`cmd/hufu/workspacecmd.go`）。新指令一律放在 `version` 子群組下，避免衝突；`workspace restore`（還原被刪除的 workspace）與 `workspace version restore`（還原 snapshot）的 help 文字 MUST 互相指明差異。新 CLI 寫在新檔 `cmd/hufu/workspaceversioncmd.go`，不要加進已超過 1000 行的 `workspacecmd.go`。

不使用：

```text
commit
checkout-index
rebase
merge
git-log
```

---

# 33. Migration / backward compatibility

## 33.1 Existing workspace with only `session_tree.json`

首次開啟 versioning：

active branch：

```text
capture live filesystem
    ↓
baseline snapshot
    ↓
bind active branch head
```

其他既有 inactive branches：

```text
workspace head = unknown
```

MUST NOT 假造。

`session list` 顯示：

```text
main        workspace: wsv-...
old-exp     workspace: unavailable (legacy branch)
```

## 33.2 Legacy branch checkout

若 branch 沒有 workspace head：

default：

```text
error
```

可 explicit：

```bash
hufu session checkout old-exp --metadata-only
```

但不能改 filesystem。

## 33.3 Existing Git repositories

沒有任何 Git metadata migration。

`.git` 完全保留。

Workspace checkout不得：

- change Git branch
- change HEAD
- stage files
- reset index
- commit

它只改 working-tree managed paths。

這可能讓 Git 顯示 working tree dirty，屬正常現象。

---

# 34. Failure semantics

新增 typed errors：

```go
var (
    ErrSnapshotNotFound               = errors.New(...)
    ErrWorkspaceUnstable              = errors.New(...)
    ErrWorkspaceChangedAfterObservation = errors.New(...)
    ErrCASCorrupt                     = errors.New(...)
    ErrCASObjectMissing               = errors.New(...)
    ErrMaterializationCollision       = errors.New(...)
    ErrSnapshotNotMaterializable      = errors.New(...)
    ErrBranchHeadConflict             = errors.New(...)
    ErrWorkspaceSnapshotUnavailable   = errors.New(...)
    ErrVersionedWorkspaceUnresolved   = errors.New(...)
    ErrVersionOperationInProgress     = errors.New(...)

    // v2 新增
    ErrUnsupportedPathName            = errors.New(...) // 非 UTF-8 path component（§8.3）
    ErrHufuignoreRequired             = errors.New(...) // 非 Git + required 缺 .hufuignore（§9.2）
    ErrWorkspaceRecoveryRequired      = errors.New(...) // subject_state.recovery_required（§22.3）
    ErrModeDowngradeRefused           = errors.New(...) // config mode < mode_floor（§31）
)
```

所有 correctness-sensitive path MUST fail closed。

禁止：

```text
missing snapshot -> use live workspace
missing branch head -> use main
corrupt object -> recapture silently
historical fork missing snapshot -> use latest
pruned snapshot -> use nearest older/newer snapshot（v2）
unauthorized mutation -> accept as external_drift（v2）
```

---

# 35. Observability

新增 structured metrics：

```text
workspace_version_capture_total
workspace_version_capture_failures_total
workspace_version_capture_duration
workspace_version_logical_bytes
workspace_version_new_cas_bytes
workspace_version_reused_cas_bytes
workspace_version_materialize_total
workspace_version_materialize_failures_total
workspace_version_materialize_duration
workspace_version_gc_reclaimed_bytes
workspace_version_recovery_actions_total
workspace_version_head_conflicts_total
```

v2 說明：hufu 沒有 Prometheus / OpenTelemetry 之類的 metrics 後端，現有 `internal/team/metrics.go` 是從事件投影出來的。上列名稱是**投影欄位名**，不是要新增 metrics 系統：

- 與 snapshot 一一對應的數值（file_count、logical_bytes、new / reused CAS bytes、capture duration）放進 `workspace_snapshot_committed` payload 或 operation row，由 `metrics.go` 投影
- 失敗、衝突、recovery 次數由 operation row 的 `state` / `detail_code` 統計
- 顯示於 `hufu workspace version status` 與既有 status 輸出

Event/inspection output：

```text
snapshot_id
root_tree_hash
parent_snapshot_id
branch_id
reason
file_count
logical_bytes
new_cas_bytes
reused_cas_bytes
```

不得 emit file content。

---

# 36. Implementation phases

---

## Phase 0 — Baseline / invariants / shared primitives

### Deliverables

新增 regression tests，固定目前行為：

- Git candidate discovery仍只是 optimization
- WorkspaceSnapshot digest仍對 actual bytes
- `.git` 不被 snapshot
- EventStore branch tagging
- Session fork event lineage
- Session checkout session rebuild
- legacy branches仍可 metadata-only 使用
- `off` / `observe` 模式下原生 worker 並行度不變（v2）

v2 新增（PR-00）：

- 把 `AtomicCreateFile` / `AtomicWriteFile` / `SyncDir` 與 interprocess flock 抽到 `internal/fsutil`；把 checksum migration 抽到 `internal/sqlmigrate`。`internal/team` 與 `internal/workspace` 保留 thin wrapper，行為不變
- 修正 §2.1.1 的兩個既有缺陷（目錄 symlink → `EISDIR`；FIFO → `os.Open` 阻塞），並加 regression tests

### No production behavior change

Phase 0 禁止加入 CAS side effect。PR-00 的兩個缺陷修正屬於 bug fix，只改變「原本會失敗或卡住」的情況。

---

## Phase 1 — CAS + SQLite core

新增：

```text
internal/workspace/versionstore/
```

實作：

- path resolver
- migration framework（使用 `internal/sqlmigrate`）
- connection pragmas（§11）
- blob CAS
- link CAS
- deterministic tree codec（含 §8.3 的 entry field values）
- tree CAS
- 自有 inclusion policy、`ExcludeSubtrees`、`.hufuignore`（§9，v2 提前到 Phase 1）
- full capture
- snapshot SQLite insert
- get snapshot
- verify snapshot
- basic diff
- basic GC dry-run
- full capture benchmark（v2：從 Phase 5 提前，§41 的 10k / 50k / 100k 檔案案例先量 full capture）

Acceptance：

```text
capture(root)
modify files
capture(root)
diff(S1,S2)
materialize(S1,temp)
tree(temp) == S1
```

另需證明：名為 `logs/`、`history/`、`tasks/`、`shared/`、`status/` 的專案目錄**有**被 capture（B5）。

不接 Coordinator。

---

## Phase 2 — Branch heads + durable publication

實作：

- `branch_heads`（含首次 head 的 `expectedGeneration = 0` 語意）
- `operations`
- `subject_state`（v2，§11.6）
- `stat_cache`（v2，§11.7）
- optimistic generation
- pending/published/orphaned/pruned
- `workspace_snapshot_committed` event schema，並加入 `IsKnownEventType` catalog
- event validation
- event-first publication adapter
- recovery of:
  - DB pending / no event → orphaned，不補寫 event（§16.2）
  - event durable / DB not projected
  - orphan object

Acceptance：

在每個 publication stage 注入「crash」後都能 deterministic recover。

fault injection 機制（v3 定案）：`versionstore` 與 `internal/team` 的 service 各自提供未匯出的測試 hook（例如 `var testHookAfterStage func(stage string) error`），只在 `_test.go` 中設定。hook 回傳 error 即模擬在該 stage 之後 crash：測試接著關閉 store / EventStore，重新開啟，執行 `Recover`，再斷言結果。不需要真的 kill process。stage 名稱以常數定義，IT7～IT9 列出的每個 fault point 對應一個常數。

---

## Phase 3 — Existing Session Tree integration

前置（v2）：subject-root lease（§27）與 fork / checkout 的 EventStore fail closed。

整合：

```text
session fork
session checkout
session diff
```

要求：

- current active branch baseline
- fork O(1) CAS reuse
- historical event snapshot resolution
- historical fork 前先保存 active branch 的 live drift（§18.2 第 6 步）
- fork crash recovery（§18.3）
- full checkout materialization
- control root 子樹排除（§9.5）
- inactive legacy branch status
- `--metadata-only` compatibility
- unmanaged workspace 維持 metadata-only（D3）

Acceptance：

```text
main:
  create A.txt="main"

fork exp

exp:
  A.txt="exp"
  create B.txt

checkout main:
  A.txt="main"
  B.txt absent

checkout exp:
  A.txt="exp"
  B.txt present
```

且 `.git` state未被 hufu 直接操作。

---

## Phase 4 — Coordinator runtime checkpoints（v2 重寫，D1）

整合 coordinator run lifecycle（**不是** ExecutionWorld attempt）：

- project lock：required 模式由 `commandWorkspaceLease` 在 team 載入時取得、持有到 coordinator 結束（§27）；`WorkspaceVersionContext.HoldsProjectLock` 反映此狀態
- `AdmitRun`：recovery、`recovery_required` / `.hufuignore` / mode floor 檢查、baseline / external drift capture（§22.1）
- `CheckpointRun`：`run_finished` 之前的 full capture；失敗時 `checkpoint_deferred`，不阻止 `run_finished`（§22.2）
- `MarkUnauthorizedMutation`：Codex `ValidateExecutionWorldDelta` 失敗時寫 recovery marker（§22.3）
- `hufu workspace version adopt` / `restore <head>` 解除 marker
- no checkpoint for empty delta

coordinator 端的接點 MUST 是 thin hook：新邏輯放在 `internal/team/workspace_version_runtime.go`。呼叫點只有三處，每處只加一個函式呼叫：`execution_events.go` 的 `beginInvocationExecutionRunWithLease`（`AdmitRun`）、`run_finalizer.go` 的 `finalizeRunPrepared` 與 `EmergencyFinalizeRun`（`CheckpointRun` / deferred）、`subagent_codex.go` 的兩個 `ValidateExecutionWorldDelta` 失敗分支（`MarkUnauthorizedMutation`）。不得修改 `coordinator_task_run.go`（5400+ 行）與 `coordinator.go`（3100+ 行）；所有變更受 `.golangci.yml` 的 gocyclo 40 限制。

Acceptance：

- retry
- resume
- provider failure
- validation failure（Codex）
- external edit between runs
- 同 project 另一 team 的 run 在兩次 run 之間改檔（D2）
- 使用者在 run 結束時改檔 → checkpoint deferred → 下次 admission 補上
- abrupt process exit

都不造成 session head 與 live filesystem 靜默分歧。

---

## Phase 5 — Doctor / GC / performance

加入：

- `workspace version doctor`（含 §29 第 13～16 項）
- `--repair`
- apply GC（`pruned` tombstone，§28）
- `workspace version downgrade`
- storage metrics（事件投影，§35）
- large-repo benchmark
- incremental Merkle rewrite benchmark（`CaptureFromDelta` library）
- branch diff benchmark
- corruption tests

---

# 37. Detailed file-change plan

## New

```text
internal/workspace/versionstore/types.go
internal/workspace/versionstore/store.go
internal/workspace/versionstore/sqlite.go
internal/workspace/versionstore/migrations.go
internal/workspace/versionstore/cas.go
internal/workspace/versionstore/codec.go
internal/workspace/versionstore/capture.go
internal/workspace/versionstore/incremental.go
internal/workspace/versionstore/materialize.go
internal/workspace/versionstore/diff.go
internal/workspace/versionstore/verify.go
internal/workspace/versionstore/recovery.go
internal/workspace/versionstore/gc.go
internal/workspace/versionstore/lock.go
internal/workspace/versionstore/paths.go
internal/workspace/versionstore/inclusion.go
internal/workspace/versionstore/hufuignore.go
internal/workspace/versionstore/subject_state.go

internal/fsutil/                 # PR-00
internal/sqlmigrate/             # PR-00

internal/team/workspace_version_service.go
internal/team/workspace_version_events.go
internal/team/workspace_version_recovery.go
internal/team/workspace_version_runtime.go

cmd/hufu/workspaceversioncmd.go
```

## Modify

```text
# PR-00
internal/team/atomic_write.go             # 改為 internal/fsutil 的 thin wrapper
internal/workspace/lock_unix.go / lock_windows.go
internal/workspace/registry_schema.go     # 改用 internal/sqlmigrate
internal/team/workspace_snapshot.go       # 修 EISDIR / FIFO 缺陷

# PR-05 之後
internal/team/session_tree.go             # 只做調度；更正過時註解
internal/team/services.go
internal/team/event_types.go              # 加入 workspace_snapshot_committed
internal/team/coordinator_eventstore.go   # startup：lease、recovery、AdmitRun（thin hook）
internal/team/run_finalizer.go            # run_finished 之前呼叫 CheckpointRun（thin hook）
internal/team/subagent_codex.go           # ValidateExecutionWorldDelta 失敗時呼叫 MarkUnauthorizedMutation（thin hook）
internal/config/config.go                 # workspace-versioning 設定
cmd/hufu/sessioncmd.go                    # EventStore fail closed、--json、workspace 語意
internal/workspace/registry.go / types.go   # 新增 Registry.GetWorkspaceByControlRoot（§26）
cmd/hufu/workspace_resolver.go            # commandWorkspaceLease 增加 project lock（§27）
```

v1 版本寫的「`internal/team/coordinator*.go` where execution-world attempt finalization occurs」在 v2 不再適用：checkpoint 掛在 run lifecycle（startup 與 `run_finished` 之前），不掛在 attempt finalization（D1）。

### Avoid

不要把大量 CAS code 塞進：

```text
internal/team/session_tree.go
cmd/hufu/sessioncmd.go
```

Session Tree 只能調度 service。

---

# 38. Required unit tests

## CAS

- same bytes → same blob hash
- different bytes → different hash
- existing valid object reused
- existing corrupt object fails
- interrupted tmp write不發布
- tree codec deterministic
- tree rejects unsorted / duplicate / invalid names
- tree hash stable across runs

## Capture

- empty workspace
- regular files
- executable mode
- nested directories
- empty directories 不出現在 tree 中（D6 fixture）
- symlink internal
- symlink escaping
- symlink 指向內部目錄 → 存成 link object，不展開、不 `EISDIR`（v2）
- special file reject
- FIFO 在 `open` 前就被拒絕，不阻塞（v2）
- 名為 `logs/`、`history/`、`tasks/`、`shared/`、`status/` 的專案目錄有被 capture（v2，B5）
- control root 在 subject root 內 → 整個子樹排除；兩者相同 → `ErrVersionedWorkspaceUnresolved`（v2）
- `.hufuignore` 解析：anchored / basename / 目錄 pattern；`!` 與 `**` 回報行號錯誤（v2）
- 非 UTF-8 檔名 → `ErrUnsupportedPathName`（v2）
- Git submodule 路徑視為 unmanaged（v2）
- file count overflow
- byte budget overflow
- file changes during read → retry/fail
- Git candidate path
- non-Git walk path
- ignored files excluded
- Hufu control paths excluded

## Snapshot graph

- baseline
- linear snapshots
- sibling fork
- same root tree / distinct snapshot IDs
- parent cycle impossible
- missing parent reject
- branch generation conflict
- 首次 head：`expectedGeneration = 0` 成功；row 已存在時回 `ErrBranchHeadConflict`（v2）
- `published → pruned` 允許；被 head 指向的 snapshot 不可 prune（v2）
- `subject_state.subject_root` 不一致 → `ErrVersionedWorkspaceUnresolved`（v2）
- config mode 低於 `mode_floor` → `ErrModeDowngradeRefused`（v2）

## Diff

- add
- modify
- delete
- mode
- file↔directory type change
- symlink target change
- identical subtree skipped

## Materialize

- clean target
- replace modified file
- delete managed old file
- preserve ignored/unmanaged file
- collision fail
- internal symlink
- escaping symlink reject
- repeated materialize idempotent
- partial failure recover
- `ExcludeSubtrees` 內的檔案不被讀、寫、刪（v2）
- 只移除因刪除 managed file 而變空的目錄（v2，D6）
- materialize 後 verification 發現 hash 不符 → operation `failed`、不宣告 materialized（v3，涵蓋 case folding 碰撞）
- `goos != "linux"` 時 effective mode 為 `off`、`workspace version` 指令回 `ErrUnsupportedPlatform`（v3，以注入的 `goos` 測試）

---

# 39. Required integration tests

## IT1 — Full fork

```text
main snapshot S1
fork exp
exp head S2, S2.root == S1.root
```

CAS new blob bytes：

```text
0
```

除 tree/snapshot metadata外不得複製全部 workspace。

## IT2 — Divergence

```text
main -> S1 -> S3
          \
           exp -> S2 -> S4
```

checkout反覆切換後 filesystem 必須完全對應各 head。

## IT3 — Fork historical event

建立：

```text
E1 -> snapshot S1
E2
E3 -> snapshot S2
E4
```

fork E2：

```text
base = S1
```

fork E4：

```text
base = S2
```

不得使用最新 global snapshot。

## IT4 — Legacy event

無 snapshot event：

```text
hufu session fork old-event
```

必須 `ErrWorkspaceSnapshotUnavailable`。

## IT5 — Git isolation

準備 Git repo：

```text
branch=feature
index staged file X
dirty file Y
```

執行 hufu fork/checkout 後：

- Git HEAD unchanged
- current branch unchanged
- index unchanged
- `.git` bytes unchanged
- working tree可因 hufu workspace materialization改變

## IT6 — External edit

run結束後 user手動改 `a.go`。

下一次 run：

```text
old head S1
live drift
-> capture S2(reason=external_drift)
-> run starts
```

不得 restore S1 覆蓋 user edit。

## IT7 — Crash publication

Fault points：

```text
after CAS
after snapshot insert
after event append
before head update
after head update
```

restart結果：

- no broken head
- no missing referenced CAS
- event/DB一致
- orphan可 GC
- 「after snapshot insert」時點：snapshot 變 `orphaned`，且 event log **沒有**新增任何事件（v2，§16.2）

## IT8 — Crash checkout

Fault points：

```text
after source capture
mid materialization
after materialized
after session rebuild
before ActiveBranch save
```

recovery完成後：

```text
active branch
session.json
filesystem
branch head
```

必須一致。

## IT9 — Crash fork（v2）

Fault points：

```text
after operation row
after child saved INACTIVE
after child snapshot attach（pending）
after child event append
after compaction materialization
before ActiveBranch save
```

recovery 結果符合 §18.3 的表：event 未 durable → child branch 被移除、snapshot orphaned；event 已 durable → complete forward。兩種情況都沒有「有 branch 無 head」的殘留。

## IT10 — Shared subject root across teams（v2）

同一 project 建立 team A、team B，mode=required：

```text
A run 進行中 → B coordinator startup 回 ErrVersionOperationInProgress
A run 進行中 → A 或 B 的 session checkout 回 ErrVersionOperationInProgress
A run 結束後修改 x.go → B 下次 run admission 產生 B 的 external_drift snapshot
```

## IT11 — Control root inside subject root（v2）

防禦性測試（v1 的 managed workspace 不會出現此佈局，§9.5）。在 service 層直接指定 control root 位於 subject root 內：

```text
capture → materialize 另一個 snapshot → 再 materialize 回來
session_tree.json、session.json、runtime/、compaction state 的 bytes 不被改動
```

control root 等於 subject root 時回 `ErrVersionedWorkspaceUnresolved`。另驗證 compatibility scope 的 run 在 v1 的 effective mode 為 `off`（D3）。

## IT12 — Project directories named like hufu bookkeeping（v2）

subject root 含 `docs/history/a.md`、`internal/tasks/b.go`、`logs/c.txt`：fork → 修改三者 → checkout 回原 branch → 三者都還原。

## IT13 — Non-Git required mode（v2）

非 Git workspace、mode=required：

```text
沒有 .hufuignore → run admission 回 ErrHufuignoreRequired
空的 .hufuignore → 正常 capture
.hufuignore 含 node_modules/ → node_modules 不被 capture，checkout 時保留
```

## IT14 — Deferred run checkpoint 與 unauthorized mutation（v2）

```text
run 結束時注入 ErrWorkspaceUnstable
→ run_finished 仍提交；head 不變；checkpoint_deferred=1
→ 下次 admission 產生 external_drift 並清除標記
→ fork 到該 run 的 run_finished 事件，拿到的是 run 開始時的 snapshot

Codex attempt 觸發 ValidateExecutionWorldDelta 失敗
→ recovery_required=1
→ 下次 admission 回 ErrWorkspaceRecoveryRequired
→ workspace version adopt 或 restore <head> 之後才能再 run
```

---

# 40. Semantic regression tests

以下既有 invariant MUST 明確回歸測試：

1. EventStore hash chain仍連續。
2. `IdempotencyKey` branch isolation仍有效。
3. `fresh session` 建立 independent root branch仍不繼承 prior event lineage。
4. execution target不因 snapshot restore改變。
5. retry仍使用 frozen task envelope。
6. resume不讀 current config 重建 historical identity。
7. workspace resource scope仍 fail closed。
8. workspace versioning不能讓 tool取得原本未授權 path。
9. snapshot capture不跟著 symlink讀出 root 外部 bytes。
10. session tree malformed lineage仍 fail closed。
11. metadata-only legacy branch不能假裝有 workspace snapshot。
12. CAS / checkpoint failure 不得 advance head，也不得讓任何 projection 宣稱 workspace 等於某個 snapshot；只能留下 durable 的 `checkpoint_deferred` 或 `recovery_required` 標記。（v2 修訂：`run_finished` 本身不因 run checkpoint 失敗而被阻止，§22.2。）
13. `off` / `observe` 模式下，原生 worker 並行度與多 team 並行 run 行為不變（v2）。
14. `required` 模式下，同 subject root 同時只能有一個 coordinator；fork / checkout / restore / GC apply 在 lease 被持有時失敗（v2）。
15. 自動 recovery 不 append 任何事件（v2，§16.2）。
16. `session fork` / `checkout` 在 EventStore 無法開啟時失敗，不再靜默以 `es == nil` 繼續（v2）。

---

# 41. Performance targets

不設定過度武斷的硬 SLA，但 coding agent MUST 建 benchmark。

至少：

```text
10k files / 1 file changed
50k files / 10 files changed
100k files / no changes
100 branches sharing same baseline
```

評估：

```text
full capture wall time
incremental capture wall time
CAS physical bytes
logical bytes
dedup ratio
diff wall time
materialize wall time
SQLite size
```

v2 補充：v1 的 runtime 每次 run admission 與 run checkpoint 都做 full capture（§22），因此下列數字 MUST 在 PR-03 就產出，不等到 Phase 5：

```text
full capture wall time（無 stat_cache）
run admission 無變更時的 wall time（有 / 無 stat_cache）
```

benchmark 以 Go `testing.B` 實作、在 `t.TempDir()` 產生合成檔案樹，結果貼在 PR 描述中。數字只作記錄，**不是**合併門檻（D9：預設 mode 為 `off`）。

核心驗收：

> Incremental capture MUST NOT 重寫所有 unchanged file blobs。

Merkle diff：

> 相同 subtree MUST 以 tree hash short-circuit。

---

# 42. API invariants

Coding agent MUST 寫成 tests / comments。

### I1 — Immutable snapshot

Published snapshot永不修改內容 identity。

### I2 — Durable head

Branch head只指向 published snapshot。

### I3 — Event before projection

Head advance only after snapshot commit event durability。

### I4 — CAS integrity

Object path hash必須匹配 bytes。

### I5 — No future workspace on historical fork

Historical event只能使用該 lineage event之前的 snapshot。

### I6 — No silent fallback

missing/corrupt snapshot不能 fallback live workspace。

### I7 — Unmanaged preservation

Materialization只刪除 hufu-managed snapshot namespace。

### I8 — Git independence

Git metadata不是 runtime authority。

### I9 — Root confinement

Capture/materialize不能逃逸 subject root。

### I10 — Session/workspace coherence

成功 checkout後：

```text
ActiveBranch == branch whose head was materialized
```

### I11 — Control-root exclusion（v2）

control root 子樹永遠不是 managed path：capture 不讀、materialize 不寫也不刪。

### I12 — No silent omission（v2）

inclusion policy 只排除 `.git`、control root 子樹、Git ignore / `.hufuignore` 路徑；其他 regular file 或 symlink 被排除即為 bug。

### I13 — Recovery never appends（v2）

自動 recovery 只能 publish 已 durable 的事件、orphan 未 durable 的 snapshot、或 complete forward；不得 append 或 replay 事件。

### I14 — Subject-root exclusivity（v2）

會改動 live filesystem 或 branch head 的 version operation（materialize、fork、checkout、restore、adopt、GC apply）只能在持有 subject-root lease 時執行；`required` 模式的 coordinator 在整個 process 生命週期持有它。

---

# 43. Rollout（v3：不是 coding agent 的工作項目）

v1 交付狀態（D9）：

- 預設 `workspace-versioning.mode: off`，行為與現在完全相同
- `observe`：run 建立 snapshot 與事件，但 `session fork` / `checkout` 維持舊行為（不 materialize）；不持有長期 lock
- `required`：完整 workspace-aware fork / checkout；同一 subject root 同時只能有一個 coordinator（§27）；非 Git workspace 需要 `.hufuignore`（§9.2）

這三種 mode 都必須在 v1 完整實作並測試。**何時**把預設改成 `observe` 或 `required`、是否先收集使用數據，由維護者決定，不在本規格範圍；coding agent 不得修改預設值。

---

# 44. Coding-agent execution order

Coding agent MUST 依序工作，不要跨 phase 一次大改。v2 依 review 重新編號（新增 PR-00 與 PR-05，原 PR-05～PR-08 順延）。

### PR-00 — Shared primitives + snapshotter hardening（v2 新增）

- `internal/fsutil`：`AtomicCreateFile` / `AtomicWriteFile` / `SyncDir` / interprocess flock（自 `internal/team`、`internal/workspace` 抽出，原處保留 thin wrapper）
- `internal/sqlmigrate`：checksum migration（registry 改用它，行為不變）
- 修正 `WorkspaceSnapshotter` 的目錄 symlink `EISDIR` 與 FIFO 阻塞（§2.1.1）
- Phase 0 regression tests

### PR-01 — Version store skeleton + schema

- package
- migrations（`internal/sqlmigrate`）
- connection pragmas
- types（含 `ObservedFile` / `ObservedDelta`、v2 reason enum）
- DB open/close
- tests

### PR-02 — CAS objects + deterministic Merkle tree

- blob/link/tree
- codecs（§8.3 entry field values、D6、非 UTF-8 拒絕）
- integrity
- tests

### PR-03 — Full capture + materialize + diff

- 自有 inclusion policy、`ExcludeSubtrees`、`.hufuignore`
- materialize 經 `os.Root`
- full capture 與無變更 admission 的 benchmark（§41）
- no Coordinator integration
- standalone tests

### PR-04 — Snapshot publication/event/recovery

- pending/published/orphaned/pruned
- branch heads（含首次 head 語意）
- operations
- `subject_state`、`stat_cache`
- event schema + `IsKnownEventType`
- recovery 不補寫事件
- crash tests

### PR-05 — Locks + workspace resolution + CLI fail-closed（v2 新增）

- `versionstore` 的 project lock（`lock.go`）與 owner 診斷檔
- `commandWorkspaceLease` 增加 project lock；required 模式在 team 載入時取得（§27）
- `hufu session` / `hufu workspace version` 的變更類子指令改經 `resolveCommandWorkspace` 取得 team lock（§26）
- `Registry.GetWorkspaceByControlRoot`（§26）
- `WorkspaceVersionContext` 建立與 `RuntimeServices` 接線；no-op 實作（§25）
- 平台判斷 `PlatformSupported()` 與非 Linux stub（§3 G9）
- `session fork` / `checkout` 的 EventStore fail closed
- `workspace-versioning` config 欄位（§31）
- 此 PR 不改變 `off` 模式任何行為

### PR-06 — Session fork integration

- branch attach
- active fork
- historical event fork（含先保存 active branch drift）
- fork crash recovery（§18.3，IT9）
- legacy behavior

### PR-07 — Session checkout / restore integration

- source checkpoint
- materialization
- forward recovery
- branch activation
- `workspace version restore`

### PR-08 — Runtime run-boundary checkpoints（v2 重寫，D1）

- `AdmitRun`：admission drift、recovery marker、`.hufuignore`、mode floor
- `CheckpointRun`：`run_finished` 之前的 full capture 與 deferred 語意
- `MarkUnauthorizedMutation` + `workspace version adopt`
- retry/resume regressions
- IT10、IT13、IT14

### PR-09 — Doctor/GC/observability/docs

- integrity repair
- GC（`pruned` tombstone）
- `workspace version downgrade`
- 事件投影 metrics 與 `workspace version status`（§32.2）
- CLI
- 文件（§45 的文件清單），並執行 `bin/check-docs`

### PR-10 — Attempt-level checkpoints（v1.x，非 v1 範圍，v1 不得實作）

- 前提：scheduler 能保證 attempt 期間 subject root 獨占，且 §15.2 的 type change 限制已解決
- 使用 `CaptureFromDelta`（reason=`attempt`）

每一 PR 都必須通過（與 CI 相同）：

```text
go test ./... -race -short -timeout 20m
go vet ./...
golangci-lint run
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./internal/fsutil/... ./internal/sqlmigrate/... ./internal/workspace/versionstore/...
```

（尚未建立的 package 從該 PR 起才納入 windows 編譯檢查。）

並至少執行新增 package 的 targeted tests。新檔案遵守 CLAUDE.md 的 800 行上限與 `.golangci.yml` 的 gocyclo 40。

---

# 45. Definition of Done

功能不以「可以存 snapshot」為完成。

完整 DoD：

- [ ] CAS blob/tree/link immutable + verified
- [ ] SQLite snapshot DAG durable
- [ ] branch head durable + optimistic generation
- [ ] event-first snapshot publication
- [ ] crash recovery
- [ ] initial baseline
- [ ] external drift capture
- [ ] O(1)-style fork metadata/CAS reuse
- [ ] historical event fork正確
- [ ] checkout真正 materialize filesystem
- [ ] current branch先安全 checkpoint
- [ ] metadata-only legacy compatibility明確
- [ ] Git HEAD/index/branch不被操作
- [ ] session diff包含 workspace diff
- [ ] GC dry-run/apply
- [ ] doctor integrity + repair
- [ ] symlink containment
- [ ] unmanaged paths preserved
- [ ] retry/resume semantic regression tests
- [ ] fault-injection tests
- [ ] benchmark（數字記錄於 PR）
- [ ] 文件：
  - 新增 `docs/architecture/workspace-versioning.md`，開頭使用既有 lifecycle header（`> Status:` / `> Authority:` / `> Verified-Commit:` / `> Supersedes:` / `> Superseded-By:`，格式同 `docs/architecture/sqlite-maintenance-policy.md`）
  - 新增 `docs/reference/workspace-versions-sqlite-schema.md`（格式參考 `docs/reference/context-sqlite-schema.md`）
  - 更新 `docs/reference/workspace-command-reference.md`（`workspace version` 子指令）
  - `docs/README.md` 加入上述兩份新文件的索引
  - `docs/architecture/execution-runtime.md` 加一段連到新文件，說明 `WorkspaceSnapshotter` 與 versioning 的分工
  - `bin/check-docs` 通過
- [ ] 自有 inclusion policy；bookkeeping 同名的專案目錄有被版本化（v2）
- [ ] control root 子樹排除（v2）
- [ ] `.hufuignore`（非 Git + required）（v2）
- [ ] subject-root lease；多 team 共用 subject root 的語意（v2）
- [ ] fork crash recovery（v2）
- [ ] run checkpoint deferred 語意（v2）
- [ ] unauthorized mutation recovery marker + adopt / restore（v2）
- [ ] mode floor + downgrade（v2）
- [ ] GC `pruned` tombstone（v2）
- [ ] PR-00：共用原語抽出、snapshotter 缺陷修正（v2）
- [ ] 非 Linux 平台可編譯、effective mode 強制 off（v3）
- [ ] `Registry.GetWorkspaceByControlRoot`；session CLI 取得 team lock（v3）

---

# 46. Final architecture

完成後 hufu 的 session 應具備：

```text
                         Session Tree
                     ┌────────┴────────┐
                     │                 │
                   main               exp
                     │                 │
                  head S8           head S9
                     │                 │
                     └──────┬──────────┘
                            │
                     Snapshot DAG
                            │
                       Merkle Trees
                            │
                      CAS File Objects
```

而 runtime state 與 workspace state 的關係變成：

```text
RunEvent lineage
      │
      ├── task/session state
      ├── decision/memory projection
      └── workspace_snapshot_committed
                     │
                     ▼
                 snapshot ID
                     │
                     ▼
               root tree hash
                     │
                     ▼
                  CAS bytes
```

這使 `session fork` 從目前的：

```text
fork runtime/event projection
```

升級為：

```text
fork versioned execution state
=
event lineage
+ session/runtime projection
+ workspace filesystem version（subject root 內的 managed paths）
```

且完全不依賴 coding agent 是否使用 Git。

v2：這裡的「execution state」只涵蓋 G8 列出的範圍；subject root 外的寫入、unmanaged 路徑、外部副作用都不會被 fork 或還原。

---

# 47. Maintainer notes

本功能最容易出現的錯誤不是 hash algorithm，而是 **authority confusion（權威來源混淆）**。

實作者必須持續遵守：

```text
Live filesystem != canonical historical snapshot

session_tree.json != workspace object store

CAS object existence != published snapshot

published snapshot != branch head

Git working tree != Hufu workspace version graph

WorkspaceSnapshotter（effect observation） != versionstore capture（v2）

LocalExecutionWorld lease（process-local） != subject-root lease（v2）

team 層級 workspace_id != project 層級 subject root（v2）

SessionData.RuntimeWorkspace != subject root（v2）
```

最重要的 publication invariant：

```text
CAS durable
    ↓
snapshot pending
    ↓
workspace_snapshot_committed durable
    ↓
snapshot published
    ↓
branch head advance
```

最重要的 checkout invariant：

```text
source branch safely captured
    ↓
target CAS verified
    ↓
filesystem materialized
    ↓
session projection rebuilt
    ↓
ActiveBranch changed
```

任何 implementation 若顛倒這兩組順序，都應視為 correctness bug，而不是 optimization trade-off。
