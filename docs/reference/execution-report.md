# 自訂執行報告區塊

> Status: active
> Authority: reference
> Verified-Commit: `b7c167e1`
> Supersedes: —
> Superseded-By: —

在 `team.yaml` 的 `execution-report.sections` 宣告區塊。Hufu 按照宣告順序顯示，每個區塊讀取一個 runtime task 的具名輸出。以下範例在同一份報告中加入 Markdown 模板和 Go renderer。

```yaml
execution-report:
  sections:
    - id: inventory-summary
      title: Inventory Summary
      source:
        task: collect-inventory
        output: inventory
        require: passed-task-output-assertion
      renderer:
        runtime: template
        template: |
          Warehouse **{{.Output.location}}** has {{len .Output.items}} inventory entries.
    - id: inventory-table
      title: Inventory by Category
      source:
        task: collect-inventory
        output: inventory
        require: passed-task-output-assertion
      renderer:
        runtime: golang
        source: ./reports/inventory
        mode: trusted-static
        timeout: 5
```

`task` 使用 task ID 或 workflow task ID，必須能唯一找到已完成且有成功 action 收據的 occurrence。`output` 是該 task 的 runtime output 名稱。`passed-task-output-assertion` 要求同一次 run 的驗收證據或成功任務收據包含針對相同 task 和輸出的通過證據。Hufu 會使用事件中的任務結果、收據和凍結輸入重驗斷言。

Markdown 模板使用 Go 的 `text/template`。輸入包含 `.Output`、`.Version`、`.RunID`、`.SectionID` 和 `.Provenance`，缺少模板所需欄位時，該區塊會顯示為 unavailable。模板以 16 KiB 為上限。

## Go renderer

在 team 內的來源目錄建立 `package main`，並提供
`Run(ctx context.Context, in io.Reader, out io.Writer) error`。
來源檢查、雜湊、子程序執行與輸出限制沿用現有的 Go runtime。
`trusted-static` 腳本以 Hufu 的作業系統使用者權限執行，請只從信任的維護者載入。
子程序的環境變數只包含自己的暫存路徑。timeout 以秒計，預設值及上限都是 5 秒。詳見 [Go action runtime](action-providers.md)。

標準輸入是一個 v1 JSON 物件。此例的 `output` 是該區塊選取的 `inventory` 值。

```json
{
  "version": 1,
  "run_id": "run-report",
  "section_id": "inventory-table",
  "output": {"location": "north", "items": ["bolts", "nuts"]},
  "provenance": {
    "run_id": "run-report",
    "task_id": "1",
    "occurrence_revision": 3,
    "attempt": 1,
    "action_invocation_id": "inventory-action",
    "runtime_outputs_hash": "sha256:…",
    "run_input_snapshot_id": "input-1",
    "run_input_snapshot_hash": "sha256:…",
    "verification_fingerprint": "…"
  }
}
```

`provenance.run_id` 識別產生該份輸出的原始 action run。這在恢復並重用驗證過的任務時可以和報告所屬的 `run_id` 不同。`verification_fingerprint` 在有該項證據時一併提供。標準輸出須為單一 JSON 物件 `{"markdown":"..."}`。

## 保存與重新開啟

每個 team 最多可設定 8 個區塊，每個區塊至多保存 64 KiB Markdown。Hufu 在 run 開始時記錄來源雜湊，在 run 結束、報告寫入交易開始前執行 renderer。來源變更、驗證證據缺失、逾時或輸出錯誤時，對應區塊顯示為 unavailable，通用報告繼續顯示該次 run 的結果和驗收狀態。

Markdown、來源識別與雜湊隨報告快照一起保存。執行時的 `--report`、`auto-report: true` 及日後的 `hufu results report` 使用同一份快照。日後編輯或移除 renderer，歷史報告仍直接使用已保存的內容。
