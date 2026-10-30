# 失敗結果收集的重新驗證與接續實作計畫

> Status: active
> Authority: reference
> Verified-Commit: —
> Supersedes: —
> Superseded-By: —

> **用途**：實作設計、驗收與交接紀錄。第一版支援範圍已完成實作與驗收；工作樹實作、限制與驗證見第 12–14 節。本文件只記錄通用機制，不收錄個別失敗任務的調查資料。
> **核對基準**：實作與交接時重新核對當前 checkout、diff 與第 3 節的基線說明，不得覆蓋既有工作。
> **產品需求**：保留已完成任務與原始工作證據，在 runtime 修正後，能重新判定失敗結果是否可接續，減少整批重跑與不必要的模型／工具成本。
> **授權範圍**：本文件描述程式實作及本地 fixture 驗證；不授權自動恢復歷史 workspace、呼叫真實模型或修改歷史資料庫。

## 1. 問題與成功條件

現有 runtime 已封存工作報告、transcript、collector request/response、prepared claims、來源與決策綁定。`resume --repair-result` 卻先依已保存的 failure reason 決定資格。若舊 runtime 將可修復的條件 schema 錯誤誤記為 `source_invalid`，更新程式後仍無法恢復。

典型失敗模式是：已封存工作與已結算決策仍有效，但組裝後的模型可編輯欄位違反條件 schema，舊分類卻將其記為來源無效。恢復評估應獨立驗證來源與決策，再判斷是否只需修復結果格式。來源紀錄中的 gaps 應保留；決策欄位須符合 verdict 契約，不能直接把來源限制複製成 decision-level missing evidence。

成功條件：在原始證據完整、契約與政策相容時，重新驗證這類舊失敗，僅修復結果階段，沿用已完成任務。歷史失敗保留，新判定與新執行記錄以追加事件表達。整體 run 仍須通過原有驗證及 acceptance。

## 2. 第一版範圍與非目標

### 納入

- local LLM、`result-transport: runtime`、shared workspace、工作已封存且執行已結束的 blocked protocol task。
- 第一版不恢復非空 `assumption_checks`／`memory_uses` 的宣告或 active decision discipline；它們需要原 context／governing decision 的獨立 reconciliation。terminal 未結束、未知退出狀態、最近一次失敗，或 paused／uncertain side effects 也先 blocked。這些拒絕是支援邊界，不會透過 collector 刪除宣告來繞過。
- 以同一 branch、原工作 attempt、原 execution target、原 pinned contract、原輸入與來源證據接續。
- 無模型的唯讀恢復評估；本地提交；受限 collector 格式修復；既有預算內的暫時收集故障恢復。
- 與現有 `resume --repair-result`、事件重播、receipt、status／inspect／JSON／report 的整合。
- 舊 checkpoint 相容性與併發、崩潰、取消、重複命令的防護。

### 不納入

- isolated workspace、extra-model fanout、external agent、runtime action、legacy tool transport 的新恢復能力；維持原有行為或明確拒絕。
- 重跑工作工具、重新做已結算的 `decision_primitive`、從現行 checkout 重建歷史來源。
- 換模型、放寬權限／schema、忽略 policy drift、跨 branch 搬移判定、缺失資料的模型補造。
- 自動修正 verdict、清空 `missing_evidence`、刪除 gaps，或把驗證失敗轉成成功。
- 一般工作失敗、未知副作用的自動 retry；這些仍走原有恢復／reconciliation。

第一版只能保證相容的歷史任務可恢復。若目前 team／model／contract 設定與原 policy 不符，必須輸出具體 blocker；缺乏可驗證歷史綁定的任務仍須拒絕恢復。

## 3. 實作前基線與既有未提交變更

本節保留計畫建立時的歷史基線，表中的缺口不代表目前仍全部存在；例如 `collectionGrantAttempts` 已由新 assessment 路徑取代。目前狀態以第 12 節為準。coding agent 必須先比對實際 checkout，重用已存在的修正。

| 檔案 | 現有責任／已知缺口 |
|---|---|
| [operator_result_repair.go](../../internal/team/operator_result_repair.go) | `validateResultRepairTarget` 依舊 reason 拒絕 source failure；grant 與 transition 已有正式入口；`collectionGrantAttempts` 看到 Prepared 就給零新增額度 |
| [result_collection_resume.go](../../internal/team/result_collection_resume.go) | 接續封存工作，不重派 worker；檢查 policy、occurrence、證據與 grant |
| [result_collection_progress.go](../../internal/team/result_collection_progress.go) | checkpoint CAS、回合消耗、格式失敗轉換、grant 額度 |
| [result_collection_events.go](../../internal/team/result_collection_events.go) | 事件驗證、projection、transition；目前沒有一般 blocked → prepared/received 的重新驗證轉移 |
| [result_collection_store.go](../../internal/team/result_collection_store.go) | CAS blob／binding hash／workspace／policy 驗證 |
| [result_collection_work.go](../../internal/team/result_collection_work.go) | 封存原工作、來源 digest 與 generation settings |
| [result_collector.go](../../internal/team/result_collector.go) | 收集與有限次修復 loop；prepared／received／blocked／committed 處理 |
| [result_collection_commit.go](../../internal/team/result_collection_commit.go) | claims decode、完整 admission、occurrence commit；不可直接當成唯讀評估器 |
| [result_admission.go](../../internal/team/result_admission.go)、[result_contract_validate.go](../../internal/team/result_contract_validate.go) | canonical validation 與分類；部分函式會記錄 validation failure，評估模式不能直接造成此副作用 |
| [result_decision_runtime.go](../../internal/team/result_decision_runtime.go) | 已結算決策的原 scope／來源／journal 綁定驗證 |
| [execution_policy_snapshot.go](../../internal/team/execution_policy_snapshot.go) | 原政策完整性與相容性；不能為恢復任意放寬 |
| [resumecmd.go](../../cmd/hufu/resumecmd.go) | branch/run 選擇與 operator repair；現行流程在載入 coordinator 前會 ActivateResumeSource，唯讀預覽不可照搬 |

計畫建立時已有的相關修正：

1. `structuredPayloadForAdmission`／`collectionDecisionSchemaFailure`：辨識 runtime 注入決策後、限於模型可編輯欄位的 canonical schema 錯誤。
2. collector manifest 顯示已驗證的 settled fixed fields 與 canonical item schema。
3. 結果收集指引區分來源 gaps 與 decision-level missing evidence。
4. `TestBoundDecisionConditionalClaimsAreRepairable` 等回歸驗證。

在本計畫開始實作前，這些改動僅改善新失敗的分類，當時**尚未**實作舊 `source_invalid` checkpoint 的恢復。該基線的測試通過不能當成本計畫完成證據。

## 4. 核心設計：判定、授權與執行分開

### 4.1 純讀取恢復評估

新增 team-neutral evaluator；建議型別名稱 `ResultCollectionRecoveryAssessment`，最終名稱依現有命名慣例調整。

輸入必須明確綁定 workspace、branch、source run、task、原 attempt、checkpoint revision/digest。評估器不得：

- 啟用 branch、更新 session projection、發事件、加失敗計數或佔用 occurrence lease。
- 建立 provider、預約模型回合、呼叫工具、重寫 CAS、觸發 schema repair。
- 呼叫會提交結果或更新驗證計數的 admission API 來假裝 dry run。

共用完整的 claims 與來源驗證規則，讓 preview 與 execute 使用相同判定，不複製一套較寬鬆的 schema／來源檢查。對無法純讀取證明的原 context 必須 blocked；不在 preview 執行 artifact materialization、assumption transition、memory event、occurrence commit 等持久化操作。`commit_local` 表示不需新模型即可進入正式提交，並不保證儲存 I/O、objective verification 或最終 acceptance 一定成功。

驗證順序：

1. 支援的 task／transport／workspace 類型、原工作已終止、無活動執行／未知副作用。
2. canonical event lineage 與 checkpoint／receipt identity、原版本可讀性。
3. 工作封存、transcript、response/prepared/accepted CAS 的 bytes/hash／binding。
4. policy、模型 target、pinned schema、run input snapshot、workspace identity。
5. accepted dependency payloads、完整來源 hash 與原 decision provenance；即使模型 claims 不合法，也必須完成可獨立執行的可信來源檢查。
6. 使用原 settled decision 組裝並驗證原 claims；做必要的純驗證，包括來源引用、契約條件、原有允許修復的表示一致性規則。

無法完成的檢查應標示 unavailable/blocker，不能當成通過。若同時存在模型格式錯誤與來源錯誤，來源錯誤優先阻擋；不能被第一個 schema error 掩蓋。

### 4.2 判定輸出

所有固定詞彙使用小寫 snake_case enum。建議 disposition：

| 值 | 判定 | 允許的下一步 |
|---|---|---|
| `commit_local` | 原 claims 目前可通過 canonical admission，或已有合法 accepted result 待完成 | 不新增模型回合，進入原提交／完成驗證 |
| `repair_claims` | 可信綁定有效，僅模型可編輯欄位違反允許修復的契約 | 保留 verdict／來源；提供完整診斷與 keyed-row 對照，使用有限 collector 回合 |
| `resume_collection` | 原工作有效，收集因既有允許的暫時失敗中斷 | 使用原輸入與原 target 接續，未收到的已預約回合仍算消耗 |
| `blocked` | 缺失／不相容／無法證明可修復 | 不發 grant，不呼叫模型；列出精確 blocker |

輸出包含 schema version、完整 task scope、原 reason、新 disposition、validator revision、檢查結果、不可變來源 references/digests、checkpoint digest、已用／剩餘／建議新增回合、assessment fingerprint。fingerprint 必須綁定實際證據與 policy，排除純顯示時間等不穩定欄位。

評估輸出預設不含 report／prompt 全文、credential 或原始敏感內容。診斷要保留欄位位置與型別；不得靠解讀歷史錯誤字串重新分類。

### 4.3 追加恢復決定與授權

原 failure event／receipt、原 collector response 與原 accepted dependencies 保留。新增有版本的恢復決定，至少記錄：原 checkpoint digest、舊 reason、新 disposition、validator revision、assessment fingerprint、原 source references、operator grant 與適用的 invocation。

在持有正確 lease／CAS 條件下，將恢復決定、grant 與 task transition 原子提交；優先擴充現有 transition event payload。若分成多事件，必須定義 journal transaction 與 crash 行為，不能出現「授權已生效但判定尚未保存」的窗口。

`validResultRepairGrant`、event validator、reducer 必須驗證新授權依據，不能仍只呼叫舊 reason 白名單而拒絕有效新 grant，也不能只因欄位寫了 `repair_claims` 就信任它。

重播使用已保存的恢復證據與判定版本驗證歷史轉換，不使用當下的新來源重新決定舊事件。runtime 升級後的新判定只能作為新事件追加。

## 5. 狀態與額度規則

**Prepared 不是合法結果的證明。** 它只表示已解析／準備 claims；完整 schema 可能在決策注入後才失敗。

- `commit_local`：重驗 prepared/response 或 accepted bytes，走既有 admission／完成流程；不增加模型額度。
- `repair_claims`：保留舊 prepared/response CAS；新增狀態事件解除不可重試的 prepared 狀態，轉成可收集狀態，攜帶新診斷。不得重置原 work binding。
- `resume_collection`：先消費 durable response/prepared/accepted，再判斷是否需要新模型呼叫。
- 新模型回合仍先 durable reserve，再發 provider；崩潰或失聯的 in-flight 回合不退款。
- `TurnsUsed` 單調增加。每次明確 operator repair 可授權最多兩個新增回合，沿用現有有界 grant 模式；本地提交與 storage-only continuation 不因 grant 取得新增模型額度。
- 額度由 disposition、原剩餘額度與歷史 grants 共同計算，不能僅由 `Prepared != nil` 決定。
- 同 checkpoint／assessment 的重複命令不得疊加額度；下一次授權須建立在新的失敗進度上並再次評估。
- 完整契約仍不成立時回到 blocked，保留最後診斷與累積用量。

逐一核對 `collectionGrantAttempts`、`collectionTurnLimit`、`reserveResultCollection`、`recordCollectionFormatFailure`、`validateCollectionTransition`、operator grant 與 occurrence lease ordering。不能只改資格白名單。

## 6. 政策與版本相容性

第一版維持既有 policy／contract／source 綁定，不新增 `ignore-drift` 或 `force`。

- runtime 的驗證／分類程式修正可以產生新的 assessment，但必須保留原 result contract 的語意與雜湊。
- 新增獨立的 recovery-validator revision，供診斷與 replay 使用；不要為表示新判定而任意提高 `CollectorSchemaRevision`，使舊工作 CAS 無法讀取。
- 若確實需要儲存格式升級，提供明確舊版本 reader／事件相容測試；禁止在讀取時重寫舊 blob。
- 更動 team 設定／模型／權限／schema 若造成 policy drift，preview 與 execute 都應阻擋並指出差異。
- collector 提示改進需記錄新 request 與執行版本；不得替換舊凍結 request。新提示只能整理原證據，不能帶入新任務、權限或重新判斷 settled verdict。
- 第一版不處理 team policy 遷移。恢復原相容設定與新增明確 migration 是不同操作，不能暗中套用目前 working tree 的語意。

## 7. CLI 與操作體驗

以下介面已加入工作樹；完整驗證結果見第 12 節：

```bash
hufu resume --workspace <workspace> --branch <branch> --run <source-run> \
  --repair-result <task-id> --dry-run --output json

hufu resume --workspace <workspace> --branch <branch> --run <source-run> \
  --repair-result <task-id> --model <original-target> --report
```

`--dry-run` 僅用於明確 result repair 的恢復評估；拒絕歧義組合，不能沿用一般 run 的 preview 而誤啟動工作。

預覽必須在 `ActivateResumeSource`、coordinator 啟動副作用、session projection 修復之前執行。若唯讀 reader 無法載入所需資料，先重構讀取入口。不能因寫入的是「診斷資料」就宣稱唯讀。

執行命令本身就是本次有界修復的明確授權；一般 resume 或 `--auto-approve` 不隱含授權舊失敗重分類。執行時重新評估，與當前 checkpoint 綁定後 CAS 提交，不能信任先前輸出的 JSON 檔案作為授權。

人類輸出至少說明：可沿用的已接受結果、所選任務、恢復方式、預計新增回合上限、是否會繼續 coordinator／尚未派發的後續任務，以及 blocker。JSON 有版本與穩定 enum。

成功修復一項不等於整體 review 完成。保留既有 workflow、驗證與 acceptance；只有同一 lineage 且驗證有效的依賴可重用。

## 8. 實作階段與交付

所有階段預設由 coding agent 依序完成，不自行啟動平行 agent 或重複 heavyweight validation。

### PR-1：純讀取評估與分類邊界

- [x] 盤點本工作樹既有修正及相關 AGENTS.md／runtime skill。
- [x] 抽出不記錄 counters／不提交的 canonical validation 路徑，供預覽與執行共用。
- [x] 建立 typed assessment、enum、fingerprint、版本與來源檢查結果。
- [x] 完整可信來源驗證先於可修復分類；複合錯誤 fail closed。
- [x] 增加舊 `source_invalid` → `repair_claims` fixture，以及真正來源失效仍 blocked 的負例。

### PR-2：durable 授權、狀態轉換與接續

- [x] 追加恢復決定，原子綁定 operator grant 與 task transition。
- [x] 更新 grant validator、event validator/reducer、checkpoint transition 與 replay。
- [x] 修正 Prepared／額度判定；保留原 budget 消耗與 response。
- [x] 將可修復狀態接入既有 collect loop，不重派 worker／decision primitive。
- [x] 覆蓋取消、crash、併發、重複命令與成功提交後的完成驗證。

### PR-3：CLI、投影與文件

- [x] 新增唯讀 preview；確認不能 ActivateResumeSource 或啟動模型。
- [x] 接入 `resume --repair-result`，明確選定 branch/run/task，提供可操作 blocker。
- [x] 更新 status／inspect、CLI JSON、report、相關 TUI 狀態投影；missing 不顯示成零。
- [x] 更新 execution-runtime、CLI reference 與範例，說明新舊 reason、額度與不支援範圍。
- [x] 驗證未接受的結果不會進入下游；accepted 依賴沿用同一 provenance。

### PR-4：完整回歸與交接

- [x] 以通用的合成 fixture 重現條件 schema 錯誤與舊失敗分類，避免把具體 team 名稱、task ID 或 schema 欄位規則寫入 core。
- [x] 完成第 9 節矩陣與第 10 節驗證。
- [x] 記錄實際修改、測試命令／exit code、相容性限制及尚未完成項目。
- [x] 真實 workspace 恢復另需使用者明確指示；不得以 fixture 通過宣稱歷史 run 已恢復。

## 9. 必要測試矩陣

使用 `newTestCoordinator`、`scriptWorker` 與現有 result collection fixtures。新測試以行為命名、使用可清理的暫存目錄，不依賴真實 provider。

| 情境 | 必須驗證 |
|---|---|
| 舊 source_invalid、決策後條件錯誤 | evaluator 分為 repair_claims；原 worker 呼叫 0；decision 推論 0；collector 修正後正常驗證 |
| inherited gaps | 原 source payload bytes/hash 不變；settled verdict 不變；限制留在 summary/source；不能靠刪 gaps 通過 |
| 合法 prepared／accepted | 本地提交或繼續完成驗證；模型 0 次；不重置原 attempt |
| 非法 prepared | 可取得正確有界修復機會；不因 Prepared 存在而假定無需模型；舊 CAS 仍可追溯 |
| 真正來源問題 | 缺失 transcript、hash mismatch、錯 branch/run/task、決策缺失／不符、schema drift 均 blocked；模型 0 次 |
| 保留來源與計數 | evaluator 驗證所有保留的完成結果，含非直接依賴，並排除 primary occurrence；驗證失敗時 AcceptedTasks 保持 unknown。`TestResultCollectionRecoveryValidatesUnrelatedRetainedSources` 驗證無效來源不得被計為可重用 |
| 複合錯誤 | schema 格式錯誤加來源引用／receipt 錯誤不可被降級成可修復 |
| 權威欄位覆寫 | 即使值相同也拒絕；不得允許模型覆蓋 fixed verdict、身份、receipt 或來源欄位 |
| 副作用／不支援類型 | 工作未完成、未知副作用、isolated、fanout 等不進入新恢復路徑 |
| preview | 前後持久化內容、active branch、task status、計數、CAS 不變；模型／工具呼叫 0 |
| preview 後資料變更 | 執行時重新驗證，stale assessment/CAS 拒絕 |
| 雙 resume／重複 grant | 最多一個有效 transition/dispatch；回合不重複增額；lease ordering 正確 |
| crash 點 | 判定/授權提交前後、reserve 後、response 後、accepted 後；不丟已接收資料、不重派 worker、不退還失聯回合 |
| 修復耗盡／取消 | 保留用量與診斷，回到 blocked；一般 resume 不隱性續額 |
| 歷史 replay | 原失敗仍存在；新事件重建相同狀態；新版 evaluator 不改寫舊事件語意 |
| 後續 workflow | 已接受依賴不重跑；僅有合法修復結果解鎖後續；最終 acceptance 仍可能拒絕 |
| 相容與呈現 | 舊 checkpoint 可讀；legacy/external/action 行為不退步；CLI/JSON/report 與 canonical outcome 一致 |

事件／來源 fixture 應保存舊 reason 與 prepared claims，確實跨 coordinator reopen/replay 測試；只在記憶體改 reason 的單元測試不足以證明可恢復歷史任務。

## 10. 驗證與資源規範

遵守根 AGENTS.md。每個實作任務只建立一個 `/tmp/codex-task.XXXXXX`，指定工具 subprocess 的 TMPDIR，保留既有 GOTMPDIR／build／module／lint caches。先檢查 `/tmp` 與 cache volumes、可用 RAM；heavyweight 驗證序列執行，Go 使用 GOMAXPROCS=2 與 `-p 2`。不和真實 team run 同時執行。

實作驗證至少包含：

```bash
# 先執行實際受影響的 focused tests，使用具體測試名稱。
mise exec -- go test -p 2 ./internal/team -run '<recovery tests>'
mise exec -- go test -p 2 ./cmd/hufu -run '<resume preview and execution tests>'

# 併發/CAS 改動需 targeted race；具體名稱以新增的行為測試替換。
mise exec -- go test -race -p 2 ./internal/team -run '<concurrent recovery tests>'

mise run test
mise run vet
mise run lint
git diff --check
```

命令中的測試名稱是佔位，不可原樣執行並以「沒有匹配測試」當作通過。保存實際命令與結果；lint 必須成功。沙箱阻擋 loopback/cache 時使用授權機制，不新增重複 cache。必要檢查完成後清理 task-owned 暫存物；保留交付文件。

## 11. 完成定義與停下條件

完成須同時具備：可唯讀評估、可追加授權恢復、歷史錯誤可追溯、可信來源不被放寬、有限額度、已完成工作不重跑、原 workflow/acceptance 正常接續，以及全矩陣與必要 lint 通過。

遇到以下情況先補設計，不得用局部 workaround 宣稱完成：

- 唯讀評估需要修復 projection／改 active branch 才能成立。
- 無法區分模型欄位錯誤與可信來源錯誤，或缺少完整來源驗證。
- 需要忽略原 policy／contract hash 才能恢復。
- 需要改歷史失敗事件、減少 TurnsUsed、覆蓋原 response 或重跑已完成工具。
- 恢復 grant 與判定不能原子化，或 replay 無法證明原授權。

第一版完成後，才另行評估 policy migration、isolated/fanout 恢復與更多一般失敗類型；不要讓這些擴充阻塞本次明確的 result collection 範圍。


## 12. 工作樹實作與交接紀錄

本輪實作沿用原 result-only repair 與 task transition，新增：

- `result_collection_recovery.go`：共用唯讀 evaluator、版本化 assessment、checkpoint／來源／policy 檢查、fingerprint 與 replay 驗證。
- `result_collection_recovery_validation.go`：在 dry-run projection 載入純驗證用的 decision catalog 與政策；不建立 provider transport，不啟動推論。
- `operator_result_repair.go`、`coordinator_eventstore.go`、`result_collection_recovery_projection.go`：重新評估舊 failure reason，assessment／grant／submission transition 同事件提交，並以 journal head 做跨 process CAS fence。
- `result_admission.go`、`result_contract_validate.go`：共用不更新 rejection counters 的 claims validation，獨立來源檢查先於可修復的 canonical schema 分類。
- `resumecmd.go`：在 ActivateResumeSource 前執行 preview；managed preview 不取得寫入 lease，dry-run setup 不啟動 workspace versioning。
- CLI JSON／Markdown report／inspect 保留恢復 disposition、原 failure reason 與 fingerprint；既有 Todo detail 供 status／TUI 顯示同一 canonical transition。

支援 `commit_local`、`repair_claims`、`resume_collection`、`blocked`。合法 Prepared 不增額；非法 Prepared 依 assessment 開啟有限 collector 修復，舊 Prepared CAS 仍保存在 assessment。已預約回合不退款。新版 grant 必須帶 assessment，舊 grant 保持 replay 相容。

恢復後仍有其他 unresolved tasks 時，沿用既有 explicit repair 的停止規則，先檢查它們再執行普通 resume；若沒有其他 unresolved tasks，才繼續 coordinator 與尚未派發的工作。preview 的 `continues_workflow` 與人類輸出會反映此差異。

目前實作的保守限制：來源相依 claims 形狀壞到無法獨立驗證其引用時仍 blocked；第一版對非空 assumption checks 或 memory attribution claims 明確 blocked；這些宣告需要先完成原 context 的獨立 reconciliation，不能透過新 collector 刪除它們來取得授權。這些不會透過新模型回合猜測或補造。缺少 accepted-task 計數時輸出 unknown／省略 JSON 欄位，不當成零。

測試使用合成事件、CAS、scriptWorker、假 decision generator 與本機 HTTP collector fixture；未呼叫真實 provider，使用隔離的測試 workspace。驗證命令與結果在下表追蹤；以下結果均已完成。

第一輪完整測試發現既有 `TestRuntimeEvidenceBindingSubmissionAndReplay/local` 的 `done: "true"` 相容性回歸：新增的來源檢查比既有 scalar normalization 更早執行。已以 `validateAssembledEvidenceInputs` 共用相同 normalization，並確認該案例及來源／恢復 focused tests 通過。

| 驗證 | 結果 |
|---|---|
| focused recovery／decision／resume／inspect／normalization tests | exit 0；實際案例含 `TestResultCollectionRecoveryRunsOnlyCollector`、`TestResumeRecoveryPreviewDoesNotActivateBranch`、`TestRuntimeEvidenceBindingSubmissionAndReplay` |
| 最終 focused race：全部 recovery／decision／resume／frozen-input hash tests | exit 0；team 51.085 秒，CLI 42.356 秒；包含雙 process、collector-only、CLI 成功／耗盡 |
| `mise run lint` | exit 0，`0 issues`；一處 whitespace 問題已修正 |
| `mise exec -- go test -p 2 -timeout 20m ./...` | exit 0；`internal/team` 587.614 秒，CLI 120.591 秒；20m 為每套件上限 |
| `mise run vet` | exit 0 |
| 文件相對連結與 `git diff --check` | 通過 |


實際執行的主要驗證命令（全部使用 GOMAXPROCS=2；TMPDIR 指向本任務私有目錄，交付前清理）：

```bash
mise exec -- go test -p 2 ./internal/team ./cmd/hufu -run 'TestRuntimeEvidenceBindingSubmissionAndReplay|TestRuntimeEvidenceBindingAggregateCapacity|TestResultCollectionRecovery|TestResultCollectionDecisionRecovery|TestResultCollectionGrantCanContinueEarlierDurableProgress|TestResultCollectionCommittedGrant|TestBoundDecisionConditionalClaimsAreRepairable|TestResumeRecovery' -count=1
mise exec -- go test -race -p 2 ./internal/team ./cmd/hufu -run 'TestResultCollectionRecovery|TestResultCollectionDecisionRecovery|TestResumeRecovery|TestResultCollectionFrozenInputHashCompatibility' -count=1
mise exec -- go test -p 2 -timeout 20m ./...
mise run vet
mise run lint
git diff --check
```

上述驗證命令與第 9 節所需的第一版驗收均已完成；直接證據與支援邊界見第 13 節。沒有保留本輪可丟棄的 binary、log 或 test workspace。未執行真實歷史 run 恢復，也未新增第一版非目標中的 policy migration、isolated／fanout 等能力。

### 前輪缺口的補完狀態

前輪列出的跨 coordinator 重啟、雙 process 授權、完整中斷邊界、下游／acceptance、CLI scope／unknown 與複合來源錯誤，均已補上直接驗收測試，見第 13 節。純讀取檢查與正式提交的責任也已逐項對照；第一版不支援的 context 宣告明確 blocked，不能透過 collector 刪除原宣告來取得授權。

最終 focused race、完整回歸、vet 與 lint 全部通過；第 8 節第一版驗收已完成。工作樹尚未提交，因此 Verified-Commit 保留為 —。真實歷史 run 的恢復仍是另外的操作。


## 13. 補完驗收紀錄（已完成）

本輪已補齊先前交接列出的缺口，最終 gate 均通過。CLI 正向 fixture 另外抓到並修正三個整合缺口：runtime direct-agent 未終結 run、direct task／receipt 漏綁已解析輸入、collection binding 錯把 canonical `sha256:` 輸入 hash 當成格式錯誤。未放寬 frozen inputs／policy／來源驗證。

| 原缺口 | 實作／直接驗收證據 |
|---|---|
| 真正重啟與舊 source_invalid | `reopenRecoveryCoordinator` 關閉 store 與 coordinator，重建政策／驗證 catalog，從事件恢復 tasks；`TestResultCollectionRecoveryRestartsAtDurableBoundaries` 涵蓋 grant 前後、reserve、response、prepared、accepted；`TestResultCollectionRecoveryRunsOnlyCollector` 在授權前後都重啟，沿用原決策，worker 0 次 |
| 雙 process resume | `TestResultCollectionRecoveryCompetingProcesses` 以同一測試 binary 啟動兩個獨立程序，兩者完成評估後同時授權；驗證只有一份 grant／transition 可重播，不複製 binary |
| 下游與 acceptance | `TestResultCollectionRecoveryPreservesDownstreamAndAcceptanceGates` 驗證 blocked／只授權時不暴露來源，提交後沿用原 attempt／限制，執行下游一次；缺交付物拒絕，交付物存在時通過 |
| 預覽識別與 unknown | assessment 保存 task／branch／source-run；文字顯示 scope；無 checkpoint 時 JSON 省略回合欄位。`TestResumeRecoveryAssessmentOutputPreservesUnknownAndBlocked` 檢查 unknown 與 scope；`TestResumeRecoveryPreviewThenExecute` 使用本機假模型，涵蓋成功與修復耗盡；普通 resume 不增加 grant／回合、不重派 worker |
| 複合錯誤 | `independentRecoveryClaims` 分別解碼來源與 receipt 相關欄位；summary／status 類型錯誤不能掩蓋 forged receipt。`TestResultCollectionRecoveryIndependentClaims` 覆蓋複合錯誤與 context 拒絕 |
| sidecar preview | 使用未綁定 runtime 的 lazy generator，只提供相同 model identity 與 catalog hash，不能建立 sidecar；`TestResultCollectionDecisionRecoveryPreviewRetainsEvidenceWithoutSnapshot` 驗證 sidecar catalog 可純讀取驗證且不重做推論；沒有 task-bound snapshot 時明確 blocked，完整正向入口另由 CLI fixture 驗證 |
| 版本化證據 | 新 assessment 使用 validator revision 2，要求 scope、canonical target、terminal/context manifest 與獨立 claims 檢查；revision 1 仍依原 proof 規則讀取。`TestResultCollectionRecoveryLegacyAssessmentFingerprint` 驗證舊資料相容 |
| native CLI scope／終結與 hash | `coordinator_run.go` 在 runtime direct-agent 返回時完成正式 run finalization，並將已解析 snapshot 綁定到 task／receipt。`result_collection.go` 接受 canonical `sha256:` 格式並保留 bare hash 讀取；`TestResultCollectionFrozenInputHashCompatibility` 驗證 hash 原樣 round-trip，CLI fixture 實際執行有宣告輸入的工作與恢復 |
| context／副作用 | 與 restore 共用 `collectionContextManifest`；同時檢查 canonical target，避免僅依 live projection。唯讀載入 terminal metadata，不取得 process ownership。`TestResultCollectionRecoveryRejectsUnreconciledContext` 覆蓋未知／部分副作用、paused checkpoint、active discipline、running／unknown／failed terminal |

### 純讀取與正式提交的責任對照

| 檢查／操作 | Preview／授權評估 | 正式提交 |
|---|---|---|
| scope、policy、contract、CAS、來源、settled decision | 完整驗證；失效 blocked；公開 CLI 必須有與 task 相符的 canonical frozen inputs | 沿用原 checkpoint，再次驗證 |
| claims schema／canonical payload／invariant assertions | 共用 `validateClaims`、來源 binding、attestation 與 finalizable contract；不加 rejection counters | 經普通 admission，不能靠 preview 取代 |
| artifact declarations | 共用 `validateSubmittedArtifactClaims`，檢查來源存在、範圍與 runtime-owned provenance | reservation 後 materialize，I/O 失敗仍拒絕 |
| context manifest | 共用 `collectionContextManifest` 檢查原 fingerprint | restore 時重新檢查 |
| memory／assumption claims、active discipline | 第一版明確 blocked，保留原 claims 與理由 | 不以新 collector 猜測、刪除或補造它們 |
| terminal／副作用 | 原 run 的 metadata 必須可證明已結束且無待 reconciliation 問題 | 保留普通 admission／completion 檢查 |
| occurrence lease、持久化、workflow verification、acceptance | 不執行、不承諾結果 | 經原有交易與完成 gate；仍可能失敗 |

### 最終驗證與交付

- `mise exec -- go test -race -p 2 ./internal/team ./cmd/hufu -run 'TestResultCollectionRecovery|TestResultCollectionDecisionRecovery|TestResumeRecovery|TestResultCollectionFrozenInputHashCompatibility' -count=1`：exit 0。
- `mise exec -- go test -p 2 -timeout 20m ./...`：最終 exit 0；team 587.614 秒、CLI 120.591 秒。前一輪唯一失敗是生成文件同步；已將額外說明保留在 architecture 文件，恢復 reference 的生成格式後重跑通過。
- `mise run vet`：exit 0。
- `mise run lint`：最終 exit 0，`0 issues`；過高的 claims evaluator 複雜度已透過分離獨立來源驗證修正。
- `git diff --check`、文件相對連結與 core／team 邊界檢查：通過。沒有新增具體 team 名稱的 core 分支。

所有 heavyweight 命令依序執行，GOMAXPROCS=2、Go `-p 2`，沿用原 caches／GOTMPDIR。任務私有 TMPDIR 在所有消費者結束後清理；未保留可丟棄的 binary、log 或測試 workspace。未提交變更，亦未操作原失敗 workspace 或呼叫原 cloud model。

## 14. 通用輸入綁定與歷史相容性

### 根因與設計要求

若只在 `materializeTaskActions` 替換 action payload 時寫入 snapshot identity，一般 worker、sidecar 與不替換 payload 的 action 就可能缺少輸入綁定。輸入 provenance 必須在共用的任務建立邊界驗證與保存，不能依賴特定工具或 action 的附帶處理。

恢復時必須區分 snapshot 不存在、task 未綁定，以及既有綁定不一致；run 中存在 snapshot 並不足以證明某個 task 的 sealed work 使用了它。

### 修正範圍與相容性

- `task_run_inputs.go` 共用綁定與驗證：先驗證 snapshot 完整性、team、schema 及既有 task binding；拒絕部分或不同綁定，不覆蓋既有 identity。
- 所有任務建立入口在 decision admission 前綁定，包含批次委派、直接呼叫、`request_agent`、agent TODO 與已有 snapshot 的取消紀錄。action materialization 也先驗證，避免覆寫不一致的 binding。
- 持久化與執行 admission 再次檢查 task 的原始綁定；沿用 TodoSpec、事件 reducer、收據與 collection checkpoint 已有欄位，不另造可繞過的狀態。
- 新 input context 到達 admission 時，即使沒有宣告 inputs，也保存 canonical 空 snapshot，使後續 collector recovery 有可驗證的 identity。同一 context 的後續無-input 呼叫沿用該 snapshot 並保留 origin run；不建立會使已完成來源失配的另一份空 snapshot。未知的 input assignments 仍拒絕。
- audit 接受一般 invocation-bound worker 的原始收據，不再把 action materialization 或 request acceptance 當成唯一輸入綁定方式。action payload／output 的既有驗證保留。
- inline direct／subagent 保存自己的實際工具 callback 與 assistant output，封存 transcript 後寫入輸入與 result-contract 綁定的收據；不沿用父任務 recorder，也不從摘要補造工具證據。這修正了新輸入綁定揭露的原有 transcript 缺口。
- 一般 replay 與後續呼叫保留舊無-input context 的既有 unbound 形式，包含所有舊任務已完成的情況，不補造歷史 snapshot。明確的 resume／result-only recovery 仍要求 canonical inputs；typed-input 舊任務不適用此相容分支。
- targeted retry／reconciliation 選取 task 原本綁定的 snapshot，而不是建立新 snapshot 或重新解析目前輸入。舊無-input 任務仍可沿用既有操作；部分、遺失或不一致的 typed binding 明確拒絕。
- 舊 checkpoint 的診斷明確區分「snapshot 不存在」與「task 沒有綁定」，不再指示以找回 snapshot 或新 `--input` 充當修復。

### 舊 checkpoint 的支援邊界

缺少歷史 task binding 時，單一 run snapshot 不能替 sealed work、已接受來源及收據補上驗證。目前不支援自動歷史補綁；恢復評估應回傳 `blocked` 與具體可理解的原因，保留原始失敗、已完成成果及報告。

若未來加入 migration，必須以原始 canonical 證據驗證 snapshot、task、來源與 policy 的關係，並原子地追加修復紀錄。重新解析現今任務、提供新 inputs、手改 checkpoint 或略過 frozen-input gate 都不能取代歷史證明。此限制不代表所有舊任務皆不可恢復；已有完整且相容綁定的任務仍依一般恢復契約評估。

### 驗證追蹤

本機假模型 CLI 測試擴充為 typed／empty inputs、直接呼叫／coordinator 委派，以及 collector 修復耗盡；成功案例檢查 worker 只執行一次、沿用原 work CAS，task／receipt／checkpoint snapshot 一致。另有建立與執行拒絕 drift、子任務／TODO replay、舊 snapshot 不自動補綁，以及 audit 收據竄改的回歸案例。

完整回歸找到 audit 對一般 worker 的過窄假設、舊 no-input replay 被誤擋，以及 targeted recovery 應先驗證必要資源鎖的順序問題；均已針對各自責任修正。補充的 input-resolution 測試另外重現後續無-input 呼叫建立不同 snapshot、以及舊 context 全部完成後被補造 snapshot；修正後沿用既有空 snapshot／origin，legacy context 保持未綁定。

| 驗證 | 結果 |
|---|---|
| focused 功能／相容性測試（team、audit、eval harness、CLI） | exit 0；包含第一輪完整回歸的受影響入口 |
| focused race（輸入綁定、來源、context、CLI 恢復、舊 replay） | exit 0；team 86.850 秒、audit 1.156 秒、CLI 96.135 秒；含空 input context 補充案例 |
| `mise run lint` | 最終 exit 0，`0 issues`；inline 收據收尾已依職責拆分，未加入排除規則 |
| `mise run vet` | exit 0 |
| 固定程式碼的最終完整回歸 | exit 0；team 604.653 秒、CLI 138.992 秒、eval harness 43.493 秒 |

主要命令（全部沿用原 caches／GOTMPDIR，GOMAXPROCS=2，TMPDIR 指向本任務私有目錄，重量級命令依序執行）：

```bash
mise exec -- go test -race -p 2 ./internal/team ./internal/auditverify ./cmd/hufu -run 'TestEmptyRunInputs|TestTaskRunInputs|TestInterruptedRunInputs|TestAgentCreatedTodos|TestVerifyRunInputBindingDimension|TestRequestAgentToolUsesTyped|TestResolveRunInputSnapshot|TestCoordinatorMaterializesAction|TestResumeRecovery|TestResultCollectionRecovery|TestPreparedCandidateInterrupted|TestDryRun|TestRequestAcceptanceDryRun|TestRuntimeActionBinds|TestCoordinatorRuntimeActionEmits|TestRunDirectAgent|TestRetryTask|TestReconcileTaskDoesNotReplay|TestTargetedRecovery|TestRunStopsAfterRecovery|TestRunReconcilesRestored|TestDecisionRuntimeFiles' -count=1
mise run lint
mise run vet
mise exec -- go test -p 2 -timeout 20m ./...
```

上述 race 驗證完成後，lint 要求將 subagent 收據收尾拆成函式；此職責拆分沿用相同呼叫順序，另以後續完整回歸、vet 與 lint 驗證。
