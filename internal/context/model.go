// Package context implements the canonical, durable context store. It is kept
// separate from the legacy prompt assembly path so shadow writes cannot alter
// the prompt sent to a model.
package context

import "time"

type ContextKind string

const (
	ContextRequirement  ContextKind = "requirement"
	ContextInstruction  ContextKind = "instruction"
	ContextDecision     ContextKind = "decision"
	ContextProgress     ContextKind = "progress"
	ContextOpenQuestion ContextKind = "open_question"
	ContextToolCall     ContextKind = "tool_call"
	ContextToolResult   ContextKind = "tool_result"
	ContextError        ContextKind = "error"
	ContextVerification ContextKind = "verification"
	ContextArtifact     ContextKind = "artifact"
	ContextConvention   ContextKind = "convention"
	ContextArchitecture ContextKind = "architecture"
	ContextPattern      ContextKind = "pattern"
	ContextSummary      ContextKind = "summary"
	ContextObservation  ContextKind = "observation"
)

type Authority string

const (
	AuthoritySystem     Authority = "system"
	AuthorityUser       Authority = "user"
	AuthorityAgent      Authority = "agent"
	AuthorityTool       Authority = "tool"
	AuthorityRepository Authority = "repository"
	AuthorityExternal   Authority = "external"
)

type TrustLevel string

const (
	TrustTrusted   TrustLevel = "trusted"
	TrustInternal  TrustLevel = "internal"
	TrustUntrusted TrustLevel = "untrusted"
)

type Priority int

const (
	PriorityBackground Priority = 10
	PriorityLow        Priority = 25
	PriorityNormal     Priority = 50
	PriorityHigh       Priority = 75
	PriorityCritical   Priority = 100
)

// Scope follows the project/team/session/agent/task/attempt hierarchy.
// Empty child fields represent a wider scope.
type Scope struct {
	ProjectID string `json:"project_id"`
	TeamID    string `json:"team_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
}

type SourceRef struct {
	Type string `json:"type"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path,omitempty"`
}

type EvidenceRef struct {
	ItemID string `json:"item_id,omitempty"`
	Type   string `json:"type"`
	Ref    string `json:"ref,omitempty"`
}

type ContextItem struct {
	ID             string            `json:"id"`
	Kind           ContextKind       `json:"kind"`
	Content        string            `json:"content"`
	ContentHash    string            `json:"content_hash"`
	Scope          Scope             `json:"scope"`
	Authority      Authority         `json:"authority"`
	TrustLevel     TrustLevel        `json:"trust_level"`
	Priority       Priority          `json:"priority"`
	MustKeep       bool              `json:"must_keep"`
	Pinned         bool              `json:"pinned"`
	Confidence     float64           `json:"confidence"`
	Source         SourceRef         `json:"source"`
	Evidence       []EvidenceRef     `json:"evidence,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	ValidFrom      *time.Time        `json:"valid_from,omitempty"`
	ValidUntil     *time.Time        `json:"valid_until,omitempty"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	SupersededBy   string            `json:"superseded_by,omitempty"`
	EmbeddingState string            `json:"embedding_state"`
	EmbeddingModel string            `json:"embedding_model,omitempty"`
}

type ContextEdge struct {
	FromID    string            `json:"from_id"`
	Relation  string            `json:"relation"`
	ToID      string            `json:"to_id"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

type RepositoryQuery struct {
	Scope             Scope
	Kinds             []ContextKind
	IncludeSuperseded bool
	IncludeExpired    bool
	Limit             int
}

type SearchRequest struct {
	Query string
	Scope Scope
	Limit int
}
type SearchResult struct {
	Item  ContextItem
	Score float64
}
