# 原生圖片輸入與 MCP 圖片成果

> Status: active
> Authority: reference
> Verified-Commit: `171381da`（MCP runtime 基準；成果查詢與匯出隨本次階段提交）
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
| Codex App Server | 圖片 adapter 尚待後續階段；目前拒絕圖片，不降為純文字 |

支援靜態 PNG、JPEG、WebP，檔案內容與 MIME 必須一致。其他 binary content、
未授權目標或不明圖片能力會被拒絕。離線 fake-provider 測試證明封包與 runtime
契約；模型的實際視覺判斷由另行安排的真實驗收確認。

## 政策與限額

`hufu.yaml` 的 `media.enabled` 必須開啟，`allowed-targets` 明列 backend、
provider-key 與 model。team 可以縮小 operator 政策，不能擴張它。
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
