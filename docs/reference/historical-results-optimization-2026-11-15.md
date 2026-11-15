# 歷史結果查詢配置優化（2026-11-15）

> Status: active
> Authority: reference
> Verified-Commit: —（working tree；HEAD `0b4f4eac23c0bfa1d031ef3437ff59ee31512f27`，source／binary SHA-256 見 JSON）
> Supersedes: —
> Superseded-By: —

## 變更與界線

依據[完整基線與 allocation profile](historical-results-performance-2026-11-15.md)，調整既有 canonical read path 的中間配置：分支投影先計算精確容量；沒有 cycle boundary 的 task replay 直接使用既有 streaming reducer 的 first-wins 去重；歷史結果一次解碼並保存每個事件當時的 team／status metadata，僅為授權 team 的 terminal rows 預留空間；最終 snapshot 預留首批結果容量，減少逐 task events／grant 的額外配置。

仍由原有唯讀 SQLite snapshot、兩次全域雜湊鏈驗證、精確 lineage、canonical task reducer、receipt／來源檢查與獨立 redacted result projection 決定結果。沒有 schema、政策、receipt、canonical hash、query deadline 或索引變更，沒有啟動真實模型。本輪是本機查詢路徑的配置優化，不代表真實模型語意品質或正式 decision 校準已完成。

## 同一 fixture 與方法的比較

[機器可讀報告](historical-results-optimization-2026-11-15.json)保存 120 次新查詢的全部原始樣本、統計、版本與基線檔 hash。`cmd/hufu/results_performance_test.go` SHA-256 與基線完全相同；1K／10K／100K 使用相同 deterministic event payload、事件數、搜尋命中和 assertions，每規模各 20 組 process-cold／same-process-warm 配對。每組都完整驗證結果與來源，並確認來源 SQLite SHA-256 沒有改變。

兩輪依序在同一主機執行，沒有隨機交錯或 OS cache eviction；fixture 位於 tmpfs。三段生成維持 scripted，不能推論真實 provider latency、token 或費用。延遲包含當時主機與 GC 變異，不能把所有差異歸因於一項配置修改。累計配置不是峰值記憶體。

| Results | Cold p50 / p95：基線 → 本輪（秒） | Warm p50 / p95：基線 → 本輪（秒） | Cold 累計配置中位數：基線 → 本輪（MiB） | 最大抽樣 RSS，cold / warm：基線 → 本輪（MiB） |
|---:|---|---|---|---|
| 1,000 | 0.147 / 0.185 → 0.133 / 0.181 | 0.137 / 0.182 → 0.132 / 0.150 | 43.3 → 37.4 | 76.5 / 77.3 → 75.4 / 76.2 |
| 10,000 | 1.386 / 1.565 → 1.316 / 1.796 | 1.401 / 1.620 → 1.305 / 1.585 | 440.8 → 353.5 | 175.5 / 178.8 → 163.2 / 164.2 |
| 100,000 | 14.695 / 15.727 → 13.407 / 13.947 | 14.076 / 14.971 → 12.632 / 13.791 | 4,476.4 → 3,515.6 | 1,184.2 / 1,194.2 → 1,025.3 / 1,025.7 |

100K cold／warm 累計配置中位數均減少約 21.5%；最大抽樣 RSS 分別減少 13.4%／14.1%。cold／warm p50 分別降低 8.8%／10.3%，p95 降低 11.3%／7.9%。**10K cold p95 反而增加 14.8%**，因此不宣稱所有規模及分位數都改善，也不靠刪除較慢樣本或反覆重跑改變結論。

完整 load 仍占 100K 總耗時約 97%，沒有達成常數時間查詢，也沒有新增快取。後續索引仍是條件式設計，若另有 latency／規模目標，必須連同 canonical freshness、scope、完整性與 read-only 語意另行驗收。

## 驗證

- `TestHistoricalResultsTeamIdentityRemainsChronological`：晚到的 team 宣告不會擴大先前結果的 scope；拒絕同 run 的衝突身份；修改 caller projection 不污染後續 snapshot。
- `TestTodoReplayPreservesFirstIdentityAndCycleBoundary`：普通 replay 與 cycle 路徑保留 first-wins event ID／branch-scoped key、有效 cycle reset 與輸入 journal 不變。
- `TestBranchProjectionOwnsEventSlice`：保留 parent cutoff、child events 與獨立 slice storage。
- 全部既有 historical/source/scope/tamper 測試、實際 Cobra query fixture、完整 `mise run test` 通過；`internal/team` 837.453 秒，`cmd/hufu` 168.064 秒。
- `mise run vet`、`mise run lint`（0 issues）、三個受影響 package 的定向 `go test -race -p 2`、`mise run test-contracts` 全部通過，實際命令與 log hashes 見 JSON。
- 初次完整測試因沙箱拒絕 localhost TCP fixture 而失敗；保留原始紀錄，取得本機 socket 執行許可後以相同來源碼完成全部 gates。沒有跳過測試或呼叫真實模型。

這些證據涵蓋 source validation 與離線端到端查詢，沒有擴大宣稱為真實服務或自然語言品質驗收。

## 資源與保留證據

本輪 scratch 峰值約 366 MiB，低於 512 MiB 預算；benchmark fixture 與私有執行檔已清除。另以本任務新增 test body 及修正前 fixture constants 證明歸屬，清除一份 146 MiB 過期 archive，保留目前 normal／race archives；連同 136 MiB benchmark binary 共回收約 282 MiB，另清理自有測試資料。

共享 GOCACHE 淨增加約 0.92 GiB，包含共用與既有資料，不能全部算成本任務產物；沒有改動 configured caches／GOTMPDIR 或清除共享依賴。私有 checkpoint `/tmp/codex-task.w65mr1` 刻意保留小型 raw samples、gate logs、source hashes、統計與清理 manifests，供重現與交接；公開 JSON 已包含全部正式樣本。原有文件位置不變，沒有新增模型呼叫。
