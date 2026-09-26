# MCP-backed ActionProvider（唯讀 MVP）：可實作規格 v2

> Status: draft（Ready for implementation；實作中，完成後移到 `docs/archive/implementation-plans/`）
> Authority: normative（實作計畫；完成後 canonical 文件為 `docs/reference/action-providers.md`，程式碼註解只引用正式文件）
> Verified-Commit: `1a244b3`
> Supersedes: —
> Superseded-By: —
> Target: `kjelly/hufu` main
> Baseline: `1a244b3`（本文所有 `file:line` 以此 commit 為準；行號會漂移，以函式名稱為主）
> Date: 2026-09-26（v2：依查核結果與使用者決策改寫初稿）
> Scope: 在既有 ActionProvider 增加 MCP transport，供 static action task 與 action catalog task 使用

## 0. 決策紀錄

使用者決策（2026-09-26）：

- **D1 啟動可用性**：任何已設定的 MCP ActionProvider 在啟動時無法綁定（manager 不存在、server 載入失敗、tool 不存在或被排除、schema 無法編譯），整個 run 在任何 model/action call 前失敗。不做「只停用該 action」的降級模式。
- **D2 結果契約**：MCP 結果有 `structuredContent`（JSON object）時以它作為 outputs；沒有時，恰好一個 text content block 轉成 `{"result": "<text>"}`。不猜測 text 是否為 JSON。不使用初稿的自訂 `{"outputs":...}` 信封。
- **D3 worker 工具面**：被 MCP ActionProvider 綁定的 tool 變成 action-only，從 worker 的所有工具面移除（一般 worker 工具列表、dynamic tool authorization snapshot、`use_dynamic_tool`）。同一 server 上未被綁定的其他 tool 維持現行行為。

技術決策（查核程式碼後定案）：

- **T1 不升 snapshot 版本**：在 v4 `ExecutionPolicySnapshot` 加 omitempty 欄位，沿用 `ActionCatalogHash`（8ceb97c）、`ResultContracts`、`ExecutionRoutes` 的先例。`validateExecutionPolicySnapshot`（`internal/team/execution_policy_snapshot.go:556`）只接受 v3 與目前版本，升到 v5 會讓所有既有 v4 session 無法 resume。
- **T2 綁定位置**：在 `NewCoordinator` 內、`bindExecutionRoutes()` 之後、`newExecutionPolicyState(c)` 之前（`internal/team/coordinator.go:1567-1570`）綁定，與 execution route「先綁定再由 snapshot 固定」的模式相同。CLI 已在 `NewCoordinator` 前建立 manager（`cmd/hufu/team_setup.go:257` 對 `:313`），所以 run、chat、resume 與多 team 入口自動共用同一個 seam，不需在 CLI 另加綁定呼叫。
- **T3 input schema 驗證器**：用已是直接依賴的 `github.com/santhosh-tekuri/jsonschema/v6`，編譯方式比照 `compileResultContract`（`internal/team/result_contract_compile.go:86-93`：`NewCompiler`、`UseLoader` 拒絕外部載入）。不使用 `use_dynamic_tool` 的子集驗證器 `validateDynamicArguments`：它的 `dynamicSchemaEligible` 不接受 `$schema`、`minimum`、`format` 等常見關鍵字，會擋掉現成 MCP server。
- **T4 timeout**：`timeout > 0` 時與 parent context 取較早到期者；`timeout: 0` 時沿用 `executeMCPTool` 既有行為：parent 沒有 deadline 就套用 30 秒（`internal/mcp/manager.go:326,384`），有 deadline 就用 parent。這與 command provider「0 表示不加限制」不同，文件必須寫明。
- **T5 失敗呈現不新增事件欄位**：MCP provider 的錯誤訊息以穩定 reason code 前綴開頭（§9），經既有 `action_failed` 的 failure signature 與 runtime action receipt 的 `error` 欄位呈現。不修改 event reducer、session projection、task journal、report 或 TUI。
- **T6 descriptor digest 不變**：不把 MCP tool 的 `outputSchema` 加入 `MCPToolDescriptorSHA256`。改動它會使既有 dynamic tool authorization snapshot 的 digest 改變，既有 session 將無法 resume。
- **T7 descriptor 保護範圍**：manager 只在啟動時 `ListTools` 一次（`internal/mcp/manager.go:192,252`），同一 process 內 descriptor 不會變。本版保證的是「跨重啟/resume 時 descriptor 改變會 fail closed」；每次呼叫前的 digest 比對保留作防禦，文件不得宣稱能偵測 server 端的即時變更。

## 1. 交付結果與效益

團隊維護者可把 team.yaml 已宣告的一個 MCP server/tool 綁定到 ActionProvider capability。static action task（workflow team）與 action catalog task（dynamic 或 workflow team）執行時，沿用既有 task admission、scheduler、`action_started`/`action_failed`/`action_completed`、runtime action receipt、output canonicalization、catalog output schema、recovery 與 resume。MCP 只負責 transport，不成為新的 action 授權來源。

效益：

1. **不經 LLM 的固定呼叫**：呼叫哪個 tool、帶什麼 arguments 由 team 設定與 catalog/static contract 決定，不由 model 決定。
2. **補上目前不可用的路徑**：依 E-31 的決策，帶 artifact scope 的 worker 呼叫 MCP 等外部工具時會被拒（`internal/team/tool_policy_gate.go:158-173`），team `mcp-servers` 對 worker 實際上大多不可用。本功能提供 runtime-owned 的呼叫路徑。
3. **跨 resume 固定目標**：server 設定或 tool descriptor 改變時，resume 以既有 execution policy drift 路徑 fail closed。
4. **沿用既有 MCP 設定與長連線**，不需每次呼叫啟動 command provider 子行程；授權經 `AuthorizeMCPCall`，留下 `policy_decision` 稽核事件。
5. 完成 team-action-catalog 計畫 §27 的延後項目（`docs/archive/implementation-plans/team-action-catalog.md:1647`）。

本版只接受 effective side-effect 為 `none` 的 action。side-effect 是維護者的宣告，不是沙箱；團隊維護者須自行確認綁定的 MCP tool 確實唯讀，正式文件要寫明這一點。

## 2. 現況（已於 1a244b3 核實，直接沿用）

- `ActionProvider` 介面 `Validate`/`Execute`、`NamedActionProvider`、`ProviderRegistry`（`internal/team/action_provider.go:95-200`）。唯一的 registry clone 點是 `LoadTeam` 對傳入 registry 的 `Clone()`（`internal/team/parse.go:1435`），每次 `LoadTeam` 都會建立新的 provider 實例。
- `registerConfiguredActionProviders`（`internal/team/action_provider.go:233`）驗證並註冊 command/golang provider；`parse.go:1201` 逐欄複製 `ActionProviderConfig`；`loadTeamMCPServers` 在註冊前已載入 `session.MCPServers`（`parse.go:1413`）。
- `runtimeWorkflow.executeActionValueForTask`（`internal/team/runtime_workflow.go:144`）：`provider.Validate` 失敗會包成 `ActionValidationError`（不可重試），`Execute` 失敗會包成 `ActionProviderError`。
- `executeRuntimeAction`（`internal/team/coordinator_task_run.go:2431`）：建立 `ActionEnvironment`（TaskID、Attempt、ActionInvocationID、CatalogInvocationID）、`decodeActionResult`、`canonicalizeRuntimeActionOutputs`、receipt 與 lifecycle 事件。static action 只在 workflow team 可執行；dynamic team 只能執行 catalog action。
- `emitRuntimeActionEvent` 寫入 `runtimeActionReceipt`，已含 `Provider`（`ProviderName()`），且 output/error 已經過 `RedactSecrets`（`coordinator_task_run.go:2672-2731`）。
- `actionProviderIdentity`/`actionProviderIdentityHash`（`internal/team/action_provider_identity.go`）進入 catalog entry hash，也用於 run-input policy hash（`internal/team/run_input.go:619`）。
- Catalog 派工時，`Action.Payload` 就是 arguments JSON（`internal/team/team_action_dispatch.go:306`）。worker 可見的 `team_action_list`/`get` 不含 capability 或 provider 資訊（`internal/team/team_action_tools.go:145-165`）。
- `MCPToolManager`：`LoadTools` 在部分 server 失敗時仍回傳 nil（`internal/mcp/manager.go:134-136`）；tool 的邏輯名稱為 `server__native`；`ExecuteAuthorizedTool` 只有在 context 帶 `ToolAuthorizer` 時才授權（`manager.go:338-363`）；`executeMCPTool` 只回傳串接後的文字，會丟棄非文字 block 與 `structuredContent`。
- 工具暴露給 worker 的路徑：`AsAgentTools` 會成為所有 worker 的 supplemental tools，不受 agent `tools:` 過濾（`internal/team/services.go:733`、`internal/team/static_tool_resolution.go:85`）；dynamic snapshot 取自 `SnapshotToolDescriptors`（`internal/team/dynamic_tool_authorization.go:231`）；另有 `coordinator_task_run.go:3666` 與 `coordinator_declared_tool_runner.go:45` 兩處呼叫 `AsAgentTools`。
- `Coordinator.AuthorizeMCPCall` 會發出 `policy_decision`（`internal/team/coordinator.go:2354`）。預設政策在 `AllowedTools` 含 `server:tool` 時放行；`SetAuthorizationPolicy` 在正式程式碼中沒有呼叫者，所以本功能的授權實際效果是稽核加上可注入政策的 seam。
- `--force-mcp` 只阻擋內建 ssh/scp 等工具（`internal/tools/tools.go:279-331`），與 ActionProvider 無關。
- 可重用的 helper：`decodeUniqueJSON`（拒絕重複 key 與多個 JSON value，`internal/team/primary_decision_evidence_adapter.go:345`）、`utils.RedactSecrets`、`CanonicalizeRuntimeOutputs`。

## 3. 設定契約

`ActionProviderConfig`（`internal/agent/agent.go:595`）新增：

    Server string `json:"server,omitempty" yaml:"server,omitempty"`
    Tool   string `json:"tool,omitempty" yaml:"tool,omitempty"`

Catalog 範例（dynamic team；須搭配現有 team 要求的 agent 定義）：

    name: incident-team
    description: Collects read-only diagnostics through an MCP-backed action
    mcp-servers:
      diagnostics:
        type: local
        command: [diagnostics-mcp]
        allowedTools: [collect_debug]
    action-providers:
      diagnostics:
        runtime: mcp
        server: diagnostics
        tool: collect_debug
        timeout: 120
    action-catalog:
      collect-debug:
        description: Collect bounded diagnostics for one service.
        capability: diagnostics
        type: collect_debug
        agent: runtime-engineer
        side-effect: none
        input-schema:
          type: object
          properties:
            service:
              type: string
          required-properties: [service]
          additional-properties: false
        access:
          discover: [runtime-engineer]
          propose: [runtime-engineer]
        invocation:
          require-proposal: true
          allow-unattended: true
          max-invocations: 2

Static task 範例（workflow team，其餘必要欄位比照 action-providers.md「Binding a provider to a task」）：

    tasks:
      - id: collect-debug
        agent: reviewer
        phase: prepare
        when-goal-contains: diagnose
        side_effect: none
        action:
          capability: diagnostics
          type: collect_debug
          payload: '{"service":"api"}'

語意：MCP 的 server 與 tool 只能來自 team.yaml 的 `action-providers`。model、proposal、catalog arguments 與 static `Action.Payload` 都不能指定或覆寫。`Action.Type` 仍是 Hufu 的 action identity，不送給 MCP tool；送出的 MCP arguments 只來自 `Action.Payload` 的 JSON object。

### 3.1 載入錯誤（`LoadTeam` 回傳 error，與既有 command/golang 形狀錯誤同層）

在 `registerConfiguredActionProviders` 增加 `runtime: mcp` 分支（函式多接收 `session.MCPServers`），provider 建構與驗證放在新檔：

1. `runtime: mcp` 時，`server`、`tool` 去空白後非空；`command`、`dir`、`source`、`mode` 必須為空；`timeout` 不得為負（沿用既有檢查）。
2. `server` 必須完全符合（區分大小寫）同一 team `mcp-servers` 的某個 key；該 server 的 `type` 須為 `local`、`remote` 或空字串。
3. `tool` 須通過該 server 的 `allowedTools`/`excludedTools`，判斷方式與 manager 相同（把 `internal/mcp` 的 `isToolAllowed` 匯出為 `IsToolAllowed`）。
4. `runtime` 為 command 或 golang 時，`server`、`tool` 必須為空（避免設定被靜默忽略）。
5. 以上都只檢查設定，不連線、不啟動 MCP server；`hufu team lint`、`hufu team validate`、`--dry-run` 因此不需要真的 server。

### 3.2 Contract findings（`ValidateTeamPolicyContracts`；runtime `LoadTeam` 視為錯誤、`team lint` 回報）

新增 finding code 到 `internal/team/contract_finding.go`；驗證函式放在新檔，從既有呼叫點各加一行：

| Code | 條件 | 呼叫點 |
|---|---|---|
| `mcp_action_side_effect_unsupported` | static task 的 capability 是 MCP provider，且 `side_effect` 不是 `none`（**空值也算違規**）；或 catalog entry 的 capability 是 MCP provider，且 `side-effect` 不是 `none` | `validateActionTaskContract`（`team_policy_lint.go:313`）、`validateActionCatalog`（`action_catalog_validate.go:15`） |
| `mcp_action_payload_invalid` | static task 沒有 `input-bindings`，且 `payload` 不是單一 JSON object、有重複 key，或超過 64 KiB（用 `decodeUniqueJSON`） | `validateActionTaskContract` |
| `mcp_action_resolver_unsupported` | run-input resolver 的 capability 是 MCP provider | `ValidateTeamPolicyContracts` |
| `mcp_action_tool_reserved` | 某 agent 的 `tools:` 明確宣告了被 MCP provider 綁定的 `server__tool`（名稱比對方式與 `mcpServerForTool` 相同） | `ValidateTeamPolicyContracts` |

MCP provider 不自動建立 catalog entry，也不把 tool 加入任何 worker 工具列表。既有 catalog access、proposal 與 coordinator 派工規則保持權威。

## 4. 啟動綁定

### 4.1 Provider 物件

新檔 `internal/team/mcp_action_provider.go`：

- `mcpActionProvider` 在 `LoadTeam` 時建立，保存 capability、server、native tool、timeout 與 `serverConfigHash`。此時尚未綁定。
- `ProviderName()` 回傳 `mcp:<server>/<tool>`，不含任何機密，而且在綁定前後都相同。catalog entry hash 在 `LoadTeam` 時計算，所以 ProviderName 不得包含 live descriptor。
- 綁定只能成功一次（mutex 保護）；對另一個 manager 再次綁定時回傳錯誤。綁定後保存 manager 參照、邏輯名稱 `server__tool`、descriptor digest，以及編譯後的 input schema。不保存 MCP client handle；manager 仍由 `teamContext` 關閉。

`serverConfigHash` 是以下 canonical JSON 的 SHA-256：`type`（空值正規化為 `local`）、`command` argv、`url`、排序後的 `allowedTools`/`excludedTools`、`noOAuth`，以及依名稱排序的 environment `{name, value_sha256}`。原始 environment 值不保存，做法比照 `ExecutionEnvironmentVariableSnapshot`。

`actionProviderIdentity` 新增 `Server`、`Tool`、`ServerConfigHash`，三者都用 `json:",omitempty"`，因此非 MCP provider 的 identity JSON、catalog entry hash 與 run-input policy hash 都不變。

### 4.2 Manager 端新 API（新檔 `internal/mcp/runtime_tools.go`，`manager.go` 只做必要的小改）

- `LoadTools` 依 server 名稱記錄載入錯誤；新增 `ServerLoadError(name string) error`。
- `AttachClient(ctx, name string, cfg MCPServerConfig, cli *client.Client) error`：接收已啟動的 client，執行 Initialize、ListTools、allow/exclude 過濾與註冊。`loadLocalServer`/`loadRemoteServer` 共用這段尾端邏輯。它也是測試用 `client.NewInProcessClient` 的注入點。
- `ReserveRuntimeTool(server, native string) (MCPTool, error)`：用 `ServerName` 與 `OrigName` 完全比對（同名但不同 server 不算），把該邏輯名稱標為 reserved，回傳 clone。對同一 tool 重複 reserve 是 idempotent。
- reserved tool 從 `AsAgentTools`、`SnapshotToolDescriptors`（因而也從 `GetTools`）排除；`ExecuteTool`/`ExecuteAuthorizedTool` 對 reserved 名稱的處理與「tool 不存在」相同。
- `ExecuteRuntimeTool(ctx, logicalName, expectedDigest, arguments string, authorize ToolAuthorizer) (RuntimeToolResult, error)`：
  - 只接受 reserved 名稱；
  - `authorize == nil` 時在 transport 前拒絕；
  - 依序檢查 digest、呼叫 authorizer，通過後才 `CallTool`；timeout 行為同 T4；
  - 回傳 `RuntimeToolResult{IsError bool; StructuredContent json.RawMessage; Content []RuntimeToolContent{Type, Text string}}`，保留全部 block 的數量與型別（非文字 block 的 Text 為空）；
  - `StructuredContent` 優先取 `RawStructuredContent`，沒有時對非 nil 的 `StructuredContent` 做 `json.Marshal`（in-process transport 可能不經 JSON）。
- 既有 `use_dynamic_tool` 使用的文字 API 與行為不變。

### 4.3 綁定流程（`NewCoordinator` 內，T2）

新檔 `internal/team/mcp_action_binding.go` 的 `(*Coordinator).bindMCPActionProviders()`，依 capability 排序處理 session 中每個 MCP provider：

1. `c.mcpManager == nil` 時回傳 `mcp_action_bind_failed: action provider "<cap>" requires MCP server "<server>", but no MCP manager is loaded`。
2. 呼叫 `ReserveRuntimeTool`。失敗時，錯誤訊息要附上 `ServerLoadError(server)` 的原因（server 載入失敗），或說明「server 未列出該 tool 或已排除」。
3. 用 `MCPToolDescriptorSHA256` 計算 digest，並以 T3 方式編譯 descriptor 的 `InputSchema`。編譯失敗時回傳 `mcp_action_bind_failed`。
4. 綁定 provider。

任何錯誤都讓 `NewCoordinator` 失敗。CLI 既有的 defer 會關閉 manager（`team_setup.go:262-266`）。無關 server 載入失敗不影響綁定（只看被引用的 server/tool）；被引用 server 的失敗不能只停在 stderr 警告。

`NewDryRunCoordinator` 不綁定，也不需要 manager。dry-run 從不執行 action。

## 5. Durable binding 與 resume

在 v4 `ExecutionPolicySnapshot`（T1）新增：

    MCPActionProviders []ExecutionMCPActionProviderSnapshot `json:"mcp_action_providers,omitempty"`

    type ExecutionMCPActionProviderSnapshot struct {
        Capability       string `json:"capability"`
        Server           string `json:"server"`
        Tool             string `json:"tool"`
        ServerConfigHash string `json:"server_config_hash"`
        DescriptorSHA256 string `json:"descriptor_sha256"`
    }

規則（builder 放新檔，`execution_policy_snapshot.go` 只加呼叫與欄位）：

1. 只在 `version == executionPolicySnapshotVersion` 時填入；依 capability 排序。不保存 endpoint、credential、原始 schema。沒有 MCP provider 的 team 省略此欄位，`ConfigurationHash` 不變。
2. `cloneExecutionPolicySnapshot` 要 clone 此 slice。`validateExecutionPolicySnapshot` 要求 entry 依 capability 排序且唯一、每個欄位非空。
3. 綁定前（僅 dry-run）`DescriptorSHA256` 為空。非 dry-run 的 `newExecutionPolicyState` 遇到未綁定的 MCP provider 時回傳錯誤。持久化的 snapshot 不可能出現空 digest。
4. Resume 不需要新比較邏輯：`ensureExecutionPolicySnapshot` 會把 journal snapshot 與 current 比較，server 設定或 descriptor 改變時走既有的 `execution policy snapshot drift detected` fail-closed 路徑。
5. v3 防護：durable snapshot 是 v3、而 session 有 MCP provider 時，在 `executionPolicySnapshotMatchesCurrent` 回傳 `execution policy snapshot v3 cannot pin MCP action providers; start a new session with --new`。v3 的相容重建不含新欄位，沒有這條防護可能會誤判相符。
6. Catalog entry hash 透過 §4.1 的 identity 欄位涵蓋 server/tool/serverConfigHash；static task 的 MCP 目標由本 snapshot 固定。已 admitted 的 occurrence 不會被 live team.yaml 的新值替換。

## 6. 執行與授權

`mcpActionProvider.Validate(action)`（錯誤經既有包裝成為不可重試的 `ActionValidationError`）：

1. capability 相符、`Type` 非空、provider 已綁定；未綁定時回傳 `mcp_action_unbound`。
2. `Payload` 不超過 64 KiB，用 `decodeUniqueJSON` 解析為單一 JSON object，且通過已編譯的 descriptor input schema。失敗時回傳 `mcp_action_payload_invalid`。catalog task 另外保留既有 catalog input-schema 驗證。

`mcpActionProvider.Execute(ctx, action)`：

1. 重跑 Validate。
2. 從 `ActionEnvironmentFromContext` 取得非空 `TaskID`、`ActionInvocationID` 與 `Attempt > 0`，缺少時回傳 `mcp_action_identity_missing`。
3. 從 team 套件私有的 context key 取出 runtime action authorizer，缺少時回傳 `mcp_action_authorization_missing`。
4. `timeout > 0` 時設定 `context.WithTimeout`（T4）。取消後不另開背景呼叫。
5. 呼叫 `ExecuteRuntimeTool`，再依 §7 轉換結果。

Runtime action authorizer：在 `executeRuntimeAction` 建立 `actionCtx` 處（`coordinator_task_run.go:2483` 附近）加一行呼叫 helper。helper 放新檔，只附掛到 team 套件私有 key。它以 `MCPAuthorizationRequest{Agent: task.Agent, Server: server, Tool: tool, AllowedTools: {"server:tool": true}, FailureMode: c.ExecutionProfile().PolicyFailureMode}` 呼叫 `c.AuthorizeMCPCall`，結果必須是 `DecisionAllow`。這是 team 設定對該 action 的窄授權，不會把原生 MCP tool 暴露給 worker。

在下列情況 MCP `CallTool` 次數必須為 0：未綁定、payload 無效、identity 缺失、authorizer 缺失、descriptor 不符、授權拒絕。

`action_started` 表示 Hufu runtime action attempt 已開始，不保證 MCP 已送出。provider 內不建立第二套 retry、Todo 或 lifecycle event。

## 7. 結果契約（D2）

`RuntimeToolResult` 依序轉成 `ActionResult`（`Artifacts` 永遠為 nil）：

1. transport error：失敗，`mcp_action_transport_failed`；逾時仍需讓 `errors.Is(err, context.DeadlineExceeded)` 成立，以保留既有 `CategoryTimeout` 分類。
2. `IsError == true`：失敗，`mcp_action_tool_error: <text blocks 串接，經 RedactSecrets，截斷至 1000 runes>`。錯誤文字絕不當成成功的 outputs。
3. 有 `StructuredContent`：原始 bytes 不超過 1 MiB，用 `decodeUniqueJSON` 解析後必須是 JSON object，作為 `Outputs`；content blocks 忽略且不持久化。否則失敗，`mcp_action_result_invalid`。
4. 沒有 `StructuredContent`：恰好一個 `text` block，長度不超過 1 MiB，轉成 `Outputs = {"result": text}`。零個 block、多個 block 或非文字 block 都失敗，`mcp_action_result_invalid`。

之後沿用 `CanonicalizeRuntimeOutputs`（256 KiB、深度、數量限制與 secret redaction）、catalog output-schema 驗證（使用 fallback 時，output schema 須宣告 `result` 字串）與 runtime action receipt。MCP 的 resource/embedded block 與任何字串都不會被當成 artifact ref 或本地路徑。raw arguments 與 raw MCP output 不額外寫入事件、report 或 provider identity。

## 8. Worker 工具面隔離（D3）

由 §4.2 的 reservation 實現，不在 team 套件各處另加過濾。綁定後，reserved tool 不會出現在：

- worker supplemental tools（`services.go:733`），包括 `tools: all` 或未宣告 `tools:` 的 worker；
- 新建 occurrence 的 `DynamicToolAuthorizationSnapshot`，因此也不在 `use_dynamic_tool` 的目標集合中；
- `coordinator_task_run.go:3666` 與 `coordinator_declared_tool_runner.go:45` 的工具集合。

即使有偽造的 tool call，`ExecuteAuthorizedTool` 對 reserved 名稱也回傳 not found。靜態宣告由 §3.2 的 `mcp_action_tool_reserved` 在 load/lint 階段拒絕。

## 9. 錯誤與恢復語意

| 階段 | 情況 | 結果 |
|---|---|---|
| Load/lint | §3.1、§3.2 的設定錯誤 | `LoadTeam` 錯誤或 lint finding；不連線 |
| Startup（`NewCoordinator`） | manager 缺、server 載入失敗、tool 不存在或被排除、schema 無法編譯 | `mcp_action_bind_failed`，run 不啟動（D1） |
| Resume admission | server 設定或 descriptor 改變、v3 snapshot | 既有 policy drift fail closed / §5.5 訊息 |
| Provider call 前 | `mcp_action_unbound`、`mcp_action_payload_invalid`、`mcp_action_identity_missing`、`mcp_action_authorization_missing`、`mcp_action_descriptor_mismatch`、`mcp_action_authorization_denied` | `action_failed` + receipt；`CallTool=0` |
| CallTool 後 | `mcp_action_transport_failed`、`mcp_action_tool_error`、`mcp_action_result_invalid`、canonical output 或 output-schema 失敗 | `action_failed` + receipt |

- 錯誤訊息一律以上表 code 加冒號開頭，再經既有 `ActionValidationError`/`ActionProviderError` 包裝；診斷文字在放進錯誤前先經 `RedactSecrets`。
- `CallTool` 已送出但逾時或斷線時，不推定沒有副作用，沿用 task 既有 recovery policy。因本版只允許 `none`，catalog 既有的預設 retry 可以使用。
- action receipt 已持久化時，resume 不得再呼叫 MCP（既有機制，需補測試）。在 `action_started` 後中斷時，沿用既有 Todo/receipt/recovery 判斷。
- MCP 沒有環境變數通道，本版不傳送 `HUFU_ACTION_INVOCATION_ID` 或 `HUFU_CATALOG_INVOCATION_ID` 給 MCP server。

## 10. 觀測與文件

- `runtimeActionReceipt` 新增 `ProviderDescriptorSHA256 string \`json:"provider_descriptor_sha256,omitempty"\``，經 runtime workflow 新增的 `providerDescriptorDigest(capability)` 取得（provider 以套件私有 interface 提供）。`Provider` 已是 `mcp:<server>/<tool>`。非 MCP provider 的 receipt 不變。
- 授權事件沿用 `AuthorizeMCPCall` 的 `policy_decision`（`kind: mcp`）。
- worker 的 `team_action_list`/`get` 仍不含 server、native tool 或 credential（既有行為，補回歸測試）。
- 更新 `docs/reference/action-providers.md`：
  - 在「Provider types」新增 `### MCP provider`，內容包括設定欄位與 §3 規則、D1 啟動失敗、§7 結果契約（含 `{"result": text}` 與 output-schema 寫法）、T4 timeout 語意、D3 worker 隔離、T7 的保護範圍、server 設定或 descriptor 改變時 resume 需 `--new`、side-effect 是宣告不是沙箱；
  - 在「Recovery, idempotency, and trust」補充 MCP identity 涵蓋的欄位；
  - 更新檔頭 `Verified-Commit`。
- 新增可載入 fixture：`internal/team/testdata/docs-mcp-action-catalog/`（§3 catalog 範例，由 docs-action-catalog-dynamic 改寫）與 `internal/team/testdata/docs-mcp-action-workflow/`（§3 static task 範例，由 docs-action-provider-example 改寫），並加入 `internal/team/action_providers_doc_example_test.go` 的載入測試。

## 11. 工作包（依序實作，每個 WP 一個 commit）

檔案規則：新邏輯放新檔（每檔不超過 800 行）。已超過 800 行的檔案（`coordinator_task_run.go`、`coordinator.go`、`execution_policy_snapshot.go`、`parse.go`）每個淨增加不超過 30 行，只放呼叫點與欄位。不新增 `.golangci.yml` 例外。錯誤以 `fmt.Errorf("doing X: %w", err)` 包裝。測試採 table-driven，並使用 in-process MCP server（mcp-go 的 `server.NewMCPServer` 加 `client.NewInProcessClient`，handler 內計數 `CallTool`），不依賴真實基礎設施或外部 credential。

**WP-1 設定與離線驗證**
- 範圍：`agent.ActionProviderConfig` 新欄位、`parse.go:1201` 複製、`mcp.IsToolAllowed` 匯出、`registerConfiguredActionProviders` 的 mcp 分支（§3.1）、`serverConfigHash`、`actionProviderIdentity` 新欄位、§3.2 四個 finding。
- 測試：
  - malformed runtime 組合（缺 server/tool、多出 command/dir/source/mode、command/golang 帶 server/tool）、未知 server、非法 server type、excluded tool、不在 allowedTools 內的 tool；
  - static task `side_effect` 為空或非 none、catalog entry 非 none、static payload 非 object 或重複 key、resolver 引用 MCP、agent 宣告 reserved tool；
  - 非 MCP team 的 `actionProviderIdentity` JSON 不含新 key（catalog 與 run-input hash 不變）；
  - `team lint` 與 `LoadTeam` 都不啟動 server。

**WP-2 MCP manager runtime API**（`internal/mcp`）
- 範圍：§4.2 全部。
- 測試：
  - `AttachClient` 搭配 in-process server；
  - `ReserveRuntimeTool` 比對 server 與 native 名稱，同名但不同 server 不符；
  - reserved tool 不出現在 `AsAgentTools`/`SnapshotToolDescriptors`，且 `ExecuteAuthorizedTool` 拒絕；
  - `ExecuteRuntimeTool` 在 authorize 為 nil、digest 不符、授權拒絕時 `CallTool=0`；
  - 保留多 block、非文字 block 與 structuredContent；
  - 取得 `ServerLoadError`；
  - 既有 `internal/mcp` 測試全數通過。

**WP-3 啟動綁定與 durable snapshot**
- 範圍：§4.1 provider 綁定、§4.3 `bindMCPActionProviders`（在 `NewCoordinator` 呼叫）、§5 snapshot 欄位、builder、clone、validate 與 v3 防護。
- 測試：
  - 綁定成功；manager 為 nil、被引用 server 載入失敗、tool 被排除、schema 無法編譯時啟動失敗；無關 server 失敗時仍能綁定；
  - `$schema: draft-07` 的 schema 可以編譯；
  - 非 MCP team 的 snapshot JSON 不含 `mcp_action_providers`；
  - 比照 `TestChangedActionCatalogProviderFailsResumeClosed`（`internal/team/action_catalog_test.go:260`）：descriptor 改變、server 設定改變時 resume 出現 drift，設定不變時 resume 成功；
  - v3 snapshot 加 MCP provider 時出現 §5.5 錯誤；`NewDryRunCoordinator` 不需 manager 即可建立；
  - CLI：以既有測試 seam `buildTeamMCPManager`/`closeTeamMCPManager`（`cmd/hufu/team_loader.go:232-235`）驗證綁定失敗時 manager 恰好關閉一次，且沒有 coordinator 啟動。

**WP-4 執行與授權**
- 範圍：§6 的 `Validate`/`Execute`、runtime action authorizer helper 與 `executeRuntimeAction` 的一行呼叫、T4 timeout。
- 測試：
  - static workflow task 與 catalog task 各成功一次，參考 `TestDynamicTeamRunsCatalogActionWithoutPhases`（`internal/team/team_action_runtime_test.go:80`）與 `newCommandActionCoordinator`（`internal/team/wp03_action_provider_test.go:151`）的模式；
  - 以 `SetAuthorizationPolicy` 注入拒絕政策，或讓 payload 無效、identity 缺失、descriptor 被竄改時，`CallTool=0`，且 receipt error 以對應 code 開頭；
  - `policy_decision` 事件存在；
  - timeout 與取消（server handler 阻塞）的分類為 timeout；
  - `--force-mcp` 下既有 command provider static task 仍能執行（回歸）。

**WP-5 結果轉換與 lifecycle**
- 範圍：§7 轉換、§10 receipt 欄位。
- 測試：
  - structuredContent object 成功；structuredContent 非 object 失敗；
  - 單一 text 轉為 `{"result": ...}`；零個、多個、非文字 block 失敗；
  - `IsError` 失敗且錯誤文字經過 redaction；超過 1 MiB 失敗；超過 256 KiB canonical 限制失敗；catalog output-schema 失敗；
  - receipt 含 `provider: mcp:<server>/<tool>` 與 `provider_descriptor_sha256`；
  - receipt 已持久化時 resume 不重播（`CallTool` 維持 1）。

**WP-6 Worker 隔離驗證**
- 範圍：§8，team 層級的整合測試。WP-2 已完成 reservation，這裡預期不需新增正式程式碼；若發現遺漏的暴露路徑則在此修正。
- 測試：
  - `tools: all` 的 worker 工具面不含 reserved tool，而同 server 的未綁定 tool 仍在；
  - 新 occurrence 的 dynamic snapshot 不含 reserved tool，`use_dynamic_tool` 無法指定它；
  - `team_action_list`/`get` 不含 server/tool/capability。

**WP-7 文件與 fixture**
- 範圍：§10 的文件與兩個 fixture 及其載入測試。
- 完成後執行 §12 的全部驗證。

## 12. 完成條件

- 沒有 MCP provider 的 team：工具面、catalog entry hash、run-input policy hash、execution policy snapshot JSON 與 `ConfigurationHash`、執行與 resume 行為都不變（WP-1、WP-3 的測試證明）。
- 同一個 MCP ActionProvider 能讓 static task 與 catalog task 都走原有 task/action lifecycle；worker 與 model 無法指定 server/tool，也看不到或呼叫不到 reserved tool。
- 被引用的 target 在啟動時不可用，run 不啟動；resume 時 server 設定或 descriptor 改變則 fail closed；授權拒絕、descriptor 不符或 arguments 無效時 `CallTool=0`，且錯誤以 §9 code 可診斷。
- 合法回應經 canonical output、可選的 catalog output schema 與 receipt；任務若宣告 objective verification，由現有驗證流程檢查。不合法回應不會被標成成功。
- Crash-resume 不重播已完成的 MCP action；需要 retry 的未完成唯讀 action 由既有 recovery machinery 處理。
- `--force-mcp` 下既有 static command/golang ActionProvider 的回歸測試通過。
- 依序通過：`go build ./...`、`go vet ./...`、`go test ./...`、`go test -race -timeout 45m ./internal/mcp/... ./internal/team/...`、`golangci-lint run`。
- `docs/reference/action-providers.md` 已更新，兩個新 fixture 由測試載入。

## 13. 不在本版範圍（相對初稿移除或改寫的部分）

- side-effect 非 `none` 的 MCP action、MCP 作為 run-input resolver、MCP 宣告的 artifacts 或 resource 內容。
- snapshot 升到 v5（改由 T1 處理）；初稿自訂的 `{"outputs":...}` 結果信封（改由 D2 處理）。
- 把 MCP `outputSchema` 納入 descriptor digest（T6）；同 process 內偵測 server 端 tool 變更，例如處理 `tools/list_changed` 或每次呼叫前重新 ListTools（T7）。
- 新的 CLI、TUI、report 呈現，以及 event reducer、session、task journal 的新欄位（T5）。operator 透過 receipt 的 `provider` 與 `provider_descriptor_sha256`，以及 policy snapshot 查看綁定。
- 「session clone 不得共享 manager descriptor slice」：唯一的 clone 點在 `LoadTeam`，provider 在每次載入時新建，不需額外處理。
- 分別處理 direct-agent、unattended、chat、resume 各入口的 setup 差異：T2 的單一 seam 已涵蓋。
- eval harness（`internal/evalharness`）支援 MCP provider：該路徑沒有 MCP manager，遇到 MCP provider 會依 D1 fail closed。
- 把 `HUFU_*_INVOCATION_ID` 經 MCP `_meta` 傳給 server。
