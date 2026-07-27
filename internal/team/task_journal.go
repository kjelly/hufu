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

	"github.com/anomalyco/hufu/internal/utils"
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
	Op                 string         `json:"op"` // "put", "del", or "err" (diagnostic only)
	Agent              string         `json:"agent"`
	Desc               string         `json:"desc"`
	Verify             string         `json:"verify,omitempty"`
	VerifyMode         string         `json:"verify_mode,omitempty"`
	Output             string         `json:"output,omitempty"`
	TS                 string         `json:"ts"`
	Round              int            `json:"round,omitempty"`
	RepoCommit         string         `json:"repo_commit,omitempty"`
	ProjectFingerprint string         `json:"project_fingerprint,omitempty"`
	Identity           *CacheIdentity `json:"identity,omitempty"`
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
	rec.Desc = utils.RedactSecrets(rec.Desc)
	rec.Output = utils.RedactSecrets(rec.Output)
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

func (c *Coordinator) recordTaskFailure(agentName, taskDesc, detail string) {
	if c == nil || c.journal == nil || agentName == "" || taskDesc == "" || detail == "" {
		return
	}
	_ = c.journal.append(journalRecord{
		Op:     "err",
		Agent:  agentName,
		Desc:   taskDesc,
		Output: detail,
		TS:     time.Now().Format(time.RFC3339),
		Round:  c.round,
	})
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
				taskDesc:   rec.Desc,
				verify:     rec.Verify,
				verifyMode: normalizeVerifyMode(rec.VerifyMode),
				output:     rec.Output,
				pinned:     true,
				identity:   id,
			})
		case "del":
			norm := normalizeTaskCacheKey(rec.Desc)
			normVerify := normalizeTaskCacheKey(rec.Verify)
			normVerifyMode := normalizeVerifyMode(rec.VerifyMode)
			entries := out[rec.Agent]
			fresh := entries[:0]
			for _, e := range entries {
				if normalizeTaskCacheKey(e.taskDesc) != norm || normalizeTaskCacheKey(e.verify) != normVerify || normalizeVerifyMode(e.verifyMode) != normVerifyMode {
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
