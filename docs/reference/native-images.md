# 原生圖片輸入與 MCP 圖片成果

> Status: active
> Authority: reference
> Verified-Commit: `70bf3d16`（projection 並行程式與離線驗證基準；M3／M4 既有基準為 `2aea4d23`，不代表 Codex 真實推論已驗收）
> Supersedes: —
> Superseded-By: —

Hufu 以私有 CAS 保存圖片內容，事件、對話與結果保存有來源的引用。
每個含圖請求都須符合凍結的 media policy、模型能力、目前 task lease、
context 與資源限額。歷史 ref 或已知 artifact ID 本身不授予送圖權限。

## 支援範圍

| 入口 | 行為 |
|---|---|
| CLI／direct worker | `--image` 依旗標順序附加；重複圖片保留 occurrence |
| coordinator／Chat | coordinator 使用 metadata；明示授權的 worker 才讀取與傳送圖片 |
| DAG／route／LLM fanout | 每個 occurrence／candidate／leaf 分別驗證來源、目標與租約 |
| MCP direct／dynamic gateway | text／image blocks 保留順序；下一模型回合以原生圖片送出 |
| MCP native action | 完整 Outputs 先驗證，再發布圖片與收據綁定的 media projection |
| `results images` | 列出已選歷史 task 結果的工具圖片；可匯出原始 bytes |
| Codex App Server | 暫不支援（Unsupported）。見下節的實作紀錄 |

支援靜態 PNG、JPEG、WebP，檔案內容與 MIME 必須一致。其他 binary content、
未授權目標或不明圖片能力會被拒絕。離線 fake-provider 測試證明封包與 runtime
契約；模型的實際視覺判斷由另行安排的真實驗收確認。

## 政策與限額

`hufu.yaml` 的 `media.enabled` 必須開啟，`allowed-targets` 明列 backend、
provider-key 與 model；Codex agent target 的 provider-key 為空。team 可以縮小 operator 政策，不能擴張它。
`image-token-upper-bound` 是每張圖片的保守 context 上界；每次出現都計數，
不能因 CAS 去重而少算。這是 admission 估計，不是模型回報的精確使用量。

| 項目 | 硬上限 |
|---|---|
| 每次請求圖片 occurrences | 8 |
| 每張 bytes／整個 bundle bytes | 16 MiB／32 MiB |
| 每張像素／單邊長度 | 36,000,000／8192 |
| 分支唯一圖片 bytes／blobs | 128 MiB／64 |

設定只能收窄硬上限；零值選擇安全預設。MCP Base64 先檢查可解碼大小，
再在保留額度內解碼及驗證，不把工具自稱的 owner、digest、path 當成權限。
工具已完成但發布或下一請求遭拒絕時，attempt 失敗；格式修復不重播工具。

## 查詢與匯出

先從相同 workspace／team 取得 task 的 result ID：

```bash
hufu results list --workspace ./workspace --team example
```

將 `RESULT_ID` 設為上一步的完整 `res-...`，再列出圖片 metadata：

```bash
hufu results images "$RESULT_ID" --workspace ./workspace --team example
hufu results images "$RESULT_ID" --workspace ./workspace --team example --output json
```

輸出包含 opaque image ID、格式、尺寸、bytes、occurrence 數量與來源。
將 `IMAGE_ID` 設為其中一個 ID，即可查詢單張或匯出：

```bash
hufu results images "$RESULT_ID" --workspace ./workspace --team example --artifact "$IMAGE_ID"
hufu results images "$RESULT_ID" --workspace ./workspace --team example --artifact "$IMAGE_ID" --export ./screenshot.png
```

匯出驗證選定分支、run、task、原始終止事件／hash、producer 與 CAS 內容，
寫出原始 bytes。目的地必須是新檔案，父目錄須已存在；拒絕既存檔案、
目的地 symlink 與父路徑 symlink。猜到其他圖片的 CAS ID 也不能匯出。
查詢與匯出不呼叫模型或重新執行 MCP。

報告、JSONL 與 TUI 只顯示圖片 metadata／opaque ID 與查詢指令，不內嵌圖片。
blob 的首位發布者不決定 task ownership；相同圖片可以有不同且各自驗證的
producer。這是 Hufu API 與儲存路徑的邊界，不宣稱隔離同 UID 的任意程序。

## Native action 與結果契約

MCP `structuredContent` 存在時，Outputs 保持該完整 object；沒有 structured
內容時，文字依序換行連接至 `result`，image-only 的 Outputs 是空 object。
圖片另存於 runtime-owned media projection 與 Artifacts，不捏造文字滿足 schema。

`x-hufu-runtime-inputs` 可用 `source_kind: "runtime_media"` 注入 native source
的完整媒體結果。模型省略該欄位；local／external submission 都拒絕覆寫。
注入 metadata 不授予其他 worker 讀取或轉送圖片的權限。詳細語法見
[native 結果欄位](agent-format.md#收據驗證的-native-結果欄位)。

## 格式版本與恢復

- 既有 static media history 保持 v1，static media compaction 保持 v2。
- 出現 tool-media carrier 時，history 使用 v2、compaction 使用 v3。
  舊 reader 會明確拒絕新格式，不能悄悄遺失圖片。
- runtime-owned media projection／receipt hash 是 optional 欄位；舊文字結果
  不會被推測成有圖片。舊 immutable report 不回填新的圖片指令；仍可用
  `results images` 驗證其既存 canonical 圖片來源。
- resume 從可見的 canonical lineage 與 CAS 核對原來源；不以較新的工具結果
  重建歷史收據。新 attempt／task 仍須取得自己的授權，歷史 carrier 不等於 lease。
- 文字 result repair 移除圖片 carrier，保留文字、錯誤與 call/result pairing。
  收據與 ordered media projection 不因修補而改寫成模型手抄的值。

交付狀態與離線驗證記錄見[實作進度](../implementation-plans/native-image-runtime-progress.md)。


## Codex App Server 圖片接入

Hufu 的 Codex App Server 整合暫不支援。開發、支援及專屬測試均已暫停。下列內容記錄既有實作。

此階段的 adapter 使用 pinned `codex-cli 0.166.0` schema 的 `text`／`localImage` union。
圖片工作會執行受限的 `--version` probe，核對實際 executable 的 SHA-256，
不依 `userAgent` 推測能力。較新、較舊或未驗證版本都在圖片 turn 前拒絕；
既有純文字 Codex 流程不增加這個版本要求。

operator policy 必須明列 Codex backend 與 exact model，`provider-key` 為空，
並提供正數的 `image-token-upper-bound`。context 容量必須明示：CLI 註冊的
operator capacity 優先，否則採 agent／team 的 `context-window`；無容量或
無法容納文字、每次圖片、輸出預留與 safety margin 時拒絕。token 計量包含
已知的 input JSON、受控路徑、developer instructions 與結果 schema，屬保守估計。

開發期間的 CLI 範例使用已設定 Codex worker 的 `example` team。

```bash
hufu --agent-team example --context-window "$CONTEXT_WINDOW" --image ./photo.png '@worker 分析這張圖片'
```

`CONTEXT_WINDOW` 應先設定為該 backend 的明示容量；這個例子不安裝 CLI、登入
或呼叫真實服務。backend 的模型設定、帳戶、endpoint 與私有持久化由已授權的
外部 app-server 管理；Hufu 核對其 effective model／cwd／sandbox 後才送出圖片 turn。

圖片只從本 task 的 canonical CAS 經當前 journal lease 投影到 control root 的
私有目錄，檔名使用 logical content ID，檔案唯讀。Hufu 不把整個 CAS 暴露為
native input，也不宣稱提供同 UID 的 OS 隔離。投影在子程序及 repair 程序排空
之後回收；副本與每次 occurrence 的資源預留都計量。

殘留 projection 也計入總容量與檔案數，上限沿 media policy 的 branch bytes／
blobs 限制。這個限制包含相同圖片在不同工作下的副本，不因 CAS 去重而忽略
崩潰殘留。同一 control root 的配額計量、發布與回收由 workspace projection
lock 協調；使用中的副本則由各 consumer 持有的 kernel lease 保護。

Workspace projection lock 只涵蓋配額檢查、完整 bundle 寫入／驗證與發布，以及
最後回收；不跨越 app-server lifetime。每個 consumer 在 projection 目錄建立
獨立、空白、0600 的 kernel lock marker（每份 projection 最多 1024 個），持有至
其子程序及 repair 排空。相同 binding 可共用已驗證的唯讀副本，最後一個
consumer 離開才刪除。不同 binding 的副本各自計入 branch bytes／blobs；額度不足時以
`limit_exceeded` 拒絕該次 admission，不自動等待配額或增加重試；
不同 `CODEX_HOME` 可在此邊界並行，同一 home 的 app-server state lock 仍序列化。

所有 marker 建立、過期判定與刪除都在 workspace lock 內進行，以 kernel lock
判斷存活，不靠 PID 或 timeout 猜測。程序崩潰留下的圖片仍計入 retained-copy
配額；同 binding 重新使用時清理已解鎖 marker 並驗證原副本。清理最多等待
workspace lock 5 秒；失敗則保留副本與 marker、釋放自己的 kernel lease，後續
仍按實際副本計量。這不是刪除任意孤立或損壞圖片的機制。

resume 保持原 binding／thread 與 logical paths：遺失副本從原 CAS 重建，
內容或權限竄改、既有 admission 的 binary identity 改變、world 不符時拒絕。原始使用者檔案不需存在。

結果格式修補只送文字，不重送圖片或要求新分析。timeout／取消先 drain child
才釋放副本與預留。成本沿既有 agent backend 的 opaque admission；有硬預算但
缺少 bounded price 時在 provider 前拒絕，未知 usage 保留上限，不當作零成本。
本 adapter 支援 canonical 圖片輸入；不增加 Codex 生成圖片 API 或 Hufu MCP 工具。
