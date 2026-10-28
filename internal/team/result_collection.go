package team

import (
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

const (
	EventTaskResultCollectionCheckpointed EventType           = "task_result_collection_checkpointed"
	resultCollectionVersion                                   = 1
	resultCollectionAcceptedMaxBytes                          = 32 << 20
	resultCollectionDefaultTurns                              = 2
	resultCollectionResponseMaxBytes                          = 2 << 20
	resultCollectionWorkMaxBytes                              = 32 << 20
	repairFailureCollectionUnavailable    RepairFailureReason = "collection_unavailable"
	repairFailureCollectionBudget         RepairFailureReason = "collection_budget_exhausted"
)

// collectionDurationFormat encodes VerificationResult.Duration as the integer
// nanoseconds that the claims schema advertises and the tool transport's
// encoding/json writes. json/v2 has no default time.Duration representation
// and rejects even an omitted zero, so every collected result, receipt, and
// checkpoint payload that can carry a verification entry needs it.
var collectionDurationFormat = jsonv1.FormatDurationAsNano(true)

type ResultCollectionState string

const (
	ResultCollectionPending   ResultCollectionState = "pending"
	ResultCollectionInFlight  ResultCollectionState = "in_flight"
	ResultCollectionReceived  ResultCollectionState = "received"
	ResultCollectionPrepared  ResultCollectionState = "prepared"
	ResultCollectionCommitted ResultCollectionState = "committed"
	ResultCollectionBlocked   ResultCollectionState = "blocked"
)

var (
	errResultCollectionStale   = errors.New("result collection checkpoint is stale")
	errResultCollectionBudget  = errors.New("result collection model-turn budget exhausted")
	errResultCollectionBlocked = errors.New("result collection requires an explicit result-only grant")
)

// ResultCollectionRef names immutable CAS evidence. It carries no filesystem
// path or model-authored provenance.
type ResultCollectionRef struct {
	ArtifactID string `json:"artifact_id"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
}

// ResultCollectionCheckpoint belongs to one original worker attempt. Its work
// binding never changes; revision, progress and collector invocations advance
// through event-first compare-and-swap updates, independently of Todo status.
type ResultCollectionCheckpoint struct {
	Version                 int                       `json:"version"`
	Revision                int                       `json:"revision"`
	State                   ResultCollectionState     `json:"state"`
	BranchID                string                    `json:"branch_id"`
	SourceRunID             string                    `json:"source_run_id"`
	TaskID                  string                    `json:"task_id"`
	Agent                   string                    `json:"agent"`
	Attempt                 int                       `json:"attempt"`
	ModelExecutionID        string                    `json:"model_execution_id"`
	FanoutIndex             int                       `json:"fanout_index,omitzero"`
	FanoutLeaf              bool                      `json:"fanout_leaf,omitzero"`
	ExecutionTarget         execution.ExecutionTarget `json:"execution_target"`
	WorkOccurrenceRevision  int                       `json:"work_occurrence_revision"`
	WorkDispatchID          string                    `json:"work_dispatch_id"`
	SubmissionRevision      int                       `json:"submission_revision"`
	SubmissionDispatchID    string                    `json:"submission_dispatch_id"`
	PolicySHA256            string                    `json:"policy_sha256"`
	TaskContractSHA256      string                    `json:"task_contract_sha256"`
	InputsID                string                    `json:"inputs_id,omitempty"`
	InputsSHA256            string                    `json:"inputs_sha256,omitempty"`
	EvidenceSHA256          string                    `json:"evidence_sha256"`
	WorkspaceIdentitySHA256 string                    `json:"workspace_identity_sha256"`
	CollectorSchemaRevision int                       `json:"collector_schema_revision"`
	Work                    ResultCollectionRef       `json:"work"`
	InvocationID            string                    `json:"invocation_id,omitempty"`
	InvocationRunID         string                    `json:"invocation_run_id,omitempty"`
	TurnsUsed               int                       `json:"turns_used"`
	TurnLimit               int                       `json:"turn_limit"`
	Request                 *ResultCollectionRef      `json:"request,omitempty"`
	Response                *ResultCollectionRef      `json:"response,omitempty"`
	Prepared                *ResultCollectionRef      `json:"prepared,omitempty"`
	Accepted                *ResultCollectionRef      `json:"accepted,omitempty"`
	FailureReason           RepairFailureReason       `json:"failure_reason,omitempty"`
	ObservedUsage           *ExecutionUsage           `json:"observed_usage,omitempty"`
}

type resultCollectionWork struct {
	Version            int                    `json:"version"`
	BindingSHA256      string                 `json:"binding_sha256"`
	Receipt            ExecutionReceipt       `json:"execution_receipt"`
	Workspace          string                 `json:"workspace"`
	StopReason         string                 `json:"stop_reason,omitempty"`
	Report             []byte                 `json:"report"`
	Input              []byte                 `json:"input,omitempty"`
	Generation         agent.GenerationParams `json:"generation,omitzero"`
	TeamGeneration     agent.GenerationParams `json:"team_generation,omitzero"`
	TimeoutSeconds     int64                  `json:"timeout_seconds,omitzero"`
	GenerationCaptured bool                   `json:"generation_captured,omitzero"`
}

type resultCollectionBlob struct {
	Version         int    `json:"version"`
	WorkSHA256      string `json:"work_sha256"`
	InvocationID    string `json:"invocation_id"`
	InvocationRunID string `json:"invocation_run_id"`
	Kind            string `json:"kind"`
	Data            []byte `json:"data"`
}

type resultCollectionEvent struct {
	Receipt ExecutionReceipt `json:"execution_receipt"`
	Result  *TaskResult      `json:"typed_result,omitempty"`
}

func cloneResultCollectionCheckpoint(value *ResultCollectionCheckpoint) *ResultCollectionCheckpoint {
	if value == nil {
		return nil
	}
	clone := *value
	if value.ObservedUsage != nil {
		clone.ObservedUsage = new(*value.ObservedUsage)
	}
	for _, ref := range []struct {
		source *ResultCollectionRef
		target **ResultCollectionRef
	}{{value.Request, &clone.Request}, {value.Response, &clone.Response}, {value.Prepared, &clone.Prepared}, {value.Accepted, &clone.Accepted}} {
		if ref.source != nil {
			*ref.target = new(*ref.source)
		}
	}
	return &clone
}

// A later execution receipt may have been constructed before collection began.
// Preserve the durable progress for the same execution, never for a new attempt.
func preserveResultCollection(incoming, existing *ExecutionReceipt) {
	if incoming == nil || existing == nil || !sameExecutionReceiptProjection(*incoming, *existing) || existing.ResultCollection == nil {
		return
	}
	if incoming.ResultCollection == nil || incoming.ResultCollection.Revision < existing.ResultCollection.Revision {
		incoming.ResultCollection = cloneResultCollectionCheckpoint(existing.ResultCollection)
		// Grants and consumed turns are one authority record. A late native
		// receipt cannot retain the checkpoint while erasing its grant history.
		preserved := cloneExecutionReceipt(existing)
		incoming.RepairProvenance = preserved.RepairProvenance
		if existing.ResultCollection.State == ResultCollectionCommitted {
			// Work completion can still hold its pre-collection receipt.
			// The accepted event owns these canonical result bindings too.
			incoming.ResultContractID = existing.ResultContractID
			incoming.ResultContractSHA256 = existing.ResultContractSHA256
			incoming.ResultPayloadSHA256 = existing.ResultPayloadSHA256
			incoming.ResultValidation = existing.ResultValidation
			incoming.ResultValidationFailures = existing.ResultValidationFailures
		}
	}
}

func collectionDigest(value any) (string, error) {
	data, err := json.Marshal(value, json.Deterministic(true), collectionDurationFormat)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validCollectionRef(ref *ResultCollectionRef) bool {
	return ref != nil && validCollectionDigest(ref.SHA256) && ref.ArtifactID == "sha256-"+ref.SHA256 && ref.Bytes > 0
}

func validCollectionDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateResultCollectionBinding(value *ResultCollectionCheckpoint) error {
	if value == nil || value.Version != resultCollectionVersion || value.CollectorSchemaRevision != resultCollectionVersion || value.Revision < 1 ||
		value.BranchID == "" || value.SourceRunID == "" || value.TaskID == "" || value.Agent == "" || value.Attempt < 1 || value.ModelExecutionID == "" ||
		value.WorkOccurrenceRevision < 1 || value.WorkDispatchID == "" || value.SubmissionRevision < value.WorkOccurrenceRevision || value.SubmissionDispatchID == "" ||
		!validCollectionDigest(value.PolicySHA256) || !validCollectionDigest(value.TaskContractSHA256) || !validCollectionDigest(value.EvidenceSHA256) || !validCollectionDigest(value.WorkspaceIdentitySHA256) ||
		value.ExecutionTarget.Validate() != nil ||
		value.TurnLimit < resultCollectionDefaultTurns || value.TurnsUsed < 0 || value.TurnsUsed > value.TurnLimit {
		return fmt.Errorf("invalid result collection binding")
	}
	if (value.InputsID == "") != (value.InputsSHA256 == "") || (value.InputsSHA256 != "" && !validCollectionDigest(value.InputsSHA256)) {
		return fmt.Errorf("invalid result collection frozen inputs")
	}
	return validateCollectionReferences(value)
}

func validateCollectionReferences(value *ResultCollectionCheckpoint) error {
	if !validCollectionRef(&value.Work) || value.Work.Bytes > resultCollectionWorkMaxBytes {
		return fmt.Errorf("invalid result collection work reference")
	}
	if value.FanoutIndex < 0 || (!value.FanoutLeaf && value.FanoutIndex != 0) {
		return fmt.Errorf("invalid result collection leaf identity")
	}
	if value.Request != nil && (!validCollectionRef(value.Request) || value.Request.Bytes > resultCollectionWorkMaxBytes*2) {
		return fmt.Errorf("invalid result collection request reference")
	}
	if value.Accepted != nil && (!validCollectionRef(value.Accepted) || value.Accepted.Bytes > resultCollectionAcceptedMaxBytes*2) {
		return fmt.Errorf("invalid result collection accepted reference")
	}
	for _, ref := range []*ResultCollectionRef{value.Response, value.Prepared} {
		if ref != nil && (!validCollectionRef(ref) || ref.Bytes > resultCollectionResponseMaxBytes*2) {
			return fmt.Errorf("invalid result collection evidence reference")
		}
	}
	return nil
}

func validateResultCollectionCheckpoint(value *ResultCollectionCheckpoint) error {
	if err := validateResultCollectionBinding(value); err != nil {
		return err
	}
	switch value.State {
	case ResultCollectionPending:
		if value.TurnsUsed != 0 || value.InvocationID != "" || value.InvocationRunID != "" || value.Request != nil || value.Response != nil || value.Prepared != nil || value.Accepted != nil {
			return fmt.Errorf("pending result collection has invocation evidence")
		}
	case ResultCollectionInFlight:
		if value.TurnsUsed < 1 || value.InvocationID == "" || value.InvocationRunID == "" || value.Request == nil || value.Response != nil || value.Prepared != nil || value.Accepted != nil {
			return fmt.Errorf("in-flight result collection has invalid invocation")
		}
	case ResultCollectionReceived, ResultCollectionPrepared, ResultCollectionCommitted:
		if value.TurnsUsed < 1 || value.InvocationID == "" || value.InvocationRunID == "" || value.Request == nil || value.Response == nil ||
			(value.State != ResultCollectionReceived && value.Prepared == nil) || (value.State == ResultCollectionCommitted && value.Accepted == nil) ||
			(value.State == ResultCollectionReceived && value.Prepared != nil) || (value.State != ResultCollectionCommitted && value.Accepted != nil) {
			return fmt.Errorf("result collection is missing durable evidence")
		}
	case ResultCollectionBlocked:
		if value.FailureReason == "" {
			return fmt.Errorf("blocked result collection has no diagnostic")
		}
	default:
		return fmt.Errorf("unknown result collection state %q", value.State)
	}
	return nil
}
