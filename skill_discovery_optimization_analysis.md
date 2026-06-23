# 自動技能發現功能最佳化與 Bug 分析報告

基於 [SKILL_DISCOVERY.md](file:///home/ubuntu/nfs/github/agent-team-cli/SKILL_DISCOVERY.md) 的描述與原始碼分析，目前自動技能發現（Automatic Skill Discovery）功能已具備基礎的滑動窗口偵測與草稿生成機制，但在其實現中存在數個**重大 Bug** 以及**未完成的設計缺陷**，阻礙了語意相似度分析和執行報告的正常運作。

以下是針對該功能的詳細分析、修復路徑與最佳化建議。

---

## 1. 現存嚴重 Bug 與設計缺陷分析

### Bug A：Sidecar 語意分析實例始終為 `nil`（造成語意分析失效）
* **現象**：系統始終降級為簡單的「工具名稱與參數比對」，無法啟用 LLM 語意相似度聚類。
* **原因**：在 [coordinator.go:L658-L660](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator.go#L658-L660) 初始化中：
  ```go
  // Enable sidecar for skill pattern detection
  if c.sidecarInst != nil {
      c.skillDetector.SetSidecar(context.Background(), c.sidecarInst)
  }
  ```
  在 `NewCoordinator` 執行此處時，`c.sidecarInst` 尚未被分配（始終為 `nil`）。該實例本應在調用 `c.Sidecar()` 時進行延遲懶加載（Lazy Initialization）。因此，`SetSidecar` 實際傳入的是 `nil`，導致 `SkillPatternDetector` 認為 sidecar 未啟用。
* **修復方案**：改為調用 `c.Sidecar()` 來觸發實例初始化：
  ```go
  if s := c.Sidecar(); s != nil {
      c.skillDetector.SetSidecar(context.Background(), s)
  }
  ```

### Bug B：`mergeSimilarSequences` 忽略了 LLM 語意聚類結果（邏輯失效）
* **現象**：不論任務語意是否相似，只要工具序列相同，就會被粗暴地合併為同一個技能。
* **原因**：在 [discovery.go:L350-L372](file:///home/ubuntu/nfs/github/agent-team-cli/internal/skill/discovery.go#L350-L372) 中的 `mergeSimilarSequences` 接收了 `clusters` 與 `threshold`，但其實作僅單純根據 `toolHash`（即工具序列本身，如 `view|edit|bash`）對候選技能進行分組與合併：
  ```go
  // Merge similar sequences
  for _, group := range groupMap {
      if len(group) > 1 {
          mergedCand := d.mergeCandidateGroup(group) // 粗暴地將同工具序列的所有任務描述合併
          ...
  ```
  這代表兩個完全不同情境的 `view -> edit -> bash` 序列（例如「修改 Go 語言編譯錯誤」與「調整前端 TS 排版」）會被合併成同一個技能草稿，破壞了技能的單一職責原則。同時，內建的 `isInSameCluster` 輔助函式完全沒有被調用。
* **修復方案**：重構 `mergeSimilarSequences`，當工具序列相同時，必須透過 `clusters` 比對其任務描述是否落在同一個語意聚類中，若不在同一個聚類中，則應拆分為獨立的技能草稿。

### Bug C：報告整合缺失，報告章節為空
* **現象**：使用 `--report` 產生的 `report.md` 中，「自動檢測技能」一欄永遠是空的。
* **原因**：在 [report.go:L135-L139](file:///home/ubuntu/nfs/github/agent-team-cli/cmd/hufu/report.go#L135-L139) 中，`gatherSkillPatterns` 函式直接回傳 `nil`：
  ```go
  func gatherSkillPatterns(coordinator *team.Coordinator) []SkillPatternReport {
      // This would need access to the skillDetector which is not exported
      // For now, return empty - the feature is primarily for real-time notification
      return nil
  }
  ```
  這是因為 `Coordinator` 並沒有導出或暴露其內部的 `skillDetector` 欄位。
* **修復方案**：
  1. 在 `Coordinator` 中新增 Getter 方法暴露偵測器：
     ```go
     func (c *Coordinator) SkillDetector() *skill.SkillPatternDetector {
         return c.skillDetector
     }
     ```
  2. 在 `report.go` 中導入 `github.com/anomalyco/hufu/internal/skill`，呼叫 `SkillDetector().FindCandidates()` 獲取發現的技能，並填充回 `SkillPatternReport` 結構。

---

## 2. 演算法與性能優化建議

### A. 參數正規化（Parameter Normalization）優化
目前 `normalizeParams` 在 [discovery.go:L170-L195](file:///home/ubuntu/nfs/github/agent-team-cli/internal/skill/discovery.go#L170-L195) 僅進行非常簡單的正規表示式替換：
* 僅匹配 `*.go`, `*.ts` 等部分副檔名。
* 大量參數如特定的 CLI 命令參數（如 `git checkout -b branch` 與 `git push origin main`）未能有效地被抽象。
* **優化方向**：優化正規化規則，特別是針對 `bash` 工具，將具體的命令子參數抽象化為 `git <subcommand>`，減少因為不重要的參數微調而導致的雜湊不一致。

### B. 滑動窗口的性能限制（Sliding Window Optimization）
* **現象**：目前每次呼叫 `RecordToolCall` 都會鎖定 Mutex 並觸發滑動窗口演算法：
  ```go
  func (d *SkillPatternDetector) RecordToolCall(agent, tool, input, taskDesc string) {
      ...
      d.toolCalls = append(d.toolCalls, record)
      d.analyzeSequencesLocked() // 每次工具調用都重複分析整個歷史序列，複雜度為 O(N * W)
  }
  ```
  隨著工具呼叫次數（N）增加，對長序列進行多尺度滑動窗口（W=3 到 10）分析會產生較大效能開銷。
* **優化方向**：
  1. 改為**增量分析（Incremental Analysis）**，每次 `RecordToolCall` 時僅掃描以最新加入的 ToolCall 結尾的窗口，不需每次都重頭掃描整個 `toolCalls` 陣列。
  2. 或是將 `analyzeSequencesLocked` 改為在 `checkSkillPatterns`（每輪任務結束）時才進行一次性批次分析，避免在每次工具執行時阻塞主線程。

---

## 3. 架構與未來功能擴充建議

### A. 新增 CLI 工具推廣命令（Promotion Command）
* **現狀**：技能草稿被存在 `workspace/skills/draft-...` 下，若要正式使用需要手動拷貝或編輯。
* **建議**：在 `cmd/hufu/` 中新增 CLI 子命令：
  ```bash
  hufu skill promote <draft-name> [--team <team-name>]
  ```
  此命令會自動將草稿從暫存工作區移動到配置的團隊搜尋路徑下（如 `.agent-teams/skills/`），將草稿轉化為永久技能。

### B. 結合 Short-Term Memory (STM) 與技能回饋機制
* 偵測到重複模式時，可自動在 STM (`stm.md`) 中載入該技能草稿的描述，以供 Coordinator 下一輪指派任務時作為決策依據，真正實現「線上自我演進」與「技能熱插拔」。

---

## 4. 具體修復與優化實作規劃

如果需要，我可以立即著手進行以下步驟的程式碼修復與測試：

1. **Step 1**: 在 `coordinator.go` 中修復 Sidecar 初始化 Bug，並導出 `SkillDetector()`。
2. **Step 2**: 實作 `cmd/hufu/report.go` 中的 `gatherSkillPatterns`，完成報告整合。
3. **Step 3**: 重構 `discovery.go` 中的 `mergeSimilarSequences`，正確使用 LLM 聚類結果進行語意合併。
4. **Step 4**: 執行 `go test ./internal/skill/...` 確保單元測試全部通過並新增相關測試。
