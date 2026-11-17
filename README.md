# herdr-pdf

在 herdr 的分割窗格裡直接看 PDF，不用離開終端機工作區。

Repo／Release：<https://github.com/tim80411/herdr-pdf>

<!-- TODO: screenshot -->

## 特色

- 純 Go 單一 binary（`bin/herdr-pdf`，約 12 MB），不需要任何系統層級的 PDF 函式庫。
- 用 [go-pdfium](https://github.com/klippa-app/go-pdfium) 把 PDFium 編譯成 WebAssembly、透過 [wazero](https://wazero.io) 執行——沒有 cgo，也不連結系統的 Poppler/MuPDF。
- Ctrl+click 任一窗格裡的 `.pdf` 路徑或 `file://` 連結，立刻在旁邊開一個分割窗格顯示。
- 依連線是本機還是遠端自動調整畫質與位元組預算，遠端連線會先送粗略預覽再補完整頁，降低翻頁延遲。
- 目前頁的前後一頁會在背景執行緒預先渲染成點陣圖；也會一併預先編碼成 PNG，正常翻頁時直接重用快取。
- 收到 `SIGTERM`／`SIGHUP`／`SIGINT` 時走跟按 `q` 一樣的收尾流程：等在飛的背景工作結束、刪除畫面上的圖片、還原終端，才結束行程，不會留下殘破畫面。

## 需求

| 項目 | 需求 |
| --- | --- |
| herdr 版本 | 0.9.2 以上（manifest 宣告的 `min_herdr_version`） |
| 作業系統 | macOS、Linux；不支援 Windows（終端幾何依賴 unix ioctl） |
| 終端機 | 需支援 Kitty Graphics Protocol（例如 Ghostty、kitty、WezTerm、iTerm2 3.6+），（herdr 預設就開 `[terminal] kitty_graphics`） |
| 建置工具 | Go 1.26 或以上（`go.mod` 宣告 `go 1.26.0`）；只有在沒有對應平台的預編譯 release 可下載時，`build.sh` 才會 fallback 用它從原始碼建置 |

## 安裝

```sh
herdr plugin install tim80411/herdr-pdf
herdr plugin enable pdf   # 若沒有自動啟用
```

`[[build]]` 會跑 manifest 宣告的 `./build.sh`：先依版本與平台去 GitHub Release 抓對應的預編譯 binary、驗 SHA-256，成功就直接安裝；抓不到（沒有網路、沒有對應 release、驗證失敗）才 fallback 用本機 Go 工具鏈建置。可用環境變數覆寫這個流程：`HERDR_PDF_RELEASE_BASE`（換下載來源）、`HERDR_PDF_FORCE_BUILD=1`（跳過下載、強制本機建置）、`HERDR_PDF_SKIP_BUILD_FALLBACK=1`（下載失敗就直接報錯，不 fallback）。

| 情境 | 怎麼做 |
| --- | --- |
| 指定版本 | `herdr plugin install tim80411/herdr-pdf --ref v0.3.2` |
| 升級 | 重新執行一次 `herdr plugin install tim80411/herdr-pdf`（herdr 沒有獨立的 update 子命令） |
| 這台機器先前用 `link` 掛過 | 先 `herdr plugin unlink pdf`，再 install |

開發這個 plugin 本身時改用本地連結，這樣改完程式碼重新 build 才會反映到執行檔：

```sh
herdr plugin link /path/to/herdr-pdf
herdr plugin enable pdf
```

## 用法

三種開啟方式：

1. **Ctrl+click**：在任一 herdr 窗格裡，對著結尾是 `.pdf` 的路徑或 `file://` 連結 Ctrl+click（比對大小寫不敏感，`.PDF` 也算）。
2. **直接呼叫 opener binary**：
   ```sh
   ./bin/herdr-pdf open /path/to/file.pdf
   ```
   內部會解析路徑後 `exec` 成 `herdr plugin pane open`，在目前窗格右側開一個新的 split 分割窗格。
3. **透過 herdr CLI 手動觸發 action**：
   ```sh
   herdr plugin action invoke open --plugin pdf
   ```
   要注意：action id 是 `open`，plugin 要另外用 `--plugin pdf` 指定，**不是**單一字串 `pdf.open`（用 `herdr plugin action list --plugin pdf` 可以看到回傳的是 `"action_id":"open"` 搭配獨立的 `"plugin_id":"pdf"`）。這個子命令的 `--help` 只列出 `--plugin <ID>` 一個選項，沒有能夾帶檔案路徑的位置參數，所以 PDF 路徑要靠 herdr 幫這次 action 準備好的點擊內容（`HERDR_PLUGIN_CLICKED_URL` 或 context JSON）才能解出來；解不出來時不會失敗，而是開一個瀏覽模式的窗格讓你挑檔案（見下一節）。

## 接到 herdr-file-viewer

`herdr-file-viewer` 是另一個 herdr plugin，按 `O` 鍵會把使用者在它 `config.toml` 設定的 `open` 指令當成 opener 執行，並把目前選取檔案的絕對路徑接在 argv 最後（stdio 全部導向 `/dev/null`，執行結果看不到任何輸出）。`O` 對任何檔案都會觸發、不分是不是 `.pdf`（目錄也算），所以直接把它指到 `herdr-pdf open` 會讓選到非 PDF 檔案時按了沒反應——這正是 `--fallback=system` 存在的原因：解析出的路徑不是 `.pdf` 時改丟給系統 opener（macOS 是 `open`，Linux 是 `xdg-open`），不再印錯誤退出。

在 file-viewer 的 `config.toml`（執行 `herdr plugin config-dir herdr-file-viewer` 可以印出設定檔位置）裡設定：

```toml
open = "/Users/<you>/.config/herdr/plugins/github/pdf-c35b21d6ca39/bin/herdr-pdf open --fallback=system"
```

- herdr 外掛的安裝目錄命名是 `<plugin-id>-<sha256(plugin-id) 前 12 碼>`，`pdf` 這個 plugin id 算出來固定是 `pdf-c35b21d6ca39`，所以這個路徑不會因為升級版本而改變，設定一次就好。
- 不確定實際路徑時可以直接列出來：
  ```sh
  ls -d ~/.config/herdr/plugins/github/pdf-*/bin/herdr-pdf
  ```
- 設定好之後按 `O`：選到 `.pdf` 會在 file-viewer 右側開這個 plugin 的閱讀窗格；選到其他檔案（含目錄）則照舊交給系統 opener 開啟。

## 快捷鍵

也可以在 herdr 的 `config.toml` 綁一個快捷鍵直接觸發開啟動作：

```toml
[[keys.command]]
key = "prefix+p"
type = "plugin_action"
command = "pdf.open"
description = "open selected PDF path in a split pane"
```

- 這裡的 `command` 要寫成合併字串 `<plugin id>.<action id>`，所以是 `pdf.open`——跟前一節 CLI 的 `herdr plugin action invoke` 要拆成 `open --plugin pdf` 兩個獨立參數是不同格式，兩處介面不同，別弄混。
- 按下快捷鍵時，action 讀 herdr 準備好的 context：先看目前選取的文字（`selected_text`）是不是 `.pdf` 路徑，相對路徑會以觸發當下那個 pane 的 cwd 補成絕對路徑。
- **沒有選取文字時，窗格會直接開在「瀏覽模式」**：一樣是右側那個 split 窗格，但先不載入 PDF，而是顯示路徑列＋目錄清單讓你挑。在外層終端機（Ghostty 等）用滑鼠選的文字 herdr 看不到，所以瀏覽模式才是按快捷鍵的日常路徑。
- 只有「完全沒有候選路徑」才會進瀏覽模式；選到的是非 `.pdf` 檔、遠端 `file://` 位址或不存在的檔案，仍然直接報錯。

### 瀏覽模式鍵位

第一行是路徑列（目前目錄的絕對路徑，太長會從前面截掉、保留尾端），中間是目錄清單（子目錄在前、尾綴 `/`，再列 `.pdf`，各自依名稱排序），最後一行是狀態列（`n/m`、過濾字串、鍵位提示）。瀏覽模式是純文字，不會動到繪圖層；選到 PDF 後同一個窗格才切換成閱讀模式並開串流。

鍵位是 fzf 風格：**打字優先**，沒有任何字母被拿去當指令，所以任何檔名都篩得到。

| 按鍵 | 動作 |
| --- | --- |
| 任何可列印字元（含中文） | 即時過濾（不分大小寫的子字串比對） |
| `Backspace` | 刪一個字；**過濾字串已空時＝回上一層**（游標會停在剛離開的那個目錄） |
| `↑`／`↓`、`Ctrl-p`／`Ctrl-n`、`Ctrl-k`／`Ctrl-j` | 上下移動 |
| `Enter` | 進入目錄，或開啟 PDF |
| `Esc` | 有過濾字就清除，沒有就離開（兩段語意） |
| `Tab` | 進入路徑編輯（見下） |
| `Ctrl-h` | 切換顯示隱藏項（`.` 開頭，預設不列） |
| `?` | 顯示鍵位說明（任意鍵返回） |
| `Ctrl-c` | 直接離開 |

兩個模式的狀態列右段都有 `? 說明`；按 `?` 會蓋上一張鍵位表，按任意鍵返回。表的內容由程式裡的鍵位資料結構產生（`internal/browser` 與 `internal/viewer` 各一份 `ui.HelpTable`），狀態列提示也出自同一份，所以這兩張 README 表格只要跟著它對齊就不會有文件與程式不同步的問題。

路徑編輯（`Tab` 進入，欄位是空的）：直接打絕對路徑、相對路徑或 `~` 開頭的路徑；再按 `Tab` 補全（候選只含目錄與 `.pdf`，多個候選時補到共同前綴，並在狀態列列出前幾個）；`Enter` 跳到該目錄或直接開該 PDF；`Esc` 取消回清單；`Ctrl-c` 離開。
- 改完 `~/.config/herdr/config.toml` 要跑 `herdr server reload-config` 才會生效。

## 鍵位

| 按鍵 | 動作 |
| --- | --- |
| `j`、`↓`、空白鍵、`n`、`PgDn` | 下一頁 |
| `k`、`↑`、`p`、`PgUp` | 上一頁 |
| `g` | 回到第一頁 |
| `G` | 若已經先輸入數字就跳到該頁，否則跳到最後一頁 |
| `0`-`9` 接 `G` 或 Enter | 跳到第 N 頁（超出範圍會夾到頭或尾） |
| `r` | 強制重繪（畫面出現殘影時用） |
| `o` | 回到瀏覽模式（目錄設為目前檔案所在目錄，游標停在該檔） |
| `?` | 顯示鍵位說明（任意鍵返回） |
| `q`、`Ctrl-C` | 離開 |

## 設定

| 環境變數 | 可選值 | 預設 | 說明 |
| --- | --- | --- | --- |
| `HERDR_PDF_TRANSPORT` | `kitty` | `kitty` | 已無作用：herdr 0.9.2 移除 socket 圖形 API 後只剩 Kitty escape 這一條路；舊值（`stream-png` 等）仍接受，viewer.log 會記一筆 |
| `HERDR_PDF_MAX_FRAME_KB` | 正整數 | 遠端 200 KB／本機 1500 KB（自動判斷） | 覆寫單張畫面的位元組預算 |
| `HERDR_PDF_PROGRESSIVE` | `1`/`0`（也接受 true/false/yes/no/on/off） | 依連線是否遠端自動決定 | 強制開關漸進式預覽 |
| `HERDR_PDF_TRACE` | `1`/`true`/`yes`/`on` | 關閉 | 把每次重繪各階段的耗時寫進 `viewer.log` |
| `PDFVIEW_DIR` | 目錄路徑 | 行程自己的工作目錄 | 沒有給 `PDFVIEW_FILE` 時，瀏覽模式從哪個目錄開始 |

前四個變數都會被 `open` 動作轉發進新開的 pane，所以直接下 `HERDR_PDF_TRACE=1 ./bin/herdr-pdf open file.pdf` 也有效。`PDFVIEW_DIR` 則是 `open` 在解不出路徑時自己帶上的：值取觸發那個 pane 的 cwd。

舊的 `stream-png` / `stream-rgba` / `direct` / `set` 值會被忽略並對應到 `kitty`：herdr 0.9.2 移除了 `pane.graphics.*`，畫面現在是直接寫到 pane 自己輸出的 Kitty graphics escape（分段 base64 PNG）。

log 檔預設寫在 `$HERDR_PLUGIN_STATE_DIR/viewer.log`（由 herdr 啟動 pane 時提供這個環境變數），沒有這個環境變數（獨立執行除錯用）時退回 `/tmp/herdr-pdf-viewer.log`；檔案權限固定是擁有者可讀寫（`0600`），因為裡面會記錄使用者開啟過的 PDF 路徑，既有檔案權限不對也會在開檔時修正回來。wazero 的編譯快取放在 `$HERDR_PLUGIN_STATE_DIR/wazero`，同樣情況下退回 `~/.cache/herdr-pdf/wazero`。

## 運作原理

```
PDF 檔案
   │  讀檔 + pdfium OpenDocument
   ▼
go-pdfium（PDFium 編譯成 WASM，wazero 執行）
   │  RenderPage → 一張 RGBA frame
   ▼
EncodeAdaptive / EncodePreview
   │  編碼成 PNG（真彩 → 灰階 → 16 階灰階 → 逐步縮小，直到符合預算）
   ▼
Kitty graphics escape（分段 base64 PNG，寫到 pane 自己的輸出）
   │  每頁換新 image id，送完再刪舊的；離開、overlay、換檔時全刪
   ▼
herdr server → Kitty Graphics Protocol escape → 終端機
```

選 PNG 而不是原始 RGBA，是因為 herdr 在 plugin 和終端機之間完全不做壓縮，拿到多少 bytes 就原樣送出去（herdr 本體原始碼 `protocol/wire.rs:1213-1222` 與 `server/client_transport.rs:119-124`）。未壓縮的 RGBA 大小是寬×高×4 bytes，填滿一個分割窗格通常是數 MB 到十幾 MB（依 pane 尺寸而定），同一頁編碼成 PNG 通常只剩幾百 KB，翻頁延遲的落差主要就來自這個位元組數差距。

點陣圖解析度上限是 2400×3200 px，等比例縮放；畫布超過上限時會先縮到上限內再渲染，避免超大分割窗格撐爆記憶體，herdr 收到圖後仍依 placement 縮放回填滿整個窗格，顯示大小不受影響。單張畫面另外限制在 16 MiB 內（herdr 每個 pane 的圖片儲存總共只有 64 MiB），找不到能壓進預算內的編碼結果時，狀態列會顯示「此頁太大，未送出」而不會硬送一個一定被拒絕的 frame。

遠端連線的判斷方式是看 herdr 所在主機的環境有沒有 `SSH_CONNECTION`／`SSH_TTY`（外掛沒辦法直接知道用戶端在哪），也可以用 `HERDR_PDF_MAX_FRAME_KB`／`HERDR_PDF_PROGRESSIVE` 手動覆寫。遠端連線會多套用兩個機制：位元組預算收緊到 200 KB（本機是 1500 KB）；以及漸進式預覽——按下翻頁鍵先送一張很小的預覽圖，約 90 毫秒後再補送完整頁，這個延遲要蓋過 herdr 自己的渲染節奏才有意義，不然兩張圖會被 herdr 合成同一個畫面而預覽根本沒機會被畫出來；連續按鍵時，新的按鍵會取消前一個還沒補完的精細版，所以按著不放只會一直送預覽。

## 疑難排解

| 症狀 | 原因 | 處理 |
| --- | --- | --- |
| 窗格空白，`viewer.log` 沒有錯誤 | 終端機不支援 Kitty graphics，或 herdr 版本低於 0.9.2 | 換成支援 KGP 的終端機；把 herdr 更新到 0.9.2 以上 |
| Ctrl+click 在 `.pdf` 路徑上沒反應 | herdr 版本低於 manifest 宣告的最低版本 0.9.2 | 把 herdr 更新到 0.9.2 以上 |
| 翻頁明顯變慢／卡頓 | 遠端連線的位元組預算收緊到 200 KB，複雜頁面要多跑幾階降階編碼才會壓到預算內；也可能是剛 resize 完，pane 尺寸快取失效要重新查一次 | 這是預期行為；本機連線可以用 `HERDR_PDF_MAX_FRAME_KB` 放寬預算，或開 `HERDR_PDF_TRACE=1` 看 log 找出是哪一階段慢 |
| `build.sh` 下載預編譯 binary 失敗，且本機也沒有可用的 Go | 沒有網路／沒有對應平台的 release，本機又沒裝 Go 1.26 以上 | 安裝 Go，或設定 `$GO` 指到已安裝的 go 執行檔，再重新跑 `herdr plugin install`／`link` |
| 開啟後窗格顯示「此 PDF 需要密碼，尚不支援」 | PDF 有加密，目前沒有密碼輸入介面 | 先用其他工具移除密碼 |
| 開啟後窗格顯示「檔案損毀或不是 PDF」 | 檔案不是合法 PDF，或已損毀 | 確認來源檔案完整 |
| 開啟後窗格顯示「無法開啟 PDF（詳見 viewer.log）」，或 `open` 直接印錯誤退出（exit code 1） | 其他未分類的開檔失敗；也可能是路徑本身被拒絕：`file://` 指向非本機 host、路徑含 NUL 位元組、或副檔名不是 `.pdf` | 看 `viewer.log` 取得詳細錯誤；確認傳入的是本機 `.pdf` 檔案的路徑 |

## 開發

```sh
go build -trimpath -ldflags="-s -w" -o bin/herdr-pdf ./cmd/herdr-pdf
# 或直接：
./build.sh

go vet ./...
go test ./...
```

以上 `go vet` 與 `go test` 兩個檢查在目前這份原始碼上都是乾淨通過的。

| 路徑 | 職責 |
| --- | --- |
| `cmd/herdr-pdf/` | CLI 進入點：`view` / `open` / `version` 子命令 |
| `internal/herdr/` | socket client（短連線 `Call`）+ 長連線 `StreamSession` |
| `internal/render/` | go-pdfium/wazero 頁面點陣化 + 頁面快取（FIFO，上限 6 張，命中不重排） |
| `internal/ui/` | raw mode 終端機生命週期、按鍵解碼、狀態列排版 |
| `internal/viewer/` | pane 的狀態機：按鍵處理、resize、render+send |
| `internal/openpath/` | `open` action／link handler 的路徑解析 |
| `spike/go-stream/` | `pane.graphics.stream` 路徑的可行性驗證程式（已過時，herdr 0.9.2 移除了該 API） |

## 已知限制與待辦

限制：

| 限制 | 說明 |
| --- | --- |
| 只支援 KGP 終端機 | 沒有 Kitty graphics 能力的終端機會看不到圖，viewer 本身偵測不到 |
| 不支援 popup 版位 | herdr 的 `popup` placement 不會給進程 `HERDR_PANE_ID`，這個外掛只會開 `split` 窗格 |
| 多個 client 同時連線時，pane 的 cell 尺寸可能不穩定 | 實際觀察過同一個 pane 前後回報 8×19 px 和 15×38 px 兩種不同尺寸；現在只把 TIOCGWINSZ 回報的像素當選點陣解析度的提示（不合理就用 10×20），顯示大小由 herdr 依 cell 框縮放 |
| 加密或損毀的 PDF 只會顯示錯誤 | 目前沒有密碼輸入介面 |
| 沒有縮放、平移、文字搜尋、大綱 | 目前一律縮放至剛好填滿窗格 |
| 只在 macOS 上實機驗證過（對應 herdr 0.9.5） | manifest 有宣告支援 Linux，但 Linux 尚未實機驗證過 |

Roadmap（原始碼裡已經預留的擴充點）：

| 項目 | 目前狀態 |
| --- | --- |
| 搜尋高亮／大綱疊層 | viewer 目前用的圖層 id 是 `page` 而不是 `primary`，就是為了讓未來這類疊層可以用另一個圖層，不用跟目前畫面搶同一層 |

## 授權與致謝

MIT License（見 `LICENSE`）。

主要依賴：

| 套件 | 授權 |
| --- | --- |
| [go-pdfium](https://github.com/klippa-app/go-pdfium) | MIT |
| PDFium（透過 go-pdfium 內嵌的 WASM 版本） | BSD-3-Clause（Google PDFium 專案本身的授權） |
| [wazero](https://github.com/tetratelabs/wazero) | Apache-2.0 |
| [go-runewidth](https://github.com/mattn/go-runewidth) | MIT |

致謝／參考：

- [olehsharov/herdr-imgpopup](https://github.com/olehsharov/herdr-imgpopup) — 驗證了 `pane.graphics` API 這條路可行
- [zenbu-labs/terminal-browser](https://github.com/zenbu-labs/terminal-browser) — `stream` 用法的參考對照
- herdr 官方 socket API 文件

---

## English (quick start)

**herdr-pdf** renders PDF pages as images in a herdr split pane, so you can read a PDF without leaving your terminal workspace. It ships as a single Go binary (`bin/herdr-pdf`, about 12 MB) that rasterizes pages with PDFium compiled to WebAssembly via [go-pdfium](https://github.com/klippa-app/go-pdfium) and [wazero](https://wazero.io) — no cgo, no system Poppler/MuPDF.

### Requirements

- herdr 0.9.2 or newer (the manifest's declared `min_herdr_version`)
- macOS or Linux; Windows is not supported (terminal geometry relies on Unix ioctls)
- A terminal with Kitty Graphics Protocol support (Ghostty, kitty, WezTerm, iTerm2 3.6+) (herdr enables `[terminal] kitty_graphics` by default)
- Go 1.26 or newer (`go.mod` pins 1.26.0) — only needed as a fallback when no prebuilt release binary matches your platform

### Install

```sh
herdr plugin install tim80411/herdr-pdf
herdr plugin enable pdf   # if it doesn't come up enabled
```

`[[build]]` runs `./build.sh`, which downloads a version- and platform-matched, SHA-256-verified prebuilt binary from the GitHub release first, and only falls back to a local `go build` on a miss (no network, no matching release, checksum failure). For plugin development, use `herdr plugin link /path/to/herdr-pdf` instead so rebuilds pick up local source changes.

Pin a version with `--ref v0.3.2`; to upgrade, just re-run `herdr plugin install tim80411/herdr-pdf` (herdr has no separate update subcommand). If this machine previously had the plugin linked, run `herdr plugin unlink pdf` first.

### Usage

- Ctrl+click any `.pdf` path or `file://` link in a herdr pane.
- Or run the opener binary directly: `./bin/herdr-pdf open /path/to/file.pdf`
- Or invoke the action through herdr's CLI: `herdr plugin action invoke open --plugin pdf` — note the action id is `open` with a separate `--plugin pdf` flag, not a combined `pdf.open`. This form has no CLI slot for a file path, so it depends on herdr's own click context to resolve one; prefer the two options above for everyday use.
- Or bind a key in herdr's `config.toml` (`[[keys.command]]`, `type = "plugin_action"`, `command = "pdf.open"` — the combined `<plugin>.<action>` form, unlike the CLI's separate `open --plugin pdf`); it opens whatever `.pdf` path is currently selected in the pane, resolved against that pane's cwd. Run `herdr server reload-config` after editing.
- With nothing selected, the pane opens in **browser mode** instead of failing: the same split pane, but showing a path bar and a directory listing rather than a document. A selection made in the outer terminal emulator never reaches herdr, so browser mode is the everyday path for the keybinding. A real error — a non-PDF, a remote `file://` host, a missing file — still reports itself rather than falling back.
- Or wire it up as the opener for **herdr-file-viewer** (a separate herdr plugin): set its `open` config to `<abs path to>/bin/herdr-pdf open --fallback=system`, and its `O` key opens `.pdf` files here while handing everything else to the OS opener (`open` on macOS, `xdg-open` on Linux) instead of doing nothing. See the "接到 herdr-file-viewer" section above for the exact config path.

### Browser mode keys

Row 1 is the path bar (the current directory, truncated from the front so its deep end stays visible); the middle is the listing (subdirectories first with a trailing `/`, then `.pdf` files, each sorted by name); the last row is the status line (`n/m`, the filter, the key hints). Browsing is plain text and touches no graphics layer — the same pane switches to reading mode and sends an image only once a file is chosen.

The keymap is fzf-style: **typing wins**, no letter is reserved for navigation, so every filename is reachable by filter.

| Key | Action |
| --- | --- |
| Any printable character (CJK included) | Filter as you type (case-insensitive substring) |
| `Backspace` | Delete one character; **with the filter already empty, go up one level** (the cursor lands on the directory just left) |
| `↑`/`↓`, `Ctrl-p`/`Ctrl-n`, `Ctrl-k`/`Ctrl-j` | Move |
| `Enter` | Enter a directory, or open a PDF |
| `Esc` | Clear the filter if there is one, otherwise quit |
| `Tab` | Edit the path bar (below) |
| `Ctrl-h` | Toggle hidden entries (dot-names, omitted by default) |
| `?` | Show the key table (any key returns) |
| `Ctrl-c` | Quit outright |

Both modes carry `? 說明` on the status row; `?` overlays a key table that any key dismisses. The table is generated from the keymap declared in code (one `ui.HelpTable` each in `internal/browser` and `internal/viewer`), and the status-line hints come from the same declaration, so these README tables only have to agree with it.

Path editing (`Tab`, starting from an empty field): type an absolute, relative, or `~`-rooted path; press `Tab` again to complete (candidates are directories and PDFs; several of them complete to the shared prefix and the first few are listed on the status row); `Enter` goes there or opens the file; `Esc` returns to the listing; `Ctrl-c` quits.

### Keys

| Key | Action |
| --- | --- |
| `j`, `↓`, space, `n`, `PgDn` | Next page |
| `k`, `↑`, `p`, `PgUp` | Previous page |
| `g` | First page |
| `G` | Jump to the typed page number, or last page if none was typed |
| digits then `G` or Enter | Jump to page N |
| `r` | Force redraw |
| `o` | Back to browser mode, on this file's directory with the cursor on it |
| `?` | Show the key table (any key returns) |
| `q`, `Ctrl-C` | Quit |

### Environment variables

| Variable | Values | Default | Effect |
| --- | --- | --- | --- |
| `HERDR_PDF_TRANSPORT` | `kitty` | `kitty` | No longer selects anything: only Kitty escapes remain since herdr 0.9.2. Old values (`stream-png` etc.) are accepted and logged |
| `HERDR_PDF_MAX_FRAME_KB` | positive integer | 200 KB remote / 1500 KB local (auto-detected) | Override the per-frame byte budget |
| `HERDR_PDF_PROGRESSIVE` | `1`/`0` (also true/false/yes/no/on/off) | auto (on for remote sessions) | Force progressive preview on or off |
| `HERDR_PDF_TRACE` | `1`/`true`/`yes`/`on` | off | Log per-stage timing to `viewer.log` |
| `PDFVIEW_DIR` | a directory path | the process's cwd | Where browser mode starts when no `PDFVIEW_FILE` was given |

### License

MIT (see `LICENSE`). PDFium is BSD-3-Clause; go-pdfium and go-runewidth are MIT; wazero is Apache-2.0.
