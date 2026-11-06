# hufu Native Image Runtime — coding agent 實作規格與驗收計畫

> Status: active
> Authority: reference
> Verified-Commit: `b97ef843`
> Supersedes: `docs/tmp/spec.md`（原提案，已移入正式計畫）
> Superseded-By: —
> **Implementation:** ready for staged implementation；本文定義待實作行為，不代表功能已存在或測試已通過。
> **Target:** `github.com/kjelly/hufu`
> **Baseline:** `b97ef843f440ab0cca85a8a453892ba2c4eef560`；2026-11-04 重新靜態核對。
> **Scope:** 通用圖片 I/O runtime；下列工作均可由 coding agent 修改程式、文件與離線測試完成。
> **Location:** `docs/implementation-plans/native-image-runtime.md`；依使用者指示移入版本控制。階段證據記於 [實作進度](native-image-runtime-progress.md)，功能契約仍以程式與正式架構／參考文件為準。

## 0. 已定案的交付邊界

圖片理解由模型負責；Hufu 負責原生傳遞、來源與授權、資源限制、持久化、恢復和可驗證成果。沿用既有 `ArtifactStore`、`ModelProfileRuntime`、`ExecutionTarget`、canonical Todo、execution policy、receipt 和 event journal，不建立第二套排程、模型目錄、記憶或 CAS。

交付分成可各自驗收的 milestone：

| Milestone | 可交付行為 | 必須先具備 |
|---|---|---|
| M0 | 私有圖片入庫、引用式對話、可重放 snapshot；含圖執行明確拒絕 | 格式／資源限制、scope、policy、無 bytes 持久化、快取隔離 |
| M1 | CLI／legacy root 的明示 Direct Agent、`RunTask` 經 hufu-local OpenAI-compatible adapter 原生送圖，支援 retry/resume | M0、逐請求能力與外送 admission、HTTP wire E2E、稽核 |
| M2 | Coordinator delegation、DAG、Chat plain/TUI、fanout/fallback 共用上述契約 | M1、各入口及生命週期回歸測試 |
| M3 | MCP mixed-content 與圖片成果的原生後續回合、授權匯出 | M2、tool provenance、runtime-owned result binding |
| M4 | 已驗證 Codex protocol 的 `localImage` 接入 | M2、schema fixture、受控 projection、fake app-server E2E |

尚未交付的入口必須在該次模型呼叫前拒絕圖片，不能悄悄只送文字。M1 不以 M3/M4 完成為前提；各 milestone 僅宣告本身已通過的支援範圍。

**開發驗收完全離線。** 真實模型呼叫、登入／憑證配置、正式部署及 canary 不在工作清單或 merge gate。使用者已指定後續真實驗收採 **Ollama `deepseek-v4.1-flash:cloud`**，並自行安排真實驗收；[官方模型頁](https://ollama.com/library/deepseek-v4.1-flash:cloud)於 **2026-11-06** 重新核對時標示 vision。此為外部驗收背景，不寫成 runtime 的模型特例、預設值或已成功呼叫的證據。指定模型不等同於提供 endpoint、憑證、圖片外送範圍或 image-token 上界；開發不能假設這些條件已具備，也不以取得它們阻擋離線實作。

本計畫不包含：圖片生成 API、OCR／計數演算法、音訊／影片／PDF、遠端圖片抓取、任意文字與圖片交錯的使用者介面、終端圖片預覽、自動轉檔／縮放／去 EXIF、自動圖片 GC，以及同一 OS 使用者下任意惡意程序之隔離。未知／未支援格式與能力一律拒絕，不加入 OCR 降級、未知能力試送或未驗證 adapter。

各項實作的預期效益如下；離線測試驗證資料流與授權，不驗證模型判讀準確率。

| 工作 | 效益 |
|---|---|
| 原生圖片輸入與有序引用 | 模型能收到原始圖片及指定順序，避免把路徑或 Base64 當成一般文字 |
| 私有 CAS、canonical binding 與恢復 | 原始檔移除後仍可恢復相同輸入，並阻止跨 task／attempt 借用圖片權限 |
| 逐請求能力、外送與資源 admission | 在送出前攔住不支援的模型、未授權目標與超限請求 |
| MCP 圖片結果與 runtime-owned binding | 後續模型能使用工具產生的圖片，完整保留來源、錯誤與 receipt，免由模型抄寫確定性資料 |
| 授權匯出與 Codex adapter | 使用者能取回原始成果；受支援的 Codex 版本可沿相同圖片授權接入 |

## 1. 現行權威與實作入口

以下定位以目前 checkout 的符號為準，不沿用舊 commit 的行號。PR-00 重新確認；改名時追蹤現行 owner，不新增平行邏輯。

| 面向 | 現況／檔案 | 實作方向 |
|---|---|---|
| SDK | `go.mod`：Fantasy `v0.45.5`、mcp-go `v1.3.0` | 沿用 pinned SDK；不為圖片功能預先升級依賴 |
| Fantasy | `AgentStreamCall` 有 `Prompt`、`Messages`、`Files`；`createPrompt` 會先加入 Messages，非空 Prompt 再追加 user message | 含圖使用完整 Messages、空 Prompt、空 Files；最後一則為合法 user/tool；禁止重複追加 |
| SDK 工具媒體 | `fantasy.NewImageResponse`、`ToolResultOutputContentMedia`；openaicompat 已有工具圖片轉後續 user image 的處理與測試 | 使用既有 wire/compiler 能力；durable tool history 統一保存 refs，不能直接保存 SDK raw-media result 或重複注入 |
| CLI／Chat | `cmd/hufu/{root,runcmd,prompt_input,chat_turn}.go` | 增量接入 typed turn；文字 API 保留相容 wrapper |
| Worker | `internal/team/{subagent_provider,subagent_hufu,coordinator_task_run}.go` | AttemptRequest 只帶 canonical media assertion；不接受它另行選擇權限 |
| CAS | `internal/team/evidence_store.go`；現有 data/meta 使用 `0755/0644` | 同一 FileArtifactStore 新增私有 media namespace；既有 artifact layout 不改 |
| Scope／policy | `artifact_access.go`、`execution_policy_snapshot.go`、`tool_policy_gate.go` | media grant 由 durable occurrence 決定，納入 policy hash |
| 對話 | `history.go` 直接序列化 Fantasy messages；`compaction_state.go` 保存 history/replacement/checkpoint | 新增引用式 codec；禁止原生 media bytes 直接進這些保存路徑 |
| 快取／去重 | `cache_policy.go`、`coordinator_taskcache.go`、`task_journal.go`、`dag_scheduler.go` 的 `taskExecutionInputCacheIdentity` | 含圖任務停用所有自動結果重用與跨 occurrence 合併；另保存 media digest |
| 模型能力／admission | `modelprofile/*`、`providerintrospection/*`、`model_capability_validation.go`、`provider_admission.go` | 三層圖片能力、provider-bound estimate、逐回合 hard gate |
| MCP | `internal/mcp/{manager,runtime_tools}.go` 丟失圖片 bytes；`mcp_action_result.go` 無完整 mixed-content 支援 | 保留有序 typed blocks、IsError、structuredContent；接入 media publisher |
| Codex | `codex_appserver_protocol.go` 目前只送 text；`codex_appserver_client.go` 提供 bounded RPC | M4 才增 localImage；較早 milestone 明確拒絕含圖目標 |
| journal | `SetNoJournal` 只停用 task-result journal；event store 另行初始化 | 判斷實際 durable stores，不以 `--no-journal` 旗標代替能力檢查 |

需追蹤的 projection 包括 Todo/task、event reducer、session、conversation compaction、receipt、inspect／JSONL／report／TUI、獨立 `audit verify`。新欄位必須沿整條生命週期傳遞。

## 2. Canonical media、turn 與授權契約

純型別與格式驗證置於 `internal/media`；依賴 ArtifactStore、Todo、事件的 binding 留在 `internal/team`，避免 package cycle。以下為欄位契約；具體型別名稱可配合現行程式，但不得改變語意。

```go
type MediaRef struct {
    ID        string `json:"id"`         // existing ArtifactStore opaque ID
    SHA256    string `json:"sha256"`     // exact original bytes
    MediaType string `json:"media_type"`
    ByteSize  int64  `json:"byte_size"`
    Width     int    `json:"width"`
    Height    int    `json:"height"`
}

type InputPart struct {
    Type    string `json:"type"` // enum: text | image
    Text    string `json:"text,omitempty"`
    MediaID string `json:"media_id,omitempty"`
}

type TurnInput struct {
    Version int         `json:"version"` // 1
    Parts   []InputPart `json:"parts"`
}

type MediaOccurrence struct {
    RunID       string `json:"run_id"`
    BranchID    string `json:"branch_id"`
    InvocationID string `json:"invocation_id"`
    TurnID      string `json:"turn_id"`
    PartIndex   int    `json:"part_index"`
    MediaID     string `json:"media_id"`
    Origin      string `json:"origin"` // enum: user_file | mcp_tool | trusted_artifact
    ToolCallID  string `json:"tool_call_id,omitempty"`
}
```

另由 runtime 保存 task binding：來源 occurrence、task ID、occurrence revision、agent、允許用途、ordered selected parts、content digest 和 binding hash。attempt grant 再綁 attempt／lease，不能只靠可重放的 ID 或 agent 名稱授權。

- `text` part 只能有 Text，`image` part 只能有 MediaID；拒絕未知 type、非法欄位組合、越界 index 和空 image ref。所有固定 tool 參數詞彙用 lowercase/snake_case enum。
- `MediaRef` 是已驗證 ArtifactRef 的 projection，不是模型可提交的權限。MIME、size、dimensions、digest、owner、來源、scope 均由 runtime 建立；模型只可從當前授權列表選擇輸入。
- `TurnInput.Parts` 是權威次序；相同圖片出現兩次時保留兩個 index。Prompt 相容 view 由同一 builder 產生，不能再當成完整含圖輸入。
- task canonical Todo 保存 media binding／digest。`taskDefFromTodoItem`、worker resolution assertion、AttemptRequest、retry、repair、replay 皆從同一 projection 取值；拒絕過期或不一致 assertion。
- delegation 新增有序 `image_inputs`，元素選擇當前已授權的 `(turn_id, part_index)`，由 runtime 解析 MediaID。不可任意指定其他 run 的 ID／路徑；重複 index 是否出現依明示輸入保留，不能自動去重或重排。
- Coordinator 可在 metadata-only 模式分派圖片：只收到 opaque refs、順序、MIME、尺寸與用途，不能宣稱自己看過圖片；worker 真正送圖才需 vision gate。metadata-only 是明確的控制流程，不能拿來完成含圖任務。
- 依賴結果、fanout、judge/skeptic、sidecar 不自動繼承圖片。僅需讀取文字結果者維持文字路徑；若要消費 bytes，必須有新的明示 grant 和逐請求 admission。
- extra-model fanout 由 parent 的 frozen ordered topology 與圖片 binding 決定各 slot；public parent 在建立任何 leaf client 前啟動 provider proxy，失敗時不得分派。每個原生圖片 request 同時驗證 parent 和 private leaf 的 live lease，durable admission 保存兩者身分及 stable model-execution ID。parent 是 task/session checkpoint 的唯一發布者，private leaf 不得以自己的 revision 覆寫 parent；result-collection 仍使用其專用 durable events。
- fanout 的每個已啟動 leaf 都須保留原始成功 receipt 與完整 accepted terminal result，包含原有 findings、限制與 structured payload。accepted result 由 runtime 綁在對應 receipt，不能由模型複製或重建，也不能以非終態 `SubmittedResult`、合併文字或 judge 選出的單一結果代替。缺少、失敗、未完成或身分不符的 leaf 阻擋整體圖片完成；讀取／重播保持原結果，不重新推導歷史判斷。
- fanout 沿用既有恢復邊界：首次工作前可由新程序從 canonical CAS 恢復；執行中的 tool-result repair 只整理原有文字證據。已 blocked 的 runtime collector 不支援 operator 追加回合，必須保留 sealed work／receipt 並拒絕恢復，不得重新看圖或宣告成功。此階段不新增 fanout 的人工 repair grant。
- `submit_result` 不要求模型複製 media digest／來源／receipt。結果中的確定性 media refs 由 runtime-owned result binding 注入；拒絕 model overrides，再進 canonical validation、hash、receipt 與保存。
- 未授權的圖片連 metadata 也不主動廣播。Memory/LTM 不保存圖片 bytes；衍生文字經既有 redaction，保留必要 lineage，視覺判斷不成為 runtime-verified fact。

## 3. CLI／Chat 輸入與狀態轉換

### 3.1 固定的首版輸入順序

```bash
# M1: explicit direct worker in one team
hufu run --team my-team --image ./a.png --image ./b.webp '@vision_worker Compare these images'
hufu --agent-team my-team --image ./a.png '@vision_worker Describe this image'
# M2: coordinator entry point and chat
hufu run --team my-team --image ./a.png 'Describe this image'
hufu chat --agent-team my-team
# /image add ./a.png
# /image list
# /image remove 1
# /image clear
```

CLI 與 Chat 統一為「一個非空 text part → 圖片依加入順序排列」。不以旗標與 positional argument 的相對位置推斷交錯；圖片-only 輸入與任意交錯 UX 不在本版。內部 typed contract 仍須忠實保存已建立的 part 順序，MCP mixed-content 可使用有序 blocks。

- `--image` 使用保留原始重複值與次序的 repeatable array flag；canonical run 的 instance-scoped options、legacy opts／profile、chat 共用 typed builder。禁止 flag 全域狀態跨命令殘留。
- 同一次含圖 CLI 需明示 team 或 `--default`，只允許一個已解析 team／非空執行 segment；純 team selection 不計成執行 segment。多 team／多執行 segment 或依賴 auto-team 猜 scope 的輸入先於任何模型呼叫拒絕；既有文字路徑不改。
- M1 只接受明示 `@worker` 的 Direct Agent 及程式 API 的 `RunTask`；一般 Coordinator／fast-path escalation 待 M2，不能因 auto route 多呼叫一次模型。Direct 結果若既有流程需要 synthesis，以原始文字目標與 canonical worker result 建立明確的非視覺消費 view，不 hydrate 該 task 的原圖、不改寫 durable media history；task 完成與 run acceptance 維持原有契約。
- `#path` 保持文字 context；偵測到圖片／二進位時不注入 bytes，給出改用 `--image` 的安全提示。不能用副檔名決定原生圖片語意，不能解析自然語言猜圖的用途。
- `--input`、`--input-file`、`--var` 維持原有 typed/text input；本計畫不新增 Base64 輸入或把圖片隱藏在 scalar 欄位裡，也不全面禁止原有一般字串。
- `--dry-run --image` 只做本機 bounded validation 和能力／policy 預檢，不寫 CAS、event、conversation，不呼叫 provider。輸出不能將 preview 標成 durable admission。

### 3.2 Chat pending images

| 狀態／事件 | 行為 |
|---|---|
| add | bounded 讀取、驗證並暫存私有 process-local bytes，保留順序；計入 staging quota |
| remove／clear | 釋放對應暫存與 quota；remove 使用畫面列出的 1-based index |
| 驗證或 admission event commit 前失敗／取消 | 保留仍有效的 pending，允許修正後重新提交；不呼叫 provider |
| canonical input checkpoint 已提交 | 消費 pending，記錄 TurnID／snapshot；後續 provider 或 checkpoint attestation 失敗不能將圖片加回下一回合 |
| checkpoint 提交狀態無法確認 | 凍結 pending 並標示需要恢復；不得自動重送，使用者可明確 clear |
| `/reset`、切換 team、正常退出 | 清除未提交 pending，釋放資源並顯示清除數量；已提交的 snapshot 保留 |
| admission 前 process crash | 未提交 staging 不承諾恢復；重開後明確從空 pending 開始 |
| admission 後 process crash | 從 durable TurnID／snapshot 恢復；不重新讀原始 path、不重送新 turn |

plain/TUI 共用相同 state machine。pending 變更不能與正在提交的 turn 競爭；以既有 chat turn 序列化機制凍結本次 snapshot。畫面只顯示安全名稱、opaque ID、size、dimensions、狀態，不顯示 bytes／完整原始路徑；終端與電子紙均採文字資訊。

圖片控制指令適用 `chat --ui plain` 與 `chat --ui chat`：`/image add <path>`、`/image list`、`/image remove <1-based index>`、`/image clear`。路徑是 literal argument，不做 shell expansion；圖片控制指令不寫入 readline history。先前回合失敗所保留的圖片不會自動附加到已排隊的下一個 prompt，使用者須在 idle 時明確提交新 prompt。舊 `--ui dashboard`／prompt injection 入口明確拒絕 `/image`，不把來源路徑當任務交給模型。

Pending staging 使用獨立的 process-wide 64 MiB logical-byte quota；provider transport 沿用自己的 quota。兩者不可巢狀持有同一 semaphore，這些 logical quotas 不代表程序 RSS 上限。提交後只從 canonical CAS 載入圖片，admission 通知會先釋放 staging bytes，再開始 provider request。

## 4. 私有儲存、ingestion 與資源限制

### 4.1 單一 ArtifactStore 的 media namespace

在既有 FileArtifactStore 內新增 `logs/artifacts/media/{data,meta}`，media namespace 及新檔案分別使用 `0700/0600`。media IDs 使用版本化 `media-` 前綴；`Put/Get/Resolve/Verify/Open/TrustedArtifactMetadata` 和 read-only opener 依 ID 分流，仍是同一 ArtifactStore，不另建 bytes store。不改 legacy data/meta 的權限與 layout。

ID 由 `(media format version, content SHA256, canonical MIME)` 決定；同一 workspace 相同 bytes 可共用一份 image blob。artifact 的 Kind=`image`、Role=`media`、logical Path=`media/image/<id>`，避免 input/tool/result 三種使用角色導致同 bytes 的 canonical metadata 衝突；使用角色、owner 和當次 producer 放在各自 occurrence binding/event，不拿 CAS 第一位寫入者的 metadata 授權其他 occurrence。

- 既有 `createOrVerifyArtifactMetadata` 的 SHA/size 檢查不足以作 media 授權；新增 media canonical metadata 比對，驗證 MIME、kind、logical path 與 schema version，不改舊物件的比對語意。
- media 的 `ListByTask`／result export 從 canonical occurrence 索引取得當前 task 的 refs，再 Resolve 內容；不能用 blob 第一位寫入者的 TaskID 過濾而漏掉後續合法使用者。相同內容比較沿用 content metadata 語意，producer／run／task 身分向 occurrence event 驗證，不把整份 legacy `sameArtifactRef` 比對一律放寬。
- private media 需要明確 ID 與 runtime binding，不把既有 digest-only Resolve 搜尋擴展為媒體列舉；持有 SHA256、logical path 或 meta 中第一位 owner 都不能取得新權限。
- 新 namespace、data/meta 不可是 symlink 或不安全特殊檔案；開啟既存 media namespace 時驗證權限。無法建立或驗證私有存放的平台拒絕含圖執行；文字與 legacy artifacts 照舊。
- `Put` 後驗證，再 commit media event／projection，最後才允許送出。失敗不刪其他 occurrence 已引用的物件；維持 CAS append-only。首版不自動 GC／retention 刪圖，workspace 刪除仍沿用既有管理契約。
- source path 不寫入 CAS metadata、receipt、prompt 或 telemetry。原始 bytes 保持不變，包含 EXIF；外送 grant 涵蓋這些原始 bytes，不宣稱已去除敏感 metadata。

### 4.2 不可信檔案與有界驗證

1. 只接受 regular file；拒絕目錄、FIFO、device、symlink（包括父路徑逃逸）。以安全開啟與同一 file handle 的 fstat／bounded read 驗證，不能先 Stat 再無限制 ReadFile，也不能讓 FIFO 在檢查前阻塞。
2. magic／實際 decoder 決定 MIME，副檔名僅供安全顯示；不符時採實際 MIME 並顯示安全警告。接受靜態 PNG、JPEG、WebP；若缺 WebP decoder，以 mise 管理的 Go 工具加入相容依賴。
3. header 檢查尺寸後仍需完整 bounded decode／格式驗證；截斷資料、異常 chunk／長度、乘法 overflow、錯誤壓縮資料均拒絕。PNG 的 APNG chunks、WebP 的 animation flags/chunks 必須明確偵測並拒絕，不可只取第一影格。
4. 不 resize、轉檔、移除 EXIF，也不 fetch URL；不支援格式或 provider 不接受該格式時拒絕，避免暗中改圖。
5. provider 每次讀圖採一次授權開啟、bounded 讀取並驗證「實際即將送出的同一份 bytes」之 digest／size／MIME／尺寸，再序列化；不能 Verify 後另開檔送出不同內容。bytes 不跨長生命週期保存。

本版上限為 hard ceilings，設定只能調低。所有數量限制在配置 schema、ingestion、MCP ingestion 和每次包含歷史圖片的 provider request 同時執行。

| 限制 | Ceiling | 計算範圍 |
|---|---|---|
| 圖片數 | 8 | pending、單 turn、單 tool result、單完整 provider request 各自適用；重複引用也計數 |
| 單張原始 bytes | 16 MiB | bounded read／base64 decode 之前與之後都要防溢出 |
| 圖片原始 bytes 合計 | 32 MiB | pending／turn／tool result／完整 request 各自適用，含歷史與重複 parts |
| 單張像素／單邊 | 36 MP／8192 px | checked arithmetic；先於完整 decode |
| branch lineage admitted unique blobs | 128 MiB、64 個 | user/tool/output 共用，跨 turn/run 累積；依 admitted events 計數，fork 繼承、resume/checkout 重建 |
| 同時完整 decode | 1 | runtime 共用 limiter；等待可取消 |
| 同時載入待傳輸原始 bytes | 64 MiB | 所有 attempt 共用 limiter；先預留再讀取，完成／失敗／取消都釋放 |

branch 配額與 media admission 在同一 canonical commit 中更新；同 blob 的重複引用不重複收取 branch bytes，但仍計入 turn/request 的圖片數與 bytes。`/reset` 只清 active context，不重置 durable branch 配額；容量滿時給出明確錯誤，不能靠 fork 或重啟繞過。磁碟剩餘空間不足亦拒絕 Put；未 admitted orphan 不算 branch 使用量，但保留其實際磁碟占用，不假裝已回收。

序列化還會產生 Base64／JSON／SDK copies；預留資源時必須計入可計算的編碼膨脹與 adapter frame ceiling，不能把 64 MiB 原始 bytes 當成程序 RSS 上限。若平台 memory／transport 限制更小，取交集；超限在傳輸前拒絕。離線 bench 記錄實際 allocation／RSS／wire size，不憑空承諾 latency SLO。資源 gate 與取消測試從 PR-01 起存在。

### 4.3 可保證的安全邊界

Hufu API 對原生載圖、MCP、匯出及結果 binding 執行 task/attempt ACL；私有檔案權限限制其他 OS 使用者。projection 僅包含該 task 的圖片，不把整個 media store 放入 worker workspace 或加入 readable roots。

`local-sandbox` 及 `0600` 不提供同 UID 惡意 shell／外部程序隔離；既有任意 shell 權限屬受信任 execution boundary。此計畫不承諾靠 opaque ID、chmod 或 prompt 防止同 UID 程序自行讀檔，也不新增 VM／容器部署工作。跨 run/task 的拒絕測試驗證 Hufu 授權 API，不冒充 OS 隔離證據。

## 5. 引用式對話、compaction 與恢復

這是 M0 的前置條件，不能留到 UX 或後續安全 PR。

- 引入版本化 persisted media message codec：保存 role、有序 text/tool parts、media occurrence refs、binding hash；不保存 FilePart.Data、media output Data、data URI 或 SDK 私有 media payload。非媒體 part 沿用現有序列化語意。
- 無媒體的舊 conversation/compaction state 仍按原版本讀寫與驗證，不重算歷史 hash。含圖 state 使用新版本與明確 ref tag；load 解析成 runtime ref representation，不能為了塞回 `[]fantasy.Message` 就在啟動時讀回所有 bytes。
- canonical history、checkpoint、compaction generations/replacement、session_history 相容 projection、debug/export 和 worker transcript 均經同一 codec；hash/checksum 對 ref representation 計算，不能因 hydrate 的 Base64／provider wire 表示改變。
- 純文字相容 projection 若無法表示圖片，僅顯示已授權 metadata 並標示非恢復權威；resume 必須讀 canonical media-aware state。缺 canonical state／binding 時拒絕恢復，不從 Markdown 猜圖。
- 唯一 request compiler 在已選定 provider-bound context 下，從 refs 驗證授權並 hydrate 短生命週期 SDK message；provider result／callbacks 中的 media data 在進入 durable state 前重新映射 refs 或按來源規則入庫。未知 raw media representation 拒絕保存，不能盲目 `json.Marshal` 後期待 secret redactor 清除。
- bytes-free 保證適用本功能管理的 typed media 資料流與其投影；它不宣稱能辨識任意文字／受信任 shell 輸出中的全部圖片編碼。已知 media namespace 對 path-enforcing tools 的存取仍走中央 policy，不用文字 redaction 偽裝 OS 隔離。
- compaction 僅摘要文字。尚未完成 task 需要的 image refs、tool-call pairing、順序及 source ranges 必須保留；文字無法縮至 fit 時拒絕該次請求。首版不自動裁剪已提交圖片來騰空間，避免模糊的「模型還需不需要這張圖」推測。
- sidecar compaction 只接收文字與安全 ref 描述；不得把 media data URI 交給文字 summarizer。所有 provider observers／LLM logs 只記 metadata，wire body 限於測試記憶體檢查，不能落地記錄。
- 缺失、損壞、binding 過期或權限不符時，在該回合 provider 前失敗；原始 source path 不存在不影響完整 CAS 的恢復。
- result-only repair 只修正提交，不重做圖片分析或有副作用工具。先重建 canonical media binding 與 grants；只需重述既有結果者不再送圖，需要再分析者走新 task occurrence。

## 6. 快取、去重與跨 occurrence 重用

**首版所有含圖任務停用自動結果快取與跨 occurrence 執行合併。** 這是確定的行為，不留下「之後決定快取鍵」的工作。

- 由 canonical task binding 判定含圖，不靠 prompt 關鍵字。停用 exact/semantic cache lookup、cache write、task journal restore/reuse、歷史結果自動套用、DAG inflight coalescing 及 extra-model 共用結果；`--no-journal` 本身不會停用其他 cache，因此不能當替代方案。
- 新增 `MediaInputDigest`：canonical ordered parts 的 `(type, text/ref identity, exact content digest)` 加上 binding/policy digest。納入 task occurrence contract、execution receipt、provider admission assertion、task input identity 與 recovery 檢查；不把隨機 attempt ID 混進 content digest。
- `taskExecutionInputCacheIdentity` 及相關 identity projection 必須辨識 media-bearing task，即使首版 bypass，也不能與既有 text-only input identity 混用。先前純文字成功結果不能完成新含圖任務。
- 同一 occurrence crash-resume 重用已接受結果仍沿用既有恢復規則：只在 media binding、內容、policy 和 receipt 全部一致時保留；這不是自動跨 task 快取。
- explicit evidence/history inspection 保留唯讀文字查詢；若把歷史圖片當作新輸入，必須由 canonical source result／lineage 明示 rebind，經一般授權及資源 gate，不能從文字 ID 取圖。
- 必測：相同問題換圖、同 bytes 換次序／出現次數、scope 不同、舊 text cache、新 process resume、並行相同 prompt 不同圖；只能在本 occurrence 合法恢復，不能錯誤 cache hit 或跨 task 合併。

## 7. 能力、policy 與逐請求 admission

### 7.1 圖片能力

在現有模型 profile 增量加入 `SupportsImageInput` 的 yes/no/unknown 與 provenance；保留 `SupportsAttachments`。只有明確 vision/image evidence 可支持 yes，通用 attachment 不推定可看圖。backend capability 和 transport adapter 的圖片支援另行記錄，三者皆 yes 才可送圖；no/unknown 拒絕，不提供未知能力試送 override。

需求從當次完整 request 的 media parts 派生，涵蓋 user、歷史、tool-produced images。task/team requirements 只能加嚴。每個 route candidate、fanout leaf、fallback、judge/skeptic 再做 gate；repair 若不消費圖片，不假造新的圖片需求或送出權限。

### 7.2 明示目標授權，不猜測 local／remote

不使用 `allow-remote-media` 加 hostname 判斷。所有圖片處理目標都必須有明示 allowlist，包含 loopback provider：本機 Ollama 也可能把請求轉到 cloud。grant 授權的是已解析且固定的 backend／provider／model，不宣稱能監控受信任外部服務內部的再次轉送。

現行 team/config parser 已接受單一 `media` policy，預設 disabled；team、operator 與 runtime ceilings 取交集，CLI 不能放寬。以下為授權與限制的設定範例；已交付入口與驗證狀態以[實作進度](native-image-runtime-progress.md)為準：

```yaml
media:
  enabled: true
  image:
    max-count: 8
    max-bytes-per-image: 16777216
    max-bytes-per-bundle: 33554432
    max-pixels: 36000000
    max-side: 8192
    max-branch-bytes: 134217728
    max-branch-blobs: 64
  allowed-targets:
    - backend: fixture_backend
      provider-key: fixture_provider
      model: fixture_vision
      image-token-upper-bound: 8192
```

`fixture_*` 和 8192 僅供離線 fixture 示範，不作真實 provider 預設或使用者模型估算。targets 不支援 wildcard；拒絕不明 backend/provider/model、重複衝突與無效 token bound。Codex target 仍須固定其 backend、上游 provider key（若 backend 可提供）與 model，並經該 backend 的 trust boundary。

operator 的全域／專案配置依載入順序取交集；一層明確停用後，後續配置不能重新啟用。team 未設定 `media` 時繼承 operator；team 設定時需明示 `enabled: true`，並只能縮小 limits／target 集合。`allowed-targets` 省略時繼承、明示 `[]` 時拒絕所有目標；token upper bound 取較大值以避免低估。未知 media 欄位及無效限制使配置失敗，不回退成較寬授權。

- grant 凍結實際 resolved backend/provider identity（含 transport/config/credential revision hash）、model、formats、limits、estimator/bound 與 policy digest；每次 request 比對，不能只比 model 名稱。
- 網路 adapter 不跟隨跨目標 redirect；transport identity 漂移時拒絕。授權不取代 provider authentication、工具權限或 subprocess `--no-net`。
- policy snapshot 的 media 欄位只在啟用／使用 media 時出現；舊 text-only hash/fixture 不變。resume 改 target、bound 或 scope 依一般 policy drift 規則拒絕，不改寫歷史。
- `--no-journal` 可用；實際 snapshot/event/projection/private CAS 無法可靠建立才回 `MEDIA_DURABILITY_REQUIRED`。`--temp` 可用，但 workspace 銷毀即失去 resume 能力。

### 7.3 Context／cost 與資源 admission

按完整 request 計算文字、tool schema、output reservation、安全餘量及每個 image occurrence 的 token 上界；Base64 長度只計 wire bytes，不能拿 bytes/4 當視覺 token。

優先使用有來源與版本的 provider-specific estimator；沒有可用公式時，只接受 trusted allowed-target 明示的保守 per-image upper bound，標為 operator estimate，不冒充模型實測。未提供 bound／未知容量／無法 fit 時拒絕；coding agent 不必取得帳戶或進行付費 calibration。fake provider 使用固定可驗證 estimator 測 gate。

admission 綁定本次 chosen target、profile、ordered media digest、policy 和最終 request；逐步工具回合也重做。cost 沿用既有 reservation／settlement，actual usage 與 estimated 分別記錄；沒有實際 usage 時保留 unknown，不能偽造計費或成功。

## 8. Provider／tool adapter

### 8.1 hufu-local Fantasy／OpenAI-compatible

- 建立單一 provider-bound message compiler。依第 5 節從 refs hydrate `TextPart/FilePart`，本次 user message 只出現一次；含圖 `AgentStreamCall` 使用 `Messages=history+currentUserMessage`、`Prompt=""`、`Files=nil`。非空 Prompt 會追加 user message，禁止與已編譯 user message 同時使用。
- image-only tool continuation 仍需合法最後 user/tool message；SDK validation 不得被空 prompt workaround 繞過。文字路徑保留原先建構方式。
- OpenAI-compatible 送出原生 image content part（data URI 屬 wire encoding，並非文字 prompt）。在同一 validated byte buffer 上計 digest 並編碼，直到送出／失敗後釋放。
- SDK 或 provider 不支援的格式／內容組合在本次 transport 前拒絕。fake HTTP endpoint 必須捕捉實際 request，解碼驗證 SHA、part 順序、roles、次數及 tool pairing；不以 `SerializeProviderRequest` unit test 代替。

### 8.2 MCP mixed content 與 output artifacts

[MCP tool result 規格](https://modelcontextprotocol.io/specification/2025-11-25/server/tools#tool-result)允許有序 content items 及獨立 structuredContent。沿用現有 direct/gateway/runtime action 的 descriptor assertion 與 authorizer，新增 typed ordered result API；舊 string API 保留 text-only wrapper，對含圖回覆明確 unsupported，不能靜默丟棄。

- 保留 text/image blocks 的順序、原始 `IsError`、structuredContent 和 tool-call identity。base64 在解碼前檢查可計算的 decoded-size 上限，解碼後套用第 4 節 validator／quota，再以 runtime publisher 建立 refs、producer descriptor hash 和 attempt binding。
- 工具 image MIME 與實際 bytes 不符時拒絕，不信任工具自稱的 owner、digest、path 或 artifact ID。未支援的其他 binary content 明確拒絕，不當成空成功。
- 單張、混合與多圖片統一使用 reference-only tool metadata。MCP publisher 先完成 bounded validation、canonical publication，再將有序 text/ref blocks 掛在對應 tool result；不能將 `NewImageResponse` 的 raw bytes 直接交給 SDK 作為 durable history。provider projection 在 admission 前才組裝合法 tool response＋後續 user media bundle，再由既有 compiler hydrate；不得同時使用 SDK raw-media conversion 而重複送圖。既有 SDK conversion 測試只證明 wire 能力，不代表來源或保存契約已滿足。
- 所有同批 tool results 完成合法 call/result pairing 後才附加 synthetic user media message；保留各結果的 block 次序與 tool provenance。synthetic message 明示 tool data，不能提升為新的人類指令。下一個 provider request 執行同樣的 capability／egress／quota admission。
- `IsError=true` 保持 error；圖片不能使工具或 task 成功。若無安全方式保留錯誤圖片，拒絕該次後續含圖回合並保留安全錯誤 metadata，不重執行已完成工具。
- `MCP ActionResult.Outputs` 在有 structuredContent 時保持該完整 object，不額外塞媒體欄位；無 structuredContent 且有文字時沿用 `outputs.result`（依序以換行連接 text blocks），image-only 時為空 object。`Artifacts` 保存 runtime-published 圖片 refs；另在 canonical receipt/result 的版本化 runtime-owned media projection 保存有序 `{type: text, text}` 或 `{type: image, artifact_id}` blocks，避免從 Outputs 重建圖片次序。若 team output schema 要求不存在的文字欄位，依一般契約拒絕，不能捏造內容滿足 schema。舊 text-only object shape 不變。
- runtime-owned 結果須走 canonical receipt/hash、local/external submission 與 replay 路徑；原始工具輸出只成為有來源 evidence，不自動升級成 verified semantic fact。

#### M3 必須一併完成的來源與消費契約

| 邊界 | 固定行為／既有 owner |
|---|---|
| 工具執行 | direct 與 dynamic gateway 共用 typed result／publisher；descriptor assertion、中央 authorizer、reserved-tool 邊界仍在 RPC 前執行。gateway offload／截斷保留 reference-only metadata，不得再次呼叫工具來取回圖片 |
| 來源發布 | 沿 `media_events.go`、既有 ArtifactStore 和 branch quota 發布 `mcp_tool` snapshot。`tool_media_published` 綁 run／branch／invocation、task／occurrence revision／attempt／dispatch、適用時的 model-execution ID、tool-call ID／descriptor hash、ordered parts／refs、snapshot／policy hash 與 IsError；runtime action 另綁 ActionInvocationID。任何工具自報的相同欄位都不是權限 |
| 原子性與失敗 | 沿既有 journal 的條件式原子 batch 將 snapshot／quota 與 producer binding 一起提交，更新 projection 後才讓 refs 可用。失敗可留下不可授權的 CAS orphan，不能留下半套可讀 grant。工具已完成但 publication 失敗時記錄 attempt failure 並停止後續模型回合；SDK 的 StopTurn 之外，runtime 必須保存該失敗，不能靠自動 worker retry 或 result repair 重跑工具 |
| 後續請求 | 將原輸入與已授權工具來源組成獨立、有序的 consumer bundle；原 task 的 MediaInputDigest 不改寫。bundle 綁目前 live lease、producer identities、完整 ordered refs 與 target／policy，再與 provider admission 原子提交；最終 HTTP boundary 及獨立 audit 均重驗。純文字起始、稍後才取得工具圖片的 task 也走此路徑，不能要求先有使用者圖片 binding 或借用其他 task 的 grant |
| SDK／計數 | 四種 provider API、token counter 和 preflight 使用相同的私有 message projection。完整 tool-call/result batch 通過 `validateToolPairs` 才能追加標示為不可信工具資料的 user media message；每個 occurrence 只送一次。不能只在 compiler 補訊息而繞過完整 request 的 token／policy admission |
| History／repair | codec、`historyReferences`、`validateMediaHistoryLineage`、compaction 及 audit 都驗證 producer binding，不能把工具輸出當成無 task owner 的人類輸入。`projectHistoryToolPairs` 改展示用 call ID 時同步更新 carrier 的配對 ID，原 producer ID／receipt 不變；文字 repair view 移除圖片 carrier，保留文字、錯誤與 pairing |
| Native action／結果 | 先沿 `canonicalizeRuntimeActionOutputs` 驗證完整 Outputs，再以 runtime publisher 綁定圖片；ordered media projection 與 Outputs 分開。擴充既有 runtime-owned result binding 的來源解析，從 canonical media projection 取值，不將 refs 塞入 Outputs 來冒充現有 output binding。local/external submission 都拒絕模型覆寫，replay 驗證原 receipt／binding，不重建成較新的來源 |

PR-05 已完成上述 runtime 契約，離線 SDK、producer／consumer、失敗不重播、結果與 replay 的證據見[進度紀錄](native-image-runtime-progress.md#pr-05mcp-原生圖片與-runtime-owned-結果)。PR-06 已完成既有 results CLI 的原圖查詢／安全匯出與 report／JSONL／TUI 投影，最終 gates 與實際 CLI smoke 見[PR-06 紀錄](native-image-runtime-progress.md#pr-06圖片成果查詢安全匯出與介面)；操作方式見[原生圖片指南](../reference/native-images.md)。M3 已交付，M4 仍待 PR-07。

### 8.3 Codex App Server

依[官方 App Server 文件](https://developers.openai.com/codex/app-server/)的 Turns／Message schema：輸入 union 包含 `text`、`image`、`localImage`；schema 可由對應版本 CLI 的 `app-server generate-json-schema` 離線產生。本計畫選定 **`localImage` + 受控私有 projection**，不使用任意遠端 URL 或 data URI variant。

- 2026-11-04 已以本機 **codex-cli 0.166.0** 成功離線產生 schema，`v2/TurnStartParams.json` SHA-256 為 `49e53282f327c25e7b39d97afacd038ef10ad5c4fb0431608b354d3a7bf6bcea`。其中 `definitions.UserInput` 經 sorted-key compact JSON 編碼的 SHA-256 為 `b082a38701b9ec3eb1ac56572e9d5d55c9016c1428ff6ddff647e24c5b9e139b`；localImage 必填 `type`/`path`、可選 `detail`。此證據僅確認 schema，未執行真實 inference 或宣告完整 app-server lifecycle 相容。
- M4 圖片版本支援先 pin **0.166.0**；PR-00 以相同公開版本離線重現並保存最小 self-contained fixture（包含所需 definitions），記錄 CLI version、來源和 schema hash。現有 text-only protocol 的 0.153.4 證據保留，不自動賦予圖片能力；不要求使用者安裝、登入或提供模型帳戶。
- adapter 用明確 union：text 含 text 欄位，localImage 含 path 欄位；fake server 依 pinned schema 驗證 `turn/start.input`。未驗證 runtime version 預設不支援圖片；版本判斷使用可靠的 binary/version identity，不解析 `userAgent` 猜能力，不因版本較新就自動放行。
- 只投影本 task 已授權 bytes，受控路徑採 logical ID、不含 source path；private directory／readonly image file 不授予整個 CAS 讀權。digest 與 frozen binding 一致才送 path；projection 複本及數量計入資源預算。
- projection 位於 runtime 管理的 control root，供該 thread/attempt 生命週期使用；外部程序退出後才能回收無後續使用的副本。resume 先驗證既有 projection 或按相同已綁定 logical path 由 CAS 重建，不能偷換 source；不能維持路徑／sandbox 契約時拒絕 resume。
- effective cwd/model/sandbox 繼續驗證；這不擴張第 4.3 節的 OS 隔離保證。result repair 不重新送圖分析；interrupt／timeout 先 drain child，再釋放 projection 與 quota。
- SDK／app-server 自己的私有持久化屬外部 backend 的 trust boundary；Hufu 日誌／receipt 只保存 refs。禁止為診斷 dump 含圖 RPC payload。

### 8.4 授權列出與匯出

工具生成圖片不新增生成模型 API，直接沿同一 publisher 入庫。先檢查現有成果命令，將 `list/show/export` 接到既有 artifact/result CLI；確無同義入口才新增通用 `hufu artifacts`，不建立 team-specific launcher。

list/show 只顯示已授權安全 metadata。export 先驗證 scope/hash，寫出原始 bytes 到明示目的地，以安全建立拒絕 symlink／既存檔案覆蓋；不把 CAS 原始路徑暴露為下載權限。report/JSONL/TUI 只引用 opaque ID 與匯出操作說明，本版不做 thumbnail／inline image。

## 9. Durable events、replay 與獨立 audit

擴充既有 event catalog／reducer，不繞過 canonical transition。媒體事件至少包含下列 typed payload，所有 payload 不含 bytes、data URI、EXIF 或 source path：

| 事件 | 最低欄位 |
|---|---|
| `media_input_admitted` | version、run/branch/invocation/turn、ordered parts、MediaRefs、snapshot/policy hash |
| `task_media_bound` | task/occurrence revision、source occurrences、ordered selection、purpose、binding hash |
| `media_provider_admitted` | task/attempt/lease identity、resolved target identity hash、media digest、estimate source、admission decision |
| `tool_media_published` | task/attempt、tool call/descriptor hash、ordered result index、refs、error status |
| `result_media_attached` | canonical result identity、refs、binding digest、receipt relation |

- Put／驗證 → snapshot event 與 canonical projection commit → provider admission event commit → transport；任一步寫入失敗都不得送出。image result 亦先 canonical publish，才宣告可用。
- idempotency keys 包含 branch 與 occurrence/turn；同一 turn 的重試重用 frozen snapshot，新 chat turn 即使 bytes 相同仍有獨立 identity。相同 key 搭配不同 payload 拒絕。
- 使用 canonical task transition；transition commit 後才開 attempt lease。media grant 綁正確 revision，不能因 commit 立即失效，也不能允許舊 lease 存取。
- resume/fallback 重用同一 ordered bundle，重新驗證 bytes 與當前 grant；不重新抓原始 path，不靠新模型結果重建歷史 binding。
- branch fork/checkout 依既有 session lineage 重放 refs 與授權來源；只有繼承的 ancestor occurrence 可重新授權，sibling／無關 run／team 不自動可讀。跨 branch rebind 是新授權事件，不修改原紀錄。
- `audit verify` 獨立從 events、canonical checkpoints、CAS 和 receipts 驗證順序／hash／scope／provider admission 一致性，不能只相信 session cache 或宣稱成功的文字。
- replay 保留完整 canonical receipt 與診斷；projection/redaction 不二次改寫已 hash outputs。加入 offline recovery fixtures，驗證舊文字 session 可讀、media policy drift 被一般 resume preflight 攔住。

## 10. Coding agent PR 工作清單

每個 PR 先確認 impact map 與依賴；測試可在開發中先 red，但提交及 merge 的 required gates 必須 green。每個 PR 都加實際行為測試，更新受影響正式文件與 migration note；不提交永久 failing tests、ticket 命名 characterization tests 或無 consumer 的 scaffold。

### PR-00 — SDK／schema fixture 與回歸基線

- 核對第 1 節 symbols、pinned dependency，以及全部入口／旁路（normal/direct/single/DAG/fanout/sidecar/action/fast-path/unattended/dry-run/recovery）。
- 用現有 fixture 證明 Fantasy Messages/Prompt precedence、原生 FilePart、已存在的 tool media conversion；測試 HTTP wire，不升級 SDK、不呼叫模型。
- 重現第 8.3 節 Codex 0.166.0 schema/version fixture；text-only 支援證據與圖片支援分開。當前尚無圖片 admission 的負向測試隨 PR-01 的 gate 交付，不能在 PR-00 提交必然失敗的未實作功能測試。
- **Exit:** 全部新增測試可獨立通過；無新公開圖片功能、無未定案的人工作業；後續 PR 可直接依本文件契約實作。

### PR-01 — M0 私有 media、canonical binding、policy、配額

- 實作第 2/4/7 節型別、validation、private namespace、metadata collision 規則、完整 decode、全域 limiter 和資源釋放。
- 接入 Todo/task projection、event/reducer、policy snapshot、lease assertion；scope/egress/格式/資源 gate 先存在。
- 停用含圖 task 自動快取、歷史 reuse 與 inflight coalescing，保存 MediaInputDigest；文字任務不變。
- **Exit:** repeated bytes/parts、多 occurrence owner、跨 task 拒絕、quota/取消、Put 後 crash、舊 artifact layout／policy hash 測試通過；實際圖片 transport 仍拒絕。

PR-01 分兩個可獨立驗證的子階段提交，保留上述整體 exit：

- **PR-01A：私有 storage 與前置拒絕。** 完成 shared ArtifactStore 的 media namespace、完整圖片驗證、process-wide decode／raw／encoding reservations，以及 universal provider／文字 artifact opener 的圖片拒絕。此時只有內部 storage API，尚無公開圖片 ingress、task grant 或模型傳輸。
- **PR-01B：canonical admission。** 完成 Todo／task projection、event/reducer、frozen policy、attempt lease、branch lineage 配額與 cache／historical reuse／inflight 隔離。PR-01B 通過整體 exit 後才標記 PR-01 完成；PR-02 的 history／recovery／audit 仍是 M0 的必要交付。

### PR-02 — M0 引用式 history／compaction／recovery／audit

- 實作第 5/9 節 codec、hydration 邊界、checkpoint schema、canonical digest、舊格式相容與獨立 audit。
- 覆蓋所有 session／history／compaction／debug/report/observer 保存路徑；任何無法識別的 raw media 不落地。
- **Exit:** bytes-free persisted documents 與事件、缺圖／竄改、branch lineage、policy drift、一般 resume/result-only repair 全部離線可驗證；private bytes 不進模型日誌或 LTM。

### PR-03 — M1 CLI／direct／single-task 原生送圖

- `--image` flags/options/profile、固定順序 builder、source ingestion、dry-run、明示 team/direct worker 與單非空執行 segment 限制、`#binary` 安全提示。
- 接入 hufu-local message compiler、三層能力、provider-bound token/cost/egress admission；既有 text API 保留 wrapper。
- 為 Direct Agent／單 task 建立最小完整圖片路徑；CLI Coordinator 入口尚未交付的組合明確拒絕，不能宣稱 M2 可用。
- **Exit:** CLI → canonical task → private CAS → 真實 HTTP fake endpoint → receipt/audit E2E；兩圖／重複圖／retry／新 process resume、不讀 source path、provider call 次數符合預期；任何前置拒絕為零該回合 transport。

### PR-04 — M2 Coordinator／Chat／DAG／fanout／fallback

- 實作有序 `image_inputs` delegation、metadata-only Coordinator、plain/TUI 共用 pending state。
- 將相同 binding 沿 Direct synthesis、DAG、extra-model/fanout、route fallback、repair／compaction 傳遞；auxiliary role 未獲圖片 grant 就維持文字結果分析。
- fanout 分兩個可驗收階段：D2A 交付各 frozen slot 的雙 lease、獨立 native admission/audit 與 accepted result 保存；D2B 以實際 SDK／新程序補驗 leaf retry、執行中的 tool-result repair、工作前 resume，以及 blocked collector 不重播工作／不追加 grant 的拒絕邊界，不以一般文字 fanout 測試替代。多候選 route 與 fanout 的組合不在此階段開啟，須在 provider 前拒絕。
- **Exit:** 第 11 節 M2 case 通過；不同圖片不能 cache hit 或合併；所有前端與 JSONL/report/inspect/TUI 顯示真實狀態；未授權 refs 不洩漏。

### PR-05 — M3 MCP 原生 mixed-content 與 runtime-owned 結果

- 新增 typed MCP result，保留 authorizer／descriptor、ordered blocks、structuredContent 和 IsError；接共用 publisher／quota／下一回合 compiler。
- 單張／混合／多圖片共用 reference-only carrier 與既有 compiler；完成第 8.2 節 producer／consumer 授權、計數及 failure latch，輸出圖片走 canonical runtime-owned binding、receipt 與 replay。
- **Exit:** fake MCP → 後續 fake VLM wire E2E；多 tool calls 的 pairing／順序、工具錯誤、invalid base64/MIME、超限、重送與 rejected result repair 通過；text-only wrapper 不回歸。

### PR-06 — M3 圖片成果查詢／安全匯出與介面文件

- 擴充第 8.4 節既有 CLI、report/JSONL/inspect/TUI；新增無網路的 export smoke 與 bounded bench。
- 補功能說明、支援矩陣、workspace/格式 migration/recovery 指南；不列 deployment/canary/人工 review 為 agent 待辦。
- **Exit:** list/show/export ACL、原始 bytes hash、拒絕覆寫／symlink、舊 session／文字結果投影測試通過；全部範例標明哪些語法已實作。

### PR-07 — M4 Codex native localImage adapter

- 依 pinned schema 實作 union、版本 capability gate、受控私有 projection、resume 與安全回收。
- fake app-server 驗證實際 RPC、effective world、interrupt/timeout、未知版本、projection 遺失／竄改與 result repair；不需要登入或付費模型。
- **Exit:** native localImage items 與 hash/順序證據正確；舊 text-only Codex suite 通過；unsupported version 在圖片 turn 前拒絕，不退文字。

**合併依賴：** `00 → 01 → 02 → 03 → 04 → 05 → 06`；`07` 依賴 `00/02/04`。安全、資源與 audit 隨各前置 PR 交付，不存在「先送圖、之後再補安全」階段。schema／結果契約改動均執行 offline contracts gate。

## 11. 離線測試矩陣與命令

| Milestone／類別 | 必測 case | 通過條件 |
|---|---|---|
| M0 ingress | PNG/JPEG/WebP、偽裝副檔名、空／截斷、APNG/animated WebP、過大、overflow、symlink/FIFO/device | 正確 MIME/digest 或穩定拒絕；壞檔不阻塞、不 transport |
| M0 store | 重複 bytes、多 owner、metadata 衝突、unsafe namespace、Put 後 event 失敗 | 授權不取自 first writer、canonical metadata 一致、無 bytes event |
| M0 persistence | history/checkpoint/replacement/observer/debug/LTM、未知 raw media | refs 可重放；媒體 bytes/Base64/data URI 不出現在持久化投影 |
| M0 cache | 同 prompt 換圖／換次序／重複次數、text cache、semantic/history cache、inflight | 含圖 bypass 全部自動 reuse；不同 occurrence 不合併 |
| M0 durability | `--no-journal`、store unavailable、`--temp`、branch fork/checkout、policy drift | 實際 durable stores 決定是否可用；不由旗標假定無事件儲存 |
| M0 resource | per-bundle／branch／concurrent load、decode limiter、取消／故障 | 先預留、超限拒絕、所有 exit 釋放、resume/fork 計數不重置 |
| M1 wire | CLI/direct/single → fake HTTP、兩圖／同圖兩次 | 原生 image parts 的解碼 SHA、role/order/count 正確 |
| M1 admission | model/backend/adapter no/unknown、無 target grant、estimate 缺失／超額、redirect | 該回合零 transport，錯誤 code 可分類 |
| M1 resume/audit | source 消失、CAS data/meta 被改、event/receipt/hash 不一致 | 完整 CAS 可恢復；缺失／竄改拒絕，獨立 audit 找出問題 |
| M2 chat | add/list/remove/clear、驗證失敗、commit 前後取消、reset／切 team／crash | pending 消費時點一致；不重送已提交 turn、不承諾未提交恢復 |
| M2 routing | metadata Coordinator → vision worker、vision→vision fallback、vision→text/unknown | 僅授權 target 收到 bytes，bundle 不變；不支援 candidate 不 transport |
| M2 lifecycle | DAG/fanout/auxiliary/compaction/result repair | scope/source ranges/tool pairing 保留，repair 不重跑分析或副作用 |
| M3 MCP | text+image、多圖、多 tool calls、structured+image、IsError、bad base64/MIME | 下一回合原生帶圖一次；順序、來源、工具錯誤語意不變 |
| M3 result | local/external submit、model override、invalid source、rejected repair/replay | runtime-owned refs 正確，完整 receipt 不被改寫，壞來源拒絕 |
| M3 export | 跨 task/ref guessing、目的地 symlink／已存在、原圖匯出 | Hufu API 授權與 hash 正確；不冒稱同 UID 程序隔離 |
| M4 Codex | schema union/version、localImage path、world mismatch、frame/timeout/interrupt/resume | fake RPC 實收圖片；未知版本／不一致在 turn 前拒絕；先 drain 再回收 |
| 全部相容 | 文字 CLI/chat/MCP/Codex、舊 policy/session/event fixtures | 舊對外語意、hash/receipt/replay 不回歸 |

使用 deterministic 自製小圖與無私人資訊的 EXIF JPEG；格式異常 fixture 可保存有來源的最小 bytes。APNG/animated WebP 必須有 fixture；不需要 AI 生成素材或第三方授權圖片。HTTP/MCP/RPC captures 限於測試記憶體，不寫含 Base64 的 test log。

每個 code PR 執行相關 targeted tests；依變更完成下列 repository gates，不能以 fake wire 測試取代完整回歸：

```bash
mise run test
mise run test-contracts
mise run vet
mise run lint
# 每個 PR 依實際改動選相關 packages，透過 mise：
mise exec -- go test -p 2 -race ./internal/team ./internal/mcp ./internal/agent ./cmd/hufu
```

含新的 `internal/media` package 時加入其 targeted/race checks；若 repo integration gate 另有要求一併執行。上述 gates 在單一 validation owner 下依序進行，不同時啟動 heavyweight commands／live team。

沿用 AGENTS.md：mise pinned toolchain、GOMAXPROCS=2／`-p 2`；一個 private task TMPDIR，保留 GOCACHE/GOTMPDIR；驗證前後記錄 cache/scratch、磁碟與可用 RAM；測試用 `t.TempDir()`，Unix socket 使用既有短路徑 fixture；結束先 drain owned processes 再清 disposable outputs。bench 使用小型合成圖，分開測單張與兩 worker，觀察 RSS/allocation/wire bytes，不同時保存多份大型輸出。

錯誤至少有穩定 machine-readable code 與 task/attempt/media opaque identity，並映射既有 failure/recovery 分類：

`IMAGE_INPUT_UNSUPPORTED`、`IMAGE_CAPABILITY_UNKNOWN`、`MEDIA_UNSUPPORTED_FORMAT`、`MEDIA_LIMIT_EXCEEDED`、`MEDIA_DECODE_INVALID`、`IMAGE_INTEGRITY_MISMATCH`、`IMAGE_INPUT_MISSING`、`MEDIA_ACCESS_DENIED`、`MEDIA_EGRESS_DENIED`、`MEDIA_BINDING_CONFLICT`、`MEDIA_DURABILITY_REQUIRED`、`IMAGE_CONTEXT_CANNOT_FIT`、`MCP_MEDIA_CONTENT_INVALID`。

「前置拒絕零 provider calls」只適用尚未開始的那一回合；工具已完成後的 media 拒絕不能改寫先前 calls 或 usage，也不能自動重跑工具。

## 12. 完成與交接

各 milestone 完成須提供：changed files、支援矩陣、canonical owner／projection impact、migration 影響、實際驗證命令與結果、未執行項目。標示 fake-provider protocol 證據與真實模型驗收的界線，不宣稱看圖準確率或尚未執行的服務驗收成功。

必要的 completion evidence：

- 支援入口有 native wire E2E，尚未支援入口有 pre-provider rejection；沒有文字降級。
- canonical input、scope、policy、digest、receipt、cache bypass 和 replay/audit 一致。
- Hufu 持久化／日誌只含 refs，private bytes 與資源上限通過負向測試；不誇大 OS/backend 隔離能力。
- resume／branch／repair 不換圖、不擴權、不重跑已完成副作用；舊文字 fixtures 與 required gates 通過。
- 所有實作待辦均可在 repository 與離線 fixtures 完成；外部模型驗收另案，沒有未回答的產品設計選項或人工 approval gate 藏在 PR exit。

如果程式碼證據顯示某個 adapter 無法履行上述契約，該 adapter 保持明確拒絕，且不得將所屬 milestone 標示完成；不用忽略圖片、猜 protocol、刪除 findings 或更改歷史 receipt 來滿足測試。修正造成衝突的具體 coding contract 後再繼續該 adapter；其他已具備條件的 milestone 可獨立完成。
