# Ollama 原生圖片真實模型驗收（2026-11-06）

> Status: active
> Authority: reference
> Verified-Commit: `403d19050fe21184451645ede2163a43f01817a4`
> Supersedes: —
> Superseded-By: —

使用者明確指定 `Ollama deepseek-v4.1-flash:cloud` 並要求執行一次真實模型驗收。
結果 **PASS**：CLI native image → canonical worker → 本機 Ollama → cloud model →
結構化結果保存 → 公開結果查詢 → 確定性 acceptance 全部完成。
這是兩張合成圖片的單次 smoke acceptance，不代表一般視覺準確率、MCP 真實服務或 Codex 真實推論已驗收。

- 原始碼：`403d19050fe21184451645ede2163a43f01817a4`；以 mise Go 1.27.2 由此 HEAD 建置 Hufu，沒有修改程式碼。
- 執行時間：2026-11-06 14:31:11.019709～14:31:16.426894 UTC；整體約 5.41 秒。
- Ollama endpoint：`http://127.0.0.1:11434`；使用 OpenAI-compatible `/v1/chat/completions`。
  前置透明 relay 轉送原始請求與回應，只記錄去敏 metadata，不產生或替換模型答案。
- 模型：`deepseek-v4.1-flash:cloud`；實際 HTTP 請求名稱相同。
  `/api/show` 回報 `vision`、`tools`、`thinking`、`completion`，context capacity 1,048,576。
- Invocation：`inv-20261106T143111.143659714Z-cc7391ec5e16`。
- Run：`run-20261106T143111.272879080Z-4e693433b7d6`；team `live-vision`、branch `main`、task `1`。
- Result：`res-5b008cbdfad3cfafdcffc2a60e40d60b531c3847af183a7e442df9ef354b5d91`。

## 固定測資與判定

兩張自製 640×480 PNG，依 **A、B、A** 送出。四位數在呼叫前隨機產生，
預期答案放在 worker 專案以外；prompt、schema 均不包含答案，worker 沒有檔案讀取工具。
Schema 固定三筆 observations；acceptance 以公開 `results show --payload` 的完整 JSON 和預期值逐項比對，
並核對透明 relay 記錄的 exact model、HTTP 200、單次視覺請求與三筆圖片 SHA／順序。

| 輸入順序 | 數字 | 紅方塊 | 藍圓形 | 三角形顏色 | 模型結果 |
|---|---|---|---|---|---|
| A | 1593 | 2 | 3 | green | 全部正確 |
| B | 2812 | 4 | 1 | yellow | 全部正確 |
| A（重複） | 1593 | 2 | 3 | green | 全部正確，保留第三筆 occurrence |

- A SHA-256：`1149d26d6de674061aaf060c661ca97f22d894756603ea07b5ae826fa493da52`，4,732 bytes。
- B SHA-256：`80c3067db0c2b2248c2c646264fc8cdd45b2194a2327561e09fd84c1e78c6c69`，4,754 bytes。
- Worker structured payload SHA-256：`bb4ddf79896650dc18302f087aeed04089eb6736d92a00ed06a29cba717bf2db`。

## 實際呼叫與 runtime 證據

- 含圖片模型請求 **1 次**，A／B／A 原始 bytes 的 SHA 與順序吻合，HTTP 200，relay 觀測 1.814 秒。
- 純文字 synthesis **1 次**，同一指定模型、沒有圖片或工具，HTTP 200，2.173 秒。
- Worker attempt **1 次**、retry **0**、fallback **0**；沒有重跑圖片分析或更換模型。
- `status --json --workers --verbose`：task `done`、result validation `valid`。
  Worker usage 為 input 4,816、output 408、total 5,224 tokens；run 輸出記錄總 tokens 6,156。
  這是 provider/runtime 回報值；不視為圖片精確 token 成本或金額。
- `inspect run ... --format json`：`outcome=completed`、`goal_satisfied=true`、`acceptance=passed`、
  `completion=pass`、1 done／0 unresolved／0 failed；event chain `verified`、projection `consistent`。
- `results show`：canonical task result `validity=accepted`、structured payload 存在，
  從公開 `--payload` 匯出後與固定答案完全相同，外部 deterministic checker `passed=true`。
- 使用 exploratory goal，run acceptance command 為 advisory；本次 command 實際通過。
  Task 本身沒有另設 per-task verify，所以 task 的 `no_objective_verifier=true` 仍如實保留。
  通過結論同時要求 runtime 終態、schema 與獨立答案比對，未將模型自述或 exit 0 單獨當作驗收。

呼叫前 team static validation 和 `run --dry-run` 均通過。
第一個啟動因驗收器設定 `max-total-tokens=24000`，小於圖片保守 admission 預留量 106,936，
在本機被拒絕；該次只有 profile 查詢，**0 次 inference**。保留該診斷，
修正驗收器至 180,000 並在全新 workspace 執行上述單次真實推論；不是重送已執行的視覺請求。
本次採每張 image-token-upper-bound 32,768、operator context 262,144、max output 4,096、
worker timeout 180 秒、run duration 300 秒。這些是隔離驗收設定，不是產品模型預設或計費估算。

## 證據與保留範圍

[證據目錄](native-image-live-acceptance-2026-11-06/) 保存原始自製 PNG、固定預期值、
公開 CLI JSON、去敏 provider metadata、wire 圖片雜湊、acceptance 比對結果及實際 fixture 設定。
`SHA256SUMS` 可核對檔案內容；未保存憑證、Authorization、Base64 封包或 runtime.sqlite。
所有 diagnosis 使用相同版本 Hufu 的 `status`、`inspect`、`results` 公開介面，沒有直接查資料庫。

該目錄中的 `/tmp/codex-task.H5DN4t` 路徑及 relay port 是歷史執行位置；
驗收結束後刪除 owned binary、workspace 與暫存程序，不能再用該路徑查詢。
既有文件位置不變。本次沒有重新執行離線全套測試：Go source／設定未變，沿用該 HEAD 已通過的 gates。

資源收尾：已清除唯一 private task scratch `/tmp/codex-task.H5DN4t`，
回收 136,757,248 bytes（約 130.4 MiB），包含本次建置的 CLI binary 與兩次本機
workspace；owned CLI／relay 程序皆已退出。保留的去敏驗收附件約 53 KiB。
共享 GOCACHE 由 24,658,530,304 變為 24,684,814,336 bytes，觀測淨增
26,284,032 bytes（約 25.1 MiB）；這是共享總量，未依時間戳推定全部為本次產物。
GOTMPDIR 保留既有 16,400,384 bytes；未刪共享依賴或變更快取路徑。
