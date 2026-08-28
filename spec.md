## 6. go.mod 依賴升級計畫

> **調查基準：** 2026-08-18；本節只記錄升級計畫，不代表已修改
> `go.mod`／`go.sum`。目前工作樹的其他變更與本計畫無關。

### 6.1 優先順序

#### P0：先處理安全性

1. 將 Go toolchain 從 `1.26.5` 升至 `1.26.6`。目前的 `govulncheck` 已在
   `net/url`、`crypto/tls` 與 `encoding/asn1` 找到標準庫漏洞；Go 1.26.6 的
   release history 列出相應安全修正。
2. 將間接依賴 `golang.org/x/net` 從 `v0.52.0` 升至 `v0.58.0`。Hufu 的
   `internal/tools/fetch.go` 會實際走到 `golang.org/x/net/html`；漏洞資料庫指出
   `v0.55.0` 以前的 HTML parser 版本受 XSS／解析問題影響，因此不應停在低於
   `v0.55.0` 的版本。

參考：

- https://go.dev/doc/devel/release#go1.26.6
- https://pkg.go.dev/vuln/GO-2026-5025

#### P1：升級 Hufu 直接使用的核心依賴

1. `charm.land/fantasy`: `v0.17.2` → `v0.41.1`
   - 目前直接使用 agent、provider、tool、message、usage、streaming 及 retry API。
   - 版本跨度大，並會把 module 的最低 Go 版本提高至 `1.26.5`。
   - 升級後需特別驗證 Ollama／OpenAI-compatible provider、tool calling、streaming、
     usage accounting、stop conditions、context length 與 retry 行為。
   - 不應只以編譯成功視為完成；Fantasy 的 provider 行為變更可能是 runtime regression。
   - 參考：https://github.com/charmbracelet/fantasy/releases
2. `github.com/mark3labs/mcp-go`: `v0.48.0` → `v0.58.0`
   - Hufu 直接使用 MCP client、tool schema 與 stdio／remote transport。
   - 需驗證 stdio、SSE／Streamable HTTP、tool authorization、schema preservation、
     timeout、disconnect/reconnect 與 DNS rebinding 防護。
   - 暫不升至 `v1.0.0-beta.1`；該版本涉及新的 MCP specification，應另開相容性計畫。
   - 參考：https://github.com/mark3labs/mcp-go/releases
3. `modernc.org/sqlite`: `v1.34.4` → `v1.56.0`
   - Hufu 的 `internal/context` 直接依賴 SQLite 保存 persistent context。
   - 這是較大的 SQLite／transpiled runtime 跨版本升級，需驗證既有 database、schema
     migration、transaction rollback、resume、並行存取及 Linux／macOS／Windows build。
   - 讓 `go mod tidy` 自動配對 `modernc.org/libc`、`memory`、`mathutil` 等 transitive
     dependencies；不要單獨手動升級 `modernc.org/libc`。
   - 參考：https://gitlab.com/cznic/sqlite/-/blob/master/CHANGELOG.md
4. `golang.org/x/sys`: `v0.43.0` → `v0.47.0`
   - Hufu 直接用於 Unix PTY／terminal，屬低風險維護更新。
   - 需驗證 PTY、terminal broker、signal、resize，以及 Linux／macOS build。
   - 參考：https://pkg.go.dev/golang.org/x/sys

### 6.2 分批實施

每一批都必須在獨立 commit 中完成，避免一次升級造成問題時無法定位來源：

1. Go 1.26.6 與 `golang.org/x/net@v0.58.0`。
2. `github.com/mark3labs/mcp-go@v0.58.0`。
3. `charm.land/fantasy@v0.41.1`，並將 `go` directive 更新至至少 `1.26.5`。
4. `modernc.org/sqlite@v1.56.0`，由 `go mod tidy` 對齊其 native/transpiled dependencies。
5. `golang.org/x/sys@v0.47.0`，可與前一批合併但應保留獨立測試結果。

### 6.3 每批驗收條件

每次依賴變更都必須保存：

1. `go mod tidy` 後的 `go.mod`／`go.sum` diff，並確認沒有不必要的 direct dependency。
2. `go test ./...`、`go vet ./...` 與 `golangci-lint run` 成功。
3. 受影響功能的 targeted tests；若涉及 SQLite 或 terminal，追加 `go test -race`。
4. `govulncheck ./...` 不再報告可由此次升級修復的漏洞。
5. provider／MCP／SQLite 的 smoke test 結果，以及失敗時的 rollback 版本。

### 6.4 已完成的升級前驗證

在不修改工作樹的臨時 module file 中，將上述四個直接依賴同時升級至建議版本後，
`go test ./...` 全部通過。這只能證明目前程式碼可編譯並通過既有測試，尚不能取代
真實 provider、MCP transport、SQLite migration 或跨平台驗證。

目前沒有立即需要更新的其他直接依賴：Bubble Tea、Bubbles、Lip Gloss、Cobra、YAML、
chromem-go、Yaegi、Gopher Lua、readline、termenv 與 pty 均未列出更高可用版本。
