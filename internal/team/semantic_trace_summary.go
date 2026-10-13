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

// SemanticRuntimeRecord is the runtime status when a run finished. Counters
// (queries, fallbacks, routes, warmups, refreshes) count that run only; the
// state, model, generation, and last refresh are as of the finish.
type SemanticRuntimeRecord struct {
	RunID      string                `json:"run_id"`
	RecordedAt time.Time             `json:"recorded_at"`
	Status     SemanticRuntimeStatus `json:"status"`
}

// finishSemanticRun runs when run runID finishes, before its identity is
// cleared: it appends the run's runtime status to the workspace, so inspect
// can show it after the process exits, and keeps the run's report section.
// Like traces it is best effort and content-free.
func (c *Coordinator) finishSemanticRun(runID string, baseline SemanticRuntimeStatus) {
	status, ok := c.SemanticRetrievalStatus()
	if !ok || c.session == nil || c.session.Workspace == "" {
		return
	}
	record := SemanticRuntimeRecord{RunID: runID, RecordedAt: time.Now().UTC(), Status: status.since(baseline)}
	appendSemanticRuntimeRecord(c.session.Workspace, record)
	report := SemanticRetrievalReportData{RunID: runID, Status: record.Status}
	traces, skipped, err := ReadSemanticRetrievalTraces(c.session.Workspace, runID)
	if err == nil {
		report.Summary, report.SkippedLines = SummarizeSemanticRetrievalTraces(traces), skipped
	}
	c.lastSemanticRunMu.Lock()
	c.lastSemanticRun = &report
	c.lastSemanticRunMu.Unlock()
}

// since returns s with every counter reduced by base, so a record counts one
// run of a runtime that serves several.
func (s SemanticRuntimeStatus) since(base SemanticRuntimeStatus) SemanticRuntimeStatus {
	s.Queries -= base.Queries
	s.SemanticResults -= base.SemanticResults
	s.TruncatedQueries -= base.TruncatedQueries
	s.RoutedSemantic -= base.RoutedSemantic
	s.RoutedLexical -= base.RoutedLexical
	s.Warmups -= base.Warmups
	s.Refreshes -= base.Refreshes
	s.RefreshFailures -= base.RefreshFailures
	var fallbacks map[string]int
	for reason, count := range s.Fallbacks {
		if delta := count - base.Fallbacks[reason]; delta > 0 {
			if fallbacks == nil {
				fallbacks = map[string]int{}
			}
			fallbacks[reason] = delta
		}
	}
	s.Fallbacks = fallbacks
	return s
}

func appendSemanticRuntimeRecord(workspace string, record SemanticRuntimeRecord) {
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	shadowTraceMu.Lock()
	defer shadowTraceMu.Unlock()
	f, err := os.OpenFile(filepath.Join(workspace, semanticRuntimeStatusFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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
// Each trace is one retrieval: a ranking request whose full query finds
// nothing retries with its goal line, so it can count twice. Served counts
// retrievals the semantic lane answered without a fallback.
type SemanticRouteSummary struct {
	Source         string `json:"source"`
	Route          string `json:"route"`
	Mode           string `json:"mode"`
	Retrievals     int    `json:"retrievals"`
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
	RunID      string         `json:"run_id,omitempty"`
	TaskID     string         `json:"task_id,omitempty"`
	Attempt    int            `json:"attempt,omitempty"`
	Phase      string         `json:"phase,omitempty"`
	Agent      string         `json:"agent,omitempty"`
	Source     string         `json:"source,omitempty"`
	Route      string         `json:"route,omitempty"`
	Share      float64        `json:"route_share"`
	Retrievals int            `json:"retrievals"`
	Served     int            `json:"served"`
	Fallbacks  map[string]int `json:"fallbacks,omitempty"`
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
		group.Retrievals++
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
		task.Retrievals++
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
// report: the runtime status of the run and a summary of its traces.
type SemanticRetrievalReportData struct {
	RunID        string                   `json:"run_id"`
	Status       SemanticRuntimeStatus    `json:"status"`
	Summary      SemanticRetrievalSummary `json:"summary"`
	SkippedLines int                      `json:"skipped_lines,omitempty"`
}

// SemanticRetrievalReport returns the report section of the latest finished
// run, or ok=false when semantic retrieval is off or no run has finished.
func (c *Coordinator) SemanticRetrievalReport() (SemanticRetrievalReportData, bool) {
	if c == nil {
		return SemanticRetrievalReportData{}, false
	}
	c.lastSemanticRunMu.Lock()
	defer c.lastSemanticRunMu.Unlock()
	if c.lastSemanticRun == nil {
		return SemanticRetrievalReportData{}, false
	}
	return *c.lastSemanticRun, true
}
