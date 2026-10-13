package team

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

const semanticRuntimeStatusFile = "semantic-runtime-status.jsonl"

// SemanticRuntimeRecord is the runtime status a run left when it closed.
type SemanticRuntimeRecord struct {
	RunID      string                `json:"run_id"`
	RecordedAt time.Time             `json:"recorded_at"`
	Status     SemanticRuntimeStatus `json:"status"`
}

// recordSemanticRuntimeStatus appends the closing runtime status to the
// workspace so inspect can show it after the process exits. Like traces it
// is best effort and content-free.
func (c *Coordinator) recordSemanticRuntimeStatus() {
	if c == nil || c.semanticRuntime == nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	data, err := json.Marshal(SemanticRuntimeRecord{RunID: c.executionRunID, RecordedAt: time.Now().UTC(), Status: c.semanticRuntime.Status()})
	if err != nil {
		return
	}
	shadowTraceMu.Lock()
	defer shadowTraceMu.Unlock()
	f, err := os.OpenFile(filepath.Join(c.session.Workspace, semanticRuntimeStatusFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(data, '\n'))
}

// ReadSemanticRetrievalTraces reads the workspace's semantic traces, keeping
// those of runID when it is set. A missing file has no traces. Lines that do
// not parse (a write cut short by a crash) are skipped and counted.
func ReadSemanticRetrievalTraces(workspace, runID string) ([]contextstore.SemanticRetrievalTrace, int, error) {
	var traces []contextstore.SemanticRetrievalTrace
	skipped, err := readJSONLines(filepath.Join(workspace, semanticTraceFile), func(trace contextstore.SemanticRetrievalTrace) {
		if runID == "" || trace.RunID == runID {
			traces = append(traces, trace)
		}
	})
	return traces, skipped, err
}

// ReadSemanticRuntimeRecords reads the closing runtime statuses, oldest
// first, keeping those of runID when it is set.
func ReadSemanticRuntimeRecords(workspace, runID string) ([]SemanticRuntimeRecord, int, error) {
	var records []SemanticRuntimeRecord
	skipped, err := readJSONLines(filepath.Join(workspace, semanticRuntimeStatusFile), func(record SemanticRuntimeRecord) {
		if runID == "" || record.RunID == runID {
			records = append(records, record)
		}
	})
	return records, skipped, err
}

func readJSONLines[T any](path string, keep func(T)) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()
	skipped := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var value T
		if json.Unmarshal(scanner.Bytes(), &value) != nil {
			skipped++
			continue
		}
		keep(value)
	}
	if err := scanner.Err(); err != nil {
		return skipped, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return skipped, nil
}

// SemanticRetrievalSummary aggregates content-free semantic traces.
type SemanticRetrievalSummary struct {
	Traces    int                    `json:"traces"`
	Runs      int                    `json:"runs"`
	Routes    []SemanticRouteSummary `json:"routes"`
	Fallbacks map[string]int         `json:"fallbacks,omitempty"`
	Tasks     []SemanticTaskSummary  `json:"tasks"`
}

// SemanticRouteSummary covers the traces of one source, route, and mode.
// Served counts requests the semantic lane answered without a fallback.
type SemanticRouteSummary struct {
	Source         string `json:"source"`
	Route          string `json:"route"`
	Mode           string `json:"mode"`
	Requests       int    `json:"requests"`
	Served         int    `json:"served"`
	Fallbacks      int    `json:"fallbacks"`
	Truncated      int    `json:"truncated"`
	P50LatencyMS   int64  `json:"p50_latency_ms"`
	P95LatencyMS   int64  `json:"p95_latency_ms"`
	P95QueueWaitMS int64  `json:"p95_queue_wait_ms"`
	P50EmbedMS     int64  `json:"p50_embed_ms"`
	P95EmbedMS     int64  `json:"p95_embed_ms"`
}

// SemanticTaskSummary covers one task attempt's traces from one source.
type SemanticTaskSummary struct {
	RunID     string         `json:"run_id,omitempty"`
	TaskID    string         `json:"task_id,omitempty"`
	Attempt   int            `json:"attempt,omitempty"`
	Phase     string         `json:"phase,omitempty"`
	Agent     string         `json:"agent,omitempty"`
	Source    string         `json:"source,omitempty"`
	Route     string         `json:"route,omitempty"`
	Share     float64        `json:"route_share"`
	Requests  int            `json:"requests"`
	Served    int            `json:"served"`
	Fallbacks map[string]int `json:"fallbacks,omitempty"`
}

// SummarizeSemanticRetrievalTraces groups traces by source, route, and mode,
// and by task attempt, in order of first appearance.
func SummarizeSemanticRetrievalTraces(traces []contextstore.SemanticRetrievalTrace) SemanticRetrievalSummary {
	summary := SemanticRetrievalSummary{Traces: len(traces), Routes: []SemanticRouteSummary{}, Tasks: []SemanticTaskSummary{}}
	type routeKey struct{ source, route, mode string }
	type taskKey struct {
		run, task string
		attempt   int
		source    string
		route     string
	}
	routeIndex := map[routeKey]int{}
	taskIndex := map[taskKey]int{}
	latencies, queueWaits, embeds := map[int][]int64{}, map[int][]int64{}, map[int][]int64{}
	runs := map[string]bool{}
	for _, trace := range traces {
		runs[trace.RunID] = true
		route := trace.Route
		if route == "" {
			route = contextstore.SemanticRouteSemantic
		}
		served := route == contextstore.SemanticRouteSemantic && trace.FallbackReason == contextstore.SemanticFallbackNone
		fallback := route == contextstore.SemanticRouteSemantic && trace.FallbackReason != contextstore.SemanticFallbackNone && trace.FallbackReason != ""

		rk := routeKey{trace.Source, route, string(trace.Mode)}
		ri, ok := routeIndex[rk]
		if !ok {
			ri = len(summary.Routes)
			routeIndex[rk] = ri
			summary.Routes = append(summary.Routes, SemanticRouteSummary{Source: rk.source, Route: rk.route, Mode: rk.mode})
		}
		group := &summary.Routes[ri]
		group.Requests++
		if served {
			group.Served++
		}
		if fallback {
			group.Fallbacks++
			if summary.Fallbacks == nil {
				summary.Fallbacks = map[string]int{}
			}
			summary.Fallbacks[string(trace.FallbackReason)]++
		}
		if trace.QueryTruncated {
			group.Truncated++
		}
		if route == contextstore.SemanticRouteSemantic {
			latencies[ri] = append(latencies[ri], trace.LatencyMillis)
			queueWaits[ri] = append(queueWaits[ri], trace.QueueWaitMillis)
			if trace.EmbedMillis > 0 {
				embeds[ri] = append(embeds[ri], trace.EmbedMillis)
			}
		}

		tk := taskKey{trace.RunID, trace.TaskID, trace.Attempt, trace.Source, route}
		ti, ok := taskIndex[tk]
		if !ok {
			ti = len(summary.Tasks)
			taskIndex[tk] = ti
			summary.Tasks = append(summary.Tasks, SemanticTaskSummary{
				RunID: tk.run, TaskID: tk.task, Attempt: tk.attempt, Phase: trace.Phase, Agent: trace.Agent,
				Source: tk.source, Route: tk.route, Share: trace.RouteShare,
			})
		}
		task := &summary.Tasks[ti]
		task.Requests++
		if served {
			task.Served++
		}
		if fallback {
			if task.Fallbacks == nil {
				task.Fallbacks = map[string]int{}
			}
			task.Fallbacks[string(trace.FallbackReason)]++
		}
	}
	delete(runs, "")
	summary.Runs = len(runs)
	for i := range summary.Routes {
		group := &summary.Routes[i]
		group.P50LatencyMS, group.P95LatencyMS = percentileMillis(latencies[i], 50), percentileMillis(latencies[i], 95)
		group.P95QueueWaitMS = percentileMillis(queueWaits[i], 95)
		group.P50EmbedMS, group.P95EmbedMS = percentileMillis(embeds[i], 50), percentileMillis(embeds[i], 95)
	}
	return summary
}

// percentileMillis is the nearest-rank percentile; 0 for no values.
func percentileMillis(values []int64, percentile int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	rank := (percentile*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// SemanticRetrievalReportData is the semantic retrieval section of a run
// report: the live runtime status and this run's traces.
type SemanticRetrievalReportData struct {
	Status       SemanticRuntimeStatus    `json:"status"`
	Summary      SemanticRetrievalSummary `json:"summary"`
	SkippedLines int                      `json:"skipped_lines,omitempty"`
}

// SemanticRetrievalReport returns the report section, or ok=false when
// semantic retrieval is off.
func (c *Coordinator) SemanticRetrievalReport() (SemanticRetrievalReportData, bool, error) {
	status, ok := c.SemanticRetrievalStatus()
	if !ok || c.session == nil || c.session.Workspace == "" {
		return SemanticRetrievalReportData{}, false, nil
	}
	traces, skipped, err := ReadSemanticRetrievalTraces(c.session.Workspace, c.executionRunID)
	if err != nil {
		return SemanticRetrievalReportData{Status: status}, true, err
	}
	return SemanticRetrievalReportData{Status: status, Summary: SummarizeSemanticRetrievalTraces(traces), SkippedLines: skipped}, true, nil
}
