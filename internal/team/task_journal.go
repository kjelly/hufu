package team

// The task journal is an append-only JSONL checkpoint of task results.
// Unlike session.json (rewritten wholesale on every mutation, so a crash
// mid-write loses everything), a torn journal only loses its last line.
// It also records tombstones: an on_failure invalidation writes a "del"
// record so a restart cannot resurrect a result the DAG already rejected.
// Entries loaded from the journal are pinned so the per-round generation
// prune in ExecuteTasks does not discard them before their first lookup.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	taskJournalFile     = "task_journal.jsonl"
	taskJournalMaxBytes = 5 << 20 // compact when the file exceeds 5MB
	taskJournalMaxAge   = 24 * time.Hour
	// taskJournalMaxLine bounds a single journal line; task outputs are
	// truncated upstream well below this.
	taskJournalMaxLine = 4 << 20
)

type journalRecord struct {
	Op                  string               `json:"op"` // "put", "del", or "err" (diagnostic only)
	Agent               string               `json:"agent"`
	TaskID              string               `json:"task_id,omitempty"`
	Desc                string               `json:"desc,omitempty"`
	Verify              string               `json:"verify,omitempty"`
	VerifyMode          string               `json:"verify_mode,omitempty"`
	VerifySpec          *VerificationSpec    `json:"verify_spec,omitempty"`
	Verification        *VerificationResult  `json:"verification,omitempty"`
	Output              string               `json:"output,omitempty"`
	FailureOutput       string               `json:"failure_output,omitempty"`
	TS                  string               `json:"ts"`
	Round               int                  `json:"round,omitempty"`
	RepoCommit          string               `json:"repo_commit,omitempty"`
	ProjectFingerprint  string               `json:"project_fingerprint,omitempty"`
	Identity            *CacheIdentity       `json:"identity,omitempty"`
	FailureFingerprints []FailureFingerprint `json:"failure_fingerprints,omitempty"`
	FailureEvent        *FailureEventPayload `json:"failure_event,omitempty"`
	// TypedResult is a read-only durability projection for stable task_id
	// lookups. It is deliberately separate from cache "put" records, whose
	// description-based identity is not authoritative evidence.
	TypedResult *TaskResult `json:"typed_result,omitempty"`
}

type taskJournal struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func taskJournalPath(workspace string) string {
	return filepath.Join(workspace, logsDir, taskJournalFile)
}

func openTaskJournal(workspace string) (*taskJournal, error) {
	if workspace == "" {
		return nil, fmt.Errorf("open task journal: empty workspace")
	}
	path := taskJournalPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("open task journal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open task journal: %w", err)
	}
	return &taskJournal{f: f, path: path}, nil
}

func (j *taskJournal) append(rec journalRecord) error {
	if rec.Op == "err" {
		// Failure records are diagnostics, not cache keys. Never persist the
		// complete task prompt; the structured event carries the task ID and
		// bounded evidence needed for investigation.
		if rec.FailureEvent != nil {
			rec.TaskID = rec.FailureEvent.TaskID
		}
		rec.Desc = ""
	}
	rec.Desc = utils.RedactSecrets(rec.Desc)
	rec.Output = utils.RedactSecrets(rec.Output)
	rec.FailureOutput = utils.RedactSecrets(rec.FailureOutput)
	rec.FailureOutput = utils.TruncateString(rec.FailureOutput, 2000)
	if rec.Op == "err" {
		rec.Output = utils.TruncateString(rec.Output, 500)
	}
	if rec.FailureEvent != nil {
		rec.FailureEvent = RedactedFailureEvent(rec.FailureEvent)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("append task journal: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append task journal: %w", err)
	}
	return nil
}

func (c *Coordinator) recordTaskFailureWithEvent(agentName, taskDesc, detail string, event *FailureEventPayload, fingerprints ...[]FailureFingerprint) {
	c.recordTaskFailureWithEventAndOutput(agentName, taskDesc, detail, event, "", fingerprints...)
}

// recordTerminalTypedTaskResult projects the sealed typed result after the
// Todo has reached done. Session/event-store state remains authoritative; the
// journal provides an append-only recovery/audit projection rather than a
// Markdown-derived result lookup.
func (c *Coordinator) recordTerminalTypedTaskResult(todoID string) {
	if c == nil || c.journal == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.ID != todoID || item.Status != TaskDone || item.TypedResult == nil {
			continue
		}
		copyResult := *item.TypedResult
		if data, err := json.Marshal(copyResult); err == nil {
			if redacted, err := utils.RedactJSON(data); err == nil {
				_ = json.Unmarshal(redacted, &copyResult)
			}
		}
		_ = c.journal.append(journalRecord{Op: "result", Agent: item.Agent, TaskID: item.ID, Desc: item.Desc, TypedResult: &copyResult, TS: time.Now().Format(time.RFC3339)})
		return
	}
}

func (c *Coordinator) recordTaskFailureWithEventAndOutput(agentName, taskDesc, detail string, event *FailureEventPayload, failureOutput string, fingerprints ...[]FailureFingerprint) {
	if c == nil || c.journal == nil || agentName == "" || taskDesc == "" || detail == "" {
		return
	}
	record := journalRecord{
		Op:            "err",
		Agent:         agentName,
		TaskID:        eventTaskID(event),
		Desc:          "",
		Output:        detail,
		FailureOutput: failureOutput,
		FailureEvent:  cloneFailureEventPayload(event),
		TS:            time.Now().Format(time.RFC3339),
		Round:         c.round,
	}
	if len(fingerprints) > 0 {
		record.FailureFingerprints = append([]FailureFingerprint(nil), fingerprints[0]...)
	}
	_ = c.journal.append(record)
}

func eventTaskID(event *FailureEventPayload) string {
	if event == nil {
		return ""
	}
	return event.TaskID
}

func (j *taskJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.f.Close(); err != nil {
		return fmt.Errorf("close task journal: %w", err)
	}
	return nil
}

// loadTaskJournal replays the journal into per-agent cache entries: "put"
// records add results, "del" tombstones remove earlier matching puts (by
// normalized description), corrupt or torn lines are skipped, entries older
// than maxAge are dropped, and each agent keeps only its last perAgentCap
// entries. Returned entries are pinned; the caller assigns generations.
// A missing journal file returns (nil, nil).
func loadTaskJournal(workspace string, now time.Time, maxAge time.Duration, perAgentCap int) (map[string][]cachedTaskEntry, error) {
	f, err := os.Open(taskJournalPath(workspace))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load task journal: %w", err)
	}
	defer func() { _ = f.Close() }()

	out := make(map[string][]cachedTaskEntry)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), taskJournalMaxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec journalRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // torn last line after a crash, or manual corruption
		}
		switch rec.Op {
		case "put":
			if rec.Agent == "" || rec.Desc == "" {
				continue
			}
			if maxAge > 0 {
				ts, err := time.Parse(time.RFC3339, rec.TS)
				if err != nil || now.Sub(ts) > maxAge {
					continue
				}
			}
			id := CacheIdentity{}
			if rec.Identity != nil {
				id = *rec.Identity
			} else {
				id.RepoCommit = rec.RepoCommit
				id.ProjectFingerprint = rec.ProjectFingerprint
			}
			out[rec.Agent] = append(out[rec.Agent], cachedTaskEntry{
				taskDesc:     rec.Desc,
				verify:       rec.Verify,
				verifyMode:   normalizeVerifyMode(rec.VerifyMode),
				verifySpec:   cloneVerificationSpecPtr(rec.VerifySpec),
				verification: cloneVerificationResult(rec.Verification),
				output:       rec.Output,
				pinned:       true,
				identity:     id,
			})
		case "del":
			norm := normalizeTaskCacheKey(rec.Desc)
			contract := taskCacheIdentityWithSpec(rec.Desc, rec.VerifySpec, rec.Verify, rec.VerifyMode)
			entries := out[rec.Agent]
			fresh := entries[:0]
			for _, e := range entries {
				entryContract := taskCacheIdentityWithSpec(e.taskDesc, e.verifySpec, e.verify, e.verifyMode)
				if normalizeTaskCacheKey(e.taskDesc) != norm || entryContract != contract {
					fresh = append(fresh, e)
				}
			}
			out[rec.Agent] = fresh
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("load task journal: %w", err)
	}
	for k, entries := range out {
		if len(entries) == 0 {
			delete(out, k)
		} else if len(entries) > perAgentCap {
			out[k] = entries[len(entries)-perAgentCap:]
		}
	}
	return out, nil
}

// compactTaskJournalIfNeeded rewrites the journal keeping only the entries
// that would survive a replay, then atomically renames it into place (the
// rename is what makes a crash during compaction safe: readers see either
// the old file or the new one, never a partial write). Survivors are
// re-stamped with the current time, which extends their age window by one
// compaction cycle — acceptable for a best-effort cache.
func compactTaskJournalIfNeeded(workspace string, maxBytes int64, now time.Time) error {
	path := taskJournalPath(workspace)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("compact task journal: %w", err)
	}
	if info.Size() <= maxBytes {
		return nil
	}

	survivors, err := loadTaskJournal(workspace, now, taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		return fmt.Errorf("compact task journal: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("compact task journal: %w", err)
	}
	ts := now.Format(time.RFC3339)
	for agentKey, entries := range survivors {
		for _, e := range entries {
			data, err := json.Marshal(journalRecord{
				Op:                 "put",
				Agent:              agentKey,
				Desc:               e.taskDesc,
				Verify:             e.verify,
				VerifyMode:         normalizeVerifyMode(e.verifyMode),
				VerifySpec:         cloneVerificationSpecPtr(e.verifySpec),
				Verification:       cloneVerificationResult(e.verification),
				Output:             e.output,
				TS:                 ts,
				RepoCommit:         e.identity.RepoCommit,
				ProjectFingerprint: e.identity.ProjectFingerprint,
				Identity:           &e.identity,
			})
			if err != nil {
				continue
			}
			if _, err := f.Write(append(data, '\n')); err != nil {
				_ = f.Close()
				_ = os.Remove(tmp)
				return fmt.Errorf("compact task journal: %w", err)
			}
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compact task journal: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compact task journal: %w", err)
	}
	return nil
}

// initTaskJournal compacts, replays, and opens the workspace journal. Loaded
// entries merge into the in-memory cache (skipping keys session.json already
// rehydrated) tagged with the current generation and pinned. Everything here
// is best-effort: failures are logged and never block the run.
func (c *Coordinator) initTaskJournal() {
	if c.noJournal || c.journal != nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	ws := c.session.Workspace
	now := time.Now()
	if err := compactTaskJournalIfNeeded(ws, taskJournalMaxBytes, now); err != nil {
		log.Printf("warning: task journal compaction failed: %v", err)
	}
	prof := c.ExecutionProfile()
	if !prof.DisableJournalRestore && !prof.DisableHistoricalTaskReuse {
		loaded, err := loadTaskJournal(ws, now, taskJournalMaxAge, maxTaskCacheEntries)
		if err != nil {
			log.Printf("warning: task journal load failed: %v", err)
		}
		if len(loaded) > 0 {
			gen := c.cacheGeneration.Load()
			c.taskResultCacheMu.Lock()
			for agentKey, recs := range loaded {
				existing := make(map[string]bool, len(c.taskResultCache[agentKey]))
				for _, e := range c.taskResultCache[agentKey] {
					existing[taskCacheIdentity(e.taskDesc, e.verify, e.verifyMode)] = true
				}
				for _, r := range recs {
					if existing[taskCacheIdentity(r.taskDesc, r.verify, r.verifyMode)] {
						continue
					}
					r.generation = gen
					c.taskResultCache[agentKey] = append(c.taskResultCache[agentKey], r)
				}
				if n := len(c.taskResultCache[agentKey]); n > maxTaskCacheEntries {
					c.taskResultCache[agentKey] = c.taskResultCache[agentKey][n-maxTaskCacheEntries:]
				}
			}
			c.taskResultCacheMu.Unlock()
		}
	}
	j, err := openTaskJournal(ws)
	if err != nil {
		log.Printf("warning: task journal open failed: %v", err)
		return
	}
	c.journal = j
}

// journalAppend writes one record. Callers must NOT hold taskResultCacheMu:
// the journal has its own mutex and disk I/O must never block cache lookups.
func (c *Coordinator) journalAppend(rec journalRecord) {
	j := c.journal
	if j == nil {
		return
	}
	if err := j.append(rec); err != nil {
		log.Printf("warning: %v", err)
	}
}
