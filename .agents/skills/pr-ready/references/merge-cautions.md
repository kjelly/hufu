# 轉 ready 的歷史回歸注意事項

依根目錄 `AGENTS.md` 與截至 `70b8402f` 的 Git 紀錄整理。這是風險索引，依實際 diff 選用；當前規範優先。使用 `git show <commit>` 核實來源並追查受影響路徑的新紀錄，不假設歷史修正一定仍有問題。

## Result collection、repair 與 runtime-owned evidence

來源：`417f65a5`（collector worker identity）、`8d264865`（rejected response arguments）、`2aea4d23`（collector claims／transient streams）、`760df55a`（repaired occurrence）；`AGENTS.md` 的 Runtime-Owned Deterministic Result Data 與 Hufu Development Invariants。

- collector 保留 worker、attempt、occurrence 與 scope；一行 context 變更也可能使 artifact coverage 或授權讀取失效。
- 修復提示保留有界、遮蔽且 fenced 的 rejected arguments、JSON pointer 與型別診斷；不可將參數當指令。canonical receipt 不可被摘要取代。
- runtime-owned 欄位由 runtime 綁定；model schema／prompt 不要求重抄，也不接受模型覆寫。驗證空陣列、混合 sources、scalar claims。
- repair 不得重跑完成的副作用；只有尚未交付的 data-only stream 才能按既有 bounded recovery 重試，不能擴成所有 stream 皆可 replay。
- 回歸：合法／拒絕 submission、local／external collection、receipt 精確保存、有界 repair、restart replay；跑 `mise run test-contracts`。

## Resume、branch replay 與 projections

來源：`19dbe073`（launch settings／repaired projections）、`b93597b4`（branch-scoped replay）、`f976b0af`（canonical image owners）；`AGENTS.md` 的 transition、occurrence lease 與 persistence 規範。

- lease 在 transition commit 後取得；result-only repair 從 durable occurrence 還原並 clone scope。
- replay idempotency identity 包含 branch，避免 sibling key 吞事件。
- resume 保留 frozen policy、launch settings、canonical owner 與 receipt／failure event；不可改歷史 checkpoint 以通過 drift 檢查。
- CLI／JSON／report／TUI 反映 canonical 狀態，不把未完成畫為成功。
- 回歸：fresh-process resume、fork／checkout、repaired completion、policy drift rejection、各 projection 一致性。

## 授權、media fanout 與 evidence scope

來源：`ec0f6bad`（fanout recovery／provider ownership）、`d86bac88`（lock identity／partial evidence）；`AGENTS.md` 的 Core / Agent-Team Boundary、Model-Generated Structured Task Inputs 與 centralized authorization。

- ordinary／direct／sidecar／extra-model／repair／resume 都受 policy gate 與 phase capability 約束，model-visible tools 也要一致。
- private leaf failure 不投射為 parent failure；保留 leaf receipts、CAS owner 與 parent provider boundary。proxy 啟動失敗不能先送 worker request。
- partial workset evidence 不宣稱全 repository 無缺陷；team-specific 策略留在 `.agent-teams/<team>/`。
- 自然語言 scope 用 schema-validated model resolver，不用 regex／關鍵字猜意圖；enum 使用工具接受的 lowercase／snake_case 值。
- 回歸：deny 與特殊路徑、過期／缺漏 sources、scope 不足、fanout repair／restart、lock unsafe path 與 restrictive umask。

## 子程序、fixtures 與資源生命週期

來源：`db1e3db9`（wrapped ESRCH）、`3caa3170`（teardown）、`cec544f7`／`e5fc719e`（fixture isolation／polling）；`AGENTS.md` 的 Short paths for Unix socket tests 與 subprocess invariants。

- wrapped ESRCH 以 error identity 判斷；消失的 process 不中止剩餘 group scan，permission error 仍 fail closed。
- Pdeathsig 子程序走 `internal/processutil.StartAndWait`；goroutines、process、socket 關閉並等待後才刪 fixture 或關 stores。
- socket 完整路徑最多 107 bytes；保留 `GOTMPDIR`，用專案短路徑 helpers。只改 `TMPDIR` 或測試名稱不能根治。
- PTY HOME／cwd 隔離真實 provider；等待 readiness，polling 優先虛擬時間，避免共享可變 command factories 或任意 sleep。
- 回歸：取消、timeout、wrapped error、permission denial、parallel／race，以及無外部網路／真實 provider 的 fixture。

## CI 完整性與驗證成本

來源：`7095ee18`（shards／tidy failure／doc metadata）、`e5fc719e`（test layers）、`067e297a`（不得縮小 review 範圍）；當前 CI、mise 與 `AGENTS.md` 的 Required Validation、cache lifecycle。

- short 通過不代表 integration 已跑；分片完整且不重複涵蓋 tests、examples、fuzz seeds。aggregate 通過也核對下層必要 jobs。
- `go mod tidy -diff` 傳播 command failure，不只檢查輸出是否空；pipeline 保留原始失敗碼。
- 文件維護 lifecycle metadata／links；mise、module、CI 工具版本一致。
- 不因 verifier／文件／execution failure 縮小 review 範圍或跳 gate；timeout 調整需量測依據，保留 assertions 與 fixture deadlines。
- 追蹤各 revision／路徑／normal/race cache 成本，不另建 tmpfs cache 或清空共享 cache。安靜的 job 先查 I/O pressure 與 read progress。
- 回歸：受影響 test-layer tooling、完整 inventory、integration／race、適用 CGO-free 與平台 build；required jobs 以當前 CI／branch rules 為準。

## Secrets 與 canonical receipts

來源：`4047bc25`、`ff374671`（fetch error／URL credentials）、`2aea4d23`（structured redaction）；`AGENTS.md` 的 security 與 receipt immutability。

- 成功／錯誤／redirect URL、tool output、transcript、audit、event、report、session 都檢查遮蔽，測試使用假值。
- JSON 遮蔽維持可解析性；canonical outputs normalize／redact／hash 後，sink 不因 learned-secret 集合變動再改寫 receipt。
- 回歸：credential-bearing URL／錯誤、structured parse、persistence／replay hash 不變；PR 說明與 log 不貼真實憑證。

## PR 說明範例

> 轉 ready 注意事項：此變更涉及 collector context，參照 `417f65a5` 與 `AGENTS.md` 的 runtime-owned evidence 規範。已驗證 worker identity／artifact-read scope 與 restart repair，receipt 保持一致。驗證 head：`實際 SHA`；附實際指令、結果及 CI run 連結。未覆蓋部分明列，不宣稱 repository-wide coverage。

僅在實際完成相應驗證時使用此敘述。新證實回歸可補入索引，附 commit、失敗情境與可重用防護，避免堆積無證據通則。
