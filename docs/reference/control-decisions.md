# Runtime control decisions

> Status: active
> Authority: reference
> Verified-Commit: — (working-tree implementation; tests are authoritative)
> Supersedes: —
> Superseded-By: —

Control decisions 讓 hufu runtime 自己的四個控制面決策點可以改問 `systemone`
decision model（例如 Ollama 上的 `nimble`），而不是只靠 sidecar 回傳 JSON 文字。
它和 agent 主動呼叫的 [`decision_primitive` tool](decision-primitives.md) 不同：
問題由 hufu 固定，agent 看不到也無法呼叫；team 只能選擇模式、門檻與連線設定。
正規契約見 [DecisionPrimitive](../architecture/decision-primitive.md) §59。

## 決策點

| Point | 問題 | Kind | active 時低信心的結果 | 預設門檻 |
|---|---|---|---|---|
| `agent-matcher` | `request_agent` 沒指定 agent 時選哪個 worker | choice（2–21 個 worker） | fail closed，要求明確指定 agent | 0.60 |
| `ask-user` | unattended 時 `ask_user` 選哪個選項（只限 `single_choice`） | choice（2–21 個選項） | 通知需要人類，請 agent 自行判斷；不猜第一個選項 | 0.60 |
| `path-reviewer` | bash 指令中的範圍外路徑是否真的存取檔案 | boolean | 保留路徑，照常走 consent | 0.90 |
| `guard-reviewer` | tool call 是否符合 agent 的所有 `guard` 規則 | boolean | deny | 0.90 |

超出範圍時該次呼叫直接走既有路徑、不記錄事件：

- 候選少於 2 個或多於 21 個；
- `ask_user` 不是 `single_choice`；
- guard rule 全文超過 4096 bytes。

確定性 guard rule、read-only bash grammar、recovery 與 DecisionEngine 都不受影響。

## 模式

- `off`（預設）：完全不呼叫 decision model、不寫事件，行為與未設定時相同。
- `shadow`：decision model 與既有 sidecar 路徑並行執行，**一律採用既有結果**，
  只記錄兩者是否一致。多出的延遲是 `max(0, model − 既有路徑)`，上限為 timeout。
- `active`：
  - decision model 給出答案且信心 ≥ 門檻時採用；
  - 信心不足時採用上表的安全結果；
  - 技術錯誤（連線失敗、timeout、未知 model、回應格式錯誤）時一律退回既有路徑，
    所以 active 的可用性不會低於 `off`。

建議先用 `shadow` 累積資料，再以 `hufu inspect control-decisions` 檢查一致率與
低信心比例後，逐點改成 `active`。

## 設定

`control-decisions:` 可以寫在使用者層級的 hufu.yaml（~/.config/hufu/hufu.yaml）、
專案目錄的 hufu.yaml 與 team.yaml。每個欄位依 team.yaml → 專案 hufu.yaml → 使用者
hufu.yaml → 預設值解析；
`points` 下的欄位也逐點、逐欄位解析。因此可以在個人 hufu.yaml 讓所有 team 跑
shadow，再由個別 team 固定自己的 active 設定。

```yaml
control-decisions:
  endpoint: http://192.168.11.117:11434/v1/systemone  # 預設 http://127.0.0.1:11434/v1/systemone
  model: nimble              # 任一點不是 off 時必填
  api-key-env: SYSTEMONE_KEY # 可選；有設定時該環境變數必須存在且非空
  timeout: 5s                # 每次呼叫；預設 5s，最大 30s
  mode: shadow               # 所有點的預設模式：off | shadow | active
  min-confidence: 0.8        # 可選；所有點的預設門檻
  points:
    path-reviewer: {mode: active, min-confidence: 0.95}
    guard-reviewer: {mode: off}
```

驗證規則：

- 載入時不連線、不暖機。
- 單一檔案只檢查模式、點名、門檻、timeout 範圍與環境變數名稱。
- 合併後至少有一點不是 `off` 時，coordinator 建立前會檢查 `model`、`endpoint` 與
  credential；不完整時 run 直接失敗。
- 不接受 inline key。

## 安全與隱私

- 送給 decision model 的每個 context 字串都先經過 secret redaction，與寫入 log 和
  session 的 redaction 相同。
- 長文字依 sidecar 的上限截斷：command 3000、arguments 2000、描述 300 runes。
- confidence 是被選候選的 raw probability，**不是校準過的正確率**。
- active 會影響授權：`path-reviewer` 可以讓路徑略過 consent，`guard-reviewer` 會核准
  或拒絕 tool call。因此只有 active 點的 transport、timeout、credential revision 與門檻
  會寫入 execution policy snapshot；這些設定改變後 resume 會 fail closed，要還原設定
  或用 `--new`。
- `off` 與 `shadow` 設定不影響 snapshot，也不影響 resume。
- systemone 呼叫不經 provider admission 或成本計算，只受 timeout 限制。Ollama 端的
  並行數決定排隊延遲。

## 觀測與報告

每次 shadow／active 呼叫寫一筆 `control_decision_observed` durable event，內容不含
問題、命令、路徑、tool arguments、選項文字、goal、endpoint 或 credential，只記錄：

- point、mode 與套用來源（`primitive`、`safe_default`、`legacy`）；
- status，以及編碼後的值：`true`／`false` 或 0-based 候選索引；
- raw confidence、門檻與 error code；
- 兩邊的耗時、既有路徑的結果，以及兩者是否一致。

事件寫入失敗只會警告，不改變決策。

- `hufu report` 的 **Control Decisions** 區段與 `--output json` 的 `control_decisions`
  顯示本次 lineage 的統計。
- `hufu inspect control-decisions [run-id] --workspace <dir>` 驗證 event chain 後彙整
  目前 branch lineage（或單一 run）的統計。每次 `--new` 都會開新的 branch，所以要
  累積多次執行的資料時請加 `--all-branches`，彙整該 workspace 的所有 branch。統計內容：
  - calls 與一致數／可比較數；
  - 低於門檻的次數，也就是 active 時會走安全結果的次數；
  - 錯誤分類；
  - 平均 confidence 與 p50／p95 延遲。
