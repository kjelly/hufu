package team

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

// dagScheduler executes one delegation round's tasks as a DAG: depends_on
// edges gate readiness, concurrency is bounded by the coordinator's
// max-concurrent semaphore, identical in-flight tasks are deduplicated, and
// on_failure edges form bounded retry loops (a failed task resets its target
// ancestor and everything that transitively depends on it).
//
// All mutable scheduler state is owned by the goroutine that calls run();
// task goroutines communicate back exclusively through eventCh, so none of it
// needs locking. The only shared structures are the inflight dedup map (its
// own mutex) and the semaphore channel.
type dagScheduler struct {
	coord      *Coordinator
	tasks      []TaskDef
	todoItems  []*TodoItem
	duplicates map[int]bool // batch indices flagged as duplicate delegations

	states     []TaskStatus
	retries    []int
	results    []agentTaskResult
	needsReset []bool
	revDeps    [][]int // revDeps[i] lists the tasks that depend on i
	eventCh    chan agentTaskResult
	inProgress int

	inflightMu sync.Mutex
	inflight   map[string]chan agentTaskResult
	sem        chan struct{}
}

func newDAGScheduler(c *Coordinator, tasks []TaskDef, todoItems []*TodoItem, duplicates map[int]bool) *dagScheduler {
	s := &dagScheduler{
		coord:      c,
		tasks:      tasks,
		todoItems:  todoItems,
		duplicates: duplicates,
		states:     make([]TaskStatus, len(tasks)),
		retries:    make([]int, len(tasks)),
		results:    make([]agentTaskResult, len(tasks)),
		needsReset: make([]bool, len(tasks)),
		revDeps:    make([][]int, len(tasks)),
		eventCh:    make(chan agentTaskResult, len(tasks)),
		inflight:   make(map[string]chan agentTaskResult),
		sem:        make(chan struct{}, c.maxConcurrent),
	}
	for i := range s.states {
		s.states[i] = TaskPending
	}
	for j, t := range tasks {
		for _, dep := range t.DependsOn {
			if dep >= 0 && dep < len(tasks) && dep != j {
				s.revDeps[dep] = append(s.revDeps[dep], j)
			}
		}
	}
	return s
}

// run launches ready tasks and processes completion events until the DAG
// drains, then marks tasks stranded by failed dependencies. The returned
// slice is indexed by task position in the input batch.
func (s *dagScheduler) run(ctx context.Context) ([]agentTaskResult, error) {
	s.launchReady(ctx)
	for s.inProgress > 0 {
		select {
		case res := <-s.eventCh:
			s.handleEvent(ctx, res)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.markStranded()
	return s.results, nil
}

// launchReady starts every pending task whose dependencies are all done.
func (s *dagScheduler) launchReady(ctx context.Context) {
	for i, t := range s.tasks {
		if s.states[i] != TaskPending {
			continue
		}
		ready := true
		for _, depIdx := range t.DependsOn {
			if depIdx >= 0 && depIdx < len(s.tasks) && s.states[depIdx] != TaskDone {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}

		s.states[i] = TaskInProgress
		s.inProgress++
		go s.runTask(ctx, t, s.todoItems[i].ID, i, s.duplicates[i])
	}
}

// handleEvent applies one task completion to the scheduler state and launches
// any tasks that became ready as a result.
func (s *dagScheduler) handleEvent(ctx context.Context, res agentTaskResult) {
	c := s.coord
	s.inProgress--
	idx := res.idx
	if idx < 0 || idx >= len(s.tasks) {
		return
	}

	// A reset wave swept this task while it was still running: discard the
	// stale result and re-queue the task.
	if s.needsReset[idx] {
		s.needsReset[idx] = false
		s.states[idx] = TaskError // no longer in flight; lets resetTask apply
		s.resetTask(idx, "reset by DAG retry")
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		s.launchReady(ctx)
		return
	}

	s.results[idx] = res

	if _, blocked := isCapabilityBlockedError(res.err); blocked {
		s.states[idx] = TaskBlocked
		return
	}

	if res.err == nil {
		s.states[idx] = TaskDone
		s.launchReady(ctx)
		return
	}

	s.states[idx] = TaskError
	if s.tasks[idx].OnFailure == nil || s.retries[idx] >= s.tasks[idx].MaxRetries {
		return
	}
	s.retries[idx]++
	if next := c.escalateTaskModelForRetry(s.tasks[idx]); next != "" {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("escalating task %q to model %s for DAG retry", s.tasks[idx].Agent, next)))
		s.tasks[idx].Model = next
	}
	targetIdx := *s.tasks[idx].OnFailure
	c.report(c.newEvent("step").withMessage(fmt.Sprintf("DAG loop triggered: task %q failed, jumping back to task %q (retry %d/%d)", s.tasks[idx].Agent, s.tasks[targetIdx].Agent, s.retries[idx], s.tasks[idx].MaxRetries)))
	s.resetWave(targetIdx)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	s.launchReady(ctx)
}

// resetWave resets the on_failure target and everything that (transitively)
// depends on it. validateOnFailureTargets guarantees the failed task itself
// is inside this set. Tasks currently in flight are only marked for reset;
// applying it immediately would let launchReady start a second concurrent
// instance (and the inflight dedup would hand that instance the stale
// in-flight result), so the reset is deferred until their event arrives.
func (s *dagScheduler) resetWave(targetIdx int) {
	visited := make(map[int]bool)
	q := []int{targetIdx}
	for len(q) > 0 {
		curr := q[0]
		q = q[1:]
		if visited[curr] {
			continue
		}
		visited[curr] = true
		if s.states[curr] == TaskInProgress {
			s.needsReset[curr] = true
		} else {
			s.resetTask(curr, "reset by DAG retry")
		}
		q = append(q, s.revDeps[curr]...)
	}
}

// resetTask returns a finished (or errored) task to Pending so it can run
// again, and invalidates its cached result so the re-run actually executes
// instead of being served the stale output. Must not be called on a task that
// is currently in flight — mark it via needsReset instead.
func (s *dagScheduler) resetTask(i int, detail string) {
	s.coord.invalidateTaskCacheWithTypedVerification(strings.ToLower(s.tasks[i].Agent), s.taskDesc(i), s.tasks[i].VerifySpec, s.tasks[i].Verify, s.tasks[i].VerifyMode)
	if s.states[i] == TaskPending {
		return // never ran in this wave; nothing else to reset
	}
	s.states[i] = TaskPending
	s.coord.taskTracker.TodoList().ResetForRetry(s.todoItems[i].ID, detail)
}

// markStranded handles tasks whose dependencies failed and therefore never
// became ready. They are marked skipped with an explicit error result —
// otherwise they would surface as zero-valued "successes" in
// formatTaskResults and their todos would hang in pending forever.
func (s *dagScheduler) markStranded() {
	c := s.coord
	stranded := false
	for i := range s.tasks {
		if s.states[i] != TaskPending {
			continue
		}
		stranded = true
		c.taskTracker.TodoList().UpdateStatus(s.todoItems[i].ID, TaskSkipped, "skipped: dependency did not complete successfully")
		s.results[i] = agentTaskResult{
			agentName: s.tasks[i].Agent,
			todoID:    s.todoItems[i].ID,
			task:      s.taskDesc(i),
			err:       fmt.Errorf("skipped: a depends_on task did not complete successfully"),
			idx:       i,
		}
	}
	if stranded {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
}

func (s *dagScheduler) taskDesc(i int) string {
	desc := s.tasks[i].Goal
	if s.tasks[i].Constraints != "" {
		desc += "\nconstraints: " + s.tasks[i].Constraints
	}
	return desc
}

// runTask is the per-task goroutine body: it rejects duplicates, acquires a
// concurrency slot, deduplicates against identical in-flight tasks, consults
// the result cache, and finally executes the task. Exactly one event is sent
// to eventCh on every path, including panics.
func (s *dagScheduler) runTask(ctx context.Context, td TaskDef, tid string, idx int, dup bool) {
	c := s.coord
	desc := td.Goal
	if td.Constraints != "" {
		desc += "\nconstraints: " + td.Constraints
	}
	agentKey := strings.ToLower(td.Agent)
	cacheKey := agentKey + ":" + taskCacheIdentityWithSpec(desc, td.VerifySpec, td.Verify, td.VerifyMode)
	// A verbatim task owns a per-todo evidence artifact. Identical concurrent
	// tasks must not share an in-flight result, or the follower would report a
	// manifest for the owner's transcript instead of producing its own evidence.
	if taskUsesVerbatimTranscript(td) {
		cacheKey += ":verbatim:" + tid
	}
	var isOwner bool

	if len(td.Requires) > 0 {
		reqs, missing := c.taskCapabilityRequirements(td.Requires)
		if len(missing) > 0 {
			detail := fmt.Sprintf("unknown capability requirement(s): %s", strings.Join(missing, ", "))
			c.taskTracker.TodoList().UpdateStatus(tid, TaskBlocked, detail)
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("needs_human").withAgent(td.Agent).withTodoID(tid).withMessage(detail))
			s.eventCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, err: capabilityBlockedError{Result: CapabilityResult{Name: strings.Join(missing, ","), Available: false, Reason: detail, CheckedAt: time.Now()}}, idx: idx}
			return
		}
		if results, err := c.checkCapabilityRequirements(ctx, reqs); err != nil {
			if blocked, ok := isCapabilityBlockedError(err); ok {
				detail := blocked.Reason
				if detail == "" {
					detail = "capability requirement blocked"
				}
				c.taskTracker.TodoList().UpdateStatus(tid, TaskBlocked, detail)
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("needs_human").withAgent(td.Agent).withTodoID(tid).withMessage(detail))
				s.eventCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, err: capabilityBlockedError{Result: blocked}, idx: idx}
				return
			}
			_ = results
			c.taskTracker.TodoList().UpdateStatus(tid, TaskError, err.Error())
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			s.eventCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, err: err, idx: idx}
			return
		}
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC] task goroutine %q/%q recovered: %v", td.Agent, tid, r)
			c.taskTracker.TodoList().UpdateStatus(tid, TaskError, fmt.Sprintf("panic: %v", r))
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			res := agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, err: fmt.Errorf("panic recovered: %v", r), idx: idx}
			if isOwner {
				s.inflightMu.Lock()
				if ch, ok := s.inflight[cacheKey]; ok {
					select {
					case ch <- res:
					default:
					}
					delete(s.inflight, cacheKey)
				}
				s.inflightMu.Unlock()
			}
			s.eventCh <- res
		}
	}()

	if dup {
		errMsg := fmt.Errorf("duplicate task: %s - reference existing completed task instead", truncateTaskDesc(td.Goal))
		c.taskTracker.TodoList().UpdateStatus(tid, TaskError, "duplicate task detected")
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("step").withAgent(td.Agent).withMessage(fmt.Sprintf("duplicate detected: %s", truncateTaskDesc(td.Goal))))
		log.Printf("[WARN] duplicate task rejected: agent=%q, task=%q", td.Agent, td.Goal)
		s.eventCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: "", err: errMsg, idx: idx}
		return
	}

	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		s.eventCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, err: ctx.Err(), idx: idx}
		return
	}

	semReleased := false
	defer func() {
		if !semReleased {
			<-s.sem
		}
	}()

	// In-flight dedup: the first task with a given key runs; identical
	// concurrent tasks release their slot and wait to share its result.
	s.inflightMu.Lock()
	if ch, ok := s.inflight[cacheKey]; ok {
		s.inflightMu.Unlock()
		semReleased = true
		<-s.sem
		select {
		case result := <-ch:
			s.eventCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: result.output, err: result.err, idx: idx}
		case <-ctx.Done():
			s.eventCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, err: ctx.Err(), idx: idx}
		}
		return
	}
	s.inflight[cacheKey] = make(chan agentTaskResult, 1)
	isOwner = true
	s.inflightMu.Unlock()

	// Check the task result cache before running. Sidecar tasks, summarized
	// tasks, and verbatim-output tasks always run fresh: a cached prose result
	// cannot satisfy a new runner-owned transcript contract.
	if !td.Sidecar && !td.Summarize && !taskUsesVerbatimTranscript(td) {
		if cached, ok := c.lookupTaskCacheWithTypedVerification(ctx, agentKey, desc, td.VerifySpec, td.Verify, td.VerifyMode); ok {
			c.report(c.newEvent("cache_hit").withAgent(td.Agent).withMessage(desc).withTodoID(tid))
			c.taskTracker.TodoList().UpdateStatusAndOutput(tid, TaskDone, utils.TruncateRunes(cached, summaryMaxRunes), cached)
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			result := agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: cached, idx: idx}
			s.inflightMu.Lock()
			s.inflight[cacheKey] <- result
			delete(s.inflight, cacheKey)
			s.inflightMu.Unlock()
			s.eventCh <- result
			return
		}
	}

	var output string
	var err error
	if td.Sidecar {
		output, err = c.executeSidecarTask(ctx, td, tid)
	} else {
		output, err = c.executeTask(ctx, td, tid)
	}
	if err == nil {
		c.storeTaskCacheWithTypedVerificationEvidence(agentKey, desc, td.VerifySpec, td.Verify, td.VerifyMode, output, verificationForTodo(c.taskTracker.TodoList().Items(), tid))
	}
	result := agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: output, err: err, idx: idx}
	if err == nil && td.PlanFirst && td.PlanID == "" {
		c.pendingPlansMu.Lock()
		if pe := c.pendingPlans[tid]; pe != nil && pe.Status == "submitted" {
			result.planText = pe.PlanText
		}
		c.pendingPlansMu.Unlock()
	}
	s.inflightMu.Lock()
	s.inflight[cacheKey] <- result
	delete(s.inflight, cacheKey)
	s.inflightMu.Unlock()
	s.eventCh <- result
}

// validateOnFailureTargets checks that every on_failure index is in range and
// points at the task itself or one of its transitive depends_on ancestors.
// This guarantees the failed task is inside the reset set when the loop
// triggers; a target outside its ancestry would re-run an unrelated task
// while the failed one stays stuck.
func validateOnFailureTargets(tasks []TaskDef) error {
	for i, t := range tasks {
		if t.OnFailure == nil {
			continue
		}
		target := *t.OnFailure
		if target < 0 || target >= len(tasks) {
			return fmt.Errorf("task %d: on_failure index %d is out of range (batch has %d tasks)", i, target, len(tasks))
		}
		if target == i {
			continue
		}
		// BFS up the depends_on edges from task i looking for target.
		visited := make(map[int]bool)
		queue := append([]int(nil), tasks[i].DependsOn...)
		found := false
		for len(queue) > 0 {
			curr := queue[0]
			queue = queue[1:]
			if curr < 0 || curr >= len(tasks) || visited[curr] {
				continue
			}
			visited[curr] = true
			if curr == target {
				found = true
				break
			}
			queue = append(queue, tasks[curr].DependsOn...)
		}
		if !found {
			return fmt.Errorf("task %d: on_failure target %d must be the task itself or one of its (transitive) depends_on ancestors", i, target)
		}
	}
	return nil
}

// detectTaskCycle returns true if the DependsOn indices form a cycle.
func detectTaskCycle(tasks []TaskDef) bool {
	n := len(tasks)
	state := make([]int, n) // 0=unvisited, 1=visiting, 2=done
	var dfs func(i int) bool
	dfs = func(i int) bool {
		if state[i] == 1 {
			return true
		}
		if state[i] == 2 {
			return false
		}
		state[i] = 1
		for _, dep := range tasks[i].DependsOn {
			// Check for self-loop (task depends on itself)
			if dep == i {
				return true
			}
			if dep >= 0 && dep < n && dfs(dep) {
				return true
			}
		}
		state[i] = 2
		return false
	}
	for i := range tasks {
		if state[i] == 0 && dfs(i) {
			return true
		}
	}
	return false
}
