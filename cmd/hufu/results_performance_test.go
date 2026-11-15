package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/resultquery"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
)

type historyPerfFixture struct {
	Count        int    `json:"count"`
	Events       int    `json:"events"`
	MatchID      string `json:"match_id"`
	StoreSHA256  string `json:"store_sha256"`
	StoreBytes   int64  `json:"store_bytes"`
	PayloadBytes int64  `json:"payload_bytes"`
	SeedNS       int64  `json:"seed_ns"`
}

type historyPerfSample struct {
	Mode                string `json:"mode"`
	Success             bool   `json:"success"`
	Error               string `json:"error,omitempty"`
	TotalNS             int64  `json:"total_ns"`
	LoadNS              int64  `json:"load_ns"`
	CandidatePipelineNS int64  `json:"candidate_pipeline_ns"`
	OutputBytes         int    `json:"output_bytes"`
	AllocatedBytes      uint64 `json:"allocated_bytes"`
	RSSBaselineBytes    uint64 `json:"rss_baseline_bytes"`
	RSSPeakBytes        uint64 `json:"rss_peak_sampled_bytes"`
	ProcessHWMBytes     uint64 `json:"process_hwm_bytes"`
	LoadedResults       int    `json:"loaded_results"`
	Matched             int    `json:"matched"`
	ScriptedCalls       int    `json:"scripted_calls"`
}

type historyPerfPair struct {
	Samples        []historyPerfSample `json:"samples"`
	SnapshotOpenNS int64               `json:"snapshot_open_ns"`
	ChainScanNS    int64               `json:"validated_scan_ns"`
	ChainEvents    int                 `json:"validated_events"`
}

func historyPerfJSON(t testing.TB, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func historyPerfHash(t testing.TB, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// Import uses the production hash-chain decoder and SQLite transaction. The
// temporary JSONL is deleted before reads; setup is outside query measurements.
func seedHistoryPerf(t testing.TB, workspace string, count int) historyPerfFixture {
	t.Helper()
	started := time.Now()
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(workspace, "seed-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	writer := bufio.NewWriter(file)
	fixture := historyPerfFixture{Count: count, Events: 2*count + 1}
	previousID, previousHash := "", ""
	sequence := 0
	appendEvent := func(kind, task string, payload any) team.RunEvent {
		sequence++
		event := team.RunEvent{SchemaVersion: 2, ID: fmt.Sprintf("history-perf-%09d", sequence), PreviousID: previousID,
			PreviousHash: previousHash, RunID: "run-history-perf", SessionID: "session-history-perf", BranchID: "main",
			InvocationID: "inv-history-perf", TaskID: task, Attempt: 1, Actor: "helper", Type: kind,
			Timestamp: "2026-11-14T12:00:00Z", Payload: historyPerfJSON(t, payload)}
		event.Hash = team.ComputeEventHash(previousHash, event.ID, kind, event.Timestamp, event.Payload)
		fixture.PayloadBytes += int64(len(event.Payload))
		if _, err := writer.Write(append(historyPerfJSON(t, event), '\n')); err != nil {
			t.Fatal(err)
		}
		previousID, previousHash = event.ID, event.Hash
		return event
	}
	appendEvent("run_started", "", map[string]any{"team": "history-perf"})
	for index := range count {
		id := strconv.Itoa(index)
		text := "ordinary saved result " + id
		if index%100 == 0 {
			text = "歷史診斷 needle " + id
		}
		appendEvent("task_created", id, map[string]any{"id": id, "agent": "helper", "status": "pending", "desc": text})
		event := appendEvent("task_completed", id, map[string]any{"id": id, "agent": "helper", "status": "done", "attempt": 1,
			"output": text, "typed_result": map[string]any{"task_id": id, "agent": "helper", "attempt": 1, "status": "success",
				"summary": text, "details": "Saved evidence: " + strings.Repeat("fixture ", 8)}})
		if index%100 == 0 {
			fixture.MatchID = team.HistoricalResultID(event)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	imported, err := team.ImportEventLog(t.Context(), workspace, file)
	if err != nil || imported != fixture.Events {
		t.Fatalf("fixture import: count=%d err=%v", imported, err)
	}
	fixture.StoreSHA256 = historyPerfHash(t, runtimestore.Path(workspace))
	info, err := os.Stat(runtimestore.Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	fixture.StoreBytes = info.Size()
	fixture.SeedNS = time.Since(started).Nanoseconds()
	return fixture
}

func historyPerfMemory() (rss, hwm uint64) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "VmRSS:" && fields[0] != "VmHWM:" {
			continue
		}
		value, _ := strconv.ParseUint(fields[1], 10, 64)
		if fields[0] == "VmRSS:" {
			rss = value * 1024
		} else {
			hwm = value * 1024
		}
	}
	return rss, hwm
}

func measureHistoryPerf(t testing.TB, workspace string, fixture historyPerfFixture, mode string) historyPerfSample {
	t.Helper()
	sample := historyPerfSample{Mode: mode}
	sample.RSSBaselineBytes, _ = historyPerfMemory()
	var peak atomic.Uint64
	peak.Store(sample.RSSBaselineBytes)
	stop := make(chan struct{})
	var monitor sync.WaitGroup
	monitor.Go(func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				rss, _ := historyPerfMemory()
				for old := peak.Load(); rss > old; old = peak.Load() {
					if peak.CompareAndSwap(old, rss) {
						break
					}
				}
			}
		}
	})
	stopMonitor := sync.OnceFunc(func() { close(stop); monitor.Wait() })
	defer stopMonitor()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	deps := resultsDeps{workspace: defaultWorkspaceCommandDeps(), now: func() time.Time {
		return time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	}}
	deps.workspace.getwd = func() (string, error) { return workspace, nil }
	deps.load = func(ctx context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
		start := time.Now()
		snapshot, err := inspectpkg.LoadHistoricalResults(ctx, scope)
		sample.LoadNS += time.Since(start).Nanoseconds()
		sample.LoadedResults += len(snapshot.Results)
		return snapshot, err
	}
	var resolverReturned time.Time
	deps.generate = func(_ context.Context, _ *operatorpkg.OperatorSnapshot, _, purpose, _ string, _ sidecar.Profile) (explainAIResult, error) {
		sample.ScriptedCalls++
		var value any
		switch purpose {
		case "historical_query_resolver":
			value = resultquery.Plan{SchemaVersion: 1, Resolution: "resolved", Intent: "find", Selectors: resultquery.Selectors{Team: "history-perf"},
				TimeRange: resultquery.TimeRange{Timezone: "UTC"}, Statuses: []string{"done"}, StatusBasis: "at_terminal", SearchTerms: []string{"needle"}, Order: "newest", Limit: 10}
			defer func() { resolverReturned = time.Now() }()
		case "historical_query_selection":
			sample.CandidatePipelineNS = time.Since(resolverReturned).Nanoseconds()
			value = map[string]any{"result_ids": []string{fixture.MatchID}}
		case "historical_query_answer":
			value = map[string]any{"blocks": []resultquery.AnswerBlock{{Kind: "fact", Text: "Saved needle result.", Sources: []string{fixture.MatchID}}}}
		default:
			return explainAIResult{}, fmt.Errorf("unexpected scripted purpose %q", purpose)
		}
		return explainAIResult{analysis: string(historyPerfJSON(t, value)), model: "scripted-no-network"}, nil
	}
	var output bytes.Buffer
	command := newResultsCommandWithDeps(deps)
	command.SetOut(&output)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"query", "歷史診斷 needle", "--workspace", workspace, "--team", "history-perf", "--branch", "main", "--model", "scripted-no-network", "--timezone", "UTC", "--output", "json"})
	err := command.ExecuteContext(t.Context())
	sample.TotalNS = time.Since(start).Nanoseconds()
	stopMonitor()
	runtime.ReadMemStats(&after)
	sample.AllocatedBytes = after.TotalAlloc - before.TotalAlloc
	rss, hwm := historyPerfMemory()
	sample.RSSPeakBytes, sample.ProcessHWMBytes = max(rss, peak.Load()), hwm
	sample.OutputBytes = output.Len()
	var envelope resultsEnvelope
	if err == nil {
		err = json.Unmarshal(output.Bytes(), &envelope)
	}
	if err == nil && (envelope.Query == nil || envelope.Query.Matched != (fixture.Count+99)/100 || len(envelope.Query.Sources) != 1 ||
		len(envelope.Query.Blocks) != 1 || envelope.Query.Sources[0].ResultID != fixture.MatchID || sample.ScriptedCalls != 3 || sample.LoadedResults != fixture.Count) {
		err = fmt.Errorf("incomplete historical query: loaded=%d calls=%d", sample.LoadedResults, sample.ScriptedCalls)
	}
	if envelope.Query != nil {
		sample.Matched = envelope.Query.Matched
	}
	sample.Success = err == nil
	if err != nil {
		sample.Error = err.Error()
	}
	return sample
}

// The benchmark re-execs this test to give every pair a fresh application
// process. No model/provider is constructed: generate is scripted at the
// existing CLI dependency seam; all storage, query and JSON paths are real.
func TestHistoricalQueryPerformanceProcess(t *testing.T) {
	path := os.Getenv("HUFU_HISTORY_PERF_CHILD")
	if path == "" {
		t.Skip("benchmark subprocess only")
	}
	var fixture historyPerfFixture
	data, err := os.ReadFile(filepath.Join(path, "fixture.json"))
	if err != nil || json.Unmarshal(data, &fixture) != nil {
		t.Fatal("missing benchmark fixture")
	}
	workspace := filepath.Join(path, "workspace")
	pair := historyPerfPair{}
	for _, mode := range []string{"process_cold", "same_process_warm"} {
		runtime.GC() // outside timing; do not evict shared OS caches.
		pair.Samples = append(pair.Samples, measureHistoryPerf(t, workspace, fixture, mode))
	}
	start := time.Now()
	snapshot, err := team.OpenRunEventSnapshot(t.Context(), workspace)
	pair.SnapshotOpenNS = time.Since(start).Nanoseconds()
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	err = snapshot.Visit(t.Context(), func(team.RunEvent) error { pair.ChainEvents++; return nil })
	pair.ChainScanNS = time.Since(start).Nanoseconds()
	closeErr := snapshot.Close()
	if err != nil || closeErr != nil || pair.ChainEvents != fixture.Events {
		t.Fatalf("full chain scan: events=%d err=%v close=%v", pair.ChainEvents, err, closeErr)
	}
	if historyPerfHash(t, runtimestore.Path(workspace)) != fixture.StoreSHA256 {
		t.Fatal("query changed canonical fixture bytes")
	}
	if err := os.WriteFile(os.Getenv("HUFU_HISTORY_PERF_SAMPLE"), historyPerfJSON(t, pair), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalQueryPerformanceFixture(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			workspace := t.TempDir()
			fixture := seedHistoryPerf(t, workspace, 100)
			if corrupt {
				store, err := runtimestore.Open(t.Context(), workspace)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				if err := store.Exec(t.Context(), "UPDATE events SET payload = '{}' WHERE sequence = 3"); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := historyPerfHash(t, runtimestore.Path(workspace))
			sample := measureHistoryPerf(t, workspace, fixture, "fixture")
			if sample.Success == corrupt || corrupt && sample.ScriptedCalls != 0 {
				t.Fatalf("query did not verify the real journal: %+v", sample)
			}
			if before != historyPerfHash(t, runtimestore.Path(workspace)) {
				t.Fatal("measurement changed the source")
			}
		})
	}
}

func BenchmarkHistoricalQueryEndToEnd(b *testing.B) {
	if runtime.GOOS != "linux" {
		b.Skip("RSS sampling uses Linux /proc")
	}
	for _, count := range []int{1000, 10_000, 100_000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			path, err := os.MkdirTemp(os.TempDir(), "history-perf-")
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = os.RemoveAll(path) })
			fixture := seedHistoryPerf(b, filepath.Join(path, "workspace"), count)
			if err := os.WriteFile(filepath.Join(path, "fixture.json"), historyPerfJSON(b, fixture), 0o600); err != nil {
				b.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				b.Fatal(err)
			}
			var pairs []historyPerfPair
			writeReport := func(complete bool) {
				if output := os.Getenv("HUFU_HISTORY_PERF_REPORT"); output != "" {
					report := struct {
						Fixture  historyPerfFixture `json:"fixture"`
						Pairs    []historyPerfPair  `json:"pairs"`
						Complete bool               `json:"complete"`
					}{fixture, pairs, complete}
					if err := team.AtomicWriteFile(output+"-"+strconv.Itoa(count)+".json", historyPerfJSON(b, report), 0o600); err != nil {
						b.Fatal(err)
					}
				}
			}
			for b.Loop() {
				outputPath := filepath.Join(path, "sample.json")
				command := exec.CommandContext(b.Context(), executable, "-test.run=^TestHistoricalQueryPerformanceProcess$", "-test.timeout=5m")
				command.Env = append(os.Environ(), "HUFU_HISTORY_PERF_CHILD="+path, "HUFU_HISTORY_PERF_SAMPLE="+outputPath)
				if output, err := command.CombinedOutput(); err != nil {
					b.Fatalf("sample process: %v: %.2000s", err, output)
				}
				data, err := os.ReadFile(outputPath)
				var pair historyPerfPair
				if err != nil || json.Unmarshal(data, &pair) != nil || len(pair.Samples) != 2 {
					b.Fatal("missing pair measurement")
				}
				pairs = append(pairs, pair)
				writeReport(false)
				for _, sample := range pair.Samples {
					if !sample.Success {
						b.Fatalf("query failed: %+v", sample)
					}
				}
			}
			for mode, name := range []string{"cold", "warm"} {
				values := make([]int64, len(pairs))
				var peak uint64
				for index, pair := range pairs {
					values[index] = pair.Samples[mode].TotalNS
					peak = max(peak, pair.Samples[mode].RSSPeakBytes)
				}
				slices.Sort(values)
				metric, index := name+"_max_ms", len(values)-1
				if len(values) >= 20 {
					metric, index = name+"_p95_ms", int(math.Ceil(.95*float64(len(values))))-1
				}
				b.ReportMetric(float64(values[index])/1e6, metric)
				b.ReportMetric(float64(peak)/(1<<20), name+"_peak_RSS_MiB")
			}
			writeReport(true)
		})
	}
}
