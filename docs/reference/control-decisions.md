# Runtime control decisions

> Status: active
> Authority: reference
> Verified-Commit: — (working tree; transport inheritance changes pending validation)
> Supersedes: —
> Superseded-By: —

Runtime 的封閉語意選擇使用共同 decision primitive，不再先跑舊 JSON 推論再補
一次判斷。工具授權、預算、hash、coverage、狀態轉移與驗收仍由確定性程式處理。
完整盤點見[實作清單](../implementation-plans/fixed-collection-decisions.md)。

## 共用推論設定

全域／專案 `hufu.yaml` 與 `team.yaml` 支援：

```yaml
decision-runtime:
  backend: systemone
  systemone:
    model: ollama/clef:cloud
    endpoint: http://127.0.0.1:11434/v1/systemone
    timeout: 60s
    # api-key-env: SYSTEMONE_API_KEY
```

按欄位解析：entry 明訂值 > team > project > global > 既有預設。System One
模型不可省略；endpoint 必須支援 System One 協定。模型名稱不決定 API 協定。
`backend: sidecar` 使用原有角色 sidecar 設定，不繼承 System One 欄位。
一般生成、摘要與理由仍使用其原本模型設定。Sidecar 的 timeout 可獨立設定，
不會套用 inactive System One timeout：

```yaml
decision-runtime:
  backend: sidecar
  sidecar:
    timeout: 20s
```

所有 `runtime_*` choice 都繼承共用 transport；新增名稱不需加入既有四種控制點
的名稱映射。該映射只處理原有 mode／confidence 政策。

## 舊 control-decisions 相容性

舊區塊的 backend／model／endpoint／api-key-env／timeout 保留相容讀取並提示遷移至
`decision-runtime`。每個設定層（global、project、team）先正規化舊欄位，再按層級
逐欄位合併；舊欄位不再是一套優先於其他層級的 transport 權威。

同一層的新舊欄位相同時接受；衝突時在推論前拒絕，診斷指出欄位與來源而不列出
credential 值。不同層之間仍可正常覆寫。設定檔讀取或 YAML 解析失敗時，推論入口拒絕執行；
後續合法設定不能掩蓋該錯誤。不存在的可選設定檔仍使用既有預設。Legacy transport 欄位省略 backend 時
沿用其 System One 語義；顯式 sidecar 的 legacy model／endpoint／credential
則維持 inactive，timeout 遷移至 `decision-runtime.sidecar.timeout`。
僅 mode／threshold 不會宣告所有 runtime choice 的 transport。

- `off`：進入共同 primitive 路徑，無新設定時使用原 sidecar。
- `active`：使用解析後的 backend；沒有共用設定也沒明訂 backend 時保留舊
  System One 預設。失敗不追加舊 sidecar 呼叫。
- `shadow`：配置載入時拒絕，提示改 active；不再雙重推論。

`points` 的 actor／semantic contract 不受共用模型設定改寫。System One 保留
各 control point 的 confidence threshold；sidecar 無 confidence，不能將 enum
合法性視為語意正確率。舊 `control_decision_observed` 事件與
`hufu inspect control-decisions` 歷史資料仍可讀；新呼叫寫入 primitive journal，
透過既有 decision-primitive 結果／JSON／report 投影檢查。

## 安全結果與容量

| 入口 | 結果與失敗策略 |
|---|---|
| agent matcher | 完整 eligible workers 加 none；無選擇時要求明確 agent |
| closed ask_user | 單選或完整多選子集加 none；未決回報未回答 |
| path reviewer | access/not_access；失敗保留 path 交既有 policy/consent |
| guard reviewer | compliant/violation；失敗拒絕 |

單選保留完整業務候選加 none，沒有固定候選數量上限；封閉多選最多四項（完整子集加 none）。
請求大小、模型 context、backend 能力、timeout 與呼叫預算仍限制可執行的決策。
超容量明確失敗，不刪候選、不截斷證據、不切換舊路徑。
`mixed`、`allowAny=true` 與自由文字回答依確認的範圍保留既有生成契約，不使用 primitive。

Request 最多 48 KiB；sidecar 額外限制完整 prompt 為 65536 runes。未經穩定
secret redaction 的內容會拒絕持久化。決策不是工具授權；後續動作仍通過原有
denied-tools、phase、transport、force-mcp 與驗收政策。
`no-net` 不禁止受信任模型／System One 推論，仍限制工具及子程序網路能力。

## 持久化與恢復

Runtime 建立不可變 request/state snapshot 或固定列 binding，將 catalog/spec、
候選、來源、actor、branch、policy identity 與決策 receipt 綁定於原有 journal。
有效 settled 決策可重用，不消耗新推論預算。started 未 settled 不得假設未送出，
直接回報 interrupted；技術錯誤不轉為肯定答案，也不隱藏重試。

模型、端點、timeout、憑證版本或來源快照漂移後不得套用不相容結果。
普通 resume 仍檢查凍結政策；設定正規化若改變舊執行的有效政策會拒絕接續，
不重寫歷史 snapshot／receipt，也不自動升級。依診斷使用既有受控 recovery
流程，或明確以 `--new` 開始新的執行；不能將單純 YAML 遷移當成繞過 drift 的理由。Result-only repair
不新增推論。診斷顯示 started/settled/reused、backend/model、設定來源、snapshot
hash 與失敗碼，不輸出憑證值。`transport.sources.authentication` 表示
api-key-env 設定的來源；不以 credential-shaped JSON key 保存來源，避免來源資料
被 secret redaction 改寫。歷史來源 map 保留原樣解碼，不補寫歷史事件。固定列決策由 runtime 注入 canonical payload，
Worker 不需重抄值或 receipt。
