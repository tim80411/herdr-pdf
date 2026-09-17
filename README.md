# herdr-pdf

在 herdr 的分割窗格裡直接看 PDF，不用離開終端機工作區。

<!-- TODO: screenshot -->

## 特色

- 純 Go 單一 binary（`bin/herdr-pdf`，約 12 MB），不需要任何系統層級的 PDF 函式庫。
- 用 [go-pdfium](https://github.com/klippa-app/go-pdfium) 把 PDFium 編譯成 WebAssembly、透過 [wazero](https://wazero.io) 執行——沒有 cgo，也不連結系統的 Poppler/MuPDF。
- Ctrl+click 任一窗格裡的 `.pdf` 路徑或 `file://` 連結，立刻在旁邊開一個分割窗格顯示。
- 依連線是本機還是遠端自動調整畫質與位元組預算，遠端連線會先送粗略預覽再補完整頁，降低翻頁延遲。
- 目前頁的前後一頁會在背景執行緒預先渲染成點陣圖；預設的 `stream-png` transport 還會一併預先編碼成 PNG，其餘 transport 只預先渲染，正常翻頁時直接重用快取。
- 收到 `SIGTERM`／`SIGHUP`／`SIGINT` 時走跟按 `q` 一樣的收尾流程：等在飛的背景工作結束、關閉串流、還原終端，才結束行程，不會留下殘破畫面。

## 需求

| 項目 | 需求 |
| --- | --- |
| herdr 版本 | 0.9.0 以上（manifest 宣告的 `min_herdr_version`） |
| 作業系統 | macOS、Linux（manifest `platforms` 只列了這兩個，尚未宣告 Windows） |
| 終端機 | 需支援 Kitty Graphics Protocol（例如 Ghostty、kitty、WezTerm、iTerm2 3.6+），且 herdr 設定要開 `[terminal] kitty_graphics`（0.9.0 預設為開） |
| 建置工具 | Go 1.26 或以上（`go.mod` 宣告 `go 1.26.0`）；只有在沒有對應平台的預編譯 release 可下載時，`build.sh` 才會 fallback 用它從原始碼建置 |

## 安裝

```sh
herdr plugin install <owner>/herdr-pdf
herdr plugin enable pdf   # 若沒有自動啟用
```

`[[build]]` 會跑 manifest 宣告的 `./build.sh`：先依版本與平台去 GitHub Release 抓對應的預編譯 binary、驗 SHA-256，成功就直接安裝；抓不到（沒有網路、沒有對應 release、驗證失敗）才 fallback 用本機 Go 工具鏈建置。可用環境變數覆寫這個流程：`HERDR_PDF_RELEASE_BASE`（換下載來源）、`HERDR_PDF_FORCE_BUILD=1`（跳過下載、強制本機建置）、`HERDR_PDF_SKIP_BUILD_FALLBACK=1`（下載失敗就直接報錯，不 fallback）。

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
   要注意：action id 是 `open`，plugin 要另外用 `--plugin pdf` 指定，**不是**單一字串 `pdf.open`（用 `herdr plugin action list --plugin pdf` 可以看到回傳的是 `"action_id":"open"` 搭配獨立的 `"plugin_id":"pdf"`）。這個子命令的 `--help` 只列出 `--plugin <ID>` 一個選項，沒有能夾帶檔案路徑的位置參數，所以手動下這行指令時，PDF 路徑要靠 herdr 幫這次 action 準備好的點擊內容（`HERDR_PLUGIN_CLICKED_URL` 或 context JSON）才能解出來；日常操作建議用方式 1 或 2，這個方式比較適合已經有點擊上下文的場景。

## 鍵位

| 按鍵 | 動作 |
| --- | --- |
| `j`、`↓`、空白鍵、`n`、`PgDn` | 下一頁 |
| `k`、`↑`、`p`、`PgUp` | 上一頁 |
| `g` | 回到第一頁 |
| `G` | 若已經先輸入數字就跳到該頁，否則跳到最後一頁 |
| `0`-`9` 接 `G` 或 Enter | 跳到第 N 頁（超出範圍會夾到頭或尾） |
| `r` | 強制重繪（畫面出現殘影時用） |
| `q`、`Ctrl-C` | 離開 |

## 設定

| 環境變數 | 可選值 | 預設 | 說明 |
| --- | --- | --- | --- |
| `HERDR_PDF_TRANSPORT` | `stream-png` / `stream-rgba` / `direct` / `set` | `stream-png` | 畫面送到終端機走哪條路徑 |
| `HERDR_PDF_MAX_FRAME_KB` | 正整數 | 遠端 200 KB／本機 1500 KB（自動判斷） | 覆寫單張畫面的位元組預算 |
| `HERDR_PDF_PROGRESSIVE` | `1`/`0`（也接受 true/false/yes/no/on/off） | 依連線是否遠端自動決定 | 強制開關漸進式預覽 |
| `HERDR_PDF_TRACE` | `1`/`true`/`yes`/`on` | 關閉 | 把每次重繪各階段的耗時寫進 `viewer.log` |

這四個變數都會被 `open` 動作轉發進新開的 pane，所以直接下 `HERDR_PDF_TRACE=1 ./bin/herdr-pdf open file.pdf` 也有效。

`stream-rgba` 是舊版直接傳未壓縮 RGBA 的做法；`direct` 會把 RGBA 寫進 `pane.graphics.info` 回報的 `file_frame_directory`、只送檔案路徑換取 herdr 的零複製傳輸，但只有在圖層是 `primary` 且用戶端已握手出 `direct-kitty` 時才會生效，握手沒成功就自動退回 `stream-png`；`set` 每頁各發一次 `pane.graphics.set`，硬上限 512 KiB（實際保留一點緩衝，卡在 500 KiB 左右），太大會自動縮圖重試——串流連不上或中途斷線時，viewer 也會自動退回這條路。

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
pane.graphics.stream（長連線 unix socket）
   │  一個 pane 開一條連線，用到關閉為止
   ▼
herdr server → Kitty Graphics Protocol escape → 終端機
```

選 PNG 而不是原始 RGBA，是因為 herdr 在 plugin 和終端機之間完全不做壓縮，拿到多少 bytes 就原樣送出去（herdr 本體原始碼 `protocol/wire.rs:1213-1222` 與 `server/client_transport.rs:119-124`）。未壓縮的 RGBA 大小是寬×高×4 bytes，填滿一個分割窗格通常是數 MB 到十幾 MB（依 pane 尺寸而定），同一頁編碼成 PNG 通常只剩幾百 KB，翻頁延遲的落差主要就來自這個位元組數差距。

點陣圖解析度上限是 2400×3200 px，等比例縮放；畫布超過上限時會先縮到上限內再渲染，避免超大分割窗格撐爆記憶體，herdr 收到圖後仍依 placement 縮放回填滿整個窗格，顯示大小不受影響。串流單張畫面另外受 herdr 端 16 MiB 的硬性上限，找不到能壓進預算內的編碼結果時，狀態列會顯示「此頁太大，未送出」而不會硬送一個一定被拒絕的 frame。

遠端連線的判斷方式是看 `pane.graphics.info` 回報的 `file_frame_transport` 是不是 `direct-kitty`，不是就當作遠端（或沒有用戶端連著）。遠端連線會多套用兩個機制：位元組預算收緊到 200 KB（本機是 1500 KB）；以及漸進式預覽——按下翻頁鍵先送一張很小的預覽圖，約 90 毫秒後再補送完整頁，這個延遲要蓋過 herdr 自己的渲染節奏才有意義，不然兩張圖會被 herdr 合成同一個畫面而預覽根本沒機會被畫出來；連續按鍵時，新的按鍵會取消前一個還沒補完的精細版，所以按著不放只會一直送預覽。

## 疑難排解

| 症狀 | 原因 | 處理 |
| --- | --- | --- |
| 窗格空白，`viewer.log` 出現 `cell_size_unavailable` 或 `feature_disabled` | 終端機不支援 Kitty graphics，或 herdr 設定沒開 | 換成支援 KGP 的終端機；在 herdr 的 `config.toml` 開啟 `[terminal] kitty_graphics` |
| Ctrl+click 在 `.pdf` 路徑上沒反應 | herdr 版本低於 manifest 宣告的最低版本 0.9.0 | 把 herdr 更新到 0.9.0 以上 |
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
| `spike/go-stream/` | `pane.graphics.stream` 路徑的可行性驗證程式 |

## 已知限制與待辦

限制：

| 限制 | 說明 |
| --- | --- |
| 只支援 KGP 終端機 | 沒有 Kitty graphics 能力的 pane，herdr 會以 `feature_disabled`（設定沒開）或 `cell_size_unavailable`（終端不支援）拒絕圖形呼叫，狀態列顯示對應原因 |
| 不支援 popup 版位 | herdr 的 `popup` placement 不會給進程 `HERDR_PANE_ID`，這個外掛只會開 `split` 窗格 |
| 多個 client 同時連線時，pane 的 cell 尺寸可能不穩定 | 實際觀察過同一個 pane 前後回報 8×19 px 和 15×38 px 兩種不同尺寸；查詢結果有 2 秒 TTL 的 stale-while-revalidate 快取，過期時先用舊值重繪，背景重新查詢下一次生效，不會卡住按鍵 |
| 加密或損毀的 PDF 只會顯示錯誤 | 目前沒有密碼輸入介面 |
| 沒有縮放、平移、文字搜尋、大綱 | 目前一律縮放至剛好填滿窗格 |
| 只在 macOS 上實機驗證過（對應 herdr 0.9.0） | manifest 有宣告支援 Linux，但 Linux 尚未實機驗證過 |

Roadmap（原始碼裡已經預留的擴充點）：

| 項目 | 目前狀態 |
| --- | --- |
| 預編譯二進位分發（fetch-or-build，`build.sh` 實作） | 已完成，見上方安裝段落；GitHub Release 目前還沒發佈過任何 tag，發佈前一律走原始碼建置 fallback |
| `direct-kitty` 零複製檔案傳輸 | 程式碼裡已經有這個 transport 選項，但需要 herdr 用戶端握手支援才會真的生效，沒握手成功會自動退回 PNG 串流 |
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

- herdr 0.9.0 or newer (the manifest's declared `min_herdr_version`)
- macOS or Linux (the manifest's declared `platforms`; Windows is not listed yet)
- A terminal with Kitty Graphics Protocol support (Ghostty, kitty, WezTerm, iTerm2 3.6+), with `[terminal] kitty_graphics` enabled in herdr's config
- Go 1.26 or newer (`go.mod` pins 1.26.0) — only needed as a fallback when no prebuilt release binary matches your platform

### Install

```sh
herdr plugin install <owner>/herdr-pdf
herdr plugin enable pdf   # if it doesn't come up enabled
```

`[[build]]` runs `./build.sh`, which downloads a version- and platform-matched, SHA-256-verified prebuilt binary from the GitHub release first, and only falls back to a local `go build` on a miss (no network, no matching release, checksum failure). For plugin development, use `herdr plugin link /path/to/herdr-pdf` instead so rebuilds pick up local source changes.

### Usage

- Ctrl+click any `.pdf` path or `file://` link in a herdr pane.
- Or run the opener binary directly: `./bin/herdr-pdf open /path/to/file.pdf`
- Or invoke the action through herdr's CLI: `herdr plugin action invoke open --plugin pdf` — note the action id is `open` with a separate `--plugin pdf` flag, not a combined `pdf.open`. This form has no CLI slot for a file path, so it depends on herdr's own click context to resolve one; prefer the two options above for everyday use.

### Keys

| Key | Action |
| --- | --- |
| `j`, `↓`, space, `n`, `PgDn` | Next page |
| `k`, `↑`, `p`, `PgUp` | Previous page |
| `g` | First page |
| `G` | Jump to the typed page number, or last page if none was typed |
| digits then `G` or Enter | Jump to page N |
| `r` | Force redraw |
| `q`, `Ctrl-C` | Quit |

### Environment variables

| Variable | Values | Default | Effect |
| --- | --- | --- | --- |
| `HERDR_PDF_TRANSPORT` | `stream-png`, `stream-rgba`, `direct`, `set` | `stream-png` | How frames reach the terminal |
| `HERDR_PDF_MAX_FRAME_KB` | positive integer | 200 KB remote / 1500 KB local (auto-detected) | Override the per-frame byte budget |
| `HERDR_PDF_PROGRESSIVE` | `1`/`0` (also true/false/yes/no/on/off) | auto (on for remote sessions) | Force progressive preview on or off |
| `HERDR_PDF_TRACE` | `1`/`true`/`yes`/`on` | off | Log per-stage timing to `viewer.log` |

### License

MIT (see `LICENSE`). PDFium is BSD-3-Clause; go-pdfium and go-runewidth are MIT; wazero is Apache-2.0.
