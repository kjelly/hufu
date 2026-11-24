# 最小 Chrome 遠端桌面

`scripts/init-chrome-desktop.sh` 在 Ubuntu／Debian 的 amd64 或 arm64 主機安裝
Xvfb、Openbox、Chrome、x11vnc、noVNC，不安裝完整 GNOME／KDE 桌面。
安裝需要 sudo；啟動桌面必須使用一般使用者，保留 Chrome sandbox。
腳本不會設定開機自動啟動，也不會安裝 Chrome Remote Desktop（CRD）。

```bash
./scripts/init-chrome-desktop.sh install
./scripts/init-chrome-desktop.sh start https://example.com
```

第一次 `start` 會互動設定 VNC 密碼，也可以先執行 `password` 子命令。
傳統 VNC 密碼只使用前八個字元；遠端傳輸透過 SSH 加密。

在自己的電腦建立 tunnel（將 `USER@SERVER` 換成主機帳號）：

```bash
ssh -N -L 6080:127.0.0.1:6080 USER@SERVER
```

然後開啟 <http://127.0.0.1:6080/vnc.html?autoconnect=true&resize=scale>，輸入
VNC 密碼。noVNC 和 VNC 都只監聽遠端主機的 `127.0.0.1`，不必開放防火牆連入埠。
獨立 Xvfb 模式下，關閉 Chrome 或按 Ctrl+C 會結束這次桌面與連線服務。
共用 CRD 模式下，noVNC 的 Ctrl+C 只停止 noVNC，CRD 與 Chrome 繼續執行。
重新執行 `start` 會保留 Chrome 設定與登入資料。此桌面只啟動 Chrome，並非應用程式安全隔離或網站白名單。

## 預設啟用 Chrome CDP

Chrome 固定使用以下啟動參數：

```text
--remote-debugging-address=127.0.0.1
--remote-debugging-port=9222
--user-data-dir=$HOME/.local/share/chrome-desktop/profile
```

這是與日常 Chrome 分開的持久 profile。腳本確認 `/json/version` 回傳
`webSocketDebuggerUrl` 後才顯示 `Chrome CDP ready`；啟動逾時或任一桌面元件退出
會清理這次程序並回報失敗。若 9222 已被占用，會在啟動前回報錯誤。

在遠端主機確認：

```bash
curl --noproxy '*' --fail http://127.0.0.1:9222/json/version
```

同機 Hufu 的 CDP 位址使用 `http://127.0.0.1:9222`。如果 Hufu 跑在自己的電腦，
另外建立 `ssh -N -L 9222:127.0.0.1:9222 USER@SERVER`；如果跑在容器，容器的
localhost 與主機不同，需要另外設定可達路徑。CDP 能控制瀏覽器，請保留 localhost
限制，只透過可信的 SSH tunnel 存取。

## 與 Chrome Remote Desktop 共用同一個桌面

先安裝及註冊 CRD，停止現有 noVNC 桌面，再將一般使用者的
`~/.chrome-remote-desktop-session` 設成以下內容（換成腳本的實際絕對路徑）：

```sh
#!/bin/sh
exec /absolute/path/to/hufu/scripts/init-chrome-desktop.sh session
```

```bash
chmod +x ~/.chrome-remote-desktop-session
```

修改後重新啟動 CRD 工作階段。CRD 提供 DISPLAY；`session` 啟動 Openbox 與
同樣啟用 CDP 的 Chrome，並記錄 display、Xauthority 路徑與 CDP 埠。

等 CRD 的 Chrome 啟動後，用同一個 Linux 帳號執行：

```bash
./scripts/init-chrome-desktop.sh start
```

`start` 預設偵測腳本建立的既有 CRD 工作階段，讓 x11vnc 直接連上相同 display。
**兩個遠端入口會看到相同視窗、分頁與游標，共用同一個 Chrome、profile 和 CDP。**
這時 `start` 的 URL 參數不會開啟新視窗，關閉 noVNC 不會關閉 CRD。
未偵測到既有工作階段時，才建立獨立 Xvfb 桌面。

CRD 必須使用上述 `session` 入口，兩邊使用相同 `CHROME_DESKTOP_DIR`，才能自動共用。
已在執行的獨立 Xvfb 桌面不能直接轉交給 CRD；請先停止它，再由 CRD 啟動工作階段。
CRD 的安裝、帳號授權及服務啟動需另外完成。

## 可調整項目

```bash
CHROME_RESOLUTION=1920x1080 NOVNC_PORT=6081 \
  ./scripts/init-chrome-desktop.sh start https://example.com
```

| 環境變數 | 預設值 | 用途 |
|---|---|---|
| `CHROME_DESKTOP_DIR` | `~/.local/share/chrome-desktop` | profile、密碼、工作階段鎖，必須為絕對路徑 |
| `CHROME_DISPLAY` | `99` | noVNC 的 Xvfb display |
| `CHROME_RESOLUTION` | `1440x900` | noVNC 的畫面大小 |
| `NOVNC_PORT` | `6080` | noVNC HTTP／WebSocket 埠 |
| `NOVNC_WEB_DIR` | `/usr/share/novnc` | noVNC 靜態網頁目錄 |
| `VNC_PORT` | `5999` | x11vnc 埠 |
| `CHROME_CDP_PORT` | `9222` | Chrome CDP 埠 |

自訂連線埠時請同步修改 SSH tunnel 及用戶端位址。埠必須互不相同，範圍
1024–65535。程序日誌直接輸出到啟動終端，方便排查啟動失敗。

參考：[Chrome 官方安裝說明](https://support.google.com/chrome/answer/95346)、
[noVNC 官方文件](https://github.com/novnc/noVNC)。
