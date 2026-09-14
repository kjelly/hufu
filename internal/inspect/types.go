// Package inspect projects persisted Hufu runtime facts without mutating the
// workspace or re-executing runtime behavior.
package inspect

import (
	"errors"
	"fmt"
	"strings"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
)

const SchemaVersion = 1

type Kind string

const (
	KindRun      Kind = "run"
	KindTask     Kind = "task"
	KindTrace    Kind = "trace"
	KindEvidence Kind = "evidence"
	KindContext  Kind = "context"
	KindReplay   Kind = "replay"
	KindStorage  Kind = "storage"
	KindOverview Kind = "overview"
)

type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

const (
	ExitSuccess   = 0
	ExitIntegrity = 1
	ExitUsage     = 2
)

var (
	ErrInvalidQuery = errors.New("invalid inspect query")
	ErrNotFound     = errors.New("inspect target not found")
	ErrAmbiguous    = errors.New("inspect target is ambiguous")
	ErrUnauthorized = errors.New("inspect target is unauthorized")
	ErrIntegrity    = errors.New("inspect integrity failure")
)

type InspectQuery struct {
	Workspace string `json:"-"`
	RunID     string `json:"run_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Attempt   int    `json:"attempt,omitzero"`
	BranchID  string `json:"branch_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	TeamID    string `json:"team_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

func (q InspectQuery) Validate(kind Kind) error {
	if q.Attempt < 0 {
		return fmt.Errorf("%w: attempt must not be negative", ErrInvalidQuery)
	}
	switch kind {
	case KindRun, KindEvidence, KindReplay:
		if strings.TrimSpace(q.RunID) == "" {
			return fmt.Errorf("%w: run id is required for %s", ErrInvalidQuery, kind)
		}
		if strings.TrimSpace(q.TaskID) != "" || q.Attempt != 0 {
			return fmt.Errorf("%w: task and attempt filters are not valid for %s", ErrInvalidQuery, kind)
		}
	case KindTrace:
		if strings.TrimSpace(q.RunID) == "" {
			return fmt.Errorf("%w: run id is required for %s", ErrInvalidQuery, kind)
		}
	case KindTask:
		if strings.TrimSpace(q.RunID) == "" || strings.TrimSpace(q.TaskID) == "" {
			return fmt.Errorf("%w: run id and task id are required for task", ErrInvalidQuery)
		}
	case KindContext:
		if strings.TrimSpace(q.RunID) == "" || strings.TrimSpace(q.TaskID) == "" {
			return fmt.Errorf("%w: run id and task id are required for context", ErrInvalidQuery)
		}
		if strings.TrimSpace(q.ProjectID) == "" {
			return fmt.Errorf("%w: project id is required for context", ErrInvalidQuery)
		}
	case KindOverview:
		if strings.TrimSpace(q.TaskID) != "" || q.Attempt != 0 || strings.TrimSpace(q.ProjectID) != "" ||
			strings.TrimSpace(q.TeamID) != "" || strings.TrimSpace(q.AgentID) != "" {
			return fmt.Errorf("%w: overview accepts only workspace, run, branch, and session", ErrInvalidQuery)
		}
	case KindStorage:
		if strings.TrimSpace(q.RunID) != "" || strings.TrimSpace(q.TaskID) != "" || q.Attempt != 0 ||
			strings.TrimSpace(q.BranchID) != "" || strings.TrimSpace(q.SessionID) != "" || strings.TrimSpace(q.ProjectID) != "" ||
			strings.TrimSpace(q.TeamID) != "" || strings.TrimSpace(q.AgentID) != "" {
			return fmt.Errorf("%w: storage inspection accepts only workspace and format", ErrInvalidQuery)
		}
	default:
		return fmt.Errorf("%w: unsupported kind %q", ErrInvalidQuery, kind)
	}
	return nil
}

func ExitCodeFor(err error) int {
	switch {
	case err == nil:
		return ExitSuccess
	case errors.Is(err, ErrIntegrity):
		return ExitIntegrity
	default:
		return ExitUsage
	}
}

type TraceRef struct {
	RunID           string `json:"run_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	BranchID        string `json:"branch_id,omitempty"`
	TaskID          string `json:"task_id,omitempty"`
	Attempt         int    `json:"attempt,omitzero"`
	AgentID         string `json:"agent_id,omitempty"`
	ExecutionTarget string `json:"execution_target,omitempty"`
	ToolCallID      string `json:"tool_call_id,omitempty"`
	EventID         string `json:"event_id,omitempty"`
	EventHash       string `json:"event_hash,omitempty"`
	EventOrdinal    int64  `json:"event_ordinal,omitzero"`
	ParentEventID   string `json:"parent_event_id,omitempty"`
	Source          string `json:"source"`
}

type TraceEntry struct {
	Ref                TraceRef `json:"ref"`
	Kind               string   `json:"kind"`
	AnchorEventID      string   `json:"anchor_event_id,omitempty"`
	AnchorEventOrdinal int64    `json:"anchor_event_ordinal,omitzero"`
	Timestamp          string   `json:"timestamp,omitempty"`
	Status             string   `json:"status,omitempty"`
	ReasonCode         string   `json:"reason_code,omitempty"`
	Refs               []string `json:"refs,omitempty"`
}

type Diagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Ref      string `json:"ref,omitempty"`
}

type Integrity struct {
	EventChain string `json:"event_chain"`
	Projection string `json:"projection"`
}

type Envelope struct {
	SchemaVersion int          `json:"schema_version"`
	Kind          Kind         `json:"kind"`
	Query         InspectQuery `json:"query"`
	Integrity     Integrity    `json:"integrity"`
	Data          any          `json:"data"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
}

type OverviewData struct {
	Operation OperationView                 `json:"operation"`
	Snapshot  *operatorpkg.OperatorSnapshot `json:"snapshot"`
	Error     *OperatorErrorView            `json:"error"`
	Warnings  []operatorpkg.DiagnosticView  `json:"warnings"`
}

type OperationView struct {
	Status string `json:"status"`
}

type OperatorErrorView struct {
	Kind    string `json:"kind"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ProjectionCheck struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	ComparedRefs []string `json:"compared_refs,omitempty"`
	DiffPaths    []string `json:"diff_paths,omitempty"`
	ReasonCode   string   `json:"reason_code,omitempty"`
}

const (
	ReasonMissingAnchor            = "missing_anchor"
	ReasonProjectionNotRunScoped   = "projection_not_run_scoped"
	ReasonProjectionChangedOnRead  = "projection_changed_during_read"
	ReasonOptionalProjectionAbsent = "optional_projection_missing"
	ReasonProjectionUnreadable     = "projection_unreadable"
	ReasonLegacySchemaUnsupported  = "legacy_schema_unsupported"
)
