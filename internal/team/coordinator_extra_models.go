package team

// Extra-models execution: running the same task across several models in
// isolated workspace/coordinator clones and merging the outputs.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/skill"
)

// executeTaskWithExtraModels executes a task across multiple models when extra-models is configured.
// verify is the task's optional deliverable-verification command; it is propagated to each model so
// every model's output is verified (and retried) independently via executeTask.
func (c *Coordinator) executeTaskWithExtraModels(
	parentCtx context.Context,
	agentName string,
	agentDef *agent.AgentDef,
	task TaskDef,
	todoID string,
) (output string, returnErr error) {
	// A durable task owns this ordered topology. Resolve it before mutating the
	// parent lifecycle so state-changing fanout cannot start after an unsafe
	// mixed/external backend admission has already been accepted.
	taskDesc := task.Goal
	models := executionTopologySelectors(task)
	if len(models) == 0 {
		primary := strings.TrimSpace(task.Model)
		if primary == "" {
			primary = strings.TrimSpace(agentDef.Generation.Model)
		}
		models = initialTaskModelTopology(agentDef, primary)
	}
	if len(models) >= 2 {
		if err := c.validateExtraModelExecutionTopology(task); err != nil {
			return "", err
		}
	}
	item := c.todoItemByID(todoID)
	resumingCollection := hasCurrentFanoutCollection(item)
	var recovery []fanoutResumeLeaf
	defer func() {
		if returnErr == nil {
			c.releaseFanoutCollectionWorkspaces(parentCtx, todoID)
		}
	}()
	if resumingCollection {
		if err := c.ensureExecutionPolicySnapshot(); err != nil {
			return "", err
		}
		saved, receipts, err := c.readFanoutSettlement(parentCtx, item)
		if err != nil {
			return "", c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
		}
		if saved != nil {
			return c.commitRecoveredFanoutOutput(parentCtx, item, saved, receipts)
		}
		recovery, err = c.prepareFanoutRecovery(parentCtx, item, task, agentDef)
		if err != nil {
			return "", c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
		}
		for index, leaf := range recovery {
			if leaf.completion != nil {
				if err := c.publishFanoutLeafCompletion(parentCtx, item, index, leaf.completion); err != nil {
					return "", c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
				}
			}
		}
	}
	// Fresh fanout dispatches workers through the parent's provider proxy.
	// Recovery starts it only when a collector, skeptic or judge needs it.
	if !resumingCollection {
		if err := c.providerBoundaryRequired(parentCtx); err != nil {
			return "", fmt.Errorf("start fanout provider boundary: %w", err)
		}
	}
	// Fanout is a parent-owned lifecycle boundary. Open the single runtime
	// occurrence once before any leaf starts; isolated leaves below receive
	// private Todo state and therefore cannot race this transition or arm a
	// second decision for the same occurrence.
	if item != nil && !resumingCollection {
		expected := TaskPending
		if item.Status == TaskPlanned {
			expected = TaskPlanned
		}
		if err := c.CommitTaskTransition(parentCtx, todoID, expected, TaskInProgress, "", "", attemptStartMetadata(1)); err != nil {
			return "", fmt.Errorf("mark fanout task started: %w", err)
		}
		c.setCurrentTaskAttempt(todoID, c.taskAttempt(todoID))
		c.reconcileTaskStatusProjection()
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
	// Retries and resumes pass the recorded topology here and never re-expand
	// live configuration.
	if len(models) < 2 {
		return c.executeSingleAgentWithModel(parentCtx, agentName, cloneAgentDef(agentDef), task, todoID, "main", 0, nil)
	}
	// Pin the parent control plane before any leaf is created. The worker
	// workspace below is deliberately disposable and must never become the
	// source of decision artifacts, index rows, or branch visibility.
	if _, err := c.pinDecisionControlPlane(); err != nil {
		return "", err
	}
	// Limit concurrent models while preserving the creation-time order.
	if len(models) > maxConcurrentModels {
		models = models[:maxConcurrentModels]
	}

	totalModels := len(models)
	if recovery == nil {
		recovery = make([]fanoutResumeLeaf, totalModels)
	}
	type indexedResult struct {
		index  int
		result *agentResult
	}
	results := make(chan indexedResult, totalModels)

	// Create a deep copy of agentDef with ExtraModels cleared for the main model
	// to prevent infinite recursion and data races.
	mainDef := cloneAgentDef(agentDef)
	mainDef.ExtraModels = nil
	mainModel := models[0]
	mainDef.Generation.Model = mainModel

	go func() {
		output, err := c.executeSingleAgentWithModel(parentCtx, agentName, mainDef, task, todoID, "main", 0, &recovery[0])
		results <- indexedResult{0, &agentResult{model: mainModel, output: output, err: err}}
	}()

	// Execute each extra model with its own deep copy
	for index, extraModel := range models[1:] {
		go func(model, slot string, topologyIndex int) {
			extraDef := cloneAgentDef(agentDef)
			extraDef.ExtraModels = nil
			extraDef.Generation.Model = model
			output, err := c.executeSingleAgentWithModel(parentCtx, agentName, extraDef, task, todoID, slot, topologyIndex, &recovery[topologyIndex])
			results <- indexedResult{topologyIndex, &agentResult{model: model, output: output, err: err}}
		}(extraModel, fanoutSlotName(index+1), index+1)
	}

	// Collect all results (continue on error)
	allResults := make([]*agentResult, totalModels)
	for range totalModels {
		result := <-results
		allResults[result.index] = result.result
	}
	if err := c.settleFanoutLeafFailures(agentName, taskDesc, todoID, resumingCollection, allResults); err != nil {
		return "", err
	}

	if item := c.todoItemByID(todoID); item != nil && item.MediaBinding != nil && !c.nativeMediaAcceptedWork(item) {
		err := media.Reject(media.DurabilityRequired, "image fanout requires accepted terminal results from every launched leaf")
		c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(err, FailureSourceError), RetryNone, FailureProtocol)
		return "", err
	}

	item = c.todoItemByID(todoID)
	var slots []fanoutSettlementSlot
	if hasCurrentFanoutCollection(item) {
		pinned, _, err := c.fanoutSettlementSlots(parentCtx, item)
		if err != nil {
			return "", c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
		}
		if len(pinned) != len(allResults) {
			return "", c.settleNormalizedRecoveryRead(item, task, rejectLocalResult(RepairFailureSource, errResultCollectionStale))
		}
		slots = pinned
	}
	// Judge the candidates when a judge model is configured; otherwise (or
	// if the judge fails) fall back to the plain concatenation merge.
	output, err := c.judgeAgentResults(parentCtx, taskDesc, todoID, allResults)
	if err != nil {
		if !errors.Is(err, errNoJudgeModel) {
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("judge failed, falling back to merge: %v", err)).withTodoID(todoID))
		}
		output = c.mergeAgentResults(allResults)
	}
	if slots != nil {
		if err := c.sealFanoutSettlement(parentCtx, item, slots, output); err != nil {
			return "", c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
		}
	}
	return c.commitFanoutOutput(parentCtx, item, output)
}

func (c *Coordinator) settleFanoutLeafFailures(agentName, taskDesc, todoID string, resumingCollection bool, results []*agentResult) error {
	item := c.todoItemByID(todoID)
	var leafErrors []string
	var resumable error
	var stopped error
	collectionFailed := resumingCollection
	allRecoverable := true
	for _, leaf := range results {
		if leaf == nil || leaf.err == nil {
			continue
		}
		leafErrors = append(leafErrors, fmt.Sprintf("%s: %v", leaf.model, leaf.err))
		if _, ok := errors.AsType[*resultCollectionFailure](leaf.err); ok {
			collectionFailed = true
			if stopped == nil && localResultRejectionReason(leaf.err) != RepairFailureCommit {
				stopped = leaf.err
			}
		}
		if returnedCollectionCanResume(item, leaf.err) {
			resumable = leaf.err
		} else {
			allRecoverable = false
		}
	}
	if len(leafErrors) == 0 {
		return nil
	}
	err := fmt.Errorf("fanout leaf execution failed: %s", strings.Join(leafErrors, "; "))
	if allRecoverable && resumable != nil {
		return fmt.Errorf("%s: %w", err, resumable)
	}
	if stopped != nil {
		err = fmt.Errorf("%s: %w", err, stopped)
	}
	if collectionFailed {
		c.PersistFailureWithClassAndStatusAndOutput(agentName, taskDesc, todoID, c.FailureDetail(err, FailureSourceError), NeedsHuman, FailureProtocol, TaskBlocked, "")
	} else {
		c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(err, FailureSourceError), RetryNone, FailureExecution)
	}
	return err
}

// executionTopologySelectors preserves backend qualification after a durable
// occurrence is replayed. The legacy ModelTopology shadow intentionally keeps
// only model leaves for old callers, but fanout dispatch must use the ordered
// canonical topology so equal model names on different backends remain
// distinct.
func executionTopologySelectors(task TaskDef) []string {
	if len(task.ExecutionTopology) > 0 {
		return modelTopologyFromExecutionTargets(task.ExecutionTopology)
	}
	return cloneModelTopology(task.ModelTopology)
}

// Private leaves share the parent's canonical media journal, branch tree,
// invocation and snapshot projection; their workspaces contain worker files.
func (c *Coordinator) canonicalJournalOwner() *Coordinator {
	if c != nil && c.modelFanout != nil && c.modelFanout.parent != nil {
		return c.modelFanout.parent.canonicalJournalOwner()
	}
	return c
}

// cloneAgentDef creates a deep copy of an AgentDef to prevent data races
// when running multiple models concurrently.
func cloneAgentDef(def *agent.AgentDef) *agent.AgentDef {
	clone := *def
	if len(def.ExtraModels) > 0 {
		clone.ExtraModels = make([]string, len(def.ExtraModels))
		copy(clone.ExtraModels, def.ExtraModels)
	}
	if len(def.AllowedPaths) > 0 {
		clone.AllowedPaths = make([]string, len(def.AllowedPaths))
		copy(clone.AllowedPaths, def.AllowedPaths)
	}
	if len(def.Guard) > 0 {
		clone.Guard = make([]string, len(def.Guard))
		copy(clone.Guard, def.Guard)
	}
	if len(def.MCPTools) > 0 {
		clone.MCPTools = make(map[string]agent.MCPToolConfig, len(def.MCPTools))
		for k, v := range def.MCPTools {
			clone.MCPTools[k] = v
		}
	}
	return &clone
}

func admittedExecutionTargetForModel(task TaskDef, model string) (execution.ExecutionTarget, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return execution.ExecutionTarget{}, fmt.Errorf("extra-model leaf model is required")
	}
	if len(task.ModelTopology) != len(task.ExecutionTopology) {
		return execution.ExecutionTarget{}, fmt.Errorf("extra-model topology has %d model leaves but %d admitted execution targets", len(task.ModelTopology), len(task.ExecutionTopology))
	}
	for index, topologyModel := range task.ModelTopology {
		if strings.TrimSpace(topologyModel) != model {
			continue
		}
		target := task.ExecutionTopology[index]
		if err := target.Validate(); err != nil {
			return execution.ExecutionTarget{}, fmt.Errorf("extra-model leaf target %q is invalid: %w", target, err)
		}
		return target, nil
	}
	return execution.ExecutionTarget{}, fmt.Errorf("extra-model leaf %q is not present in admitted execution topology", model)
}

func admittedExecutionTargetAt(task TaskDef, index int) (execution.ExecutionTarget, error) {
	if index < 0 || index >= len(task.ExecutionTopology) {
		return execution.ExecutionTarget{}, fmt.Errorf("extra-model topology index %d is out of range", index)
	}
	target := task.ExecutionTopology[index]
	if err := target.Validate(); err != nil {
		return execution.ExecutionTarget{}, fmt.Errorf("extra-model leaf target %q is invalid: %w", target, err)
	}
	return target, nil
}

// validateExtraModelExecutionTopology is the fanout admission boundary for
// state-changing tasks. Hufu's local LLM leaves receive private workspaces,
// but an external agent backend has no declared isolated execution world. A
// state-changing topology that crosses backend kinds is therefore rejected
// before any leaf worker is launched; silently running such leaves would make
// their writes race in one execution world.
func (c *Coordinator) validateExtraModelExecutionTopology(task TaskDef) error {
	if len(task.ExecutionTopology) < 2 {
		return nil
	}
	effect := task.SideEffect
	if c != nil {
		effect = c.effectiveSideEffect(task)
	}
	if effect == "" || effect == SideEffectNone {
		return nil
	}
	if c == nil || c.ExecutionRegistry() == nil {
		return fmt.Errorf("state-changing extra-model fanout requires an execution registry")
	}
	backends := make(map[string]struct{}, len(task.ExecutionTopology))
	for _, target := range task.ExecutionTopology {
		if err := target.Validate(); err != nil {
			return fmt.Errorf("state-changing extra-model topology target %q is invalid: %w", target, err)
		}
		backend, err := c.ExecutionRegistry().ResolveBackend(target.Backend)
		if err != nil {
			return fmt.Errorf("state-changing extra-model topology backend %q is unavailable: %w", target.Backend, err)
		}
		if backend.Kind() == execution.BackendKindAgent {
			return fmt.Errorf("state-changing extra-model fanout cannot use external agent backend %q without an isolated execution world", target.Backend)
		}
		backends[execution.CanonicalTargetBackendName(target.Backend)] = struct{}{}
	}
	if len(backends) > 1 {
		names := make([]string, 0, len(backends))
		for name := range backends {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("state-changing extra-model fanout crosses execution backends %s without demonstrably isolated worlds", strings.Join(names, ", "))
	}
	return nil
}

// executeSingleAgentWithModel executes a single agent task with a pre-configured agentDef.
// The caller must ensure agentDef.ExtraModels is nil to prevent infinite recursion.
// Extra models execute in isolated Coordinator instances with cloned sessions
// to prevent data races on c.session.Workspace.
func (c *Coordinator) executeSingleAgentWithModel(
	parentCtx context.Context,
	agentName string,
	agentDef *agent.AgentDef,
	task TaskDef,
	todoID string,
	slot string,
	topologyIndex int,
	recovery *fanoutResumeLeaf,
) (string, error) {
	task.Agent = agentDef.Name
	task.Model = agentDef.Generation.Model
	// Each isolated leaf must execute on the exact admitted topology target
	// corresponding to its model. Never leave the parent primary target on a
	// leaf: mixed-backend fanout would otherwise silently route every leaf to
	// the first backend and its limiter.
	leafTarget, err := admittedExecutionTargetAt(task, topologyIndex)
	if err != nil && len(task.ExecutionTopology) == 0 {
		// Compatibility for ephemeral callers that predate canonical topology
		// admission. Durable occurrences always take the indexed path above.
		leafTarget, err = admittedExecutionTargetForModel(task, task.Model)
	}
	if err != nil {
		return "", err
	}
	parentItem := c.todoItemByID(todoID)
	if parentItem != nil {
		admitted, err := admittedExecutionTargetAt(taskDefFromTodoItem(parentItem), topologyIndex)
		if err != nil {
			return "", err
		}
		if admitted != leafTarget {
			return "", fmt.Errorf("extra-model leaf target differs from parent admitted topology")
		}
	}
	task.ResolvedExecutionTarget = leafTarget
	// Validate the parent policy before profile discovery or constructing a
	// disposable leaf. Admission is checked again at the actual provider edge.
	if err := c.ensureExecutionPolicySnapshot(); err != nil {
		return "", err
	}
	sealLeaf, err := c.fanoutLeafNeedsCompletion(agentDef, task, leafTarget)
	if err != nil {
		return "", err
	}

	var resumeCheckpoint *ResultCollectionCheckpoint
	var resumeWork *resultCollectionWork
	if recovery != nil {
		if completion := recovery.completion; completion != nil {
			return completion.Output, c.setAttemptReceipt(todoID, &completion.Receipt)
		}
		resumeCheckpoint, resumeWork = recovery.checkpoint, recovery.work
	}

	var subWS string
	if resumeWork != nil {
		subWS = resumeWork.Workspace
	} else {
		subWS, err = c.createFanoutWorkspace(agentName, agentDef.Generation.Model)
		if err != nil {
			return "", err
		}
	}
	keepCollectionWorkspace := resumeWork != nil
	defer func() {
		if keepCollectionWorkspace {
			return
		}
		if rmErr := os.RemoveAll(subWS); rmErr != nil {
			log.Printf("warning: failed to remove isolated workspace %s: %v", subWS, rmErr)
		}
	}()

	// Clone Coordinator with isolated session to prevent data race on c.session.Workspace.
	// Direct mutation of c.session.Workspace is unsafe because multiple goroutines
	// execute this function concurrently for different models.
	isolatedSession := cloneSession(c.session, subWS)
	// Keep the frozen parent definitions. The runtime-owned target binding and
	// executionModelOverride make the leaf singleton without changing policy.
	isolatedCoord := cloneCoordinator(c, isolatedSession)
	// A leaf performs worker work only. Give it a private lifecycle projection
	// while keeping the shared journal for admission; the parent owns the one
	// durable task_created/status/decision lineage. Execution events and the
	// leaf receipt are still produced and merged into the parent below.
	if parentItem != nil {
		leafItem := cloneTodoItem(parentItem)
		leafItem.Status = TaskPending
		leafItem.Detail = ""
		leafItem.Output = ""
		leafItem.TypedResult = nil
		leafItem.ProviderBinding = nil
		leafItem.BackendBinding = nil
		leafItem.ExecutionReceipt = nil
		leafItem.ExecutionReceipts = nil
		if resumeCheckpoint != nil {
			leafItem.Status = TaskInProgress
			leafItem.OccurrenceRevision, leafItem.DispatchID = resumeCheckpoint.SubmissionRevision, resumeCheckpoint.SubmissionDispatchID
			receipt := cloneExecutionReceipt(collectionReceipt(parentItem, resumeCheckpoint))
			leafItem.ExecutionReceipt = &receipt
			leafItem.ExecutionReceipts = []ExecutionReceipt{receipt}
		}
		leafTracker := NewTaskTracker()
		leafTracker.TodoList().SetRunID(c.contextRunID())
		leafTracker.TodoList().AddReserved([]*TodoItem{leafItem})
		isolatedCoord.taskTracker = leafTracker
		isolatedCoord.modelFanout = &modelFanoutBinding{parent: c, taskID: todoID, index: topologyIndex, target: leafTarget, sealExternalResult: sealLeaf}
	}
	task.executionModelOverride = strings.TrimSpace(agentDef.Generation.Model)
	// Every isolated leaf is explicitly singleton. The parent Todo retains the
	// full topology because all leaves share its lifecycle record.
	task.ModelTopology = []string{task.Model}
	task.ExecutionTopology = []execution.ExecutionTarget{leafTarget}
	parentRetries := 0
	if parentItem != nil {
		parentRetries = parentItem.Retries
	}
	isolatedCoord.modelExecutionID = modelFanoutExecutionID(c.contextRunID(), todoID, agentName, agentDef.Generation.Model, slot, parentRetries)
	if resumeCheckpoint != nil {
		isolatedCoord.modelExecutionID = resumeCheckpoint.ModelExecutionID
	}

	leafCtx := context.WithValue(parentCtx, leafExecutionKey{}, true)
	result, err := isolatedCoord.executeTask(leafCtx, task, todoID)
	if leafItem := isolatedCoord.todoItemByID(todoID); leafItem != nil && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		if currentResultCollection(leafItem) != nil || returnedCollectionCanResume(leafItem, err) {
			keepCollectionWorkspace = true
		}
		if mergeErr := c.mergeFanoutLeafReceipts(parentCtx, isolatedCoord, leafItem, topologyIndex, result, sealLeaf, err == nil); err == nil {
			err = mergeErr
		}
	}

	// Merge skill usage statistics from the isolated coordinator back into the main coordinator
	c.skillUsageMu.Lock()
	isolatedCoord.skillUsageMu.Lock()
	for name, state := range isolatedCoord.skillUsage {
		if mainState, ok := c.skillUsage[name]; ok {
			mainState.Count += state.Count
			if mainState.Agents == nil {
				mainState.Agents = make(map[string]bool)
			}
			for agent := range state.Agents {
				mainState.Agents[agent] = true
			}
		} else {
			var agentsCopy map[string]bool
			if state.Agents != nil {
				agentsCopy = make(map[string]bool, len(state.Agents))
				for ak, av := range state.Agents {
					agentsCopy[ak] = av
				}
			}
			c.skillUsage[name] = &skillUsageState{
				Name:   state.Name,
				Count:  state.Count,
				Agents: agentsCopy,
			}
		}
	}
	isolatedCoord.skillUsageMu.Unlock()
	c.skillUsageMu.Unlock()

	return result, err
}

func (c *Coordinator) mergeFanoutLeafReceipts(ctx context.Context, leaf *Coordinator, leafItem *TodoItem, index int, output string, seal, terminal bool) error {
	var mergeError error
	var completed bool
	for i := range leafItem.ExecutionReceipts {
		receipt := leafItem.ExecutionReceipts[i]
		completedAttempt := terminal && leafItem.Status == TaskDone && leafItem.ExecutionReceipt != nil &&
			receipt.ModelExecutionID == leaf.modelExecutionID && receipt.Attempt == leafItem.ExecutionReceipt.Attempt
		if completedAttempt {
			receipt.AcceptedResult = cloneTaskResult(leaf.GetTaskResult(leafItem.ID))
			if cp := receipt.ResultCollection; cp != nil && cp.FanoutLeaf && cp.State == ResultCollectionCommitted {
				seal = true
				receipt.VerifyResult = cloneVerificationResult(leafItem.VerifyResult)
			}
			completed = true
		}
		if err := c.setAttemptReceipt(leafItem.ID, &receipt); err != nil {
			if mergeError == nil {
				mergeError = fmt.Errorf("merge %s leaf receipt: %w", fanoutSlotName(index), err)
			}
			continue
		}
		if completedAttempt && seal {
			parent := c.todoItemByID(leafItem.ID)
			projected := slices.IndexFunc(parent.ExecutionReceipts, func(candidate ExecutionReceipt) bool {
				return sameExecutionReceiptProjection(candidate, receipt)
			})
			if projected < 0 {
				return &resultCollectionFailure{Reason: RepairFailureSource, Cause: errResultCollectionStale}
			}
			if err := c.sealFanoutLeafCompletion(ctx, parent, index, output, parent.ExecutionReceipts[projected]); err != nil {
				return &resultCollectionFailure{Reason: localResultRejectionReason(err), Cause: err}
			}
		}
	}
	if seal && terminal && !completed {
		return &resultCollectionFailure{Reason: RepairFailureSource, Cause: errResultCollectionStale}
	}
	return mergeError
}

func (c *Coordinator) createFanoutWorkspace(agentName, model string) (string, error) {
	token := fmt.Sprintf("%d-%d-%s", time.Now().UnixNano(), extraWSSeq.Add(1), sanitizeModel(agentName))
	workspace := filepath.Join(c.session.Workspace, "extra-models", sanitizeModel(model)+"-"+token)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return "", fmt.Errorf("failed to create isolated workspace: %w", err)
	}
	shared := filepath.Join(c.session.Workspace, "shared")
	if _, err := os.Stat(shared); err == nil {
		if err := copyDir(shared, filepath.Join(workspace, "shared")); err != nil {
			log.Printf("warning: failed to copy shared dir to isolated workspace: %v", err)
		}
	}
	return workspace, nil
}

// cloneCoordinator creates a Coordinator copy sharing all references except session.
// Mutex/atomic fields are zero-initialized (each clone gets independent locks).
// This avoids the go vet "copies lock value" warning and prevents data races
// on lock state while keeping all shared resources accessible.
func cloneCoordinator(orig *Coordinator, newSession *TeamSession) *Coordinator {
	// Deep copy/clone all shared mutable maps and slices under their original locks
	var agentCacheClone map[string]fantasy.Agent
	var agentToolNameCacheClone map[string][]string
	orig.agentCacheMu.RLock()
	if orig.agentCache != nil {
		agentCacheClone = make(map[string]fantasy.Agent, len(orig.agentCache))
		for k, v := range orig.agentCache {
			agentCacheClone[k] = v
		}
	}
	if orig.agentToolNameCache != nil {
		agentToolNameCacheClone = make(map[string][]string, len(orig.agentToolNameCache))
		for k, names := range orig.agentToolNameCache {
			agentToolNameCacheClone[k] = append([]string(nil), names...)
		}
	}
	orig.agentCacheMu.RUnlock()

	var conversationHistoryClone []fantasy.Message
	orig.conversationHistoryMu.Lock()
	if orig.conversationHistory != nil {
		conversationHistoryClone = make([]fantasy.Message, len(orig.conversationHistory))
		copy(conversationHistoryClone, orig.conversationHistory)
	}
	var conversationHistorySourceCountsClone []int
	if orig.conversationHistorySourceCounts != nil {
		conversationHistorySourceCountsClone = make([]int, len(orig.conversationHistorySourceCounts))
		copy(conversationHistorySourceCountsClone, orig.conversationHistorySourceCounts)
	}
	conversationHistorySourceRangesClone := cloneSourceRanges(orig.conversationHistorySourceRanges)
	nextSourceIndex := orig.conversationHistoryNextSourceIndex
	orig.conversationHistoryMu.Unlock()

	var skillsClone []*skill.SkillDef
	orig.skillsMu.RLock()
	if orig.skills != nil {
		skillsClone = make([]*skill.SkillDef, len(orig.skills))
		copy(skillsClone, orig.skills)
	}
	orig.skillsMu.RUnlock()

	var autoLoadedSkillsClone []*skill.SkillDef
	orig.autoLoadedSkillsMu.RLock()
	if orig.autoLoadedSkills != nil {
		autoLoadedSkillsClone = make([]*skill.SkillDef, len(orig.autoLoadedSkills))
		copy(autoLoadedSkillsClone, orig.autoLoadedSkills)
	}
	orig.autoLoadedSkillsMu.RUnlock()

	var skillUsageClone map[string]*skillUsageState
	orig.skillUsageMu.Lock()
	if orig.skillUsage != nil {
		skillUsageClone = make(map[string]*skillUsageState, len(orig.skillUsage))
		for k, v := range orig.skillUsage {
			var agentsCopy map[string]bool
			if v.Agents != nil {
				agentsCopy = make(map[string]bool, len(v.Agents))
				for ak, av := range v.Agents {
					agentsCopy[ak] = av
				}
			}
			skillUsageClone[k] = &skillUsageState{
				Name:   v.Name,
				Count:  v.Count,
				Agents: agentsCopy,
			}
		}
	}
	orig.skillUsageMu.Unlock()

	var delegatedTasksClone map[string]int
	orig.delegatedTasksMu.Lock()
	if orig.delegatedTasks != nil {
		delegatedTasksClone = make(map[string]int, len(orig.delegatedTasks))
		for k, v := range orig.delegatedTasks {
			delegatedTasksClone[k] = v
		}
	}
	orig.delegatedTasksMu.Unlock()

	var capabilityCacheClone map[string]CapabilityResult
	orig.capabilityCacheMu.Lock()
	if orig.capabilityCache != nil {
		capabilityCacheClone = make(map[string]CapabilityResult, len(orig.capabilityCache))
		for k, v := range orig.capabilityCache {
			capabilityCacheClone[k] = v
		}
	}
	orig.capabilityCacheMu.Unlock()

	var forcedSkillNamesClone map[string]bool
	if orig.forcedSkillNames != nil {
		forcedSkillNamesClone = make(map[string]bool, len(orig.forcedSkillNames))
		for k, v := range orig.forcedSkillNames {
			forcedSkillNamesClone[k] = v
		}
	}

	var pendingPlansClone map[string]*PlanEntry
	var approvedOutputsClone map[string]string
	var approvedErrorsClone map[string]error
	orig.pendingPlansMu.Lock()
	if orig.pendingPlans != nil {
		pendingPlansClone = make(map[string]*PlanEntry, len(orig.pendingPlans))
		for k, v := range orig.pendingPlans {
			pendingPlansClone[k] = &PlanEntry{
				TodoID:      v.TodoID,
				Agent:       v.Agent,
				Goal:        v.Goal,
				PlanText:    v.PlanText,
				Status:      v.Status,
				ReviewCount: v.ReviewCount,
				Task:        cloneTaskDef(v.Task),
			}
		}
	}
	if orig.approvedOutputs != nil {
		approvedOutputsClone = make(map[string]string, len(orig.approvedOutputs))
		for k, v := range orig.approvedOutputs {
			approvedOutputsClone[k] = v
		}
	}
	if orig.approvedErrors != nil {
		approvedErrorsClone = make(map[string]error, len(orig.approvedErrors))
		for k, v := range orig.approvedErrors {
			approvedErrorsClone[k] = v
		}
	}
	orig.pendingPlansMu.Unlock()

	var sessionToolPermissionsClone map[string]bool
	orig.sessionToolPermissionsMu.RLock()
	if orig.sessionToolPermissions != nil {
		sessionToolPermissionsClone = make(map[string]bool, len(orig.sessionToolPermissions))
		for k, v := range orig.sessionToolPermissions {
			sessionToolPermissionsClone[k] = v
		}
	}
	orig.sessionToolPermissionsMu.RUnlock()

	var workerSummariesClone map[string]string
	orig.workerSummariesMu.Lock()
	if orig.workerSummaries != nil {
		workerSummariesClone = make(map[string]string, len(orig.workerSummaries))
		for k, v := range orig.workerSummaries {
			workerSummariesClone[k] = v
		}
	}
	orig.workerSummariesMu.Unlock()

	orig.executionProfileMu.RLock()
	executionProfileCopy := orig.executionProfile
	orig.executionProfileMu.RUnlock()

	orig.goalModeMu.RLock()
	goalModeCopy := orig.goalMode
	orig.goalModeMu.RUnlock()

	orig.compactionMu.Lock()
	compactionStateClone := cloneCompactionState(orig.compactionState)
	compactionBranchIDCopy := orig.compactionBranchID
	compactionRecoveryErrCopy := orig.compactionRecoveryErr
	lastCompactionSummaryCopy := cloneStructuredSummary(orig.lastCompactionSummary)
	orig.compactionMu.Unlock()

	orig.sidecarInitMu.Lock()
	sidecarInitCopy := orig.sidecarInit
	sidecarInstCopy := orig.sidecarInst
	orig.sidecarInitMu.Unlock()

	orig.guardInitMu.Lock()
	guardInitCopy := orig.guardInit
	guardInstCopy := orig.guardInst
	orig.guardInitMu.Unlock()

	orig.judgeInitMu.Lock()
	judgeInitCopy := orig.judgeInit
	judgeInstCopy := orig.judgeInst
	orig.judgeInitMu.Unlock()

	var sessionDataClone *SessionData
	if orig.sessionData != nil {
		var entriesCopy []SessionEntry
		if orig.sessionData.Entries != nil {
			entriesCopy = make([]SessionEntry, len(orig.sessionData.Entries))
			copy(entriesCopy, orig.sessionData.Entries)
		}
		sessionDataClone = &SessionData{
			CreatedAt:                   orig.sessionData.CreatedAt,
			UpdatedAt:                   orig.sessionData.UpdatedAt,
			Rounds:                      orig.sessionData.Rounds,
			ExecutionPolicySnapshot:     cloneExecutionPolicySnapshot(orig.sessionData.ExecutionPolicySnapshot),
			Entries:                     entriesCopy,
			CoordinatorContextManifests: cloneContextInjectionManifests(orig.sessionData.CoordinatorContextManifests),
			DecisionPrimitiveResults:    slices.Clone(orig.sessionData.DecisionPrimitiveResults),
		}
	}

	var coreToolsClone []fantasy.AgentTool
	if orig.coreTools != nil {
		coreToolsClone = make([]fantasy.AgentTool, len(orig.coreTools))
		copy(coreToolsClone, orig.coreTools)
	}

	// Share the contract-warning dedup set so the isolated coordinator does
	// not re-emit contract_warning events for the same todoID that the
	// parent already emitted (reviewer P2 — extra-models duplicate).
	// Thread-safe via contractWarningsOnce: concurrent extra-model clones all
	// resolve the same dedup pointer (never reassigning a non-nil set).
	contractWarnings := orig.contractWarningsDedup()
	providerSemState := orig.sharedProviderSemaphoreState()
	backendSemState := orig.sharedBackendSemaphoreState()

	// Isolated extra-model coordinators must report their LLM usage to the
	// parent run's no-progress budget. Keep a distinct namespace per clone so
	// cumulative receipt accounting deduplicates retries within one clone while
	// still counting the same task executed by different extra models.
	usageOwner := orig
	usageNamespace := fmt.Sprintf("extra-clone-%d", extraWSSeq.Add(1))
	if orig.noProgressUsageOwner != nil {
		usageOwner = orig.noProgressUsageOwner
		if orig.noProgressUsageNamespace != "" {
			usageNamespace = orig.noProgressUsageNamespace + ":" + usageNamespace
		}
	}

	//nolint:staticcheck,SA5011
	orig.stepConfirmFnMu.RLock()
	stepConfirmFnCopy := orig.stepConfirmFn
	orig.stepConfirmFnMu.RUnlock()
	//nolint:staticcheck,SA5011

	clone := &Coordinator{
		session:                            newSession,
		providerManager:                    orig.providerManager,
		mcpManager:                         orig.mcpManager,
		coreTools:                          coreToolsClone,
		historicalTaskReader:               orig.HistoricalTaskReader(),
		ollamaWebKeyPresent:                orig.ollamaWebKeyPresent,
		secretRegistry:                     orig.secretRegistry,
		decisionPrimitives:                 orig.decisionPrimitives,
		decisionPrimitiveGate:              orig.decisionPrimitiveGate,
		controlDecisions:                   orig.controlDecisions,
		agentCache:                         agentCacheClone,
		agentToolNameCache:                 agentToolNameCacheClone,
		round:                              orig.round,
		verbose:                            orig.verbose,
		think:                              orig.think,
		reportStatus:                       orig.reportStatus,
		sessionData:                        sessionDataClone,
		taskTracker:                        orig.taskTracker,
		restoredTodoIDs:                    orig.restoredTodoIDsSnapshot(),
		admittedTodoIDs:                    orig.admittedTodoIDsSnapshot(),
		skills:                             skillsClone,
		conversationHistory:                conversationHistoryClone,
		conversationHistorySourceCounts:    conversationHistorySourceCountsClone,
		conversationHistorySourceRanges:    conversationHistorySourceRangesClone,
		conversationHistorySourceOffset:    orig.conversationHistorySourceOffset,
		conversationHistoryNextSourceIndex: nextSourceIndex,
		lastCompactionSummary:              lastCompactionSummaryCopy,
		compactionState:                    compactionStateClone,
		compactionBranchID:                 compactionBranchIDCopy,
		compactionRecoveryErr:              compactionRecoveryErrCopy,
		initialPrompt:                      orig.initialPrompt,
		projectDir:                         orig.projectDir,
		allowedPaths:                       append([]string(nil), orig.allowedPaths...),
		artifactStoreRoot:                  orig.artifactStoreRootPath(),
		auditLogger:                        orig.auditLogger,
		sshSessionMgr:                      orig.sshSessionMgr,
		skillUsage:                         skillUsageClone,
		delegatedTasks:                     delegatedTasksClone,
		capabilityCache:                    capabilityCacheClone,
		capabilityInflight:                 make(map[string]chan CapabilityResult),
		contextRepo:                        orig.contextRepo,
		memoryRankingPolicy:                orig.memoryRankingPolicy,
		workerMemorySvc:                    orig.workerMemorySvc,
		semanticRuntime:                    orig.semanticRuntime,
		semanticSettings:                   orig.semanticSettings,
		semanticTraceHasher:                orig.semanticTraceHasher,
		semanticTraceSink:                  orig.semanticTraceSink,
		sharedMemorySvc:                    orig.sharedMemorySvc,
		eventStore:                         orig.eventStore,
		eventJournal:                       orig.EventJournal(),
		decisionControlPlane:               orig.decisionControlPlaneSnapshot(),
		decisionStore:                      orig.decisionStoreSnapshot(),
		decisionIndexProjection:            orig.decisionIndexProjectionSnapshot(),
		executionRunID:                     orig.executionRunID,
		executionProfile:                   executionProfileCopy,
		goalMode:                           goalModeCopy,
		modelList:                          orig.modelList,
		modelProfileRuntime:                orig.modelProfileRuntime,
		sidecarModel:                       orig.sidecarModel,
		sidecarInst:                        sidecarInstCopy,
		sidecarInit:                        sidecarInitCopy,
		guardModel:                         orig.guardModel,
		guardInst:                          guardInstCopy,
		guardInit:                          guardInitCopy,
		judgeModel:                         orig.judgeModel,
		judgeInst:                          judgeInstCopy,
		judgeInit:                          judgeInitCopy,
		planReviewerModel:                  orig.planReviewerModel,
		cachedWorkerContext:                orig.cachedWorkerContext,
		autoLoadedSkills:                   autoLoadedSkillsClone,
		forcedSkillNames:                   forcedSkillNamesClone,
		maxConcurrent:                      orig.maxConcurrent,
		executionPolicy:                    orig.executionPolicy,
		costManager:                        orig.costManager,
		providerSemState:                   providerSemState,
		backendSemState:                    backendSemState,
		sessionTime:                        orig.sessionTime,
		tokenBudgetOwner:                   orig.tokenBudgetRoot(),
		skillDetector:                      orig.skillDetector,
		skillGenerator:                     orig.skillGenerator,
		skillPatternsDetected:              orig.skillPatternsDetected,
		hooks:                              orig.hooks,
		rbashMode:                          orig.rbashMode,
		restrictedPath:                     orig.restrictedPath,
		noNet:                              orig.noNet,
		forceMCP:                           orig.forceMCP,
		pendingPlans:                       pendingPlansClone,
		approvedOutputs:                    approvedOutputsClone,
		approvedErrors:                     approvedErrorsClone,
		forcePlanFirst:                     orig.forcePlanFirst,
		autoSkillsEnabled:                  orig.autoSkillsEnabled,
		sessionToolPermissions:             sessionToolPermissionsClone,
		workerSummaries:                    workerSummariesClone,
		stepConfirmFn:                      stepConfirmFnCopy,
		workerAgentOverride:                orig.workerAgentOverride,
		repairAgentOverride:                orig.repairAgentOverride,
		contractWarnings:                   contractWarnings,
		noProgressUsageOwner:               usageOwner,
		noProgressUsageNamespace:           usageNamespace,
	}
	clone.sessionResume.Store(orig.sessionResume.Load())
	cacheDeps := taskCacheDependenciesFor(clone)
	cacheDeps.AppendJournal = nil
	clone.taskCache = orig.TaskCache().Fork(cacheDeps)
	return clone
}

// sanitizeModel removes characters unsafe for file paths from a model name.
func sanitizeModel(model string) string {
	s := strings.ReplaceAll(model, "/", "-")
	s = safeNameRegex.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "default"
	}
	return s
}

// copyDir recursively copies a directory tree, preserving file permissions and symlinks.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// Get full file info to preserve permissions
		info, err := entry.Info()
		if err != nil {
			return err
		}

		// Handle symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			if err := os.Symlink(linkTarget, dstPath); err != nil {
				return err
			}
			continue
		}

		// Handle directories
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}

		// Handle regular files with original permissions
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dstPath, data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// mergeAgentResults concatenates outputs from multiple models
func (c *Coordinator) mergeAgentResults(results []*agentResult) string {
	var b strings.Builder

	b.WriteString("## Multi-Model Response Integration\n\n")

	for _, r := range results {
		modelLabel := r.model
		if modelLabel == "" {
			modelLabel = "main"
		}

		fmt.Fprintf(&b, "### Model: %s\n\n", modelLabel)

		if r.err != nil {
			fmt.Fprintf(&b, "*Error: %v*\n\n", r.err)
		} else {
			b.WriteString(r.output)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("---\n")

	return b.String()
}
