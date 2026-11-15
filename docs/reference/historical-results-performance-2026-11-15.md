# 歷史結果查詢：完整本機路徑性能基線（2026-11-15）

> Status: active
> Authority: reference
> Verified-Commit: —（working tree；HEAD `0b4f4eac23c0bfa1d031ef3437ff59ee31512f27`，benchmark source 與 binary SHA-256 見 JSON）
> Supersedes: —
> Superseded-By: —

## 範圍與完成界線

本次量測補足舊 projection-only benchmark：使用真實 SQLite workspace、唯讀私有快照、完整全域事件鏈驗證、分支／team 範圍、歷史結果重播、Cobra `results query`、候選搜尋、證據編譯、來源檢查與 JSON 輸出。只有 resolver／selection／answer 三次生成使用既有 dependency seam 的固定離線回應；**沒有真實 provider 呼叫，不把 fake 生成時間、tokens 或 USD 當成真實模型數字**。

[機器可讀報告](historical-results-performance-2026-11-15.json)保留所有原始樣本、fixture 大小與 hash、統計、source／binary identity 及方法限制。可重現的程式位於 [results performance benchmark](../../cmd/hufu/results_performance_test.go)。

後續[配置優化比較](historical-results-optimization-2026-11-15.md)使用完全相同的 benchmark source 與 fixture 形狀另行量測；本文件及其 JSON 保留原始基線數字，後續結果不覆寫本輪樣本。

## 方法

- 每個 1K／10K／100K 規模各建立一份獨立 synthetic workspace；使用 production event-log importer，在 SQLite transaction 內匯入有效 hash chain。
- 每份資料有一個 run、main branch、team，N 個 terminal task results、2N+1 個 events；包含 typed result、Unicode 及每 100 筆一個搜尋命中。此固定形狀不代表任意 payload／分支數分布。
- 每個規模執行 20 個 fresh child processes；每個 child 先執行一個新 Cobra command，再建立另一個 command 執行同一查詢，共 20 組 cold／warm 配對、120 次正式查詢。
- `process_cold` 是 fresh process 的首次查詢；`same_process_warm` 是同程序第二次查詢。每次仍重新建立 SQLite snapshot 並執行既有完整驗證，沒有讀取 shortcut 或新增搜尋索引。
- Fixture 在 `/tmp` 的 tmpfs，沒有驅逐共享 OS cache；這不是實體磁碟冷讀。OS process launch／Go init 不含在 command timer 內。兩次查詢前的 GC 在計時外，沒有呼叫 FreeOSMemory 或全機 drop_caches。
- 全流程 timer 包含 command 建構／flags／scope、完整 load、三段 scripted query、來源驗證及 JSON 輸出；回覆後的 test assertion 不算入耗時。
- p50 為樣本中位數；p95 用 nearest rank（20 筆中的第 19 筆），各 mode／規模獨立計算。不把單次樣本叫作 p95。
- RSS 每 5ms 抽樣，另保留 `/proc` process HWM；後者在同一 pair 內累計，不能視為獨立的 warm peak。Go 累計 allocations 與 peak RSS 分開呈現。
- Snapshot open 與單次完整 validated scan 在每組查詢後另行觀察，屬於 warm component measurements，不能加總或相減冒充單次查詢的精確分解。
- 所有 120 次都驗證完整 result count、搜尋命中數、三次 scripted calls、單一合法來源及回答；每組結束比對來源 `runtime.sqlite` SHA-256，確認內容未變。
- 環境：Linux amd64、Go 1.27.2、Intel Core i7-6700K、GOMAXPROCS=2；量測期間只有一個本次重量級驗證 owner，不聲稱整台主機獨占。

## 全流程查詢結果

| Terminal results | Cold p50 / p95 | Warm p50 / p95 | Cold / warm RSS 最大抽樣值 | SQLite 大小 |
|---:|---:|---:|---:|---:|
| 1,000 | 0.147 / 0.185 s | 0.137 / 0.182 s | 76.5 / 77.3 MiB | 1.24 MiB |
| 10,000 | 1.386 / 1.565 s | 1.401 / 1.620 s | 175.5 / 178.8 MiB | 11.47 MiB |
| 100,000 | 14.695 / 15.727 s | 14.076 / 14.971 s | 1184.2 / 1194.2 MiB | 114.70 MiB |

所有規模皆為 20/20 組成功；沒有排除失敗、截短歷史內容或放寬 60 秒 query deadline。Warm 並非每個規模都比較快，不能將這組結果解讀成已實作跨查詢 result cache。

## 成本位置與配置量

| Results | Cold load 占總耗時中位數 | Cold load p50 | Candidate pipeline p95（cold / warm） | 每次累計配置中位數（cold / warm） |
|---:|---:|---:|---:|---:|
| 1,000 | 95.3% | 0.140 s | 9.1 / 5.5 ms | 43.3 / 42.8 MiB |
| 10,000 | 96.3% | 1.339 s | 54.1 / 44.0 ms | 440.8 / 440.4 MiB |
| 100,000 | 97.0% | 14.196 s | 473.7 / 458.3 ms | 4,476.4 / 4,475.0 MiB |

Load 是既有 `inspect.LoadHistoricalResults` 的完整時間，包含快照、兩次完整事件掃描、lineage／結果投影與排序。Candidate pipeline 是 resolver 回傳至 selection callback 之間的時間，包含 resolver 解碼、`Retrieve`、excerpts 與 selection prompt 編譯，並非只量字串搜尋。累計配置是 TotalAlloc 差值，包含量測 instrumentation，不是同時佔用的 RAM。

| Results | 獨立 snapshot open p95 | 獨立完整 chain scan p95 | 每次 scan 驗證 events | Payload 大小 |
|---:|---:|---:|---:|---:|
| 1,000 | 12.0 ms | 16.9 ms | 2,001 | 0.37 MiB |
| 10,000 | 86.7 ms | 167.9 ms | 20,001 | 3.76 MiB |
| 100,000 | 772.0 ms | 1770.7 ms | 200,001 | 38.17 MiB |

**結論：100K 的完整 load 約佔 97% 耗時；只報 candidate search 會低估約一個數量級以上的使用者等待時間。** 本輪維持 `event_replay`；沒有新增落盤結果搜尋／投影索引，index build 指標是「不適用」，不是 0ms 的成功建置。SQLite 原有 schema／索引維持不變。

## 另行 CPU／allocation 剖析

正式樣本完成後，以相同 100K 資料形狀另建 fixture，額外執行兩個查詢與一次獨立 validated scan，使用 Go CPU／heap profiler。這兩個查詢不混入前述 120 次統計；匯入 fixture 在另一個程序完成，不含在 sampled process 內。

診斷程序 wall duration 32.40 秒，累計 CPU samples 39.51 秒；GOMAXPROCS=2 下 CPU samples 可因 runtime／GC 並行而大於 wall time。以下配置百分比取自該程序的 `alloc_space` 抽樣，cum 包含下游呼叫，不能相加：

| 配置位置 | Flat | Cumulative |
|---|---:|---:|
| `team.reduceTodoEventStream` 的 reducer loop | 13.77% | 23.05% |
| `team.projectBranchLineage` | 11.53% | 11.53% |
| `inspect.projectHistoricalResults` | 8.65% | 55.30% |
| `inspect.historicalLineage` 的第二趟 visitor | 5.82% | 5.82% |
| `inspect.LoadHistoricalResults` | 5.40% | 92.20% |
| `team.cloneTodoItem` | 4.26% | 4.26% |

CPU flat 排名前列有 SHA-256 block（8.71%）、GC／span scanning、memory copy 與 JSON string scan。這不是精確的 query-exclusive 階段分解；完整 raw top、profile hashes、診斷 fixture 與 binary identity 見 JSON。結果支持先查核 replay／lineage／結果投影的配置與複製，**不支持移除 hash verification 或只加一個文字搜尋索引就宣稱解決完整等待時間**。

## 可重現命令

使用目前 checkout；以下產物均在單一 private task directory，原有 Go caches／GOTMPDIR 不變。保留 JSON 報告後再清除 binary 與 fixture。

```bash
perf_dir=$(mktemp -d /tmp/codex-task.XXXXXX)
trap 'rm -rf -- "$perf_dir"' EXIT
TMPDIR="$perf_dir" mise exec -- env GOMAXPROCS=2 go test -p 2 -c \
  -o "$perf_dir/history-perf.test" ./cmd/hufu
TMPDIR="$perf_dir" GOMAXPROCS=2 HUFU_HISTORY_PERF_REPORT="$perf_dir/history" \
  "$perf_dir/history-perf.test" -test.run='^$' \
  -test.bench='^BenchmarkHistoricalQueryEndToEnd$' \
  -test.benchtime=20x -test.timeout=60m

# 指定的 JSON 是保留交付物；離開 shell 後其餘暫存產物由 trap 清理。
PERF_INPUT_DIR="$perf_dir" python3 - <<'PY'
import json, os, pathlib
root = pathlib.Path(os.environ["PERF_INPUT_DIR"])
reports = [json.loads((root / f"history-{n}.json").read_text())
           for n in (1000, 10000, 100000)]
assert all(r["complete"] and len(r["pairs"]) == 20 for r in reports)
pathlib.Path("history-query-performance-local.json").write_text(
    json.dumps({"raw_measurements": reports}, indent=2) + "\n")
PY
```

Benchmark 本身會在每個規模結束刪除自有 dataset，每次 child reader 也會關閉與回收私有 snapshot。每組樣本原子保存 checkpoint，未完成的 raw report 帶 `complete: false`，不得拿它建立完成或 p95 結論。

## 驗證與後續邊界

- `TestHistoricalQueryPerformanceFixture` 的有效／損毀資料案例通過；損毀 chain 必須在 scripted generation 前拒絕，來源內容不變。
- 完整性能量測、`mise run test`（CLI 163.907s、team 839.568s）、`mise run vet`、`mise run lint`（0 issues）及新增 fixture 的 targeted race（2.313s）均通過。CPU／allocation 剖析另行完成，不改正式樣本。
- 本次不改 production runtime、查詢讀取預算、完整性／scope 判定、模型選擇或 production 設定。
- 沒有量真實 LLM latency／tokens／費用、實體磁碟冷讀、live append contention 或自然語言 recall/support；[歷史結果查詢計畫](../implementation-plans/historical-task-results-llm-query.md)的 30 題與 90%／95% 品質門檻仍未驗收。
- 後續可由 coding agent 優先處理的是已定位 replay／lineage／結果投影的配置與複製，先重現、再做保留契約的局部改善及同 fixture 對比。是否建立可重建結果索引應依瓶頸與量測決定，不能以索引繞過 canonical chain／lineage 驗證。
- 正式 decision 校準仍需真實已結案結果；隔離工程維持非 P0、另案選型，不把外部前置工作列為本輪 coding 欠工。
