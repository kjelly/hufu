# Universal Decision Runtime 精確契約附件

這是已完成實作的 Universal Decision Runtime V2 契約與機械可驗證附件，現封存為 implementation record。內容只包含 coding agent 能以 repository 程式碼、fixtures、fake backends 與自動化測試完成的工作；不以人工核准、production 操作或 live provider 作為完成前置條件。

實作依契約的 PR-0 至 PR-7 順序完成，對應 commits：`b789109`、`c6113c0`、`6fce6e2`、`bebeee8`、`3a74969`、`61ac888`、`6595894`、`d3198ef`。本目錄保存當時的 strict schemas、fixtures、profile literals 與原始 implementation plan；目前 runtime 行為與後續維護仍以 repository 程式碼及測試為準。

## 內容

- `*.schema.json`：完整 strict wire schemas（JSON Schema 2020-12）；`runtime-contracts.schema.json` 補齊 requirement、runtime occurrence、terminal proof、manifest proof 與 `run_finished` decision extension。
- `*-v2.json`：三份不可變 V2 profile bundle literals；沒有繼承或隱藏預設。
- `bundle-digests.json`、`canonical-test-vectors.json`：本版新 digest domain 的測試向量。
- `fixtures/`：合成正反例與對應內容定址 artifact；不代表真實 run。
- `validate_contract_schemas.go`：repository-native schema與schema-level fixture checker。
- `validate_contract_examples.py`：selected cross-field invariant與digest checker；有`jsonschema`時也重驗schemas。

```bash
go run validate_contract_schemas.go
python validate_contract_examples.py
```

Go validator 使用repository既有module dependency，驗證所有schemas及schema-level正反例；Python validator另驗cross-fieldsemantics與digests。Python環境若有`jsonschema`會重複驗schema，沒有時自動委派給前一個Go command，不需要安裝套件。兩者全程不呼叫網路或模型。這些測試不涵蓋 Hufu Go compilation、event reducer implementation、scheduler exclusion、tool isolation、signal 或磁碟crash-recovery；那些都必須由codingagent使用repositorytests、fakebackends與faultinjection完成，不要求真實provider或production環境。

`profile-bundles.schema.json` 用 exact constants 驗證內建 catalog，刻意不接受任意 preset overlay；它不是自訂 inline policy 的通用 schema。

前版文件雜湊保存在 `source-spec.sha256`。
