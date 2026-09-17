package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNotFound  = errors.New("workspace registry entry not found")
	ErrAmbiguous = errors.New("workspace registry selector is ambiguous")
	ErrConflict  = errors.New("workspace registry conflict")
)

type Project struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Alias       string     `json:"alias,omitempty"`
	SubjectRoot string     `json:"subject_root"`
	StateDir    string     `json:"state_dir"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
}

type Workspace struct {
	ID                   string     `json:"id"`
	ProjectID            string     `json:"project_id"`
	TeamName             string     `json:"team_name"`
	ContextScopeID       string     `json:"context_scope_id"`
	ControlRoot          string     `json:"control_root"`
	State                string     `json:"state"`
	OperationID          string     `json:"operation_id,omitempty"`
	PendingPath          string     `json:"pending_path,omitempty"`
	RequiresFreshSession bool       `json:"requires_fresh_session"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	LastUsedAt           *time.Time `json:"last_used_at,omitempty"`
}

type Operation struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	ProjectID   string     `json:"project_id,omitempty"`
	WorkspaceID string     `json:"workspace_id,omitempty"`
	State       string     `json:"state"`
	DetailCode  string     `json:"detail_code,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type TrashWorkspace struct {
	TrashID              string     `json:"trash_id"`
	WorkspaceID          string     `json:"workspace_id"`
	ProjectID            string     `json:"project_id"`
	TeamName             string     `json:"team_name"`
	ContextScopeID       string     `json:"context_scope_id"`
	OriginalControlRoot  string     `json:"original_control_root"`
	TrashPath            string     `json:"trash_path"`
	State                string     `json:"state"`
	OperationID          string     `json:"operation_id,omitempty"`
	RequiresFreshSession bool       `json:"requires_fresh_session"`
	DeletedAt            time.Time  `json:"deleted_at"`
	PurgeAfter           *time.Time `json:"purge_after,omitempty"`
}

type ListOptions struct {
	Selector string
}

type Registry interface {
	RegisterProject(context.Context, string) (Project, error)
	ResolveProject(context.Context, string) (Project, error)
	ResolveProjectByRoot(context.Context, string) (Project, error)
	ListProjects(context.Context, ListOptions) ([]Project, error)
	SetAlias(context.Context, string, string) error
	ClearAlias(context.Context, string) error
	RebindProject(context.Context, string, string) error
	GetWorkspace(context.Context, string, string) (Workspace, error)
	ListWorkspaces(context.Context, string) ([]Workspace, error)
	ListTrashWorkspaces(context.Context) ([]TrashWorkspace, error)
	ListIncompleteOperations(context.Context) ([]Operation, error)
	CreateWorkspace(context.Context, string, string) (Workspace, error)
	Close() error
}

type ResolveMode string

const (
	ResolveExisting ResolveMode = "existing"
	ResolveEnsure   ResolveMode = "ensure"
	ResolvePreview  ResolveMode = "preview"
)

type ResolveRequest struct {
	StartDir      string
	TeamName      string
	ExplicitExact string
	ExplicitRoot  string
	TemporaryRoot string
	Mode          ResolveMode
}

type Resolution struct {
	ProjectID      string
	WorkspaceID    string
	ContextScopeID string
	TeamName       string
	SubjectRoot    string
	ControlRoot    string
	Managed        bool
	WouldCreate    bool
}

type AmbiguousSelectorError struct {
	Selector   string
	Candidates []Project
}

func (e *AmbiguousSelectorError) Error() string {
	values := make([]string, 0, len(e.Candidates))
	for _, candidate := range e.Candidates {
		values = append(values, candidate.ID+" ("+candidate.SubjectRoot+")")
	}
	return fmt.Sprintf("%v %q: %s", ErrAmbiguous, e.Selector, strings.Join(values, ", "))
}

func (e *AmbiguousSelectorError) Unwrap() error { return ErrAmbiguous }

type CreateStage string

const (
	CreateStageReserved         CreateStage = "reserved"
	CreateStageOperationMarked  CreateStage = "operation_marked"
	CreateStageWorkspaceMarked  CreateStage = "workspace_marked"
	CreateStageRenamed          CreateStage = "renamed"
	CreateStageBeforeActivation CreateStage = "before_activation"
)

type RegistryOption func(*registryOptions)

type registryOptions struct {
	idGenerator IDGenerator
	now         func() time.Time
	createHook  func(CreateStage) error
}

func WithIDGenerator(generator IDGenerator) RegistryOption {
	return func(options *registryOptions) { options.idGenerator = generator }
}

func WithClock(now func() time.Time) RegistryOption {
	return func(options *registryOptions) { options.now = now }
}

func WithCreateHook(hook func(CreateStage) error) RegistryOption {
	return func(options *registryOptions) { options.createHook = hook }
}
