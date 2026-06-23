# 關於最近 2 次 Commit 的程式碼品質與潛在 Bug 審查報告 (最新版)

本報告針對專案 **hufu** 最近兩次 Commit 的變動進行了詳盡的品質審查與潛在 Bug 分析：

*   **Commit 1 (20e5339)**: `fix(ltm): only remove history file after successful read`
*   **Commit 2 (ef862c8)**: `refactor: isolate workspace per extra model and improve TUI log display`

---

## 1. 肯定與修復確認 (Commendations & Verified Fixes)

我們非常高興地確認，在最近這兩次 Commit 中，您針對我們先前報告中指出的一系列關鍵併發崩潰與資料流失問題進行了精確的修復，極大地提升了系統的強健性：

1.  **徹底修復 `ltm.go` 靜默資料遺失 Bug** (Commit `20e5339`)：
    *   在歷史記憶提取邏輯中，將 `os.Remove(path)` 移入 `readErr == nil` 的區塊，確保歷史檔案只有在讀取成功且完成提取後才被刪除，防止了讀取失敗時資料的永久流失。
2.  **成功移除原地修改 `c.session.Workspace` 的 Race Condition** (Commit `ef862c8`)：
    *   移除了在協程中動態竄改和還原共享會話中 `Workspace` 的不安全寫法，改為實作 `cloneSession` 與 `cloneCoordinator` 為並行模型創建獨立的會話與協調器上下文。
3.  **解決了 TUI 中日誌混淆的問題** (Commit `ef862c8`)：
    *   在 TUI 接收端處理 `TaskLogMsg` 時加入了 `msg.Model` 的前綴標記（例如 `[qwen3]`）。這使得並行多模型執行的日誌得以在畫面上清晰地區分。
4.  **改善了 `display.go` 物件封裝完整度** (Commit `ef862c8`)：
    *   新增並呼叫了 `tb.set(...)` 方法，消除了直接存取與鎖定私有 map 的設計瑕疵。

---

## 2. 新寫法引入的嚴重併發安全性 Bug (New Concurrency Bug)

在 Commit `ef862c8` 中，為了避免 `go vet` 的「copies lock value」警告並防止資料競爭，您在 `cloneCoordinator` 中選擇將互斥鎖（Mutex）零值化（Zero-initialized），但在**淺拷貝 map 變數**的設計下，這**引入了一個隱蔽且致命的併發資料競爭 (Data Race) 漏洞**。

### Bug A：零值化鎖與共享 Map 導致的併發讀寫競爭 (Critical Data Race)
*   **問題描述**：
    在 [coordinator.go:L3447-L3498](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator.go#L3447-L3498) 的 `cloneCoordinator` 函式中，複製了諸多 map 變數，但**零值化**了其鎖欄位（如 `agentCacheMu`、`pendingPlansMu`、`skillUsageMu` 等）：

    ```go
    // [ coordinator.go ]
    func cloneCoordinator(orig *Coordinator, newSession *TeamSession) *Coordinator {
        return &Coordinator{
            session:               newSession,
            ...
            agentCache:            orig.agentCache,      // 淺拷貝 map 指標，共用同一個 Map 記憶體實例！
            skillUsage:            orig.skillUsage,      // 共用同一個 Map 記憶體實例！
            pendingPlans:          orig.pendingPlans,    // 共用同一個 Map 記憶體實例！
            approvedOutputs:       orig.approvedOutputs, // 共用同一個 Map 記憶體實例！
            ...
            // 互斥鎖/讀寫鎖欄位（如 mu、agentCacheMu、skillUsageMu）在此處被零值化（未指派值）
        }
    }
    ```

*   **為何有問題**：
    1.  在 Go 語言中，`map` 是參考型別 (Reference Type)。上述欄位在複製後，克隆出來的 `isolatedCoord` 和原本的 `c` **指向完全相同的 Map 資料結構**。
    2.  因為鎖欄位（例如 `skillUsageMu` 和 `agentCacheMu`）在克隆體中被零值化，克隆體 and 原 Coordinator **各自擁有獨立且互不干涉的互斥鎖實例**。
    3.  當多個 ExtraModels 在多個 Go 協程中並行呼叫 `executeTask` 時：
        *   協程 A 在 `isolatedCoord` 上讀寫共享的 `c.skillUsage`，它鎖定的是 `isolatedCoord.skillUsageMu`。
        *   協程 B 在原 `c`（或另一個 `isolatedCoord2`）上讀寫共享的 `c.skillUsage`，它鎖定的是 `c.skillUsageMu`（或其自身的鎖）。
        *   由於**鎖定的是不同的互斥鎖**，它們對同一個 Map 的併發讀寫**完全失去了互斥保護作用**！
    4.  這會在多個模型同時使用技能或觸發 Agent 快取時，導致 Go 運行時拋出不可恢復的致命錯誤並崩潰：
        `fatal error: concurrent map writes`
        或 `fatal error: concurrent map read and map write`。

*   **修復建議**：
    我們需要確保共用的 Map 必須共用同一個鎖。這有兩種可行的修復方式：

    *   **方案一（推薦，維持共享狀態）**：將 `Coordinator` 中需要共享的 Mutex 欄位改為**指標型別**（例如將 `agentCacheMu sync.RWMutex` 改為 `agentCacheMu *sync.RWMutex`）。在初始化時分配指針，克隆時只複製指標。這樣所有的克隆體在鎖定時，都會鎖定同一個底層 mutex，實現跨克隆體的互斥：
        ```go
        type Coordinator struct {
            agentCacheMu *sync.RWMutex
            // 克隆時直接複製指標即可，Go vet 亦不會報錯：
            // agentCacheMu: orig.agentCacheMu
        }
        ```
    *   **方案二（若不需要共享狀態）**：在 `cloneCoordinator` 中，為這些 map 進行**深拷貝 (Deep Copy)**。但需要注意的是，像 `skillUsage`（技能使用次數統計）等欄位如果深拷貝，子協程寫入的數據將無法累加回主協調器，因此方案一更為安全且符合業務邏輯。
