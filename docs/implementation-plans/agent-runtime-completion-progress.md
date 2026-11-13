# Agent runtime 功能補齊實作紀錄

> Status: active
> Authority: reference
> Verified-Commit: `5bf54fd7`
> Supersedes: —
> Superseded-By: —

規格：[Agent runtime 功能補齊](agent-runtime-completion.md)。

## 基線

- 原始程式版本：`5bf54fd7a9e3beac08ae4c5c165ca24ce98dc5ff`。
- 文件晉升前 tracked working tree 乾淨；`docs/tmp/spec.md` 為未追蹤的 ignored 文件。
- 文件晉升 commit：`ba8fa0a9`；`bin/check-docs` 通過（106 active files、689 relative links）。
- sandbox 內 baseline `mise exec -- env GOMAXPROCS=2 mise run test` 已確認至少 cmd/hufu、internal/agent、internal/evalharness 因本機 TCP socket `operation not permitted` 失敗；已終止該輪自有程序，不能將此結果解讀為程式回歸。
- 在允許本機 fixture socket 的執行環境重新執行完整 baseline：全套通過（exit 0）；cmd/hufu 201.793s、internal/improver 144.402s、internal/team 1208.543s、web-research/tests 0.544s。
- `mise run vet`、`mise run lint` 均通過（exit 0）；完整 baseline 已確認為綠燈。
- 全套 baseline 結束後 GOCACHE 約 9.7 GiB（相對初始 8.3 GiB 成長約 1.4 GiB）；task scratch 約 1.3 MiB，已完成 Go 工作目錄由工具清理。
- 重量驗證前容量：`/tmp` 8 GiB／可用約 7.6 GiB，available RAM 約 9.3 GiB；cache volume 可用約 69 GiB。既有 GOCACHE 約 8.3 GiB、GOTMPDIR 約 981 MiB、GOMODCACHE 約 17 GiB。這些是既有共享資料，不視為本任務獨佔產物。
- 本次只授權本地程式實作與離線 fixture；不執行真實模型實驗。

- 已清理中斷的 sandbox baseline 自有 Go 工作目錄約 484 MiB；以其中 testlog 的私有 TMPDIR 確認來源，清除前檢查無程序消費者。未清除共享 cache。

- 實測 mise task 會重新套用 `.mise.toml` 的 `GOMAXPROCS=4`，外層 `env GOMAXPROCS=2` 不會覆寫巢狀 task。Baseline 實際為 4、Go `-p 2`；當時 available RAM 超過 9 GiB，符合提高並行的容量條件。後續 focused 命令直接使用 `mise exec -- env GOMAXPROCS=2 go ...`。

## 階段狀態

| 階段 | 狀態 | 證據 |
| --- | --- | --- |
| 1 決策設定繼承 | 待實作 | — |
| 2 Step 執行歸因 | 待實作 | — |
| 3 中斷恢復回歸 | 待實作 | — |
| 4 跨 run economics 比較 | 待實作 | — |
