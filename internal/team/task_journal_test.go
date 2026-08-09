package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeJournalLines(t *testing.T, workspace string, lines ...string) {
	t.Helper()
	path := taskJournalPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTaskJournalRoundtrip(t *testing.T) {
	ws := t.TempDir()
	j, err := openTaskJournal(ws)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ts := now.Format(time.RFC3339)
	recs := []journalRecord{
		{Op: "put", Agent: "coder", Desc: "task A", Verify: "test -f a.txt", Output: "out A", TS: ts, Round: 1},
		{Op: "put", Agent: "coder", Desc: "task B", Output: "out B", TS: ts, Round: 1},
		{Op: "put", Agent: "writer", Desc: "task C", Output: "out C", TS: ts, Round: 2},
	}
	for _, r := range recs {
		if err := j.append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 2 || len(got["writer"]) != 1 {
		t.Fatalf("unexpected replay result: %+v", got)
	}
	if got["coder"][0].output != "out A" || got["coder"][0].verify != "test -f a.txt" || !got["coder"][0].pinned {
		t.Errorf("entry not replayed as pinned put: %+v", got["coder"][0])
	}
}

func TestTerminalTypedResultIsProjectedToJournal(t *testing.T) {
	workspace := t.TempDir()
	journal, err := openTaskJournal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "runner", Desc: "sealed task"}})[0]
	if err := tracker.TodoList().SetTypedResult(item.ID, &TaskResult{TaskID: item.ID, Status: TaskResultStatusSuccess, Summary: "done", RawOutputRef: &ArtifactRef{Path: "logs/task-output/1.jsonl", SHA256: "sealed", Bytes: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "done", "done"); err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{journal: journal, taskTracker: tracker}
	coord.recordTerminalTypedTaskResult(item.ID)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(taskJournalPath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"op":"result"`, `"task_id":"1"`, `"typed_result"`, `"sha256":"sealed"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("journal omitted %q: %s", want, data)
		}
	}
}

func TestTaskJournalKeepsDistinctVerifyModes(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	ts := now.Format(time.RFC3339)
	writeJournalLines(t, ws,
		`{"op":"put","agent":"coder","desc":"inspect","verify":"systemctl is-active api","verify_mode":"success","output":"active","ts":"`+ts+`"}`,
		`{"op":"put","agent":"coder","desc":"inspect","verify":"systemctl is-active api","verify_mode":"expected_failure","output":"inactive","ts":"`+ts+`"}`,
	)

	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 2 {
		t.Fatalf("journal replay entries = %#v, want separate entries for each verify mode", got["coder"])
	}
	if got["coder"][0].verifyMode != "success" || got["coder"][1].verifyMode != "expected_failure" {
		t.Fatalf("journal verify modes = %q, %q", got["coder"][0].verifyMode, got["coder"][1].verifyMode)
	}
}

func TestTaskJournalTombstone(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	ts := now.Format(time.RFC3339)
	writeJournalLines(t, ws,
		`{"op":"put","agent":"coder","desc":"Fix  The Bug","output":"v1","ts":"`+ts+`"}`,
		`{"op":"del","agent":"coder","desc":"fix the bug","ts":"`+ts+`"}`,
	)

	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 0 {
		t.Errorf("tombstone did not remove matching put (case/whitespace-insensitive): %+v", got["coder"])
	}
}

func TestTaskJournalPutAfterDel(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	ts := now.Format(time.RFC3339)
	writeJournalLines(t, ws,
		`{"op":"put","agent":"coder","desc":"task","output":"v1","ts":"`+ts+`"}`,
		`{"op":"del","agent":"coder","desc":"task","ts":"`+ts+`"}`,
		`{"op":"put","agent":"coder","desc":"task","output":"v2","ts":"`+ts+`"}`,
	)

	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 1 || got["coder"][0].output != "v2" {
		t.Errorf("put after del should replay the newer result: %+v", got["coder"])
	}
}

func TestTaskJournalSkipsCorruptLines(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	ts := now.Format(time.RFC3339)
	writeJournalLines(t, ws,
		`{"op":"put","agent":"coder","desc":"good","output":"ok","ts":"`+ts+`"}`,
		`{"op":"put","agent":"coder","desc":"torn`, // simulated crash mid-write
	)

	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 1 || got["coder"][0].taskDesc != "good" {
		t.Errorf("corrupt line handling wrong: %+v", got["coder"])
	}
}

func TestTaskJournalIgnoresErrorOp(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	ts := now.Format(time.RFC3339)
	writeJournalLines(t, ws,
		`{"op":"err","agent":"coder","desc":"task","output":"source=task_timeout | error=context deadline exceeded","ts":"`+ts+`"}`,
		`{"op":"put","agent":"coder","desc":"good","output":"ok","ts":"`+ts+`"}`,
	)

	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 1 || got["coder"][0].taskDesc != "good" {
		t.Errorf("error op should be ignored on replay: %+v", got["coder"])
	}
}

func TestTaskJournalAgeCap(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	old := now.Add(-48 * time.Hour).Format(time.RFC3339)
	fresh := now.Format(time.RFC3339)
	writeJournalLines(t, ws,
		`{"op":"put","agent":"coder","desc":"old","output":"o","ts":"`+old+`"}`,
		`{"op":"put","agent":"coder","desc":"fresh","output":"f","ts":"`+fresh+`"}`,
	)

	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 1 || got["coder"][0].taskDesc != "fresh" {
		t.Errorf("age cap not applied: %+v", got["coder"])
	}
}

func TestTaskJournalPerAgentCap(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	ts := now.Format(time.RFC3339)
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, `{"op":"put","agent":"coder","desc":"task `+strings.Repeat("x", i+1)+`","output":"o","ts":"`+ts+`"}`)
	}
	writeJournalLines(t, ws, lines...)

	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 50 {
		t.Errorf("per-agent cap not applied: got %d entries", len(got["coder"]))
	}
}

func TestTaskJournalMissingFile(t *testing.T) {
	got, err := loadTaskJournal(t.TempDir(), time.Now(), taskJournalMaxAge, 50)
	if err != nil || got != nil {
		t.Errorf("missing journal should be (nil, nil), got (%v, %v)", got, err)
	}
}

func TestCompactTaskJournal(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	ts := now.Format(time.RFC3339)
	writeJournalLines(t, ws,
		`{"op":"put","agent":"coder","desc":"keep","output":"k","ts":"`+ts+`"}`,
		`{"op":"put","agent":"coder","desc":"gone","output":"g","ts":"`+ts+`"}`,
		`{"op":"del","agent":"coder","desc":"gone","ts":"`+ts+`"}`,
	)

	// Below the size threshold: no-op.
	if err := compactTaskJournalIfNeeded(ws, 1<<20, now); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(taskJournalPath(ws))
	if !strings.Contains(string(data), `"del"`) {
		t.Fatalf("no-op compaction rewrote the file")
	}

	// Force compaction with a tiny threshold: tombstoned entry disappears.
	if err := compactTaskJournalIfNeeded(ws, 1, now); err != nil {
		t.Fatal(err)
	}
	got, err := loadTaskJournal(ws, now, taskJournalMaxAge, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["coder"]) != 1 || got["coder"][0].taskDesc != "keep" {
		t.Errorf("compaction survivors wrong: %+v", got["coder"])
	}
	data, _ = os.ReadFile(taskJournalPath(ws))
	if strings.Contains(string(data), `"del"`) || strings.Contains(string(data), "gone") {
		t.Errorf("compacted journal still contains dead records:\n%s", data)
	}
}

func TestPinnedEntriesSurviveGenerationPrune(t *testing.T) {
	c := &Coordinator{taskResultCache: make(map[string][]cachedTaskEntry)}
	c.taskResultCache["coder"] = []cachedTaskEntry{
		{taskDesc: "pinned task", output: "p", generation: 0, pinned: true},
		{taskDesc: "unpinned task", output: "u", generation: 0},
	}

	// Simulate the generation-bump prune from ExecuteTasks.
	newGen := c.cacheGeneration.Add(1)
	c.taskResultCacheMu.Lock()
	for key, entries := range c.taskResultCache {
		var fresh []cachedTaskEntry
		for _, e := range entries {
			if e.generation == newGen || e.pinned {
				fresh = append(fresh, e)
			}
		}
		c.taskResultCache[key] = fresh
	}
	c.taskResultCacheMu.Unlock()

	entries := c.taskResultCache["coder"]
	if len(entries) != 1 || entries[0].taskDesc != "pinned task" {
		t.Errorf("pinned entry did not survive prune: %+v", entries)
	}

	// invalidateTaskCache must remove pinned entries regardless.
	c.invalidateTaskCache("coder", "Pinned  Task")
	if len(c.taskResultCache["coder"]) != 0 {
		t.Errorf("invalidateTaskCache kept a pinned entry: %+v", c.taskResultCache["coder"])
	}
}
