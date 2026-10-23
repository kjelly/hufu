# Agent team 的 DecisionPrimitive

> Status: active
> Authority: reference
> Verified-Commit: — (working-tree implementation; tests are authoritative)
> Supersedes: —
> Superseded-By: —

`decision_primitive` 是原生 agent tool，不會啟動 `hufu decisionrt` 子程序。
team 維護者定義有限答案與授權；一般模式的 agent 只提供宣告的 context，
固定列模式則由 runtime 建立 context。既有 `decision:` / DecisionEngine 保留
自己的 evidence 與 stage 流程，其有限選擇也使用此 primitive；決策不授權或執行 action。

## 設定與授權

在 team.yaml 宣告：

```yaml
decision-primitives:
  request_size:
    backend: systemone
    endpoint: http://127.0.0.1:11434/v1/systemone
    model: nimble
    version: "1"
    kind: choice
    question: Classify the work described by summary into one size.
    options:
      - {id: small, description: A small bounded change}
      - {id: medium, description: Several related changes}
      - {id: large, description: Broad architectural work}
    inputs: {summary: string}
    agents: [helper, coordinator]
    timeout: 5s
    min-confidence: 0.8
    max-calls: 100
```

worker 的 frontmatter 必須明確列出工具：

```yaml
---
name: helper
tools: view, grep, decision_primitive
---
```

`tools: all` 或空白不會隱含授予此工具。worker 必須同時出現在該 decision 的
`agents`，且中央 policy gate、team denylist、phase、closed tool sequence 與
unattended allowlist 仍適用。coordinator 以保留身分 `coordinator` 授權；只有
catalog 授權它時才加入其工具集。`--force-mcp`（或 agent 對應設定）
會禁止此工具，包括 `rule`。`no-net` 允許受信任模型及 System One 推論，
仍禁止工具／子程序網路。result-only repair 不新增推論。

未設定 `decision-primitives` 的 team 不會取得新工具，也不需要 decision model。
設定載入只驗證 contract，不探測 endpoint、不暖機、不推論。
catalog 最多 64 個 entry，完整公開 tool contract 不得超過 256 KiB。

推論設定可由全域／專案 hufu.yaml 與 team.yaml 的 `decision-runtime` 共用：
entry 明訂值 > team > 專案 > 全域 > 原有預設，逐欄位合併。
System One 缺必要模型時報錯；sidecar 不繼承 System One 的模型、端點或憑證。
詳見[共同設定與控制入口](control-decisions.md)。題目、候選、grants 與呼叫上限仍由 entry 宣告。

## Agent 呼叫與回傳

```json
{"name":"request_size","context":{"summary":"Update two related configuration fields"}}
```

agent 不能指定 question、options、backend、model、endpoint、credential、timeout
或 confidence policy。context 必須精確匹配 `inputs` 的必要 scalar 欄位；不接受
額外欄位、巢狀物件、null、重複 JSON key 或未知 top-level 欄位。

回傳 JSON 包含 `result` 與 `receipt`：

- `result.status` 為 `decided` 或 `abstained`；`decided` 才有可使用的 typed value。
- `result.value` 為 `choice`、`boolean` 或 `integer` 中唯一一項。
- `receipt` 綁定 spec ID/version、request digest、backend/model、status、reason
  及 duration；不包含 context 或自由文字 rationale。
- 低信心與 backend 主動棄權是成功回傳的 `abstained`，不是 technical error。
- provider、timeout、輸入或 policy 失敗回傳穩定類別的 tool error，不猜測答案。

team prompt 應明訂遇到 `abstained` 或 technical error 時請求澄清、交由較完整的
推理流程，或停止；不可將棄權解讀成預設選項。語意由模型判讀，不用關鍵字表
或 regex 從任務文字猜測 execution scope。

## 支援欄位

| 欄位 | Contract |
| --- | --- |
| `backend` | `systemone`、`sidecar` 或 `rule`；可繼承共同設定，未設定時預設 sidecar |
| `endpoint` | 只限 `systemone`,預設 `http://127.0.0.1:11434/v1/systemone` |
| `model` | systemone 解析後必須存在，可繼承共同設定；sidecar 省略時使用既有 sidecar model，不繼承 System One model |
| `api-key-env` | 可選環境變數名稱；有宣告但值缺失時載入失敗，不允許 inline key |
| `version`, `question` | 必填；遵循 core Spec 的 ID、長度與 UTF-8 驗證 |
| `kind` | `choice`、`boolean`、`integer_range` |
| `options` | 僅 choice，2–21 個唯一機器 ID；description 可選 |
| `range` | 僅 integer_range，`{min: 0, max: 4}`；最多 21 個值；systemone 至少 2 個 |
| `inputs` | 必要 context 欄位名稱 → `string`、`boolean`、`number`；最多 64 個 |
| `agents` | 精確 agent 名稱列表，最多 64 個；未知 worker 載入失敗 |
| `timeout` | 每次 backend attempt，預設 `5s`、最大 `60s`;hufu.yaml 的 `timeouts.decision` 可調高上限 |
| `min-confidence` | 可選 0–1 門檻；不足時 abstain |
| `require-calibrated` | 預設 false；System One raw confidence 不滿足此要求 |
| `fallback` | 預設無；僅可明訂 systemone → `rule`，且 rule 永遠 abstain |
| `max-calls` | 每個 entry 在 active session lineage 的 primitive 呼叫限額，預設 100，最大 10000；含失敗與未完成呼叫，明訂 fallback 不另計 |
| `block-on` | 可選，只限 `choice`；列出會擋下呼叫者的選項 ID。worker 拿到其中一個決定後，這次嘗試不能再執行會修改狀態的工具(含 dynamic MCP gateway),`submit_result` 也只接受 `blocked`;唯讀工具仍可使用。下一次嘗試不受影響 |

`systemone` 重用既有 native protocol adapter；模型必須由服務端提供。
其 confidence 是選中候選的 raw probability，**不是校準過的正確率**。
`rule` 目前永遠 abstain，適合離線驗證 contract，不會自行猜測 domain 答案。
`backend: sidecar` 用語言模型決策：模型從候選中挑一個 token,不回報信心值。

- 它走 team 既有的 model provider、request admission 與 usage 計算，所以 token 會計入 run 的 budget。
- 不能設定 `endpoint`、`api-key-env`、`min-confidence`、`require-calibrated`;沒有信心值，任何信心門檻都會讓每次呼叫都 abstain,因此載入時直接拒絕。
- 只要輸出格式正確就一律 `decided`;`block-on` 與 `fallback: rule` 照常可用。
- 明訂的 `model` 會納入 execution policy 的模型集合，啟動時會驗證。
- Primitive prompt 上限為 65536 runes，使用獨立 DecisionProfile 與 512 output tokens 上限；實際模型 context admission 仍可能拒絕。超限直接失敗，不截斷證據。

## 持久化、恢復與預算

每次 inference 先持久化 `decision_primitive_started`，再持久化
`decision_primitive_settled`；settled 寫入失敗時不向 agent 發布結果。
事件包含 typed result/receipt。新 binding 使用獨立有界 request artifact，事件
記錄其 hash/reference 及有效 transport；不保存 credential 值。legacy v1 仍可讀。
session checkpoint、`--output json` 的 team `decision_primitives` 與 report 的
Helper Decisions 提供 content-free metadata 投影；工具回應仍走既有 tool log/TUI。

同 branch、actor、durable task 建立事件、catalog hash 與 request digest 的已提交
`decided` / `abstained` 結果可跨 retry、crash-resume 與不同 tool call ID 重用。
coordinator 的重用範圍是同 run。不同 branch 不共用；同 coordinator 的並行呼叫
序列化，extra-model clone 共用父 coordinator 的 service/journal 與限額。

只有 started、沒有 settled 時回報 interrupted，不能假設 provider 尚未收到請求。
相同 binding 的 settled technical failure 也不以新 tool call ID 重跑。
成功的 reuse 記錄 `decision_primitive_reused` 與原 settled event，且不再消耗推論預算。
來源、候選、政策或狀態改變後不得套用舊結果；既有 recovery 仍負責 retry/blocked。
只有明訂的 core fallback 會執行第二個 backend attempt，不自動切換到另一模型。

先嘗試驗證 settled replay；需要新推論時才檢查既有 run budget，耗盡則拒絕(`decision_budget_exceeded`)。
wrap-up 只停止新的派工，仍在執行的 worker 照常可以呼叫。推論遵守 caller
cancellation 與每次 attempt timeout。System One protocol 沒有可靠 token usage，因此不偽造 token
數字、不計入 worker LLM token usage；此工具另受 `max-calls` 與 timeout 限制。

完整 catalog、grants、policy、transport 及 resolved credential revision 的 hash
綁入 execution policy snapshot。resume 發現設定漂移時依既有 admission fail closed，
需要還原原設定或使用 `--new`。credential 不以明文寫入 snapshot。
既有 CLI 仍獨立運作，不讀取 team catalog。
