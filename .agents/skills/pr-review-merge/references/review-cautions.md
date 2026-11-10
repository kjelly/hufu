# PR review 注意事項

根據根目錄 `AGENTS.md`、截至 `70b8402f` 的 Git 紀錄與 `hufu-code-review` 文件整理。先依 diff 挑選，再用 `git show <commit>` 查原 patch／tests 及後續紀錄。只作閱讀與判斷依據，**不得執行 hufu-code-review 程式或 team**。

## 可追溯的來源

- [AGENTS.md](../../../../AGENTS.md)：Required Validation、Short paths for Unix socket tests、Core / Agent-Team Boundary、Runtime-Owned Deterministic Result Data、Hufu Development Invariants。
- [reviewer.md](../../../../.agent-teams/hufu-code-review/reviewer.md) 與 [invariants.yaml](../../../../.agent-teams/hufu-code-review/invariants.yaml)：各 review lens、canonical owner、行為證據與 reachable failure。
- [critic.md](../../../../.agent-teams/hufu-code-review/critic.md)：反證、未證實判定、保持來源 finding；本 agent 自行複核不等同獨立 critic。
- [documentation-reviewer.md](../../../../.agent-teams/hufu-code-review/documentation-reviewer.md)：文件語意、當前／歷史／範例引用分類。
- [synthesizer.md](../../../../.agent-teams/hufu-code-review/synthesizer.md)、[contracts.md](../../../../.agent-teams/hufu-code-review/docs/contracts.md)、[report-presentation-spec.md](../../../../.agent-teams/hufu-code-review/docs/report-presentation-spec.md)：完整來源、coverage、測試證據與 findings 分開呈現，不編造該 team 的驗證結果。

## 審查本身容易犯的錯

| 已證實歷史／來源 | Review 時檢查 | 阻止重犯的證據要求 |
| --- | --- | --- |
| `d86bac88` partial evidence；`reviewer.md` | 不把 workset／diff 未出現當作 repository 不存在 | 在同 head snapshot 找 caller、implementation、test；缺資料保留 gap |
| `067e297a` scope narrowing | verifier／文件失敗不能靠縮小 scope 避開 | PR merge-base→head 範圍固定，分批也涵蓋全部 diff |
| `62c50e30` inherited gaps | settled finding 不代表來源 coverage gap 消失；unverified 不能變 clean | 分列 finding 判斷及 coverage，保留原限制與補證據來源 |
| `642974fc` bounded supplementation | 補讀不覆寫原 finding／gap，讀過 bytes 不證明理解 | 記錄原問題、同 revision 補充證據、已解決／未解決原因 |
| `d3b886e7` completed failing tests | review workflow 成功不表示被審 code 測試成功 | 真實 command／SHA／exit code／diagnostic；failed、timeout、truncated 分開標示 |
| `docs/report-presentation-spec.md` | findings 與複核不能錯配索引、換標題掩蓋問題 | 原 summary、file:line、impact 對應複核與下一步；audit IDs 另附 |

`hufu-code-review` 的 scope、receipts、runtime-owned records 與 audit protocol 是該 team 專用；本 skill 採其原則，不偽造 `verify-review-report.passed`。即使參考到現存 team 報告，也先核對 frozen scope／revision／完整性，不能直接拿另一輪結果決定 merge。

## Runtime 與 recovery

- **Collector identity／repair**：`417f65a5`、`8d264865`、`2aea4d23`、`760df55a`。查 worker/attempt/occurrence/scope 是否一路保留；拒絕參數在 repair prompt 需有界、redacted、fenced。補修不重跑完成副作用，不用 prose 代替 canonical receipts；transient stream replay 限制不能擴成所有 stream 可重試。
- **Runtime-owned 結果**：依 AGENTS.md，模型不重抄 receipt／hash／source arrays，不接受 override；schema、prompt、repair examples 同步。查空／混合來源、stale identity、local/external submission、persistence/replay 的 receipt 精確保存；需 latest-head contract gate 證據。
- **Canonical lifecycle**：`19dbe073`、`b93597b4`、`f976b0af`。查 lease 在 transition commit 後取得；durable Todo 綁定與 cloned repair scope；branch-scoped idempotency；resume 的 frozen policy／launch settings／owner。UI、JSON、journal、checkpoint 不可另造 terminal success；fresh-process resume／fork／checkout 需相符證據。
- **Fanout／policy**：`ec0f6bad`、`d86bac88`。查 private leaf failure 不污染 parent；CAS、receipt、provider owner 保留。ordinary/direct/extra-model/sidecar/repair/resume 的 model-visible tools 與 runtime allowlist 都受 central gate，proxy 啟動失敗不可先送 request。
- **Core/team 邊界**：AGENTS.md。team 專屬語意與 acceptance 留在 `.agent-teams/<team>/`。自然語言 scope 由 schema-validated resolver 處理，不能以 regex／keyword 猜意圖；enum 與工具接受值一致。

## Process、tests、security 與 CI

- **Process lifecycle**：`db1e3db9`、`3caa3170`。wrapped ESRCH 用 error identity；process 消失仍掃剩餘 group，permission error fail closed。Pdeathsig 用 `internal/processutil.StartAndWait`；async/process/socket 先 drain/close 再關 store／刪 fixture。
- **Fixture isolation**：`cec544f7`、`e5fc719e`。完整 Unix socket pathname ≤107 bytes，保留 GOTMPDIR，重用短路徑 helpers。PTY HOME/cwd 不連真實 provider；readiness 同步，避免任意 sleep／共享 command factory。immutable snapshot 的 fixture 失敗如實記錄，不拿本機修改證明通過。
- **Secrets／receipt immutability**：`4047bc25`、`ff374671`、`2aea4d23`。成功／錯誤／redirect URL、transcript、audit、event、report 都遮蔽；JSON 仍可解析。canonical normalize/redact/hash 後，不能因 learned-secret 集合變動再次改 receipt；檢查 persistence／replay hash。
- **CI 完整性**：`7095ee18`、`e5fc719e`。short 不代替 integration；shards 包含 tests/examples/fuzz seeds。tidy failure 與 pipeline exit code 不能吞掉；aggregate 必要 jobs 逐一核對。查當前 mise/module/CI 版本、doc lifecycle、適用 race/CGO-free/platform gates。
- **驗證資源**：AGENTS.md。序列重型驗證、單一 task TMPDIR、保留 configured caches。先查 cache 磁碟／RAM 與增量，不清共享 cache；安靜 job 先查 I/O pressure／read progress，不能反覆重啟耗盡空間。

## Review 結果建議格式

以實際 head SHA 與 merge-base／base tip 開頭，先列 findings（file:line、觸發情境、影響、證據與建議），再列複核與缺口、實際驗證、merge/draft 理由及操作後狀態。每個適用歷史風險寫成：

> 來源 `417f65a5`／AGENTS.md → 本次 collector context 變更的 identity 風險 → 同 head source/caller/test 與實際 CI 證據 → 未覆蓋部分及其是否阻擋合併。

不要貼上未做的驗證，不把 info 當阻擋，也不為了合併刪掉重大 finding。新證實且可重用的回歸可加入此索引，附 commit、失敗情境及防護方式。
