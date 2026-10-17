# 定性 belief 基礎

> Status: active
> Authority: reference
> Verified-Commit: `2389ae726f394fda2897a786369a9764920ee06d`
> Supersedes: —
> Superseded-By: —

`internal/belief/qualitative` 是不需要機率模型的獨立 Go 套件，提供具來源資訊的
`evidence`、`hypothesis`、`relation`、`gap`、`retraction` 型別、嚴格輸入驗證及
決定性投影。它不修改 [既有數值 belief](belief-state.md)，沒有啟用任何 team。
本階段不是完整 belief runtime，也不是 PR #19 所提的診斷派工迴圈。

## 已實作的 API 與界線

- [Go function: Decode](../../internal/belief/qualitative/validate.go)：讀取一個 strict JSON
  `Input`，拒絕未知欄位、重複 JSON key、null、大小越界與無效型別；`records` 必須明示，
  沒有 records 可用 `[]`。不將空字串、缺值或未知值補成已知事實。
- [Go function: Validate](../../internal/belief/qualitative/validate.go)：typed Go callers
  也使用同一驗證契約；typed `Records: nil` 與空集合等價。
- [Go function: Project](../../internal/belief/qualitative/project.go)：從完整輸入集合
  重建新 `State`，不使用前一次 cache，不修改輸入；錯誤時不回傳部分 state。
- [可執行 Go 範例](../../internal/belief/qualitative/example_test.go)：建立 evidence、
  hypothesis、support 與 gap，再撤回 evidence，從 `supported` 變成 `unknown`。

套件沒有 I/O、clock、模型呼叫、機率、factor inference、授權、task 成功判定或 dispatch。
沒有 SQLite/event journal 串接、持久化服務、runtime adapter、team 設定或 CLI。
`hufu inspect beliefs` 仍只讀既有數值功能，不會顯示此套件的投影。沒有資料遷移需求；
沒有變更 production execution path、既有 model/policy schema 或 default behavior。

## 輸入與來源契約

`Input` 固定 `schema_version: 1`、`reducer_version: qualitative_v1`、`Scope` 與
`Records`。每個 `Record` 有 ID、kind、完整 Scope、Source，及恰好一個符合 kind 的
payload。[型別定義](../../internal/belief/qualitative/types.go) 是目前 canonical API。

| 類型 | 意義與驗證 |
| --- | --- |
| `Scope` | `branch_id`、`subject_id`、`subject_revision`、`environment_revision` 全部必填；每筆 record 必須完全相符 |
| `Source` | `id`、`revision`、`reference`、小寫 64 字元 SHA-256 `digest` 必填；同一 source ID 在集合內不得換 revision、locator 或 digest |
| `Evidence` | `kind` 明確區分 `fact`、`interpretation`、`objective_verification`；`knowledge` 是 `known` 或 `unknown` |
| `Hypothesis` | 一個待解釋的 `claim`；不要求互斥或完備，不宣稱是根因 |
| `Relation` | 明確指向一個 hypothesis 與 evidence，以 `support` 或 `counterevidence` 表達有來源的解讀，並必須有 rationale |
| `Gap` | 指向 hypothesis 的具名待回答問題；不會自動提出或執行 action |
| `Retraction` | 指向同集合內非 retraction record 的永久 tombstone，附來源與 reason |

ID/revision 使用 1–128 bytes 的 ASCII 英數、`_`、`-`、`.`、`:`。`reference` 是
最多 512 bytes 的 opaque locator，不解析為路徑或 URL。claim/statement/rationale/
question/reason 最多 4096 bytes，必須為非空白 UTF-8，禁止除換行與 tab 以外的控制字元。
每份輸入最多 1 MiB、2048 筆含重複的 records、512 筆不同 ID 的 records；typed calls
也檢查序列化後大小。驗證錯誤僅回傳穩定 code 與欄位分類，不回顯來源內容。

source digest 的格式正確不表示來源可信：本套件不開啟 reference、不比對原始 artifact
bytes、不驗證簽章、runtime receipt、branch lineage 或 ACL。所有 scope/source 值都是
呼叫端提供的宣告。未來 adapter 必須先完成來源授權、hash/receipt/revision 驗證、秘密
遮罩及 durable admission，才可把投影接到其他系統。套件不遮罩 statement，也不應直接
將未審查的內容保存或顯示給使用者；保留原始輸入避免偷偷重寫來源 identity。

`fact` 是來源所報告的事實，不是套件自行證明的真理；`interpretation` 保留推論性質。
`objective_verification` 必須附 `receipt_id`、`verifier_revision` 與
`outcome: pass|fail|unknown`；只有這個 kind 可以有 verification 欄位。這些仍是待上游
驗證的 reference，填入欄位本身不能取得可信 receipt。pass/fail 不自動支持或反駁
hypothesis，必須另有明確 relation，並保留該 relation 自己的來源。

已知 evidence 必須有 statement；unknown evidence 禁止 statement，不能把不明結果
假裝成可用敘述。verification outcome `unknown` 必須搭配 knowledge `unknown`，反之
亦然。timeout、缺失或損壞的結果如何轉成未知值，是尚未實作的 adapter 責任；本套件
不把 `timeout` 當作 `fail` 或合法 outcome。未知 evidence 的 relation 只進 pending list。

## 投影、衝突與撤回

每個 hypothesis view 包含 `support_ids`、`counterevidence_ids`、
`pending_relation_ids`、`gap_ids`。列表指向完整 records，來源與 rationale 不會丟失。
`State.Records` 是排序、去重後的完整 evidence/hypothesis/relation/gap/retraction
投影，保留已撤回內容供稽核；`retracted_ids` 明示被撤回者。

| Status | 僅描述有效的明確關聯 |
| --- | --- |
| `unknown` | 沒有有效 support 或 counterevidence；pending/gap 仍可存在 |
| `supported` | 至少一條已知、未撤回 evidence 的 support，沒有有效 counterevidence |
| `countered` | 有有效 counterevidence，沒有有效 support |
| `contested` | 兩種立場同時存在，不能靠多筆 support 把反證投票消除 |
| `withdrawn` | hypothesis 本身已撤回，active lists 為空，歷史 records 仍保留 |

這些 status 不代表 truth、confidence、驗證通過或 task success。有 gap 仍可能是
`supported`；它表示「存在支持且仍有問題」，不表示問題已解決。相同來源的多個 record
或不同 ID 的同義語句不做語意去重，也不作為獨立樣本或增加權重。反證是顯式 relation，
不使用自然語言相似度找矛盾；identity conflict 則是無效輸入，兩者不可混用。

同 scope 中所有 kind 共用 record ID namespace。相同 ID、相同內容的重送是 no-op；
不同內容（包括來源或 payload kind）回傳 `identity_conflict`，沒有 last-write-wins。
來源 ID 也採不可變 identity；新來源版本必須明確使用新 source ID，subject/environment
版本改變則必須建立獨立 Scope 的完整輸入，不偷偷帶入舊投影。

任何 relation/gap/retraction 的 reference 必須存在且種類正確；即使該 record 已撤回，
也不能略過 schema 與 reference 驗證。撤回 evidence 會將仍有效的 relation 列為 pending；
撤回 relation/gap 會移出 active lists；撤回 hypothesis 會標示 withdrawn。retraction
不能指向另一個 retraction 或自身，也不支援撤銷 tombstone。修正需新 record ID。

## 重播與輸入順序

本 API 接受完整集合，不是 append service 或增量 event reducer。records 可以在其
引用目標前面出現；只要完整集合能解引用即可。所有 records、lists 與 tombstones 都
使用穩定 ID 排序，相同完整集合的任何順序與精確重複 delivery 產生相同 state/digest。
撤回即使出現在原 record 前也有效；之後重送原 record 不復活它。

`input_digest` 綁定 version、scope 與去重後的完整 records；`digest` 再綁定投影與
input digest，採 domain-separated deterministic JSON SHA-256。它們用於重播一致性，
不是來源真實性的證明；省略歷史 tombstone 再呼叫 API 會得到不同集合的投影，套件本身
不儲存歷史，不能偵測呼叫端刪掉了歷史。未來 journal adapter 必須提供完整、已驗證的
branch lineage 與撤回紀錄，不能把本 API 的集合排序當成事件時間、因果次序或權限。

此版本沒有 source retraction cascade：要撤回來源的所有 claims，呼叫端必須逐筆提供
retraction。沒有自動過期、gap resolution、可信度排序、dependency graph、數值 bridge
或動作選擇；這些需要各自的 admission/replay 契約與測試後才能串接。

## 驗證範圍

單元與可執行範例涵蓋 typed/JSON validation、size limits、三種 evidence 性質、
未知值、顯式反證、重複／衝突、source/revision/branch 不符、referential integrity、
tombstone 不復活、重排與 JSON 往返重播、input/output 不互相 alias，以及 concurrent
readers。fuzz target 驗證任意可接受 JSON 都可穩定重播。既有 `internal/belief` 的數值
測試照常執行；不以這些純函式測試宣稱已完成 runtime、SQLite 或 CLI 整合。
