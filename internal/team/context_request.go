package team

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

const ContextRequestSchemaVersion = 1

type ContextTrigger string

const (
	ContextTriggerCoordinatorStart ContextTrigger = "coordinator_start"
	ContextTriggerContinuation     ContextTrigger = "continuation"
	ContextTriggerTaskDispatch     ContextTrigger = "task_dispatch"
	ContextTriggerRetry            ContextTrigger = "retry"
	ContextTriggerToolFailure      ContextTrigger = "tool_failure"
	ContextTriggerSidecarTask      ContextTrigger = "sidecar_task"
	ContextTriggerSkillMatch       ContextTrigger = "skill_match"
	ContextTriggerGuardReview      ContextTrigger = "guard_review"
	ContextTriggerPlanReview       ContextTrigger = "plan_review"
	ContextTriggerJudge            ContextTrigger = "judge"
	ContextTriggerSkeptic          ContextTrigger = "skeptic"
	ContextTriggerRepair           ContextTrigger = "repair"
	// ContextTriggerAuxiliary is retained only for non-model JIT context
	// tools. Every auxiliary model call uses one of the purpose-specific
	// triggers above.
	ContextTriggerAuxiliary ContextTrigger = "auxiliary"
)

type ContextFailure struct {
	Class      TaskFailureClass `json:"class,omitempty"`
	Component  string           `json:"component,omitempty"`
	ErrorClass string           `json:"error_class,omitempty"`
	// EvidenceRefs are runtime-only opaque references used to construct a
	// bounded retrieval query. They must never be serialized with a request:
	// manifests/events store only the request hash and a content-free call ID.
	EvidenceRefs  []string `json:"-"`
	ToolName      string   `json:"tool_name,omitempty"`
	ToolInputHash string   `json:"tool_input_hash,omitempty"`
	ExitCode      *int     `json:"exit_code,omitempty"`
}

type ContextRequest struct {
	SchemaVersion int            `json:"schema_version"`
	RequestID     string         `json:"request_id"`
	RunID         string         `json:"run_id"`
	TaskID        string         `json:"task_id,omitempty"`
	Attempt       int            `json:"attempt"`
	Goal          string         `json:"goal"`
	Constraints   string         `json:"constraints,omitempty"`
	AgentName     string         `json:"agent_name,omitempty"`
	AgentRole     string         `json:"agent_role,omitempty"`
	Phase         Phase          `json:"phase"`
	Trigger       ContextTrigger `json:"trigger"`
	ActionType    string         `json:"action_type,omitempty"`
	Purpose       string         `json:"purpose,omitempty"`
	// ModelExecutionID disambiguates concurrent isolated executions of the
	// same task/agent/attempt (notably extra-model workers). It is an opaque,
	// deterministic identity, never a provider request payload.
	ModelExecutionID       string   `json:"model_execution_id,omitempty"`
	Capabilities           []string `json:"capabilities,omitempty"`
	TouchedPaths           []string `json:"touched_paths,omitempty"`
	DependencyIDs          []string `json:"dependency_ids,omitempty"`
	VerificationCriteria   string   `json:"verification_criteria,omitempty"`
	CandidateIDs           []string `json:"candidate_ids,omitempty"`
	SelectionContract      string   `json:"selection_contract,omitempty"`
	RecoveryDisposition    string   `json:"recovery_disposition,omitempty"`
	EnvironmentFingerprint string   `json:"environment_fingerprint,omitempty"`
	// Parent identity is runtime-only. It is explicitly included in the
	// request fingerprint and projected content-free into the manifest, but is
	// not a prompt fragment or a serialized request payload.
	ParentTrigger             ContextTrigger  `json:"-"`
	ParentRequestID           string          `json:"-"`
	ParentManifestFingerprint string          `json:"-"`
	Failure                   *ContextFailure `json:"failure,omitempty"`
}

func (r ContextRequest) Validate() error {
	if r.SchemaVersion != ContextRequestSchemaVersion {
		return fmt.Errorf("context request schema version must be %d", ContextRequestSchemaVersion)
	}
	if strings.TrimSpace(r.RequestID) == "" || strings.TrimSpace(r.RunID) == "" {
		return fmt.Errorf("context request requires request_id and run_id")
	}
	canonical := r
	canonical.AssignRequestID()
	if r.RequestID != canonical.RequestID {
		return fmt.Errorf("context request_id does not match canonical request fingerprint")
	}
	if r.Attempt < 1 {
		return fmt.Errorf("context request attempt must be 1-based")
	}
	if !validContextTrigger(r.Trigger) {
		return fmt.Errorf("invalid context trigger %q", r.Trigger)
	}
	if !validContextRequestPhase(r.Phase) {
		return fmt.Errorf("invalid context phase %q", r.Phase)
	}
	return r.validateTriggerFields()
}

func (r ContextRequest) validateTriggerFields() error {
	require := func(label string, values ...string) error {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s context request requires %s", r.Trigger, label)
			}
		}
		return nil
	}
	if err := require("run_id", r.RunID); err != nil {
		return err
	}
	switch r.Trigger {
	case ContextTriggerCoordinatorStart, ContextTriggerContinuation:
		return require("goal, agent_role, and phase", r.Goal, r.AgentRole, string(r.Phase))
	case ContextTriggerTaskDispatch:
		return require("task_id, goal, agent identity, agent_role, phase, and model_execution_id", r.TaskID, r.Goal, r.AgentName, r.AgentRole, string(r.Phase), r.ModelExecutionID)
	case ContextTriggerRetry:
		if r.Attempt <= 1 || r.Failure == nil {
			return fmt.Errorf("retry context request requires attempt > 1 and failure context")
		}
		return require("task_id, goal, agent identity, agent_role, phase, and model_execution_id", r.TaskID, r.Goal, r.AgentName, r.AgentRole, string(r.Phase), r.ModelExecutionID)
	case ContextTriggerToolFailure:
		if r.Failure == nil {
			return fmt.Errorf("tool_failure context request requires failure context")
		}
		return require("task_id, tool name, error class, and tool input hash", r.TaskID, r.Failure.ToolName, contextFailureClass(r.Failure), r.Failure.ToolInputHash)
	case ContextTriggerSkillMatch:
		return require("goal and agent_role", r.Goal, r.AgentRole)
	case ContextTriggerGuardReview:
		if r.Failure == nil {
			return fmt.Errorf("guard_review context request requires tool evidence")
		}
		return require("agent identity, tool name, tool input hash, and guard component", r.AgentName, r.Failure.ToolName, r.Failure.ToolInputHash, r.Purpose)
	case ContextTriggerPlanReview:
		return require("task_id, plan/revision identity, and verification criteria", r.TaskID, r.ActionType, r.VerificationCriteria)
	case ContextTriggerJudge:
		if len(r.CandidateIDs) == 0 {
			return fmt.Errorf("judge context request requires candidate identities")
		}
		return require("task_id and selection contract", r.TaskID, r.SelectionContract)
	case ContextTriggerSkeptic:
		if len(r.CandidateIDs) == 0 {
			return fmt.Errorf("skeptic context request requires candidate identity")
		}
		return require("task_id and verification contract", r.TaskID, r.VerificationCriteria)
	case ContextTriggerRepair:
		if r.Failure == nil || len(r.Failure.EvidenceRefs) == 0 {
			return fmt.Errorf("repair context request requires approved failure evidence")
		}
		return require("task_id and recovery disposition", r.TaskID, r.RecoveryDisposition)
	case ContextTriggerSidecarTask, ContextTriggerAuxiliary:
		return require("goal, agent_role, and phase", r.Goal, r.AgentRole, string(r.Phase))
	default:
		return fmt.Errorf("invalid context trigger %q", r.Trigger)
	}
}

func contextFailureClass(failure *ContextFailure) string {
	if failure == nil {
		return ""
	}
	if value := strings.TrimSpace(failure.ErrorClass); value != "" {
		return value
	}
	return strings.TrimSpace(string(failure.Class))
}

func validContextTrigger(trigger ContextTrigger) bool {
	switch trigger {
	case ContextTriggerCoordinatorStart, ContextTriggerContinuation, ContextTriggerTaskDispatch, ContextTriggerRetry, ContextTriggerToolFailure, ContextTriggerSidecarTask, ContextTriggerSkillMatch, ContextTriggerGuardReview, ContextTriggerPlanReview, ContextTriggerJudge, ContextTriggerSkeptic, ContextTriggerRepair, ContextTriggerAuxiliary:
		return true
	default:
		return false
	}
}

func validContextRequestPhase(phase Phase) bool {
	switch phase {
	case PhaseInit, PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify:
		return true
	default:
		return false
	}
}

func normalizedRequestTokens(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (r ContextRequest) RetrievalQuery() string {
	parts := []string{strings.TrimSpace(r.Goal), "phase:" + strings.ToLower(string(r.Phase)), "trigger:" + string(r.Trigger)}
	if role := strings.ToLower(strings.TrimSpace(r.AgentRole)); role != "" {
		parts = append(parts, "role:"+role)
	}
	for _, capability := range normalizedRequestTokens(r.Capabilities) {
		parts = append(parts, "capability:"+capability)
	}
	if r.Failure != nil {
		if class := strings.ToLower(strings.TrimSpace(r.Failure.ErrorClass)); class != "" {
			parts = append(parts, "error_class:"+class)
		} else if class := strings.ToLower(strings.TrimSpace(string(r.Failure.Class))); class != "" {
			parts = append(parts, "error_class:"+class)
		}
		if tool := strings.ToLower(strings.TrimSpace(r.Failure.ToolName)); tool != "" {
			parts = append(parts, "tool:"+tool)
		}
		for _, evidence := range r.Failure.EvidenceRefs {
			if redacted := strings.TrimSpace(utils.RedactSecrets(evidence)); redacted != "" {
				parts = append(parts, "evidence:"+redacted)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func (r ContextRequest) Fingerprint() string {
	canonical := r
	canonical.RequestID = ""
	canonical.Goal = utils.RedactSecrets(strings.TrimSpace(canonical.Goal))
	canonical.Constraints = utils.RedactSecrets(strings.TrimSpace(canonical.Constraints))
	canonical.VerificationCriteria = utils.RedactSecrets(strings.TrimSpace(canonical.VerificationCriteria))
	canonical.Capabilities = normalizedRequestTokens(canonical.Capabilities)
	canonical.TouchedPaths = normalizedRequestTokens(canonical.TouchedPaths)
	canonical.DependencyIDs = normalizedRequestTokens(canonical.DependencyIDs)
	canonical.CandidateIDs = normalizedRequestTokens(canonical.CandidateIDs)
	if canonical.Failure != nil {
		failure := *canonical.Failure
		failure.EvidenceRefs = nil
		canonical.Failure = &failure
	}
	// Parent metadata is intentionally not serialized as a request, but must
	// still separate child invocation identity and replay fingerprints.
	data, _ := json.Marshal(struct {
		ContextRequest
		ParentTrigger             ContextTrigger `json:"parent_trigger,omitempty"`
		ParentRequestID           string         `json:"parent_request_id,omitempty"`
		ParentManifestFingerprint string         `json:"parent_manifest_fingerprint,omitempty"`
	}{
		ContextRequest:            canonical,
		ParentTrigger:             canonical.ParentTrigger,
		ParentRequestID:           canonical.ParentRequestID,
		ParentManifestFingerprint: canonical.ParentManifestFingerprint,
	})
	return hashContentKey(string(data))
}

func (r *ContextRequest) AssignRequestID() {
	if r == nil {
		return
	}
	fingerprint := r.Fingerprint()
	if len(fingerprint) > 24 {
		fingerprint = fingerprint[:24]
	}
	r.RequestID = "ctx-" + strconv.Itoa(r.Attempt) + "-" + fingerprint
}
