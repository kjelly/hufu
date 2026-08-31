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
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
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
) (string, error) {
	taskDesc := task.Goal
	// Limit concurrent models
	models := agentDef.ExtraModels
	if len(models) > maxConcurrentModels-1 { // -1 for main model
		models = models[:maxConcurrentModels-1]
	}

	totalModels := len(models) + 1 // +1 for main model
	results := make(chan *agentResult, totalModels)

	// Create a deep copy of agentDef with ExtraModels cleared for the main model
	// to prevent infinite recursion and data races.
	mainDef := cloneAgentDef(agentDef)
	mainDef.ExtraModels = nil
	mainModel := mainDef.Generation.Model

	go func() {
		output, err := c.executeSingleAgentWithModel(parentCtx, agentName, mainDef, task, todoID, "main")
		results <- &agentResult{model: mainModel, output: output, err: err}
	}()

	// Execute each extra model with its own deep copy
	for index, extraModel := range models {
		go func(model, slot string) {
			extraDef := cloneAgentDef(agentDef)
			extraDef.ExtraModels = nil
			extraDef.Generation.Model = model
			output, err := c.executeSingleAgentWithModel(parentCtx, agentName, extraDef, task, todoID, slot)
			results <- &agentResult{model: model, output: output, err: err}
		}(extraModel, fmt.Sprintf("extra-%d", index+1))
	}

	// Collect all results (continue on error)
	var allResults []*agentResult
	for i := 0; i < totalModels; i++ {
		result := <-results
		allResults = append(allResults, result)
	}

	// Judge the candidates when a judge model is configured; otherwise (or
	// if the judge fails) fall back to the plain concatenation merge.
	judged, err := c.judgeAgentResults(parentCtx, taskDesc, todoID, allResults)
	if err == nil {
		return judged, nil
	}
	if !errors.Is(err, errNoJudgeModel) {
		c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("judge failed, falling back to merge: %v", err)).withTodoID(todoID))
	}
	merged := c.mergeAgentResults(allResults)
	return merged, nil
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
) (string, error) {
	task.Agent = agentDef.Name
	task.Model = agentDef.Generation.Model

	// Create isolated workspace for this model to prevent concurrent file conflicts.
	// Use unique token (timestamp + agentName) to prevent collision when multiple
	// agents use the same model simultaneously.
	token := fmt.Sprintf("%d-%d-%s", time.Now().UnixNano(), extraWSSeq.Add(1), sanitizeModel(agentName))
	subWS := filepath.Join(c.session.Workspace, "extra-models", sanitizeModel(agentDef.Generation.Model)+"-"+token)
	if err := os.MkdirAll(subWS, 0o755); err != nil {
		return "", fmt.Errorf("failed to create isolated workspace: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(subWS); rmErr != nil {
			log.Printf("warning: failed to remove isolated workspace %s: %v", subWS, rmErr)
		}
	}()

	// Copy shared/ directory from main workspace
	sharedSrc := filepath.Join(c.session.Workspace, "shared")
	sharedDst := filepath.Join(subWS, "shared")
	if _, err := os.Stat(sharedSrc); err == nil {
		if err := copyDir(sharedSrc, sharedDst); err != nil {
			log.Printf("warning: failed to copy shared dir to isolated workspace: %v", err)
		}
	}

	// Clone Coordinator with isolated session to prevent data race on c.session.Workspace.
	// Direct mutation of c.session.Workspace is unsafe because multiple goroutines
	// execute this function concurrently for different models.
	isolatedSession := cloneSession(c.session, subWS)
	isolatedCoord := cloneCoordinator(c, isolatedSession)
	isolationIdentity := strings.Join([]string{c.contextRunID(), todoID, agentName, agentDef.Generation.Model, slot}, "\x00")
	isolatedCoord.modelExecutionID = "model-execution-" + hashContentKey(isolationIdentity)

	result, err := isolatedCoord.executeTask(parentCtx, task, todoID)

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

	var taskResultCacheClone map[string][]cachedTaskEntry
	orig.taskResultCacheMu.RLock()
	if orig.taskResultCache != nil {
		taskResultCacheClone = make(map[string][]cachedTaskEntry, len(orig.taskResultCache))
		for k, v := range orig.taskResultCache {
			var sliceCopy []cachedTaskEntry
			if v != nil {
				sliceCopy = make([]cachedTaskEntry, len(v))
				copy(sliceCopy, v)
			}
			taskResultCacheClone[k] = sliceCopy
		}
	}
	orig.taskResultCacheMu.RUnlock()

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
			Entries:                     entriesCopy,
			CoordinatorContextManifests: append([]ContextInjectionManifest(nil), orig.sessionData.CoordinatorContextManifests...),
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

	return &Coordinator{
		session:                            newSession,
		providerManager:                    orig.providerManager,
		mcpManager:                         orig.mcpManager,
		coreTools:                          coreToolsClone,
		agentCache:                         agentCacheClone,
		agentToolNameCache:                 agentToolNameCacheClone,
		round:                              orig.round,
		verbose:                            orig.verbose,
		think:                              orig.think,
		reportStatus:                       orig.reportStatus,
		sessionData:                        sessionDataClone,
		taskTracker:                        orig.taskTracker,
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
		auditLogger:                        orig.auditLogger,
		sshSessionMgr:                      orig.sshSessionMgr,
		skillUsage:                         skillUsageClone,
		delegatedTasks:                     delegatedTasksClone,
		taskResultCache:                    taskResultCacheClone,
		capabilityCache:                    capabilityCacheClone,
		capabilityInflight:                 make(map[string]chan CapabilityResult),
		memoryStore:                        orig.memoryStore,
		contextRepo:                        orig.contextRepo,
		memoryRankingPolicy:                orig.memoryRankingPolicy,
		workerMemorySvc:                    orig.workerMemorySvc,
		sharedMemorySvc:                    orig.sharedMemorySvc,
		eventStore:                         orig.eventStore,
		executionRunID:                     orig.executionRunID,
		executionTeamRevision:              orig.executionTeamRevision,
		executionProfile:                   executionProfileCopy,
		goalMode:                           goalModeCopy,
		modelList:                          orig.modelList,
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
		contractWarnings:                   contractWarnings,
		noProgressUsageOwner:               usageOwner,
		noProgressUsageNamespace:           usageNamespace,
	}
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
