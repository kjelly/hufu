# Native Image Runtime 實作進度

> Status: active
> Authority: reference
> Verified-Commit: `b97ef843`
> Supersedes: —
> Superseded-By: —

依 [實作規格](native-image-runtime.md) 分階段交付，每階段驗證後獨立 commit。未通過驗收的階段不標示完成。

## Baseline

- 原始碼基準：`b97ef843f440ab0cca85a8a453892ba2c4eef560`；工作樹起始無 tracked changes。
- Toolchain：mise Go 1.27.2、golangci-lint 2.14.0；GOMAXPROCS=2，Go commands 使用 `-p 2`。
- 2026-11-04 驗證前：GOCACHE 22,107,688 KiB、GOMODCACHE 16,897,080 KiB；沿用既有配置，未建立新快取。這是共享既存總量，非本工作新增。
- `/tmp` 可用 7.4 GiB、available RAM 12 GiB；cache/module volume 可用 61 GiB。
- `mise run test`：執行中；其餘 baseline gates 尚未執行。完成後記錄實際結果，不能把 pending 當成通過。

## 階段狀態

| 階段 | 狀態 | 證據／commit |
|---|---|---|
| 文件移入正式目錄 | 已整理，待 commit | 更新文件 map 與本紀錄 |
| PR-00 SDK/schema fixture 與回歸基線 | 尚未開始 | 先確認 baseline |
| PR-01 M0 private media/binding/policy/quota | 尚未開始 | — |
| PR-02 M0 history/compaction/recovery/audit | 尚未開始 | — |
| PR-03 M1 CLI/direct/single native images | 尚未開始 | — |
| PR-04 M2 coordinator/chat/DAG/routing | 尚未開始 | — |
| PR-05 M3 MCP/result binding | 尚未開始 | — |
| PR-06 M3 inspect/export/docs | 尚未開始 | — |
| PR-07 M4 Codex localImage | 尚未開始 | — |
