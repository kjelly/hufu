package team

// The task journal is an append-only checkpoint of task results, one row per
// record in runtime.sqlite's task_journal table. Unlike the session
// checkpoint, which is replaced wholesale on every mutation, it only grows.
// It also records tombstones: an on_failure invalidation writes a "del"
// record so a restart cannot resurrect a result the DAG already rejected.
// Entries loaded from the journal are pinned so the per-round generation
// prune in ExecuteTasks does not discard them before their first lookup.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	taskJournalMaxBytes = 5 << 20 // compact when the records exceed 5MB
	taskJournalMaxAge   = 24 * time.Hour
)

type journalRecord struct {
	Op                  string                     `json:"op"` // "put", "del", "err", or "progress" (diagnostic only)
	Agent               string                     `json:"agent"`
	TaskID              string                     `json:"task_id,omitempty"`
	RunID               string                     `json:"run_id,omitempty"`
	Desc                string                     `json:"desc,omitempty"`
	Verify              string                     `json:"verify,omitempty"`
	VerifyMode          string                     `json:"verify_mode,omitempty"`
	VerifySpec          *VerificationSpec          `json:"verify_spec,omitempty"`
	Verification        *VerificationResult        `json:"verification,omitempty"`
	Output              string                     `json:"output,omitempty"`
	FailureOutput       string                     `json:"failure_output,omitempty"`
	TS                  string                     `json:"ts"`
	Round               int                        `json:"round,omitempty"`
	RepoCommit          string                     `json:"repo_commit,omitempty"`
	ProjectFingerprint  string                     `json:"project_fingerprint,omitempty"`
	Identity            *CacheIdentity             `json:"identity,omitempty"`
	FailureFingerprints []FailureFingerprint       `json:"failure_fingerprints,omitempty"`
	FailureEvent        *FailureEventPayload       `json:"failure_event,omitempty"`
	ActionProgress      *ActionProgressObservation `json:"action_progress,omitempty"`

	DynamicToolAuthorization *DynamicToolAuthorizationSnapshot `json:"dynamic_tool_authorization,omitempty"`
	ResourceScopeSnapshot    *TaskResourceScopeSnapshot        `json:"resource_scope_snapshot,omitempty"`
	ResultContract           *ResultContractRef                `json:"result_contract,omitempty"`
	WorkerWorkspace          *WorkerWorkspacePolicy            `json:"worker_workspace,omitempty"`
	ExecutionRoute           *ExecutionRouteBinding            `json:"execution_route,omitempty"`
	CatalogAction            *CatalogActionBinding             `json:"catalog_action,omitempty"`

	// TypedResult is a read-only durability projection for stable task_id
	// lookups. It is deliberately separate from cache "put" records, whose
	// description-based identity is not authoritative evidence.
	TypedResult      *TaskResult                `json:"typed_result,omitempty"`
	ContextManifests []ContextInjectionManifest `json:"context_manifests,omitempty"`
	DecisionID       string                     `json:"decision_id,omitempty"`
	Assumptions      []DecisionAssumption       `json:"assumptions,omitempty"`
	DecisionStale    bool                       `json:"decision_stale,omitempty"`
}

func (c *Coordinator) recordDecisionAssumptionProjection(entry DecisionIndexEntry) error {
	if c == nil || c.journal == nil {
		return nil
	}
	assumptions := append([]DecisionAssumption(nil), entry.Assumptions...)
	return c.journal.append(journalRecord{
		Op: "decision_assumptions", TaskID: entry.TaskID, RunID: entry.RunID,
		DecisionID: entry.DecisionID, Assumptions: assumptions, DecisionStale: entry.Stale,
		TS: time.Now().Format(time.RFC3339),
	})
}

func recordDecisionIndexTaskProjection(workspace string, entry DecisionIndexEntry) error {
	journal, err := openTaskJournal(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = journal.Close() }()
	return journal.append(journalRecord{
		Op: "decision_assumptions", TaskID: entry.TaskID, RunID: entry.RunID,
		DecisionID: entry.DecisionID, Assumptions: append([]DecisionAssumption(nil), entry.Assumptions...),
		DecisionStale: entry.Stale, TS: time.Now().Format(time.RFC3339),
	})
}

type taskJournal struct {
	mu    sync.Mutex
	store *runtimestore.Store
}

func openTaskJournal(workspace string) (*taskJournal, error) {
	if workspace == "" {
		return nil, fmt.Errorf("open task journal: empty workspace")
	}
	store, err := runtimestore.Open(context.Background(), workspace)
	if err != nil {
		return nil, fmt.Errorf("open task journal: %w", err)
	}
	return &taskJournal{store: store}, nil
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
	if err := j.store.Exec(context.Background(), insertTaskJournalSQL, time.Now().UTC().Format(time.RFC3339Nano), data); err != nil {
		return fmt.Errorf("append task journal: %w", err)
	}
	return nil
}

const insertTaskJournalSQL = `INSERT INTO task_journal(recorded_at, record) VALUES(?, ?)`

func insertTaskJournalRecord(ctx context.Context, q runtimestore.Queryer, record []byte) error {
	_, err := q.ExecContext(ctx, insertTaskJournalSQL, time.Now().UTC().Format(time.RFC3339Nano), record)
	return err
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
		copyResult := cloneTaskResult(item.TypedResult)
		if data, err := json.Marshal(copyResult); err == nil {
			if redacted, err := utils.RedactJSON(data); err == nil {
				_ = json.Unmarshal(redacted, copyResult)
			}
		}
		runID := c.executionRunID
		if runID == "" {
			runID = c.taskTracker.TodoList().RunID()
		}
		_ = c.journal.append(journalRecord{Op: "result", Agent: item.Agent, TaskID: item.ID, RunID: runID, Desc: item.Desc, TypedResult: copyResult, ContextManifests: normalizeContextManifests(item.ContextManifests), DynamicToolAuthorization: cloneDynamicToolAuthorizationSnapshot(item.DynamicToolAuthorization), ResourceScopeSnapshot: cloneTaskResourceScopeSnapshot(item.ResourceScopeSnapshot), ResultContract: item.ResultContract.clone(), WorkerWorkspace: item.WorkerWorkspace.clone(), ExecutionRoute: item.ExecutionRoute.clone(), CatalogAction: item.CatalogAction.clone(), TS: time.Now().Format(time.RFC3339)})
		return
	}
}

func (c *Coordinator) recordTaskFailureWithEventAndOutput(agentName, taskDesc, detail string, event *FailureEventPayload, failureOutput string, fingerprints ...[]FailureFingerprint) {
	if c == nil || c.journal == nil || agentName == "" || taskDesc == "" || detail == "" {
		return
	}
	runID := c.executionRunID
	if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		runID = c.taskTracker.TodoList().RunID()
	}
	record := journalRecord{
		Op:            "err",
		Agent:         agentName,
		TaskID:        eventTaskID(event),
		RunID:         runID,
		Desc:          "",
		Output:        detail,
		FailureOutput: failureOutput,
		FailureEvent:  cloneFailureEventPayload(event),
		TS:            time.Now().Format(time.RFC3339),
		Round:         c.round,
	}
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item != nil && item.ID == record.TaskID {
				record.ContextManifests = normalizeContextManifests(item.ContextManifests)
				break
			}
		}
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
	if err := j.store.Close(); err != nil {
		return fmt.Errorf("close task journal: %w", err)
	}
	return nil
}

// loadTaskJournal replays the journal into per-agent cache entries: "put"
// records add results, "del" tombstones remove earlier matching puts (by
// normalized description), undecodable records are skipped, entries older
// than maxAge are dropped, and each agent keeps only its last perAgentCap
// entries. Returned entries are pinned; the caller assigns generations.
// A workspace without a journal returns (nil, nil). It never creates
// runtime.sqlite.
func loadTaskJournal(workspace string, now time.Time, maxAge time.Duration, perAgentCap int) (map[string][]cachedTaskEntry, error) {
	store, err := runtimestore.OpenRead(context.Background(), workspace)
	if errors.Is(err, runtimestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load task journal: %w", err)
	}
	defer func() { _ = store.Close() }()
	records, err := readTaskJournalRecords(context.Background(), store.DB())
	if err != nil {
		return nil, fmt.Errorf("load task journal: %w", err)
	}
	return replayTaskJournal(records, now, maxAge, perAgentCap), nil
}

// readTaskJournalRecords returns every record in journal order.
func readTaskJournalRecords(ctx context.Context, q runtimestore.Queryer) ([][]byte, error) {
	rows, err := q.QueryContext(ctx, `SELECT record FROM task_journal ORDER BY sequence`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records [][]byte
	for rows.Next() {
		var record []byte
		if err := rows.Scan(&record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func replayTaskJournal(records [][]byte, now time.Time, maxAge time.Duration, perAgentCap int) map[string][]cachedTaskEntry {
	out := make(map[string][]cachedTaskEntry)
	for _, record := range records {
		var rec journalRecord
		if err := json.Unmarshal(record, &rec); err != nil {
			continue // manual corruption
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
	for k, entries := range out {
		if len(entries) == 0 {
			delete(out, k)
		} else if len(entries) > perAgentCap {
			out[k] = entries[len(entries)-perAgentCap:]
		}
	}
	return out
}

// compactTaskJournalIfNeeded replaces the journal with only the entries
// that would survive a replay, in one transaction, once its records exceed
// maxBytes. Survivors are re-stamped with the current time, which extends
// their age window by one compaction cycle — acceptable for a best-effort
// cache.
func compactTaskJournalIfNeeded(workspace string, maxBytes int64, now time.Time) error {
	if !runtimestore.Exists(workspace) {
		return nil
	}
	err := withRuntimeStore(workspace, func(store *runtimestore.Store) error {
		ctx := context.Background()
		var size int64
		if err := store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(length(record)), 0) FROM task_journal`).Scan(&size); err != nil {
			return err
		}
		if size <= maxBytes {
			return nil
		}
		return store.WithTx(ctx, func(tx *sql.Tx) error {
			records, err := readTaskJournalRecords(ctx, tx)
			if err != nil {
				return err
			}
			survivors := replayTaskJournal(records, now, taskJournalMaxAge, maxTaskCacheEntries)
			if _, err := tx.ExecContext(ctx, `DELETE FROM task_journal`); err != nil {
				return err
			}
			ts := now.Format(time.RFC3339)
			for _, agentKey := range slices.Sorted(maps.Keys(survivors)) {
				for _, e := range survivors[agentKey] {
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
					if err := insertTaskJournalRecord(ctx, tx, data); err != nil {
						return err
					}
				}
			}
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("compact task journal: %w", err)
	}
	return nil
}

// ExportTaskJournal writes workspace's task journal records to w as JSONL in
// journal order, the format the retired task_journal.jsonl used. A workspace
// without runtime.sqlite exports nothing.
func ExportTaskJournal(ctx context.Context, workspace string, w io.Writer) error {
	store, err := runtimestore.OpenRead(ctx, workspace)
	if errors.Is(err, runtimestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("export task journal: %w", err)
	}
	defer func() { _ = store.Close() }()
	records, err := readTaskJournalRecords(ctx, store.DB())
	if err != nil {
		return fmt.Errorf("export task journal: %w", err)
	}
	for _, record := range records {
		if _, err := w.Write(append(record, '\n')); err != nil {
			return fmt.Errorf("export task journal: %w", err)
		}
	}
	return nil
}

// initTaskJournal compacts, replays, and opens the workspace journal. Loaded
// entries merge into the in-memory cache (skipping keys the session
// checkpoint already rehydrated) tagged with the current generation and pinned. Everything here
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
			var seeds []TaskCacheSeed
			for agentKey, recs := range loaded {
				for _, r := range recs {
					seeds = append(seeds, TaskCacheSeed{
						AgentKey: agentKey, Task: r.taskDesc, Output: r.output,
						VerifySpec: r.verifySpec, Verify: r.verify, VerifyMode: r.verifyMode,
						Verification: r.verification, Identity: r.identity,
						Pinned: true, Deduplicate: true,
					})
				}
			}
			c.TaskCache().Restore(seeds)
		}
	}
	j, err := openTaskJournal(ws)
	if err != nil {
		log.Printf("warning: task journal open failed: %v", err)
		return
	}
	c.journal = j
}

// journalAppend writes one record. Cache callers release their own mutex
// before entering here so disk I/O never blocks cache lookups.
func (c *Coordinator) journalAppend(rec journalRecord) {
	j := c.journal
	if j == nil {
		return
	}
	if err := j.append(rec); err != nil {
		log.Printf("warning: %v", err)
	}
}
