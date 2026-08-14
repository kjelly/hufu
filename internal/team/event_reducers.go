package team

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// ReduceToSessionData replays a sequence of RunEvents to reconstruct a SessionData projection.
func ReduceToSessionData(events []RunEvent) *SessionData {
	events = normalizeReplayEvents(events)
	session := NewSession()
	for _, e := range events {
		switch e.Type {
		case "run_started":
			if session.CreatedAt == "" && e.Timestamp != "" {
				session.CreatedAt = e.Timestamp
			}
		case "user_message_added", "assistant_message_added":
			var payload struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Content != "" {
				role := payload.Role
				if role == "" {
					if strings.HasPrefix(e.Type, "user") {
						role = "user"
					} else {
						role = "assistant"
					}
				}
				session.AddEntry(role, payload.Content)
			}
		case "criterion_re_evaluated":
			var payload struct {
				After        []CriterionResult `json:"after"`
				Progress     TaskProgress      `json:"progress"`
				ProgressedAt string            `json:"progressed_at"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && len(payload.After) > 0 {
				session.CriterionResults = payload.After
				if payload.Progress == ProgressAdvanced {
					if payload.ProgressedAt != "" {
						session.LastCriterionProgressAt = payload.ProgressedAt
					} else if e.Timestamp != "" {
						// Older events did not include an explicit timestamp. The
						// event time remains a deterministic approximation only for
						// a recorded criterion advance.
						session.LastCriterionProgressAt = e.Timestamp
					}
				}
			}
		case "criterion_checkpoint_saved":
			var payload struct {
				Checkpoint CriterionCheckpoint `json:"checkpoint"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Checkpoint.CriterionID != "" {
				filtered := session.CriterionCheckpoints[:0]
				for _, checkpoint := range session.CriterionCheckpoints {
					if checkpoint.CriterionID != payload.Checkpoint.CriterionID {
						filtered = append(filtered, checkpoint)
					}
				}
				session.CriterionCheckpoints = append(filtered, payload.Checkpoint)
			}
		case "run_finished":
			var payload struct {
				Outcome          RunOutcome        `json:"outcome"`
				GoalSatisfied    bool              `json:"goal_satisfied"`
				Acceptance       *AcceptanceResult `json:"acceptance"`
				Stats            RunStats          `json:"stats"`
				Metrics          RunMetrics        `json:"metrics"`
				EvidenceManifest *EvidenceManifest `json:"evidence_manifest"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Outcome != "" {
				session.RunResult = &RunResult{
					Outcome: payload.Outcome, GoalSatisfied: payload.GoalSatisfied,
					Acceptance: payload.Acceptance, Stats: payload.Stats,
					Metrics: payload.Metrics, EvidenceManifest: payload.EvidenceManifest,
				}
			}
		case "diagnostic_packet":
			var payload struct {
				Packet DiagnosticPacket `json:"packet"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Packet.ID != "" {
				payload.Packet = normalizeDiagnosticPacket(payload.Packet)
				seen := false
				for _, existing := range session.DiagnosticPackets {
					if existing.ID == payload.Packet.ID {
						seen = true
						break
					}
				}
				if !seen {
					session.DiagnosticPackets = append(session.DiagnosticPackets, payload.Packet)
				}
			}
		case "plan_revision":
			var payload struct {
				Revision PlanRevision `json:"revision"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Revision.ID != "" {
				seen := false
				for _, existing := range session.PlanRevisions {
					if existing.ID == payload.Revision.ID {
						seen = true
						break
					}
				}
				if !seen {
					session.PlanRevisions = append(session.PlanRevisions, payload.Revision)
				}
			}
		case "plan_review":
			var payload struct {
				Review PlanReviewResult `json:"review"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Review.RevisionID != "" {
				updated := false
				for i := range session.PlanReviews {
					if session.PlanReviews[i].RevisionID == payload.Review.RevisionID {
						session.PlanReviews[i] = payload.Review
						updated = true
						break
					}
				}
				if !updated {
					session.PlanReviews = append(session.PlanReviews, payload.Review)
				}
			}
		}
	}
	session.Tasks = ReduceToTodoList(events)
	if len(session.Tasks) > 0 {
		session.DelegationPhase = DelegationPhaseActive
	} else {
		session.DelegationPhase = DelegationPhaseInitialPending
	}
	return session
}

// ReduceToTodoList replays a sequence of RunEvents to reconstruct a TodoItem slice projection.
func ReduceToTodoList(events []RunEvent) []*TodoItem {
	events = normalizeReplayEvents(events)
	taskMap := make(map[string]*TodoItem)
	var taskOrder []string

	for _, e := range events {
		if e.Type == "criterion_re_evaluated" && e.TaskID != "" {
			var payload struct {
				Progress         TaskProgress `json:"progress"`
				ProgressCriteria []string     `json:"progress_criteria"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Progress != "" {
				if item := taskMap[e.TaskID]; item != nil {
					item.Progress = payload.Progress
					item.ProgressCriteria = append([]string(nil), payload.ProgressCriteria...)
				}
			}
			continue
		}
		if e.Type == "failure_fingerprint" {
			var payload struct {
				Fingerprint FailureFingerprint `json:"fingerprint"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Fingerprint.Digest != "" && e.TaskID != "" {
				item, exists := taskMap[e.TaskID]
				if !exists {
					item = &TodoItem{ID: e.TaskID, Status: TaskError, Agent: e.Actor}
					taskMap[e.TaskID] = item
					taskOrder = append(taskOrder, e.TaskID)
				}
				seen := false
				for i := range item.FailureFingerprints {
					existing := &item.FailureFingerprints[i]
					if existing.Digest == payload.Fingerprint.Digest {
						if existing.Occurrences < 1 {
							existing.Occurrences = 1
						}
						occurrences := payload.Fingerprint.Occurrences
						if occurrences < 1 {
							occurrences = 1
						}
						if occurrences > existing.Occurrences {
							existing.Occurrences = occurrences
						}
						seen = true
						break
					}
				}
				if !seen {
					if payload.Fingerprint.Occurrences < 1 {
						payload.Fingerprint.Occurrences = 1
					}
					item.FailureFingerprints = append(item.FailureFingerprints, payload.Fingerprint)
				}
			}
			continue
		}
		if e.Type == "artifact_created" {
			var payload struct {
				Artifact    ArtifactRef `json:"artifact"`
				Path        string      `json:"path"`
				Description string      `json:"description"`
				TaskID      string      `json:"task_id"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				continue
			}
			taskID := e.TaskID
			if taskID == "" {
				taskID = payload.TaskID
			}
			if taskID == "" {
				continue
			}
			item := taskMap[taskID]
			if item == nil {
				item = &TodoItem{ID: taskID, Status: TaskPending}
				taskMap[taskID] = item
				taskOrder = append(taskOrder, taskID)
			}
			artifact := payload.Artifact
			if artifact.Path == "" {
				artifact.Path, artifact.Description = payload.Path, payload.Description
			}
			if item.TypedResult == nil {
				item.TypedResult = &TaskResult{TaskID: taskID}
			}
			duplicate := false
			for _, existing := range item.TypedResult.Artifacts {
				if (artifact.ID != "" && existing.ID == artifact.ID) || (artifact.ID == "" && existing.Path == artifact.Path) {
					duplicate = true
					break
				}
			}
			if !duplicate && artifact.Path != "" {
				item.TypedResult.Artifacts = append(item.TypedResult.Artifacts, artifact)
			}
			continue
		}
		if e.Type == "task_removed" {
			taskID := e.TaskID
			if taskID == "" {
				var payload struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(e.Payload, &payload)
				taskID = payload.ID
			}
			if taskID != "" {
				if _, exists := taskMap[taskID]; exists {
					delete(taskMap, taskID)
					for i, id := range taskOrder {
						if id == taskID {
							taskOrder = append(taskOrder[:i], taskOrder[i+1:]...)
							break
						}
					}
				}
			}
			continue
		}
		if !strings.HasPrefix(e.Type, "task_") {
			continue
		}

		var payload struct {
			ID                  string                    `json:"id"`
			Phase               Phase                     `json:"phase"`
			Action              *Action                   `json:"action"`
			PlanTaskID          string                    `json:"plan_task_id"`
			ContractID          string                    `json:"contract_id"`
			ContractHash        string                    `json:"contract_hash"`
			ContractRevision    int                       `json:"contract_revision"`
			Description         string                    `json:"description"`
			Desc                string                    `json:"desc"`
			Status              string                    `json:"status"`
			Detail              string                    `json:"detail"`
			MaxRetries          int                       `json:"max_retries"`
			Retries             int                       `json:"retries"`
			Output              string                    `json:"output"`
			Summary             string                    `json:"summary"`
			Agent               string                    `json:"agent"`
			Model               string                    `json:"model"`
			Skills              []string                  `json:"skills"`
			InjectedSkills      []string                  `json:"injected_skills"`
			LoadedSkills        []string                  `json:"loaded_skills"`
			Source              string                    `json:"source"`
			ParentID            string                    `json:"parent_id"`
			DependsOn           []string                  `json:"depends_on"`
			OnFailure           string                    `json:"on_failure"`
			Verify              string                    `json:"verify"`
			VerifyMode          string                    `json:"verify_mode"`
			VerifySpec          *VerificationSpec         `json:"verify_spec"`
			VerifyResult        *VerificationResult       `json:"verify_result"`
			TypedResult         *TaskResult               `json:"typed_result"`
			ExecutionReceipt    *ExecutionReceipt         `json:"execution_receipt"`
			ExecutionReceipts   []ExecutionReceipt        `json:"execution_receipts"`
			Kind                TaskKind                  `json:"kind"`
			Advances            []string                  `json:"advances"`
			ExpectedStateChange string                    `json:"expected_state_change"`
			Progress            TaskProgress              `json:"progress"`
			ProgressCriteria    []string                  `json:"progress_criteria"`
			FailureFingerprints []FailureFingerprint      `json:"failure_fingerprints"`
			Execution           ExecutionContract         `json:"execution"`
			RecoveryHypothesis  *RecoveryHypothesis       `json:"recovery_hypothesis"`
			SideEffect          SideEffectClass           `json:"side_effect"`
			Recovery            RecoveryPolicy            `json:"recovery"`
			ReconcileTool       string                    `json:"reconcile_tool"`
			RecoveryState       string                    `json:"recovery_state"`
			RuntimeError        *ExecutionError           `json:"runtime_error"`
			Resolution          *TaskResolution           `json:"resolution"`
			DiagnosticHints     []string                  `json:"diagnostic_hints"`
			LastOperation       string                    `json:"last_operation"`
			MemoryManifests     []MemoryInjectionManifest `json:"memory_manifests"`
			ResetForRetry       bool                      `json:"reset_for_retry"`
		}
		_ = json.Unmarshal(e.Payload, &payload)

		taskID := e.TaskID
		if taskID == "" {
			taskID = payload.ID
		}
		if taskID == "" {
			continue
		}

		desc := payload.Desc
		if desc == "" {
			desc = payload.Description
		}

		item, exists := taskMap[taskID]
		failureEvent, hasFailureEvent := mergeFailureEventJSON(nil, e.Payload)
		if !exists {
			item = &TodoItem{
				ID:                  taskID,
				Phase:               payload.Phase,
				Action:              payload.Action,
				PlanTaskID:          payload.PlanTaskID,
				ContractID:          payload.ContractID,
				ContractHash:        payload.ContractHash,
				ContractRevision:    payload.ContractRevision,
				Desc:                desc,
				Status:              TaskPending,
				MaxRetries:          payload.MaxRetries,
				Retries:             payload.Retries,
				Agent:               payload.Agent,
				Model:               payload.Model,
				Skills:              append([]string(nil), payload.Skills...),
				InjectedSkills:      append([]string(nil), payload.InjectedSkills...),
				LoadedSkills:        append([]string(nil), payload.LoadedSkills...),
				Source:              payload.Source,
				ParentID:            payload.ParentID,
				DependsOn:           payload.DependsOn,
				OnFailure:           payload.OnFailure,
				Kind:                payload.Kind,
				Advances:            append([]string(nil), payload.Advances...),
				ExpectedStateChange: payload.ExpectedStateChange,
				Progress:            payload.Progress,
				ProgressCriteria:    append([]string(nil), payload.ProgressCriteria...),
				FailureFingerprints: append([]FailureFingerprint(nil), payload.FailureFingerprints...),
				Execution:           payload.Execution,
				RecoveryHypothesis:  cloneRecoveryHypothesis(payload.RecoveryHypothesis),
				SideEffect:          payload.SideEffect,
				Recovery:            payload.Recovery,
				ReconcileTool:       payload.ReconcileTool,
				RecoveryState:       payload.RecoveryState,
				RuntimeError:        payload.RuntimeError,
				Resolution:          payload.Resolution,
				DiagnosticHints:     append([]string(nil), payload.DiagnosticHints...),
				LastOperation:       payload.LastOperation,
				TypedResult:         payload.TypedResult,
				FailureEvent:        failureEvent,
			}
			taskMap[taskID] = item
			taskOrder = append(taskOrder, taskID)
		}

		if desc != "" {
			item.Desc = desc
		}
		if payload.Phase != "" && e.Type != "task_failed" && e.Type != "task_blocked" && e.Type != "task_protocol_incomplete" {
			item.Phase = payload.Phase
		}
		if payload.Action != nil {
			item.Action = payload.Action
		}
		if payload.PlanTaskID != "" {
			item.PlanTaskID = payload.PlanTaskID
		}
		if payload.ContractID != "" {
			item.ContractID = payload.ContractID
		}
		if payload.ContractHash != "" {
			item.ContractHash = payload.ContractHash
		}
		if payload.ContractRevision != 0 {
			item.ContractRevision = payload.ContractRevision
		}
		if payload.Agent != "" {
			item.Agent = payload.Agent
		}
		if payload.Model != "" {
			item.Model = payload.Model
		}
		if len(payload.Skills) > 0 {
			item.Skills = append([]string(nil), payload.Skills...)
		}
		if len(payload.InjectedSkills) > 0 {
			item.InjectedSkills = append([]string(nil), payload.InjectedSkills...)
		}
		if len(payload.LoadedSkills) > 0 {
			item.LoadedSkills = append([]string(nil), payload.LoadedSkills...)
		}
		if payload.Source != "" {
			item.Source = payload.Source
		}
		if payload.ParentID != "" {
			item.ParentID = payload.ParentID
		}
		if len(payload.DependsOn) > 0 {
			item.DependsOn = payload.DependsOn
		}
		if payload.OnFailure != "" {
			item.OnFailure = payload.OnFailure
		}
		if payload.Kind != "" {
			item.Kind = payload.Kind
		}
		if len(payload.Advances) > 0 {
			item.Advances = append([]string(nil), payload.Advances...)
		}
		if payload.ExpectedStateChange != "" {
			item.ExpectedStateChange = payload.ExpectedStateChange
		}
		if payload.Progress != "" {
			item.Progress = payload.Progress
			item.ProgressCriteria = append([]string(nil), payload.ProgressCriteria...)
		}
		if payload.MaxRetries != 0 {
			item.MaxRetries = payload.MaxRetries
		}
		if payload.Retries != 0 {
			item.Retries = payload.Retries
		}
		for _, fingerprint := range payload.FailureFingerprints {
			seen := false
			for i := range item.FailureFingerprints {
				existing := &item.FailureFingerprints[i]
				if existing.Digest == fingerprint.Digest {
					if existing.Occurrences < 1 {
						existing.Occurrences = 1
					}
					occurrences := fingerprint.Occurrences
					if occurrences < 1 {
						occurrences = 1
					}
					if occurrences > existing.Occurrences {
						existing.Occurrences = occurrences
					}
					seen = true
					break
				}
			}
			if !seen {
				if fingerprint.Occurrences < 1 {
					fingerprint.Occurrences = 1
				}
				item.FailureFingerprints = append(item.FailureFingerprints, fingerprint)
			}
		}
		if payload.Execution.Kind != "" || payload.Execution.AllowsReplay != nil || payload.Execution.RequiresResult || payload.Execution.RequiresVerification {
			item.Execution = payload.Execution
		}
		if payload.SideEffect != "" {
			item.SideEffect = payload.SideEffect
		}
		if payload.Recovery != "" {
			item.Recovery = payload.Recovery
		}
		if payload.ReconcileTool != "" {
			item.ReconcileTool = payload.ReconcileTool
		}
		if payload.RecoveryState != "" {
			item.RecoveryState = payload.RecoveryState
		}
		if payload.RuntimeError != nil {
			item.RuntimeError = payload.RuntimeError
		}
		if payload.Resolution != nil {
			item.Resolution = payload.Resolution
		}
		if len(payload.DiagnosticHints) > 0 {
			item.DiagnosticHints = append([]string(nil), payload.DiagnosticHints...)
		}
		if payload.LastOperation != "" {
			item.LastOperation = payload.LastOperation
		}
		if payload.RecoveryHypothesis != nil {
			item.RecoveryHypothesis = cloneRecoveryHypothesis(payload.RecoveryHypothesis)
		}
		if payload.Verify != "" {
			item.Verify = payload.Verify
			item.VerifyMode = payload.VerifyMode
		}
		if payload.VerifySpec != nil {
			item.VerifySpec = cloneVerificationSpecPtr(payload.VerifySpec)
		}
		if payload.VerifyResult != nil {
			item.VerifyResult = payload.VerifyResult
		}
		if payload.TypedResult != nil {
			item.TypedResult = payload.TypedResult
		}
		if payload.Detail != "" {
			item.Detail = payload.Detail
		}
		if hasFailureEvent {
			item.FailureEvent, _ = mergeFailureEventJSON(item.FailureEvent, e.Payload)
		}
		if len(payload.ExecutionReceipts) > 0 {
			item.ExecutionReceipts = mergeExecutionReceipts(item.ExecutionReceipts, payload.ExecutionReceipts)
		}
		if payload.ExecutionReceipt != nil {
			item.ExecutionReceipt = payload.ExecutionReceipt
			item.ExecutionReceipts = appendExecutionReceipt(item.ExecutionReceipts, *payload.ExecutionReceipt)
		}
		if len(payload.MemoryManifests) > 0 {
			item.MemoryManifests = payload.MemoryManifests
		}

		switch e.Type {
		case "task_created":
			if (!isReducerTerminalTaskStatus(item.Status) || payload.ResetForRetry) && payload.Status != "" {
				item.Status = TaskStatus(payload.Status)
				if payload.ResetForRetry {
					item.Output = ""
					item.VerifyResult = nil
					item.RuntimeError = nil
					item.FailureEvent = nil
					item.RecoveryState = RecoveryStateNotStarted
					item.LastOperation = ""
					item.Progress = ProgressUnknown
					item.ProgressCriteria = nil
					item.StartedAt = time.Time{}
					item.EndedAt = time.Time{}
					item.ModelTime = 0
					item.ToolTime = 0
				}
			}
		case "task_started":
			if !isReducerTerminalTaskStatus(item.Status) {
				item.Status = TaskInProgress
			}
		case "task_verifying":
			if !isReducerTerminalTaskStatus(item.Status) {
				item.Status = TaskVerifying
			}
		case "task_planned":
			if !isReducerTerminalTaskStatus(item.Status) {
				item.Status = TaskPlanned
			}
		case "task_paused":
			if !isReducerTerminalTaskStatus(item.Status) {
				item.Status = TaskPaused
			}
		case "task_completed":
			item.Status = TaskDone
			if payload.Output != "" {
				item.Output = payload.Output
			}
		case "task_failed":
			item.Status = TaskError
			if payload.Output != "" {
				item.Output = payload.Output
			}
			if item.Detail == "" && payload.Summary != "" {
				item.Detail = payload.Summary
			}
		case "task_skipped":
			item.Status = TaskSkipped
		case "task_blocked":
			item.Status = TaskBlocked
			if payload.Output != "" {
				item.Output = payload.Output
			}
			if item.Detail == "" && payload.Summary != "" {
				item.Detail = payload.Summary
			}
		case "task_protocol_incomplete":
			item.Status = TaskProtocolIncomplete
			if payload.Output != "" {
				item.Output = payload.Output
			}
			if item.Detail == "" && payload.Summary != "" {
				item.Detail = payload.Summary
			}
		case "task_reset":
			item.Status = TaskPending
		}
	}

	result := make([]*TodoItem, 0, len(taskOrder))
	for _, id := range taskOrder {
		result = append(result, taskMap[id])
	}
	return result
}

// normalizeReplayEvents gives reducers deterministic duplicate semantics:
// the first occurrence of an event ID or durable idempotency key wins. Input
// order is retained because the hash chain is the authoritative ordering; an
// out-of-order slice remains replayable without allowing a late non-terminal
// transition to reopen a terminal task.
func normalizeReplayEvents(events []RunEvent) []RunEvent {
	if len(events) == 0 {
		return nil
	}
	result := make([]RunEvent, 0, len(events))
	seenIDs := make(map[string]struct{}, len(events))
	seenKeys := make(map[string]struct{}, len(events))
	for _, event := range events {
		if event.ID != "" {
			if _, seen := seenIDs[event.ID]; seen {
				continue
			}
			seenIDs[event.ID] = struct{}{}
		}
		if event.IdempotencyKey != "" {
			if _, seen := seenKeys[event.IdempotencyKey]; seen {
				continue
			}
			seenKeys[event.IdempotencyKey] = struct{}{}
		}
		result = append(result, event)
	}
	return result
}

func isReducerTerminalTaskStatus(status TaskStatus) bool {
	switch status {
	case TaskDone, TaskError, TaskSkipped, TaskBlocked, TaskProtocolIncomplete:
		return true
	default:
		return false
	}
}

func appendExecutionReceipt(receipts []ExecutionReceipt, receipt ExecutionReceipt) []ExecutionReceipt {
	for i, existing := range receipts {
		if existing.RunID == receipt.RunID && existing.TaskID == receipt.TaskID && existing.Attempt == receipt.Attempt && existing.TranscriptRef == receipt.TranscriptRef {
			if existing.RepairProvenance == nil && receipt.RepairProvenance != nil {
				receipts[i] = receipt
			}
			return receipts
		}
	}
	res := append(receipts, receipt)
	sort.SliceStable(res, func(i, j int) bool {
		if res[i].Attempt != res[j].Attempt {
			return res[i].Attempt < res[j].Attempt
		}
		return res[i].StartedAt.Before(res[j].StartedAt)
	})
	return res
}

func mergeExecutionReceipts(existing, incoming []ExecutionReceipt) []ExecutionReceipt {
	for _, receipt := range incoming {
		existing = appendExecutionReceipt(existing, receipt)
	}
	return existing
}
