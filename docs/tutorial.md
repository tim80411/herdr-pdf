# herdr-pdf 是怎麼做出來的

## 1. 這份教學在講什麼

這份教學把 `herdr-pdf` 從「有沒有人做過」到「在遠端連線上翻頁也即時」的四天，拆成**方法**（怎麼決定）與**實作**（怎麼寫）兩條線寫下來。它不是 API 文件，README 已經負責那件事；它回答的是「為什麼每個地方長成這樣」，以及「哪些結論是踩出來的、怎麼踩到的」。

成品長這樣：在 herdr 裡對任何一行結尾是 `.pdf` 的路徑 Ctrl+click，右邊會長出一個分割窗格（split pane）。窗格上方是整頁 PDF 的點陣圖，靠 herdr 的圖形圖層畫上去；最底下一行是反白的狀態列，左邊是檔名（CJK 檔名會正確截斷），中間是 `頁 3/18  [j/k 翻頁 g/G 首尾 q 離開]`，右邊是這次重畫的診斷字串，例如 `png:gray(145KB) 807x1140 21ms`。按 `j` 換下一頁；在遠端連線上按下去會先出現一張略糊的整頁，約 90 毫秒後自動變清晰。

全部程式碼是一支 Go 靜態執行檔，約 12 MB，內含編成 WebAssembly 的 PDFium，不連結任何系統 PDF 函式庫。

| 項目 | 內容 |
| --- | --- |
| 程式碼規模 | 主程式 + 5 個 internal 套件，2,590 行 Go，另有 591 行測試 |
| 外部相依 | go-pdfium、wazero、go-runewidth、golang.org/x/{sys,term}（見 `go.mod`） |
| 時程 | 2026-09-11 研究 → 09-14 commit `4951cd7`，共四天 |
| 途中丟掉的東西 | 一整份能跑的 Python MVP（531 行），在第三天被 Go 取代 |

---

## 2. 方法：從「有沒有人做過」到「可以用」的七步

每一步的共同形狀是：**先問一個能被否證的問題，用第一手證據回答，再決定要不要往下蓋。**「先 spike 再蓋」不是口號，是這個專案唯一沒有返工的部分。

### 步驟表

| # | 問了什麼 | 怎麼驗證 | 得到什麼 | 花多久 |
| --- | --- | --- | --- | --- |
| 1 | 有沒有人做過 herdr 的 PDF viewer？ | 三個平行研究：marketplace 全文搜尋、herdr 原始碼逐檔讀、終端 PDF 工具全貌 | 1,022 個 plugin 裡零命中；不是做不到，是 plugin 系統只有三個月、圖形 API 四天前才預設開啟 | 半天 |
| 2 | `pane.graphics.set` 真的能畫出東西嗎？ | 寫最小 Python 腳本，對自己所在的 pane 送一張 PNG，**請使用者用眼睛確認** | 能。這一步之後所有架構討論才有意義 | 1 小時 |
| 3 | 完整的 viewer 需要處理哪些狀態？ | 直接做一份 Python MVP，真的開 18 頁 PDF、真的送鍵翻頁 | 能看能翻。同時暴露出 512 KiB 上限、cell 尺寸會跳動、CJK 狀態列三個真問題 | 1 天 |
| 4 | 這份 MVP 可以發佈嗎？ | 派一個代理人做唯讀架構審閱，每條發現都要有重現步驟 | 不行。找出 5 個 P0（含一個會吃滿一顆 CPU 核心的空轉迴圈）、9 個 P1 | 半天 |
| 5 | 要不要換語言？ | 三個候選各派一個代理人**實際安裝、實際編譯、實際 render**，不准只讀文件 | Python 在乾淨機器裝不起來；Go 與 TypeScript 幾乎平手；使用者選 Go | 半天 |
| 6 | `pane.graphics.stream` 的握手到底長怎樣？ | 用 Go 寫一支 spike，對活著的 pane 連送三張 3.18 MB 的 RGBA frame | 沒有握手。第一個 request 之後，這條連線的每一行都被當成 frame header | 1 小時 |
| 7 | 為什麼使用者說「慢」？ | 兩輪效能調查，每輪都埋 log、做 A/B、請使用者重新體感 | 瓶頸從頭到尾都在「送到終端機的位元組量」，不在 plugin | 1 天 |

### 三個沒得商量的規矩

**每個主張要有 `file:line` 或實測數字。** 研究報告裡「`set` 上限是 512 KiB」不是引用文件，是 `herdr-src/src/api/schema/panes.rs:5` 的常數；「popup 不能畫圖」不是推論，是 `src/app/popup.rs:139` 主動呼叫 `without_pane_identity()`，而 `src/pane.rs:158` 真的做了 `env_remove`。差別在於前者會過期，後者可以自己重新確認。

**人眼驗證不可省。** 第 2 步的 spike 拿到了 `{"type":"ok"}`，server log 乾淨，但那不代表像素上了螢幕——後來在 herdr 原始碼裡確認，一個 frame 的圖形資產超過聚合上限時，server 會把圖整個剝掉**但仍然回成功**（`src/server/headless/render.rs`、上限在 `src/protocol/wire.rs:29`）。「沒有錯誤」不等於「畫出來了」，所以每個里程碑都以「請你去看那個 pane」結尾。

**丟掉會痛的東西要在它變貴之前丟。** 第 5 步丟掉 531 行能跑的 Python，是因為第 4 步已經把它的問題全部列成表，且第 5 步的實測顯示問題不在程式碼品質而在分發模型。再晚兩天丟，成本會翻倍。

---

## 3. herdr plugin 的解剖

一個 herdr plugin 就是一份 manifest 加上幾支被 herdr spawn 的程式。`herdr-plugin.toml` 全文只有四個區段：

```toml
id = "pdf"
min_herdr_version = "0.9.0"
platforms = ["macos", "linux"]

[[build]]
command = ["./build.sh"]

[[panes]]
id = "view"
placement = "split"
command = ["./bin/herdr-pdf", "view"]

[[actions]]
id = "open"
command = ["./bin/herdr-pdf", "open"]

[[link_handlers]]
pattern = "(?i)^(file://)?.*\\.pdf$"
action = "open"
```

| 區段 | 誰會執行它 | 這裡踩過的點 |
| --- | --- | --- |
| `[[build]]` | `plugin link` / `install` / `update` 時，在**暫存目錄**裡跑完才整包搬家 | 必須冪等。`build.sh:16` 就是一行 `go build` 覆寫，跑幾次都一樣 |
| `[[panes]]` | 使用者或 action 要求開窗格時 spawn，`placement` 決定窗格形態 | 只能用 `split`（或 tab/zoomed/overlay），**不能用 popup** |
| `[[actions]]` | link handler、CLI、agent 都能觸發，跑完就結束 | action id 不能含 `.`；herdr 用 `normalize_local_identifier()` 驗 |
| `[[link_handlers]]` | Ctrl+click 或 OSC 8 連結被啟動時比對 | 這是 Rust regex 寫在 TOML 裡，`\\.` 才是字面上的點 |

### plugin pane 就是一個普通的 PTY

這是最容易想錯的一點。herdr 開出來的 plugin 窗格**不是**一塊 canvas，而是一個正常的偽終端：你的 process 的 stdin 是使用者的按鍵，stdout 是文字輸出，herdr 用內建的 VT 解析它。所以：

- 鍵盤**完全不走 API**。要讀按鍵就是把 stdin 轉成 raw mode 自己讀 byte。
- 狀態列**就是普通的 escape sequence**，寫進 stdout 即可（`internal/ui/status.go:80`）。
- 圖片**不能**用 stdout 吐 Kitty escape sequence——herdr 會解析它但永遠不畫。唯一的路是 socket API。

三層的分工是這個專案的骨架：

| 層 | 是什麼 | 對 viewer 的意義 |
| --- | --- | --- |
| process | herdr spawn 出來的 `herdr-pdf view`，cwd 是 plugin 根目錄 | 它的生死就是窗格的生死，不用自己管視窗 |
| PTY | 這個窗格的終端 | 按鍵、狀態列、alt screen、autowrap 全在這層 |
| socket | `$HERDR_SOCKET_PATH` 上的 newline-delimited JSON | **唯一能畫圖的路** |

### 環境變數

| 變數 | 用途 | 在哪讀 |
| --- | --- | --- |
| `HERDR_PANE_ID` | 自己的窗格 id，所有 `pane.graphics.*` 都要帶 | `cmd/herdr-pdf/main.go:66` |
| `HERDR_SOCKET_PATH` | API socket 位置，沒有就用預設路徑 | `internal/herdr/client.go:20-26` |
| `HERDR_PLUGIN_STATE_DIR` | plugin 的持久化目錄，log 與 wazero 快取都放這 | `internal/viewer/viewer.go:98`、`internal/render/pdf.go:65` |
| `HERDR_PLUGIN_CLICKED_URL` / `HERDR_PLUGIN_CONTEXT_JSON` | link handler 被觸發時的點擊內容 | `internal/openpath/resolve.go:51-67` |

`HERDR_PANE_ID` 是 herdr 在 spawn 時最後注入的，保證是自己的窗格（`herdr-src/src/pane.rs:155`）。

### 為什麼 placement 不能用 popup

popup 窗格在建立時走的是另一條路：`src/app/popup.rs:139` 呼叫 `PaneLaunchEnv::from_extra(extra_env).without_pane_identity()`，而它對應到 `src/pane.rs:157-158` 的 `PaneLaunchIdentity::OmitPane` 分支，那裡執行的是 `cmd.env_remove(HERDR_PANE_ID_ENV_VAR)`。

注意那是 **`env_remove` 而不是「沒有設定」**：連從父行程繼承來的值都會被主動清掉。沒有窗格 id 就無法呼叫任何 `pane.graphics.*`，所以 popup 這條路是徹底封死的，不是「比較難」而是「不可能」。這個結論在寫第一行程式碼之前就從原始碼裡確認，manifest 從此只宣告 `split`。

---

## 4. 畫圖的唯一正路：`pane.graphics.*` socket API

### 一條連線一個 request

herdr 的 API server 對每條連線只呼叫一次 `read_initial_request_line`（`src/api/server.rs:168`），處理完就回應並關閉，後面沒有迴圈。實測在同一條連線上連送兩個 ping，只有第一個有回應。

所以 client 的形狀是被協定決定的：`internal/herdr/client.go:55` 的 `Call()` 每次都重新 dial、寫一行、讀一行、關閉。沒有連線池，因為連線池在這裡沒有意義。

| 方法 | 用途 | 特性 |
| --- | --- | --- |
| `pane.graphics.info` | 取 cell 像素尺寸、`file_frame_transport`、`file_frame_directory` | 短連線，每次重查 |
| `pane.graphics.set` | 一次性送一張 base64 圖 | 短連線；**解碼後**上限 512 KiB（`panes.rs:5`） |
| `pane.graphics.stream` | 長連線，之後只送 frame | 上限 16 MiB／frame（`panes.rs:6`） |
| `pane.graphics.clear` | 移除圖層 | 短連線；stream 斷線時 herdr 會自動清 |

### stream 的握手真相

這是花了一支 spike 才確定的事：**「切換到 streaming transport」沒有任何協定動作。**

`src/api/server.rs:200-202` 在 socket server 這一層就攔截 `Method::PaneGraphicsStream`，把整條連線的所有權交給另一個 handler，那個 handler 寫回一行 `{"type":"ok"}` 後就進入無窮的 frame 讀取迴圈。沒有 hello、沒有 upgrade、沒有第二個 socket。唯一的要求是：**`pane.graphics.stream` 必須是這條連線上的第一個、也是唯一一個 request。**

推論是：open 成功後在同一條連線上送 `pane.graphics.info` 會怎樣？實測回的是

```
{"error":{"code":"invalid_frame","message":"invalid frame header: missing field `format` at line 1 column 76"}}
```

它把那行 JSON 當成壞掉的 frame header 了。所以 `internal/herdr/stream.go:14-19` 的型別註解直接寫死這件事：這個型別**永遠不能**拿去做別的呼叫。

### 沉默即成功

inline frame 送成功時 herdr **什麼都不寫回來**，只有被拒絕才回一行然後關閉 stream；只有 file frame 每張都會收到真的 ack。這個非對稱必須寫進 client 的抽象裡，否則就會在等一個永遠不會來的 ack。作法是背景 goroutine 持續讀，讀到任何東西就把整個 session 標成 failed：

```go
// SendInline writes one inline frame ("png", "rgb", "rgba", or "bgra"). It
// does not wait for an ack (there isn't one on success); check Failed()
// afterward to detect a rejection.
func (s *StreamSession) SendInline(format string, payload []byte, width, height int, placement Placement) error {
	if err := s.Failed(); err != nil {
		return err
	}
	if len(payload) == 0 {
		return fmt.Errorf("empty %s frame", format)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.frameNo++
	hdr := frameHeader{
		Format: format, ImageWidth: width, ImageHeight: height,
		DataLength: len(payload), Sequence: s.frameNo,
		Revision: s.frameNo, Placement: placement,
	}
	return s.writeFrame(hdr, payload)
}
```

`internal/herdr/stream.go:182-204`。送出後不 block，錯誤由 `Failed()` 在下一次送出時或送出後查詢。搭配 `internal/herdr/stream.go:145-150` 的 `pushAck`：channel 有 buffer 且滿了就丟，因為「沒有人在等的 ack」不能把讀取端卡死。

### 兩個容易誤判的細節

**連線關閉會清圖層。** `internal/herdr/stream.go:268-271` 的 `Close()` 註解寫明這件事。好處是行程結束時圖自動消失，不會留下殘影；代價是 stream 模式下必須有一個活著的行程持著連線。

**placement 的 grid 要用 ceil。** `internal/viewer/viewer.go:540-549` 的 `computePlacement` 用 `ceilDiv`，因為圖片邊緣落在半格上時無條件捨去會讓最後一列被切掉。另外要知道終端機畫圖時，圖片自身的像素尺寸寫在 `s=`/`v=`，但**佔幾格**是取自 placement 的 `c=`/`r=`（`src/kitty_graphics.rs:799,820`）——所以送一張縮小的圖進同一個 placement，畫面上會是「糊」不是「變小」。第 7 節的漸進式細緻化完全建立在這個性質上。

---

## 5. 渲染：go-pdfium 走 wazero

### 為什麼選它

`klippa-app/go-pdfium` 有兩種後端：cgo 動態連結，以及純 Go 的 WebAssembly 模式——用 wazero 執行一份 `go:embed` 進 binary 的 pdfium wasm。選後者的理由全部是分發面的：

| 面向 | 結果 |
| --- | --- |
| cgo | 不需要。`CGO_ENABLED=0` 就能編 |
| 系統相依 | 零。不連結 Poppler、不連結 MuPDF |
| 授權 | go-pdfium 是 MIT，PDFium 本體是 BSD-3-Clause 家族，沒有 AGPL 疑慮 |
| 交叉編譯 | 在同一台 macOS 上、不裝任何工具鏈，六個 target 全部一行指令編成（linux/darwin/windows × amd64/arm64） |
| 代價 | wasm 解譯比原生慢；單頁 render 約 85–128 ms |

### 冷啟動約 1 秒多與那個快取目錄

wazero 第一次要把 pdfium 的 wasm 編譯成機器碼，首次啟動實測約 1 秒多；有持久化編譯快取後降到幾十毫秒。這個落差大到足以決定一個設計：

```go
func WazeroCacheDir() string {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return filepath.Join(d, "wazero")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "herdr-pdf", "wazero")
}
```

`internal/render/pdf.go:64-70`。重點是**絕對不能用相對路徑**，否則 cwd 一變快取就跟著跑掉，每次開窗格都要重付這約 1 秒多的編譯成本。

還有一個順序問題：pdfium 初始化必須發生在**進入 raw mode 之前**（`internal/viewer/viewer.go:309-311`）。不然使用者看到的是一個沒有游標、完全不動的黑畫面撐上約 1 秒多，那看起來就是當掉。

### RGBA stride 要自己收緊

go-pdfium 回的是 `*image.RGBA`，它的 `Stride` 不保證等於 `4*width`；而 herdr 的 frame 協定要求剛好 `width*height*4` bytes、沒有任何列尾填充。`internal/render/pdf.go:217-232` 的 `TightRGBA` 逐列 copy 處理這件事，順便吃掉 `Bounds().Min` 不在原點的情況。這是在 spike 階段就發現的——如果直接把 `src.Pix` 丟出去，圖會斜掉。

另外 `resp.Cleanup()` 必須在 `TightRGBA` 複製完之後才呼叫：WebAssembly 模式下那塊 pixel buffer 只在 Cleanup 之前有效。

### 頁面 FIFO 快取與相鄰頁預取

兩層快取，兩個不同的目的：

| 快取 | 位置 | key | 容量 | 解決什麼 |
| --- | --- | --- | --- | --- |
| 頁面點陣圖 | `internal/render/pdf.go:199-243`（`renderOnce`／`storeLocked`） | (page, maxW, maxH) | 6 | 回看剛看過的頁不必重算（render 從 ~500 ms 變 0） |
| 編碼後的 PNG | `internal/viewer/viewer.go:1147-1164`（`storePNGAt`） | (page, w, h) | 4 | 翻頁時不必在按鍵路徑上跑色階階梯 |

兩者都是先進先出（FIFO），不是 LRU：命中不會把 key 移到佇列尾端，容量滿了固定淘汰最先塞進去的那個。兩個 key 都含畫布尺寸，所以 resize 後不會拿到舊尺寸的陳舊結果。

預取在**當前頁畫完之後**才啟動，而且會看使用者往哪個方向翻（`internal/viewer/viewer.go:737-741`）：往下讀就先暖下一頁。要暖的頁在啟動 goroutine 之前就決定好，goroutine 不碰 viewer 狀態，所以不需要為它加鎖。

pdfium 這邊是單執行緒的（wazero pool 設 `MaxTotal: 1`），所以背景預取和主迴圈共用同一個 instance 必須靠 `renderMu` 序列化；`Close()` 也要拿這把鎖，否則行程結束時還在飛的預取會撞上被拆掉的 document。

---

## 6. 終端接管

viewer 要同時擁有整個窗格的畫面，又要保證離開時把終端還原乾淨。四件事：

```go
const (
	enterAltScreen  = "\x1b[?1049h"
	hideCursor      = "\x1b[?25l"
	disableAutowrap = "\x1b[?7l"
	reverseVideo    = "\x1b[7m"
)
```

`internal/ui/term.go:13-23`。

**alt screen** 讓 viewer 不污染使用者原本的 scrollback。**raw mode** 讓按鍵一個一個進來，不等 Enter。**關掉 autowrap** 是為了一個具體的失敗：狀態列如果多寫一欄，終端會自動換行、整個畫面往上捲一列，狀態列就跑到圖層底下看不見了。

還原時每一步各自獨立包起來（`internal/ui/term.go:60-85`）：如果 `term.Restore()` 因為 stdin 已經關閉而失敗，後面的「開回 autowrap、顯示游標、離開 alt screen」仍然要執行。把它們串在同一個 `defer` 裡，一步失敗就會留下一個沒有游標的終端。

### 按鍵要讀到 CSI 的 final byte

方向鍵是 `\x1b[A`，三個 byte；但 PageUp 是 `\x1b[5~`，四個。只吃固定三個 byte 的解析器會把 `5` 當成未知鍵、把剩下的 `~` 留在 buffer 裡，變成下一次按鍵——Python 版就是這樣，架構審閱時被標成 P2。

正確作法是一路讀到 CSI 的 final byte（0x40–0x7E）才分派（`internal/ui/keys.go:81-83`），並且對超長序列設上限以免讀到天荒地老。

### EOF 是「停止」，不是「現在沒有按鍵」

Python 版有一個 P0：PTY master 關閉後，`os.read` 一直回空 bytes，主迴圈不匹配任何分支也不 sleep，**實測 0.5 秒轉了 570,000 圈**，吃滿一顆核心。

Go 版把它做成型別上的區別：`internal/ui/keys.go:24-27` 有一個獨立的 `KeyEOF`，註解明寫「呼叫端必須把它當成停止」。`StartKeyReader` 收到 EOF 後關閉 channel，主迴圈的 `case ev, ok := <-keysCh` 在 `!ok` 時直接 return。空轉在型別層面就不可能發生了。

### 狀態列的 CJK 寬度：為什麼 `len()` 會錯

`len("簡章(中文版).pdf")` 是 11（Go 會更糟，是 byte 數），但它在終端機上佔 **16 欄**：East Asian Wide/Fullwidth 的字元一個佔兩欄。用錯的數字去算截斷位置，狀態列就會多寫幾欄、觸發 autowrap、把自己捲到圖層下面。

`internal/ui/status.go:15-17` 用 `go-runewidth` 算真實顯示寬度，`StatusLine` 只填到 `cols-1`（`:52`）——**刻意不填最後一欄**，因為某些終端在寫滿最後一欄時就會捲動，這是第二道保險。

---

## 7. 效能真相與解法

### 第一輪：位元組量，不是 plugin

Go 版剛寫完時走的是 `stream-rgba`：直接把未壓縮的 RGBA 丟進 stream。plugin 端的數字很漂亮（不用編碼），但使用者說慢。

追下去發現，一張填滿窗格的 807×1140 frame 是 **3,679,920 bytes**，而這條路上**沒有任何一段會壓縮它**：

| 階段 | 發生什麼 | 證據 |
| --- | --- | --- |
| server 收 body | 以 64 KB 為單位讀進沒有預先配置容量的 `Vec` | `pane_graphics_stream.rs:442` |
| 存成圖層 | 對整段 3.7 MB 做一次雜湊當指紋 | `app/pane_graphics.rs:211-212` |
| 每個 client 各一份 | `.to_vec()` 逐 client 完整複製 | `kitty_graphics.rs:636` |
| server→client 線路 | bincode 序列化，**無壓縮** | `protocol/wire.rs:1601-1623` |
| Kitty 編碼 | base64，chunk 大小 3072 bytes，**沒有 `o=z` zlib** | `kitty_graphics.rs:20` |

最後那一步把 3.68 MB 撐成 **4,917,342 bytes 的跳脫字元、1,198 個 chunk**，每按一次 `j` 就要這樣一次。

**為什麼換 PNG 有效**：herdr 從不解碼 PNG。長度校驗對 PNG 直接放行（`app/api/pane_graphics.rs:512-524`），然後 `kitty_graphics.rs:688-694` 把它映射成 Kitty 的原生格式碼 100，壓縮後的位元組一路原封不動送到外層終端機自己解。所以換 PNG 不只省 socket，是上表**每一階段同步縮小**。同一頁的 458,726 bytes PNG 只產生 612,986 bytes 跳脫字元、150 個 chunk。

同一輪還修掉三件 plugin 自己在按鍵路徑上做的蠢事：每次翻頁重查 `pane.graphics.info`（從 viewer 行程內部量，閒置後第一次穩定花 **約 106 ms**，成因沒查出來，所以直接把它移出按鍵路徑改成 stale-while-revalidate，見 `internal/viewer/viewer.go:604-623`）、同步算圖（改成預取）、先清畫面再算圖（`showStatus` 把清畫面延到最後一刻）。

結果：plugin 端每次翻頁從 39–57 ms（快取未命中時 145–160 ms）降到 **0.5–1.4 ms**。

### 第二輪：遠端，以及「容量 1、滿了就丟」

使用者在筆電上透過 SSH 看桌機的 herdr，說「還是有感」。第二輪的戰場因此完全不同：

| 問題 | 結論 | 證據 |
| --- | --- | --- |
| 遠端這段有壓縮嗎 | **完全沒有**。相依只有 base64 與 bincode，herdr 呼叫 ssh 時也沒帶 `-C` | `Cargo.toml:23-25`、`remote/attach.rs:573-606` |
| 圖形跟文字同一則訊息嗎 | 是。大圖會拖慢同一次的文字更新 | `protocol/wire.rs:1213-1222` |
| 有沒有 backpressure | **這是關鍵**。render 車道容量固定為 1，原始註解寫著 "Capacity is one so slow clients cannot build lag"；滿了就**直接丟棄該影格**，等下一個 tick 用最新狀態重算 | `server/client_transport.rs:119-124` |

判讀因此改變：延遲不是佇列爆炸，而是「等前一張傳完 ＋ 等下一個 render tick 重算」。**縮小位元組不只省傳輸時間，更是減少被丟棄重試的次數。**

兩個對策：

**自適應色階階梯**（`internal/render/encode.go:60-122`）。先試真彩，超過預算就降 8-bit 灰階，再超就降 16 階灰階（image/png 會寫成 4 bits/pixel），最後才動解析度。順序是刻意的——文字頁降色深時字仍然銳利，降解析度才是唯一會傷可讀性的一階。註解把這件事寫下來了：

```go
// shrinkSteps are the scale factors tried after colour depth has already
// been spent. Downscaling is last because it is the only rung that costs
// legibility on a text page; dropping to greyscale and then to 16 levels is
// close to free on a document that is mostly black ink on white paper.
var shrinkSteps = []float64{0.85, 0.72, 0.61, 0.5}
```

這個階梯**永遠不會因為太大而回傳錯誤**，找不到符合預算的就回傳最小的那個：寧可稍微超標，也不要空白畫面。預算是遠端 200 KB、本機 1500 KB（`internal/viewer/viewer.go:182-185`），遠端與否是看 `pane.graphics.info` 回報的 `file_frame_transport` 是不是 `direct-kitty`——那個旗標由 client 握手決定（`client/handshake.rs:57-84`），空的就代表遠端或根本沒有 client 連著。

**漸進式細緻化**。按鍵當下先送一張 16 階灰階的小圖（8–21 KB），90 毫秒後再補完整頁；中途再按鍵就取消補送。

90 毫秒不是隨手挑的：

```go
// refineDelay must exceed herdr's ~16ms render cadence (MIN_RENDER_INTERVAL,
// see herdr src app/mod.rs:38), or the server coalesces both frames into one
// tick and the preview is never drawn. It also doubles as the cancel window:
// a second keypress within refineDelay skips the refine entirely.
const refineDelay = 90 * time.Millisecond
```

取消靠一個 atomic 的世代計數器 `redrawGen`（`internal/viewer/viewer.go:920-934`）：重畫時遞增，補送前比對，不一致就放棄。實測連按六次 `j` 只產生一次補送，其餘全被取消。

### 第二輪實測

| 頁 | 真彩 PNG 基準 | 按鍵當下送出 | 策略 | 90 ms 後補送 | 縮減 |
| --- | --- | --- | --- | --- | --- |
| 2 | 187,635 | **12,286** | `preview@45` → `rgb` | 187,635 | 15.3× |
| 3 | 86,959 | **7,773** | `preview@45` → `rgb` | 86,959 | 11.2× |
| 5 | 378,207 | **21,402** | `preview@45` → `gray` | 151,991 | 17.7× |
| 6 | 383,994 | **20,104** | `preview@45` → `gray` | 148,980 | 19.1× |

使用者的回報從「還是有感」變成「即時」。

### 三個被否決的「預先上傳」設計

想法都一樣：既然翻頁時才送圖太慢，能不能先把相鄰頁偷偷傳上去？三個版本都被原始碼證據擋下來：

| 方案 | 判決 | 證據 |
| --- | --- | --- |
| (a) 只帶 placement、不帶資料的 frame | **不可行，而且會斷線**。缺 `data_length` 回 `"frame requires data_length or file"`，`data_length == 0` 回 `"frame data_length must be greater than zero"`，兩者都直接關掉整條 stream。整個 `pane.graphics.*` 只有四個方法，**沒有任何只改 placement 或 z_index 的 API** | `pane_graphics_stream.rs:260-283`；`api/schema.rs:198-213` |
| (b) 放在可視範圍外預先上傳 | **不可行**。placement 起點超出 viewport 時 `clipped_placement` 回 `None`，編碼函式開頭就提前返回——不上傳也不顯示 | `kitty_graphics.rs:891-893` |
| (b′) 同一格、`z_index` 較低的圖層預先上傳 | **原始碼層面成立，但沒實作**。`z_index` 是**開 stream 時就固定**的，每張 frame 的 header 只能帶 placement；要做就得改用 `pane.graphics.set` 和一套雙層狀態機，而它唯一的好處在當時的環境**完全無法驗證**（測試期間沒有任何 client 連著） | `kitty_graphics.rs:497-556` |

否決 (b′) 的理由值得單獨記住：**一個量不到、失敗時會讓流量加倍的機制，不適合設成預設。** 設計與證據留在報告裡，環境允許時再實作。

---

## 8. 踩坑清單

| # | 坑 | 症狀 | 怎麼發現 | 修法 | 位置 |
| --- | --- | --- | --- | --- | --- |
| 1 | `pane.graphics.set` 上限 512 KiB，比的是**解碼後**大小 | 掃描件頁面回 `image_too_large` | spike 用不同大小實測：700 KiB 回錯誤，800 KiB 直接 broken pipe（撞到 `MAX_INITIAL_REQUEST_BYTES` 1 MiB） | 預設改走 stream；`set` 降為降級路徑並保留 12 KiB 餘裕 | `viewer.go:31`；`panes.rs:5`、`server.rs:32` |
| 2 | popup 窗格拿不到 `HERDR_PANE_ID` | 圖完全畫不出來，且無錯誤可查 | 讀 herdr 原始碼，發現是主動 `env_remove` | manifest 只宣告 `split` | `popup.rs:139`、`pane.rs:158` |
| 3 | cell 像素尺寸會跳動 | 同一個窗格前後兩秒回報 `8x19` 與 `15x38`，圖的縮放跑掉 | Python MVP 的 viewer.log 抓到相隔一秒的兩行 | 不快取；後來改成有 TTL 的 stale-while-revalidate | `viewer.go:196-200`、`:604-623` |
| 4 | CJK 檔名用 `len()` 算寬度 | 狀態列溢位觸發 autowrap，捲到圖層底下看不見 | 實測 `簡章(中文版).pdf` 顯示寬度 16、`len` 11 | go-runewidth 算真實寬度，只填到 `cols-1`，並關掉 autowrap | `status.go:15-17,52`；`term.go:19` |
| 5 | stdin EOF 造成 100% CPU 空轉 | PTY 關閉後吃滿一顆核心 | 架構審閱寫了重現腳本：0.5 秒轉 570,000 圈 | 獨立的 `KeyEOF` 常數，主迴圈必須當成「停止」 | `keys.go:24-27,44-48` |
| 6 | 錯誤碼在 top-level 而不是 `result` 裡 | 檢查 `result["code"]` 的那段是死碼，永遠不會執行 | 對照 herdr 的 response schema | `*Error` 型別對應 top-level，狀態列按 `.Code` 分流訊息 | `client.go:28-33`；`viewer.go:788-804` |
| 7 | `pane.graphics.stream` 不在公開 schema 裡 | `herdr api schema` 列不出來，以為不存在 | 讀原始碼發現標了 `#[schemars(skip)]` | 照原始碼實作，不依賴 schema 輸出 | `api/schema.rs:204-206` |
| 8 | stream 連線上不能做別的呼叫 | 送一次 `info` 就收到 `invalid_frame` 並斷線 | spike 實測 | 型別註解寫死「此連線永不重用」，`info`/`layout` 各開短連線 | `stream.go:14-19` |
| 9 | 只帶 placement 的 frame 會關掉 stream | 預先上傳的想法直接讓畫面消失 | 讀 `pane_graphics_stream.rs` 的兩個錯誤分支 | 放棄該設計，改做漸進式細緻化 | `pane_graphics_stream.rs:260-283` |
| 10 | `herdr plugin action invoke pdf.open` 不是合法指令 | 以為能用一個字串指定 plugin 加 action | 實跑 `herdr plugin action list --plugin pdf`，回傳的是分開的 `action_id` 與 `plugin_id` | 正確寫法是 `invoke open --plugin pdf`；README 已更正並說明它沒有位置參數可帶路徑 | `README.md`「用法」第 3 項 |
| 11 | herdr 的 pane id 可以含字母 | 用正規表示式假設 `w8:p<數字>` 會解析失敗 | 效能調查期間實際出現 `w8:pK`、`w8:pQ` | 一律當成不透明字串傳遞，不解析 | `main.go:140-142` |
| 12 | 環境變數在 shell 設了會被默默忽略 | 設了 `HERDR_PDF_TRANSPORT` 卻沒有效果 | A/B 對照時發現兩邊行為一樣 | `open` 子命令把四個變數以 `--env` 轉發進新窗格 | `main.go:130-139` |
| 13 | Python venv 在乾淨機器上會卡住 | 全新 macOS 跳出阻斷式 GUI 對話框要求裝 Command Line Tools；Debian/Ubuntu 沒裝 `python3-venv` 直接失敗 | 語言選型時逐情境實測安裝流程 | 換成單一 Go 靜態執行檔 | 見第 9 節 |
| 14 | `go.mod` 寫兩段式版本號會被拒絕 | 本機 Go 工具鏈把 `go 1.26` 判定成「go.mod 需要更新」，直接拒絕編譯 | `go build`／`go mod tidy -diff` 實測 | 一律寫三段式 `go 1.26.0`；`build.sh` 用同一個值當 `check_go` 的下限 | `go.mod:3`、`build.sh` 的 `check_go` |
| 15 | `io.WriteString` 會繞過自訂的 `Write` | 測試用的假 writer 把 `bytes.Buffer` 內嵌進去時，`io.WriteString` 偵測到底層有 `WriteString` 方法就直接呼叫它，完全跳過外層覆寫的 `Write`，記錄／回錯邏輯永遠不會觸發 | 寫終端還原的失敗測試時發現斷言對不上 | 把 `bytes.Buffer` 改成一般欄位而不是內嵌，斷絕 `io.StringWriter` 這條捷徑 | `internal/ui/term_test.go`（`errWriter`） |
| 16 | 每次按鍵各開一個 90ms 的 refine goroutine | 連續快速翻頁時，goroutine 數量隨按鍵次數線性成長，且互相之間沒有取消關係 | 壓力測試：50 次翻頁＋背景工作，斷言 goroutine 數回到基準 | 改成單一 `*time.Timer` 加單一 pending 欄位，新按鍵直接覆蓋前一份還沒送出的 payload | `internal/viewer/viewer.go`（`scheduleRefine`，約 1073 行起） |
| 17 | pdfium 的開檔錯誤只給格式化字串，不給結構化錯誤碼 | 想依錯誤類型分流訊息，但 `OpenDocument` 回傳的 `error` 沒有可比對的欄位 | 讀 go-pdfium 對應原始碼與實際錯誤文字 | 比對錯誤訊息裡的關鍵字：含 `password` 是錯誤碼 4（密碼錯誤或缺密碼），含 `incorrect format`／`format error` 是錯誤碼 3（結構無法解析），其餘不分類 | `internal/render/pdf.go:120-132`（`classifyOpenError`） |

---

## 9. 語言選型是怎麼決定的

前提是第 4 步的架構審閱：Python MVP 的程式碼品質沒問題，問題在「別人要怎麼裝」。所以四個候選各派一個代理人去**實際安裝、實際編譯、實際 render**，禁止只讀文件。

| 候選 | 零設定 | 產物體積 | 效能（單頁 render） | 授權 | 改寫成本 |
| --- | --- | --- | --- | --- | --- |
| **Python + pypdfium2**（現狀） | **差**。乾淨 macOS 跳 GUI 對話框；Debian/Ubuntu 預設無 `python3-venv` 直接失敗 | venv 約 30 MB | 原生綁定，最快 | BSD-3 + MIT-CMU，乾淨 | 0（不用改） |
| **TypeScript + Bun** | 好。`bun build --compile` 出單一執行檔，一台 Mac 編出五個平台 | **61–98 MB** | 97 ms（WASM） | MIT（避開 AGPL 的 mupdf） | 約 1.5 人日 |
| **Go + go-pdfium/wazero** | 好。`CGO_ENABLED=0` 單一靜態執行檔，一台機器編出六個平台 | **11–14 MB** | 95–128 ms（WASM） | MIT + BSD-3 | 約 2.5–3 天 |
| **Rust + pdfium-render** | **差**。預編譯的 pdfium 只發動態庫，實際交付是「執行檔 + 平台專屬 .dylib/.so/.dll」兩個檔案 | — | 原生，最快 | MIT/Apache-2.0 | 約 3–3.5 天（且本機無工具鏈，全部未實測） |

Go 與 TypeScript 在「零設定」這個最重要的軸上幾乎平手，差別集中在體積（11 MB 對 61–98 MB）與一個已知的坑（Bun compile 後找不到 wasm 檔，需要 `import ... with { type: "file" }` 的 workaround 才能繞過）。Rust 在「單一 binary」這個前提上直接不成立，除非自己編一次 PDFium 的靜態庫——那是接近「編一次 Chromium 子集」的建置工程。

決定權交給使用者，選了 Go。實際改寫花了大約半天（不是估的 2.5–3 天），因為 spike 已經把 socket 協定和 pdfium 的用法都驗證過了。

---

## 10. 怎麼繼續

| 項目 | 內容 | 估時 |
| --- | --- | --- |
| 縮放與平移 | 目前掃描件和小字文件沒有任何辦法看清楚，這是最大的功能缺口 | 3 小時 |
| 跳頁 UI | 數字前綴已經能用，但沒有視覺回饋，輸入到一半看不到自己打了什麼 | 1 小時 |
| Linux 實機驗證 | manifest 已經宣告 `linux`，但從未在 Linux 上跑過，那目前是未經檢驗的承諾 | 1 小時 |
| `direct-kitty` 路徑實測 | 程式路徑已經寫好也被探針驗證過，但從沒在真的協商出 `direct-kitty` 的環境下跑完整 viewer | 1 小時 |
| 自帶 `skills/` | 讓 AI agent 知道「使用者要看 PDF 時可以開這個 plugin」 | 1 小時 |

---

## 11. 方法論回顧

**一、spike 先行，而且 spike 要小到能丟。** `spike/go-stream/main.go` 只有 309 行，做兩件事：送一張 PNG、送三張 RGBA。它回答了「stream 的握手長怎樣」這個沒有文件的問題，然後就功成身退。整個專案沒有任何一次「寫了三天才發現協定理解錯了」。

**二、第一手覆核，不接受「代理人說做了」。** 每份回報都重跑一次測試、對一次 `file:line`、送一次鍵看 log。有一次代理人把 Pillow 的版本號誤當成 pypdfium2 的版本寫進報告，就是在覆核時抓到的。

**三、A/B 要先埋 log 再比。** `HERDR_PDF_TRACE=1` 的分段計時（`internal/viewer/viewer.go:232-274`）是效能調查能收斂的唯一原因。它從**按鍵被主迴圈取出**開始計時，不是從重畫函式開始——這樣按著不放時的排隊延遲才會顯示出來，而不是被藏起來。沒有這個，「哪一段慢」只會是猜測。

**四、人眼驗證不可省。** 兩次「慢」的回報都來自使用者的體感，兩次都不是 plugin 端的數字能看出來的：第一次 plugin 端明明更快，第二次 plugin 端已經是 1 ms。最終確認「即時」的也是人眼。API 回 `ok` 只代表 herdr 收下了。

**五、註解只留「為什麼」。** 最後一輪清掉了所有覆述程式碼在做什麼的註解，留下的都是沒有寫下來就會被改錯的資訊：`refineDelay` 為什麼是 90 ms 而不是 50 ms（要蓋過 herdr 的 16 ms 渲染節奏）、`shrinkSteps` 為什麼排在色深後面（降解析度是唯一會傷可讀性的一階）、frame file 的檔名為什麼要輪流用（herdr 會持著那個檔案並在畫圖時重新驗證）。這些註解好幾條直接引用 herdr 本體的 `file:line`，因為那才是可以重新確認的來源。
