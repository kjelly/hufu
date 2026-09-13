package team

// Task result cache and duplicate-delegation detection, plus result
// formatting for the orchestrator.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// errAllWorkerTasksFailed is a task-outcome sentinel, not an agent-tool
// transport failure. The agent tool renders it as ordinary structured
// evidence so the coordinator can follow the recorded recovery disposition.
var errAllWorkerTasksFailed = errors.New("all worker tasks failed")

type agentTaskResult struct {
	agentName      string
	todoID         string
	task           string
	output         string
	err            error
	planText       string
	failedCriteria []string
	idx            int
}

// cachedTaskEntry stores a previously completed task and its output for dedup.
type cachedTaskEntry struct {
	taskDesc     string
	verify       string
	verifyMode   string
	verifySpec   *VerificationSpec // typed verification spec (takes precedence over verify/verifyMode)
	verification *VerificationResult
	output       string
	generation   int64 // cacheGeneration at time of storage
	// pinned marks entries restored from a previous run (session.json or the
	// task journal); the per-round generation prune keeps them so they survive
	// until their first lookup. invalidateTaskCache removes them regardless.
	pinned   bool
	identity CacheIdentity
}

type taskCacheDependencies struct {
	PolicyEngine  func() PolicyEngine
	Identity      func(TaskCacheLookupRequest) CacheIdentity
	Forbidden     func(task, verify string) bool
	SimilarTask   func(context.Context, string, []string, time.Duration) (int, error)
	ObserveThink  func(TaskCacheLookupScope, string)
	AppendJournal func(journalRecord)
}

type defaultTaskCache struct {
	mu         sync.RWMutex
	entries    map[string][]cachedTaskEntry
	generation atomic.Int64
	deps       taskCacheDependencies
}

func newDefaultTaskCache(deps taskCacheDependencies) *defaultTaskCache {
	return &defaultTaskCache{entries: make(map[string][]cachedTaskEntry), deps: deps}
}

func (d taskCacheDependencies) isZero() bool {
	return d.PolicyEngine == nil && d.Identity == nil && d.Forbidden == nil && d.SimilarTask == nil &&
		d.ObserveThink == nil && d.AppendJournal == nil
}

func taskCacheDependenciesFor(c *Coordinator) taskCacheDependencies {
	return taskCacheDependencies{
		PolicyEngine: c.PolicyEngine,
		Identity: func(req TaskCacheLookupRequest) CacheIdentity {
			agentKey := req.AgentKey
			if req.Scope != TaskCacheLookupExecution {
				agentKey = ""
			}
			return c.ComputeCacheIdentity(agentKey, req.Task, req.Verify, req.VerifyMode)
		},
		Forbidden: c.IsCacheForbidden,
		SimilarTask: func(ctx context.Context, task string, candidates []string, timeout time.Duration) (int, error) {
			sidecar := c.AgentPool().Sidecar()
			if sidecar == nil {
				return -1, nil
			}
			sidecarCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return sidecar.SimilarTask(sidecarCtx, task, candidates)
		},
		ObserveThink: func(scope TaskCacheLookupScope, task string) {
			if !c.think {
				return
			}
			if scope == TaskCacheLookupExecution {
				c.emitThinkSidecar("SimilarTask", fmt.Sprintf("checking cache for semantically similar task: %.50s", task))
				return
			}
			c.emitThinkSidecar("SimilarTask", fmt.Sprintf("checking semantic similarity across all history: %.50s", task))
		},
		AppendJournal: func(record journalRecord) {
			record.Round = c.round
			c.journalAppend(record)
		},
	}
}

func (tc *defaultTaskCache) cachePolicy() CachePolicy {
	if tc.deps.PolicyEngine == nil {
		return CacheUse
	}
	return tc.deps.PolicyEngine().GetCachePolicy()
}

func (tc *defaultTaskCache) identity(req TaskCacheLookupRequest) CacheIdentity {
	if tc.deps.Identity == nil {
		return CacheIdentity{}
	}
	return tc.deps.Identity(req)
}

func (tc *defaultTaskCache) isFresh(entry cachedTaskEntry, identity CacheIdentity) bool {
	if tc.deps.PolicyEngine == nil {
		return true
	}
	return tc.deps.PolicyEngine().IsCacheFresh(entry, identity)
}

func (tc *defaultTaskCache) forbidden(task, verify string) bool {
	return tc.deps.Forbidden != nil && tc.deps.Forbidden(task, verify)
}

func (tc *defaultTaskCache) appendJournal(record journalRecord) {
	if tc.deps.AppendJournal != nil {
		tc.deps.AppendJournal(record)
	}
}

func (tc *defaultTaskCache) entriesFor(agentKey string) []cachedTaskEntry {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return slices.Clone(tc.entries[agentKey])
}

func (tc *defaultTaskCache) Lookup(ctx context.Context, req TaskCacheLookupRequest) (TaskCacheLookupResult, bool) {
	if tc == nil {
		return TaskCacheLookupResult{}, false
	}
	policy := tc.cachePolicy()
	if policy == CacheBypass || policy == CacheRefresh || tc.forbidden(req.Task, req.Verify) {
		return TaskCacheLookupResult{}, false
	}

	normalizedSpec := normalizedVerificationSpecForCache(req.VerifySpec, req.Verify, req.VerifyMode)
	if isTaskResultVerificationSpec(normalizedSpec) {
		return TaskCacheLookupResult{}, false
	}
	evidenceSpec := normalizedSpec
	if req.VerifySpec == nil {
		evidenceSpec = nil
	}
	if normalizedSpec != nil {
		if err := validateVerificationSpec(*normalizedSpec); err != nil {
			return TaskCacheLookupResult{}, false
		}
	}

	all := tc.entriesFor(req.AgentKey)
	if req.Scope == TaskCacheLookupCurrentRun {
		currentRun := make([]cachedTaskEntry, 0, len(all))
		for _, entry := range all {
			if !entry.pinned {
				currentRun = append(currentRun, entry)
			}
		}
		all = currentRun
	}
	if len(all) == 0 {
		return TaskCacheLookupResult{}, false
	}

	target := tc.identity(req)
	if req.Scope == TaskCacheLookupExecution {
		generation := tc.generation.Load()
		for i := len(all) - 1; i >= 0; i-- {
			entry := all[i]
			if entry.generation == generation && entry.matchesWithSpec(req.Task, req.VerifySpec, req.Verify, req.VerifyMode) && entry.verificationEvidenceFresh(evidenceSpec) && tc.isFresh(entry, target) {
				return TaskCacheLookupResult{Output: entry.output, MatchedTask: entry.taskDesc}, true
			}
		}
	}

	for i := len(all) - 1; i >= 0; i-- {
		entry := all[i]
		if entry.matchesWithSpec(req.Task, req.VerifySpec, req.Verify, req.VerifyMode) && entry.verificationEvidenceFresh(evidenceSpec) && tc.isFresh(entry, target) {
			return TaskCacheLookupResult{Output: entry.output, MatchedTask: entry.taskDesc}, true
		}
	}

	if tc.deps.SimilarTask == nil {
		return TaskCacheLookupResult{}, false
	}
	semanticEntries := all
	timeout := 5 * time.Second
	if req.Scope == TaskCacheLookupExecution {
		generation := tc.generation.Load()
		semanticEntries = make([]cachedTaskEntry, 0, len(all))
		for _, entry := range all {
			if entry.generation == generation {
				semanticEntries = append(semanticEntries, entry)
			}
		}
		timeout = 10 * time.Second
	} else if len(semanticEntries) > 100 {
		semanticEntries = semanticEntries[len(semanticEntries)-100:]
	}

	eligible := make([]cachedTaskEntry, 0, len(semanticEntries))
	for _, entry := range semanticEntries {
		if entry.matchesVerificationContract(req.VerifySpec, req.Verify, req.VerifyMode) && entry.verificationEvidenceFresh(evidenceSpec) && tc.isFresh(entry, target) {
			eligible = append(eligible, entry)
		}
	}
	if len(eligible) == 0 {
		return TaskCacheLookupResult{}, false
	}

	descriptions := make([]string, len(eligible))
	for i, entry := range eligible {
		descriptions[i] = entry.taskDesc
	}
	if tc.deps.ObserveThink != nil {
		tc.deps.ObserveThink(req.Scope, req.Task)
	}
	idx, err := tc.deps.SimilarTask(ctx, req.Task, descriptions, timeout)
	if err != nil || idx < 0 || idx >= len(eligible) {
		return TaskCacheLookupResult{}, false
	}
	return TaskCacheLookupResult{Output: eligible[idx].output, MatchedTask: eligible[idx].taskDesc}, true
}

func (tc *defaultTaskCache) Store(req TaskCacheStoreRequest) {
	if tc == nil || tc.cachePolicy() == CacheBypass {
		return
	}
	normalizedSpec := normalizedVerificationSpecForCache(req.VerifySpec, req.Verify, req.VerifyMode)
	if (normalizedSpec != nil && normalizeVerifyMode(normalizedSpec.Mode) == "observation") || isTaskResultVerificationSpec(normalizedSpec) {
		return
	}
	if req.VerifySpec != nil && requiresFreshVerificationEvidence(normalizedSpec) && (req.Verification == nil || req.Verification.ExitCode != 0 || req.Verification.EvaluatedAt.IsZero() || req.Verification.Fingerprint == "") {
		return
	}
	lookup := TaskCacheLookupRequest{
		Scope: TaskCacheLookupExecution, AgentKey: req.AgentKey, Task: req.Task,
		VerifySpec: req.VerifySpec, Verify: req.Verify, VerifyMode: req.VerifyMode,
	}
	identity := tc.identity(lookup)
	tc.mu.Lock()
	tc.entries[req.AgentKey] = append(tc.entries[req.AgentKey], cachedTaskEntry{
		taskDesc: req.Task, verify: req.Verify, verifyMode: normalizeVerifyMode(req.VerifyMode),
		verifySpec: cloneVerificationSpecPtr(normalizedSpec), verification: cloneVerificationResult(req.Verification),
		output: req.Output, generation: tc.generation.Load(), identity: identity,
	})
	if len(tc.entries[req.AgentKey]) > maxTaskCacheEntries {
		tc.entries[req.AgentKey] = tc.entries[req.AgentKey][1:]
	}
	tc.mu.Unlock()

	tc.appendJournal(journalRecord{
		Op: "put", Agent: req.AgentKey, Desc: req.Task, Verify: req.Verify,
		VerifyMode: normalizeVerifyMode(req.VerifyMode), VerifySpec: cloneVerificationSpecPtr(normalizedSpec),
		Verification: cloneVerificationResult(req.Verification), Output: req.Output,
		TS: time.Now().Format(time.RFC3339), RepoCommit: identity.RepoCommit,
		ProjectFingerprint: identity.ProjectFingerprint, Identity: &identity,
	})
}

func (tc *defaultTaskCache) Invalidate(req TaskCacheInvalidateRequest) {
	if tc == nil {
		return
	}
	normalized := normalizeTaskCacheKey(req.Task)
	contract := taskCacheIdentityWithSpec(req.Task, req.VerifySpec, req.Verify, req.VerifyMode)
	tc.mu.Lock()
	entries := tc.entries[req.AgentKey]
	fresh := entries[:0]
	for _, entry := range entries {
		entryContract := taskCacheIdentityWithSpec(entry.taskDesc, entry.verifySpec, entry.verify, entry.verifyMode)
		if normalizeTaskCacheKey(entry.taskDesc) != normalized || entryContract != contract {
			fresh = append(fresh, entry)
		}
	}
	tc.entries[req.AgentKey] = fresh
	tc.mu.Unlock()

	tc.appendJournal(journalRecord{
		Op: "del", Agent: req.AgentKey, Desc: req.Task, Verify: req.Verify,
		VerifyMode: normalizeVerifyMode(req.VerifyMode), VerifySpec: cloneVerificationSpecPtr(req.VerifySpec),
		TS: time.Now().Format(time.RFC3339),
	})
}

func (tc *defaultTaskCache) AdvanceGeneration() {
	if tc == nil {
		return
	}
	newGeneration := tc.generation.Add(1)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for key, entries := range tc.entries {
		fresh := make([]cachedTaskEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.generation == newGeneration || entry.pinned {
				fresh = append(fresh, entry)
			}
		}
		tc.entries[key] = fresh
	}
}

func (tc *defaultTaskCache) Restore(seeds []TaskCacheSeed) {
	if tc == nil || len(seeds) == 0 {
		return
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	generation := tc.generation.Load()
	for _, seed := range seeds {
		if seed.Deduplicate {
			identity := taskCacheIdentity(seed.Task, seed.Verify, seed.VerifyMode)
			duplicate := false
			for _, entry := range tc.entries[seed.AgentKey] {
				if taskCacheIdentity(entry.taskDesc, entry.verify, entry.verifyMode) == identity {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
		}
		tc.entries[seed.AgentKey] = append(tc.entries[seed.AgentKey], cachedTaskEntry{
			taskDesc: seed.Task, verify: seed.Verify, verifyMode: normalizeVerifyMode(seed.VerifyMode),
			verifySpec: cloneVerificationSpecPtr(seed.VerifySpec), verification: cloneVerificationResult(seed.Verification),
			output: seed.Output, generation: generation, pinned: seed.Pinned, identity: seed.Identity,
		})
		if n := len(tc.entries[seed.AgentKey]); n > maxTaskCacheEntries {
			tc.entries[seed.AgentKey] = tc.entries[seed.AgentKey][n-maxTaskCacheEntries:]
		}
	}
}

func (tc *defaultTaskCache) Fork(deps taskCacheDependencies) TaskCache {
	clone := newDefaultTaskCache(deps)
	if tc == nil {
		return clone
	}
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	for key, entries := range tc.entries {
		copied := make([]cachedTaskEntry, len(entries))
		for i, entry := range entries {
			copied[i] = entry
			copied[i].verifySpec = cloneVerificationSpecPtr(entry.verifySpec)
			copied[i].verification = cloneVerificationResult(entry.verification)
		}
		clone.entries[key] = copied
	}
	return clone
}

func (tc *defaultTaskCache) currentGeneration() int64 {
	if tc == nil {
		return 0
	}
	return tc.generation.Load()
}

type duplicateTodoMatch struct {
	Item   *TodoItem
	Reason string
}

func cloneVerificationResult(src *VerificationResult) *VerificationResult {
	if src == nil {
		return nil
	}
	copy := *src
	copy.Spec = cloneVerificationSpecPtr(src.Spec)
	return &copy
}

func verificationForTodo(items []*TodoItem, id string) *VerificationResult {
	for _, item := range items {
		if item != nil && item.ID == id {
			return cloneVerificationResult(item.VerifyResult)
		}
	}
	return nil
}

func normalizeTaskCacheKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// normalizeVerifyMode gives equivalent default and explicit success modes one
// cache identity while keeping all other verification semantics distinct.
func normalizeVerifyMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "success":
		return "success"
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

// verificationSpecCacheKey produces a stable string key for a normalized
// VerificationSpec for use in cache identity. It canonically encodes
// assertions as sorted JSON.
func verificationSpecCacheKey(vs *VerificationSpec) string {
	if vs == nil {
		return ""
	}
	// Canonical order includes Equals, since json_assert allows multiple
	// all-of assertions for the same path.
	assertions := canonicalJSONAssertions(vs.Assertions)
	assertionKey := ""
	for _, a := range assertions {
		assertionKey += fmt.Sprintf("%s=%s;", a.Path, canonicalJSONAssertionValue(a.Equals))
	}
	taskResultAssertions := canonicalTaskResultAssertions(vs.TaskResultAssertions)
	taskResultAssertionKey := ""
	for _, a := range taskResultAssertions {
		taskResultAssertionKey += fmt.Sprintf("%s:%s=%s;", a.Pointer, a.Op, canonicalJSONAssertionValue(a.Value))
	}
	return fmt.Sprintf("type:%s|mode:%s|cmd:%s|path:%s|assertions:%s|task_result_assertions:%s|workset:%s|terminal:%t|verified:%t|statuses:%s",
		string(vs.Type), normalizeVerifyMode(vs.Mode), normalizeTaskCacheKey(vs.Command),
		normalizeTaskCacheKey(vs.Path), assertionKey, taskResultAssertionKey, normalizeTaskCacheKey(vs.WorksetSourceTask), vs.WorksetRequireTerminal, vs.WorksetRequireVerified, strings.Join(vs.WorksetAcceptedStatuses, ","))
}

func isTaskResultVerificationSpec(spec *VerificationSpec) bool {
	return spec != nil && spec.Type == VerifyTaskResultAssert
}

func taskCacheIdentity(taskDesc, verify, verifyMode string) string {
	return normalizeTaskCacheKey(taskDesc) + "\nverify:" + normalizeTaskCacheKey(verify) + "\nverify_mode:" + normalizeVerifyMode(verifyMode)
}

// normalizedVerificationSpecForCache translates legacy verification fields and
// fills typed defaults before cache matching. This keeps mixed legacy/typed
// task definitions semantically compatible, including a legacy verify_mode on
// a typed spec which omits Mode.
func normalizedVerificationSpecForCache(verifySpec *VerificationSpec, verify, verifyMode string) *VerificationSpec {
	if verifySpec == nil && strings.TrimSpace(verify) == "" {
		return nil
	}
	var spec VerificationSpec
	if verifySpec != nil {
		spec = *verifySpec
	}
	normalized := NormalizeVerificationSpec(spec, verify, verifyMode)
	return &normalized
}

func taskCacheIdentityWithSpec(taskDesc string, verifySpec *VerificationSpec, verify, verifyMode string) string {
	if normalized := normalizedVerificationSpecForCache(verifySpec, verify, verifyMode); normalized != nil {
		return normalizeTaskCacheKey(taskDesc) + "\nverify_spec:" + verificationSpecCacheKey(normalized)
	}
	return normalizeTaskCacheKey(taskDesc) + "\nverify_spec:none"
}

// duplicateTaskIdentity identifies a delegation for suppression purposes.
// Verification is part of the observable task contract: two otherwise equal
// tasks with different assertions must both run. Preserve the legacy no-
// verifier key so existing in-memory delegated-task counters retain their
// meaning across this migration.
func duplicateTaskIdentity(agentName, taskDesc string, verifySpec *VerificationSpec, verify, verifyMode string) string {
	agentKey := strings.ToLower(agentName)
	if normalizedVerificationSpecForCache(verifySpec, verify, verifyMode) == nil {
		return agentKey + ":" + normalizeTaskDesc(taskDesc)
	}
	return agentKey + ":" + taskCacheIdentityWithSpec(taskDesc, verifySpec, verify, verifyMode)
}

func (e cachedTaskEntry) matches(taskDesc, verify, verifyMode string) bool {
	return normalizeTaskCacheKey(e.taskDesc) == normalizeTaskCacheKey(taskDesc) &&
		normalizeTaskCacheKey(e.verify) == normalizeTaskCacheKey(verify) &&
		normalizeVerifyMode(e.verifyMode) == normalizeVerifyMode(verifyMode)
}

// matchesVerificationContract reports whether an entry may be reused for a
// request with the supplied verification contract. Semantic cache lookup must
// use this too: legacy fields are empty for many typed specs, so comparing
// only verify/verifyMode could otherwise conflate distinct command_exit
// verifiers.
func (e cachedTaskEntry) matchesVerificationContract(verifySpec *VerificationSpec, verify, verifyMode string) bool {
	entrySpec := normalizedVerificationSpecForCache(e.verifySpec, e.verify, e.verifyMode)
	requestedSpec := normalizedVerificationSpecForCache(verifySpec, verify, verifyMode)
	// Observation-mode entries must not be reused for cache hits
	if entrySpec != nil && normalizeVerifyMode(entrySpec.Mode) == "observation" {
		return false
	}
	if requestedSpec != nil && normalizeVerifyMode(requestedSpec.Mode) == "observation" {
		return false
	}
	return verificationSpecCacheKey(entrySpec) == verificationSpecCacheKey(requestedSpec)
}

func (e cachedTaskEntry) matchesWithSpec(taskDesc string, verifySpec *VerificationSpec, verify, verifyMode string) bool {
	return normalizeTaskCacheKey(e.taskDesc) == normalizeTaskCacheKey(taskDesc) &&
		e.matchesVerificationContract(verifySpec, verify, verifyMode)
}

func requiresFreshVerificationEvidence(spec *VerificationSpec) bool {
	return spec != nil && (spec.Type == VerifyFileExists || spec.Type == VerifyFileAbsent || spec.Type == VerifyJSONAssert || spec.Type == VerifyWorksetComplete)
}

func (e cachedTaskEntry) verificationEvidenceFresh(spec *VerificationSpec) bool {
	if !requiresFreshVerificationEvidence(spec) {
		return true
	}
	if spec.Type == VerifyWorksetComplete {
		// Group completeness depends on live child states and the registered
		// source artifact, so a prior verifier result is never reusable.
		return false
	}
	// JSON produced by a command has no stable local artifact to fingerprint;
	// fail closed rather than treating old command output as current evidence.
	if spec.Type == VerifyJSONAssert && strings.TrimSpace(spec.Path) == "" {
		return false
	}
	if e.verification == nil || e.verification.EvaluatedAt.IsZero() || e.verification.Fingerprint == "" {
		return false
	}
	current := ComputeVerificationFingerprint(*spec, e.verification, e.verification.WorkDir)
	return current == e.verification.Fingerprint
}

// lookupTaskCache checks whether newTask has a semantically equivalent prior
// result for agentKey (lowercase agent name).
//
// Lookup order:
//  1. Exact match in current generation (fast path, same workspace state)
//  2. Exact match across all generations (fast path, workspace state may differ but goal is identical)
//  3. Sidecar semantic similarity in current generation only (slower, requires LLM call)
//
// Returns (cachedOutput, true) on a hit.
func (c *Coordinator) lookupTaskCache(ctx context.Context, agentKey, newTask string) (string, bool) {
	return c.lookupTaskCacheWithVerify(ctx, agentKey, newTask, "")
}

func (c *Coordinator) lookupTaskCacheWithVerify(ctx context.Context, agentKey, newTask, verify string) (string, bool) {
	return c.lookupTaskCacheWithVerification(ctx, agentKey, newTask, verify, "")
}

func (c *Coordinator) lookupTaskCacheWithVerification(ctx context.Context, agentKey, newTask, verify, verifyMode string) (string, bool) {
	return c.lookupTaskCacheWithTypedVerification(ctx, agentKey, newTask, nil, verify, verifyMode)
}

func (c *Coordinator) lookupTaskCacheWithTypedVerification(ctx context.Context, agentKey, newTask string, verifySpec *VerificationSpec, verify, verifyMode string) (string, bool) {
	result, ok := c.TaskCache().Lookup(ctx, TaskCacheLookupRequest{
		Scope: TaskCacheLookupExecution, AgentKey: agentKey, Task: newTask,
		VerifySpec: verifySpec, Verify: verify, VerifyMode: verifyMode,
	})
	return result.Output, ok
}

// lookupTaskCacheAllGenerations checks for semantically similar tasks across ALL
// generations (not just current). This is used for duplicate detection before
// delegating tasks.
//
// Lookup order:
//  1. Exact match across all generations (fast path)
//  2. Sidecar semantic similarity across all generations (slower, requires LLM call)
//
// Returns (cachedOutput, cachedTaskDesc, true) on a hit.
func (c *Coordinator) lookupTaskCacheAllGenerations(ctx context.Context, agentKey, newTask string) (string, string, bool) {
	return c.lookupTaskCacheAllGenerationsWithVerify(ctx, agentKey, newTask, "")
}

func (c *Coordinator) lookupTaskCacheAllGenerationsWithVerify(ctx context.Context, agentKey, newTask, verify string) (string, string, bool) {
	return c.lookupTaskCacheAllGenerationsWithVerification(ctx, agentKey, newTask, verify, "")
}

func (c *Coordinator) lookupTaskCacheAllGenerationsWithVerification(ctx context.Context, agentKey, newTask, verify, verifyMode string) (string, string, bool) {
	result, ok := c.TaskCache().Lookup(ctx, TaskCacheLookupRequest{
		Scope: TaskCacheLookupAllGenerations, AgentKey: agentKey, Task: newTask,
		Verify: verify, VerifyMode: verifyMode,
	})
	return result.Output, result.MatchedTask, ok
}

// lookupTaskCacheCurrentRunWithVerification is restricted to entries produced
// by this run: entries pinned from a previous
// run (session.json / task journal) are skipped. Duplicate *rejection* must
// use this variant — a user who explicitly asks to re-run a mission would
// otherwise have the new run's first task killed as a "duplicate" of work
// from the previous run. Cross-run reuse stays available through the
// non-restricted lookup, which returns the cached output instead of an error.
func (c *Coordinator) lookupTaskCacheCurrentRunWithVerification(ctx context.Context, agentKey, newTask, verify, verifyMode string) (string, string, bool) {
	return c.lookupTaskCacheCurrentRunWithTypedVerification(ctx, agentKey, newTask, nil, verify, verifyMode)
}

func (c *Coordinator) lookupTaskCacheCurrentRunWithTypedVerification(ctx context.Context, agentKey, newTask string, verifySpec *VerificationSpec, verify, verifyMode string) (string, string, bool) {
	result, ok := c.TaskCache().Lookup(ctx, TaskCacheLookupRequest{
		Scope: TaskCacheLookupCurrentRun, AgentKey: agentKey, Task: newTask,
		VerifySpec: verifySpec, Verify: verify, VerifyMode: verifyMode,
	})
	return result.Output, result.MatchedTask, ok
}

const maxTaskCacheEntries = 50

// storeTaskCache saves a completed task result so future similar tasks within
// the same coordinator round (same cacheGeneration) can skip re-execution.
func (c *Coordinator) storeTaskCache(agentKey, taskDesc, output string) {
	c.storeTaskCacheWithVerify(agentKey, taskDesc, "", output)
}

func (c *Coordinator) storeTaskCacheWithVerify(agentKey, taskDesc, verify, output string) {
	c.storeTaskCacheWithVerification(agentKey, taskDesc, verify, "", output)
}

func (c *Coordinator) storeTaskCacheWithVerification(agentKey, taskDesc, verify, verifyMode, output string) {
	c.storeTaskCacheWithTypedVerification(agentKey, taskDesc, nil, verify, verifyMode, output)
}

func (c *Coordinator) storeTaskCacheWithTypedVerification(agentKey, taskDesc string, verifySpec *VerificationSpec, verify, verifyMode, output string) {
	c.storeTaskCacheWithTypedVerificationEvidence(agentKey, taskDesc, verifySpec, verify, verifyMode, output, nil)
}

func (c *Coordinator) storeTaskCacheWithTypedVerificationEvidence(agentKey, taskDesc string, verifySpec *VerificationSpec, verify, verifyMode, output string, verification *VerificationResult) {
	c.TaskCache().Store(TaskCacheStoreRequest{
		AgentKey: agentKey, Task: taskDesc, Output: output, VerifySpec: verifySpec,
		Verify: verify, VerifyMode: verifyMode, Verification: verification,
	})
}

// invalidateTaskCache removes all cached results for the given agent whose
// task description matches taskDesc (normalized), across all generations.
// This forces a genuine re-execution when an on_failure DAG loop resets a
// previously completed task — otherwise lookupTaskCache would serve the stale
// output and the retry would be a no-op.
func (c *Coordinator) invalidateTaskCache(agentKey, taskDesc string) {
	c.invalidateTaskCacheWithVerify(agentKey, taskDesc, "")
}

func (c *Coordinator) invalidateTaskCacheWithVerify(agentKey, taskDesc, verify string) {
	c.invalidateTaskCacheWithVerification(agentKey, taskDesc, verify, "")
}

func (c *Coordinator) invalidateTaskCacheWithVerification(agentKey, taskDesc, verify, verifyMode string) {
	c.invalidateTaskCacheWithTypedVerification(agentKey, taskDesc, nil, verify, verifyMode)
}

// invalidateTaskCacheWithTypedVerification removes only entries whose complete
// verification contract matches. A typed verifier is part of the cache
// identity; comparing legacy verify fields alone would delete unrelated typed
// results which conventionally leave those fields empty.
func (c *Coordinator) invalidateTaskCacheWithTypedVerification(agentKey, taskDesc string, verifySpec *VerificationSpec, verify, verifyMode string) {
	c.TaskCache().Invalidate(TaskCacheInvalidateRequest{
		AgentKey: agentKey, Task: taskDesc, VerifySpec: verifySpec, Verify: verify, VerifyMode: verifyMode,
	})
}

func (c *Coordinator) findExistingTodoDuplicate(ctx context.Context, agentKey, desc string, verifySpec *VerificationSpec, verify, verifyMode string) *duplicateTodoMatch {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) == 0 {
		return nil
	}

	normalizedNew := taskCacheIdentityWithSpec(desc, verifySpec, verify, verifyMode)
	exactEligible := make([]*TodoItem, 0, len(items))
	semanticEligible := make([]*TodoItem, 0, len(items))
	for _, item := range items {
		if item == nil || strings.ToLower(item.Agent) != agentKey {
			continue
		}

		switch item.Status {
		case TaskPending, TaskPlanned, TaskInProgress, TaskPaused:
			exactEligible = append(exactEligible, item)
			semanticEligible = append(semanticEligible, item)
		case TaskError:
			exactEligible = append(exactEligible, item)
			if isPermissionBlockedFailureDetail(item.Detail) {
				semanticEligible = append(semanticEligible, item)
			}
		}
	}

	for _, item := range exactEligible {
		if taskCacheIdentityWithSpec(item.Desc, item.VerifySpec, item.Verify, item.VerifyMode) == normalizedNew {
			return &duplicateTodoMatch{
				Item:   item,
				Reason: fmt.Sprintf("existing task %s already has status %s", item.ID, item.Status),
			}
		}
	}

	if len(semanticEligible) == 0 {
		return nil
	}

	s := c.AgentPool().Sidecar()
	if s == nil {
		return nil
	}

	pastDescs := make([]string, len(semanticEligible))
	for i, item := range semanticEligible {
		pastDescs[i] = taskCacheIdentityWithSpec(item.Desc, item.VerifySpec, item.Verify, item.VerifyMode)
	}

	sidecarCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if c.think {
		c.emitThinkSidecar("SimilarTask", fmt.Sprintf("checking todo similarity against active/failed tasks: %.50s", desc))
	}
	idx, err := s.SimilarTask(sidecarCtx, taskCacheIdentityWithSpec(desc, verifySpec, verify, verifyMode), pastDescs)
	if err != nil || idx < 0 || idx >= len(semanticEligible) {
		return nil
	}

	item := semanticEligible[idx]
	reason := fmt.Sprintf("similar to existing task %s with status %s", item.ID, item.Status)
	if item.Status == TaskError && isPermissionBlockedFailureDetail(item.Detail) {
		reason = fmt.Sprintf("similar to blocked task %s; previous failure was permission-related", item.ID)
	}
	return &duplicateTodoMatch{Item: item, Reason: reason}
}

func (c *Coordinator) checkDuplicateTasks(ctx context.Context, tasks []TaskDef) ([]string, map[int]bool, map[int]*duplicateTodoMatch) {
	var warnings []string
	duplicates := make(map[int]bool)
	suppressed := make(map[int]*duplicateTodoMatch)

	// First pass: build local counts for this batch to handle duplicates within the batch
	localCounts := make(map[string]int)
	for _, t := range tasks {
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := duplicateTaskIdentity(t.Agent, desc, t.VerifySpec, t.Verify, t.VerifyMode)
		localCounts[key]++
	}
	_ = localCounts

	c.delegatedTasksMu.Lock()
	// Second pass: check exact duplicates and increment global counts
	// Track how many we've seen in this batch so far (for in-batch dedup: first instance proceeds, rest are duplicates)
	batchSeen := make(map[string]int)
	for i, t := range tasks {
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := duplicateTaskIdentity(t.Agent, desc, t.VerifySpec, t.Verify, t.VerifyMode)
		batchSeen[key]++

		// Check if this exact task was already delegated in a previous round
		if c.delegatedTasks[key] > 0 {
			warnings = append(warnings, fmt.Sprintf("EXACT DUPLICATE: %s (agent=%s, count=%d)", truncateTaskDesc(desc), t.Agent, c.delegatedTasks[key]+batchSeen[key]))
			duplicates[i] = true
			continue
		}

		// Check for duplicates within the current batch (first instance proceeds, rest are duplicates)
		if batchSeen[key] > 1 {
			warnings = append(warnings, fmt.Sprintf("EXACT DUPLICATE (in batch): %s (agent=%s, count=%d)", truncateTaskDesc(desc), t.Agent, batchSeen[key]))
			duplicates[i] = true
			continue
		}
	}

	// Increment global counts for all non-duplicate tasks
	for i, t := range tasks {
		if duplicates[i] {
			continue
		}
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := duplicateTaskIdentity(t.Agent, desc, t.VerifySpec, t.Verify, t.VerifyMode)
		c.delegatedTasks[key]++
	}
	c.delegatedTasksMu.Unlock()

	// Third pass: current todo-list duplicate check (active work and recent failures).
	for i, t := range tasks {
		if duplicates[i] {
			continue
		}
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		agentKey := strings.ToLower(t.Agent)
		if match := c.findExistingTodoDuplicate(ctx, agentKey, desc, t.VerifySpec, t.Verify, t.VerifyMode); match != nil {
			duplicates[i] = true
			suppressed[i] = match
			warnings = append(warnings, fmt.Sprintf("SUPPRESSED DUPLICATE: %s (agent=%s, %s)", truncateTaskDesc(desc), t.Agent, match.Reason))
		}
	}

	// Fourth pass: semantic duplicate check against completed history from
	// THIS run only. Entries pinned from a previous run must not reject new
	// tasks: "re-run the verification" would have its first task errored as a
	// duplicate of last run's work.
	if !c.ExecutionProfile().DisableSemanticDedup {
		for i, t := range tasks {
			if duplicates[i] {
				continue
			}
			desc := t.Goal
			if t.Constraints != "" {
				desc += "\nconstraints: " + t.Constraints
			}
			agentKey := strings.ToLower(t.Agent)
			dupCtx, dupCancel := context.WithTimeout(ctx, 5*time.Second)
			cachedOutput, cachedDesc, cacheOK := c.lookupTaskCacheCurrentRunWithTypedVerification(dupCtx, agentKey, desc, t.VerifySpec, t.Verify, t.VerifyMode)
			dupCancel()
			if cacheOK {
				warnings = append(warnings, fmt.Sprintf("SEMANTIC DUPLICATE: %s (similar to completed task: %q)", truncateTaskDesc(desc), truncateTaskDesc(cachedDesc)))
				duplicates[i] = true
				log.Printf("[WARN] duplicate task detected: agent=%q, task=%q, similar to=%q", t.Agent, desc, cachedDesc)
			} else {
				_ = cachedOutput
			}
		}
	}
	return warnings, duplicates, suppressed
}

func formatTaskResults(results []agentTaskResult, totalTasks int, duplicateWarnings []string) (string, error) {
	var b strings.Builder
	successCount := 0
	errorCount := 0
	planCount := 0
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		if r.err != nil {
			errorCount++
			fmt.Fprintf(&b, "## Agent: %s\n**Status**: ERROR", r.agentName)
			if r.todoID != "" {
				fmt.Fprintf(&b, "\n**Todo ID**: %s", r.todoID)
			}
			fmt.Fprintf(&b, "\n**Error**: %s", r.err)
		} else if r.planText != "" {
			// Plan submitted - don't count as success, just informational
			planCount++
			fmt.Fprintf(&b, "## Agent: %s\n**Status**: PLAN SUBMITTED\n**Todo ID**: %s\n\n%s", r.agentName, r.todoID, r.planText)
		} else {
			successCount++
			fmt.Fprintf(&b, "## Agent: %s\n**Status**: Success", r.agentName)
			if r.todoID != "" {
				fmt.Fprintf(&b, "\n**Todo ID**: %s", r.todoID)
			}
			fmt.Fprintf(&b, "\n\n%s", r.output)
		}
	}
	summary := fmt.Sprintf("\n\n---\nSummary: %d/%d tasks completed successfully", successCount, totalTasks)
	if errorCount > 0 {
		summary += fmt.Sprintf(", %d failed", errorCount)
	}
	b.WriteString(summary)
	if len(duplicateWarnings) > 0 {
		b.WriteString("\n\n**Warning**: You have delegated the same task to the same agent multiple times. This suggests you may be stuck in a loop. Consider using a different approach, agent, or calling `finish` with your best answer so far:\n")
		for _, w := range duplicateWarnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	// Error only if all tasks failed AND no plans were submitted
	if successCount == 0 && errorCount > 0 && planCount == 0 {
		return b.String(), fmt.Errorf("%w: %d task(s)", errAllWorkerTasksFailed, len(results))
	}
	return b.String(), nil
}

func truncateTaskDesc(task string) string {
	const maxLen = 80
	if utf8.RuneCountInString(task) > maxLen {
		runes := []rune(task)
		return string(runes[:maxLen])
	}
	return task
}

func normalizeTaskDesc(task string) string {
	return strings.Join(strings.Fields(strings.ToLower(task)), " ")
}
