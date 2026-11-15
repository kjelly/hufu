# 歷史任務結果查詢驗證記錄

> Status: active
> Authority: reference
> Verified-Commit: —（原始功能驗證為 2026-10-14 工作樹；2026-11-15 完整本機性能量測另列 source／binary identity）
> Supersedes: —
> Superseded-By: —

本次交付新增 [results CLI](../guides/historical-results.md)。驗證使用 Go 1.27.2、Linux amd64、temporary workspaces 與本地 fixture providers。fixtures 測試讀取、身份、scope、schema、錯誤和 provider transport 的契約；不代表實際模型的語意準確率。

> 最新性能狀態：已補齊 [2026-11-15 完整本機查詢基線](historical-results-performance-2026-11-15.md)，包含 1K／10K／100K 各 20 組冷／熱配對、SQLite 私有快照、全鏈驗證、projection、實際 Cobra query、記憶體與來源 byte preservation。以下 projection-only 數字保留為歷史紀錄，不能和新版不同 fixture／scope 的量測作直接前後比較。

## 覆蓋的行為

- [歷史 projection 測試](../../internal/inspect/results_test.go)：不同 run 重用 task-id、team 範圍、canonical result 讀取、tasks 清理、journal 清空、新 session root、retry invalidation、branch inheritance、read-only 與 hash-chain corruption 拒絕。
- [LLM 查詢測試](../../internal/resultquery/query_test.go)：Unicode／日期、schema 與 scope 驗證、未知來源引用、零候選、明確 recency fallback、取消／provider failure、bounded excerpts 與提示注入資料邊界。
- [CLI 測試](../../cmd/hufu/resultscmd_test.go)：list/search/latest/show/invocation 不要求 run-id、team 共用 flag、run responses、snapshot cursor、JSON 失敗輸出、preview、registry project/workspace 解析，以及即時 terminal／JSONL reference。
- [最終回覆測試](../../cmd/hufu/results_final_test.go)：final 使用既有 run_response，task locator 僅導向同一 workspace/team/session/run/branch；未完成 run 不挪用其他 run 的回覆，跨 branch 歧義須指定 branch。show 顯示 kind 和 final 查閱指令。
- [回覆列表測試](../../cmd/hufu/results_overview_test.go)：runs 只列 final responses，省略 worker 約束與 storage/index；預覽有界、空列表有說明、JSON 保存全文，分頁與閱讀指令保持 scope；明確 list 保持 worker 查詢。
- [完整回覆測試](../../cmd/hufu/results_read_test.go)：result alias 與無子指令的 stdout 輸出最近一次保存的完整正文，不附 metadata、preview 或其他 fallback；長篇 multiline、JSON 與空結果／worker-only／irrelevant filter 均有驗證。這是 per-team response body，不宣稱歷史記錄可還原未保存的 multi-team CLI 聚合文件。
- [呈現測試](../../cmd/hufu/results_presentation_test.go)：來源資訊分流至 stderr、quiet／JSON／payload-only 契約、大於 16 KiB payload、精確整數、scope／時間條件、縮排、severity／reason、終端控制清理與渲染預算。
- [呈現 PTY 測試](../../cmd/hufu/results_presentation_pty_test.go)：真實 binary 的 TTY 判斷、light／dark Markdown、無色彩與 epaper 降級，以及 stdout 管線原文；不用實際模型。
- [真實 binary 測試](../../cmd/hufu/results_process_test.go)：真實 Cobra/main/provider gateway 與三次 sidecar generation，檢查 provider request 沒有 tools 或其他 team sentinel，輸出是一份 JSON，query usage 與歷史 run 分離。
- [事件通知測試](../../internal/team/result_identity_test.go)：保存失敗不發布 result-ready；保存成功發布的 ID 與 terminal event 一致，branch inheritance 保持身份。
- [TUI 測試](../../internal/tui/result_identity_test.go)：result identity 的 L1 model purity、task status 保持，以及 L2 detail header/reference/width 顯示。引用亦透過 task logs 使用既有複製操作。本案沒有新增 terminal mode；原 PTY suite 仍作回歸。

- [報告來源呈現測試](../../.agent-teams/hufu-code-review/reviewgate/report_presentation_test.go)：主報告使用原始問題標題、位置與複核理由；完整身份資料保留於稽核附錄。涵蓋多來源重用 finding index、JSON 整數 `0.0`、來源內容不變、空結果與缺少對應欄位的拒絕。

## 程式檢查

| 命令 | 結果 |
| --- | --- |
| 新增與受影響的 targeted tests | 通過 |
| `go test -race ./cmd/hufu -run '^TestResultsPresentationBinaryTTYAndPipe$' -count=1`（同時設定 TMPDIR／GOTMPDIR） | 通過；含 TERM=dumb 與 16 色降級 |
| `go test ./.agent-teams/hufu-code-review/reviewgate -count=1`（同時設定 TMPDIR／GOTMPDIR） | 通過；含報告順序、原始證據與四種 Critic verdict |
| `hufu team validate --team hufu-code-review`、`hufu list hufu-code-review`、temporary workspace dry-run | 通過；dry-run 不呼叫模型、不執行任務 |
| 真實 binary 搭配本地 fixture provider | 通過，三次生成、三階段契約與 usage aggregation 正確 |
| `TMPDIR=/tmp GOTMPDIR=/tmp go test -race ./internal/inspect ./internal/resultquery ./internal/team ./internal/tui ./cmd/hufu -run 'Test(Historical\|Result\|ExplainCommand)' -count=1` | 通過；需要本地 fixture socket 權限 |
| `TMPDIR=/tmp GOTMPDIR=/tmp go vet ./...` | 通過 |
| `GOLANGCI_LINT_CACHE=/tmp/hufu-historical-lint-cache golangci-lint run` | 通過，0 issues |
| `bin/check-docs`、`git diff --check` | 通過 |
| `TMPDIR=/tmp GOTMPDIR=/tmp go test -p 1 -timeout 20m ./...` | 通過；完整 package suite，使用本地 fixture socket 權限 |

初次完整執行使用 cache temporary directory，遇到 chat E2E 的 deadline 與 SQLite fsync 導致的 package timeout。既有 [環境 baseline](context-delivery-baseline.md) 要求同時設定 TMPDIR 與 GOTMPDIR；換到 `/tmp` 後，chat E2E 單獨通過，沒有修改 test assertion 或產品流程。另一次 sandbox 執行拒絕 httptest loopback socket，因此使用工具權限流程重跑 local fixtures，不把這些案例改成 skip。

## 2026-11-15 配置優化

[同 fixture 的完整前後比較](historical-results-optimization-2026-11-15.md)已完成：120 次查詢全部成功，100K 累計配置減少約 21.5%、最大抽樣 RSS 減少約 13–14%，cold／warm p95 為 13.95／13.79 秒。10K cold p95 較基線增加 14.8%，保留全部樣本與限制。完整 test、vet、lint、定向 race 與離線契約 gates 通過；未修改 canonical sources、雜湊驗證或真實模型品質驗收狀態。

## Projection 性能量測

執行 `go test ./internal/inspect -run '^$' -bench BenchmarkHistoricalResultProjection -benchtime=1x -count=1`，單次 iteration 的 in-memory fixture 結果如下：

| terminal results | projection 時間 | 累計配置 bytes |
| --- | --- | --- |
| 1K | 212.7 ms | 18.2 MB |
| 10K | 2.111 s | 185.9 MB |
| 100K | 23.153 s | 1.882 GB |

這是 [projection benchmark](../../internal/inspect/results_bench_test.go)，不包含 SQLite 讀取、完整 chain validation、index build 或 LLM latency；bytes 是累計 allocations，不是 peak RSS。當時有其他 Go validation 工作並行，單次樣本不能用來建立 p95 或 production SLO。完整本機冷／熱量測已由上述新版基線補齊；查詢仍使用 event_replay，是否新增結果索引須另依瓶頸決定，不能把舊表當成索引已完成的證據。

## 實際模型品質界線

2026-10-14 原始驗證當時的 Ollama endpoint 未提供 service，沒有執行本案真實模型自然語言問題集。後續 context-query 的合成模型 A/B 與本次完整本機性能量測，均不代替至少 30 題、top-10 recall 90% 與 answer support 95% 的品質驗收。可宣稱 transport、scope、budget、schema、canonical result 契約與已記錄本機性能完成；不能宣稱本案實際語意準確率已達標。
