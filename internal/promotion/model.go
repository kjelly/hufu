package promotion

import contextstore "github.com/kjelly/hufu/internal/context"

type Type = contextstore.PromotionType
type Status = contextstore.PromotionStatus
type SourceSnapshot = contextstore.PromotionSourceSnapshot
type Metrics = contextstore.PromotionMetrics
type Proposal = contextstore.PromotionProposal

const (
	TypeSkill       = contextstore.PromotionTypeSkill
	TypeTeamPolicy  = contextstore.PromotionTypeTeamPolicy
	TypeAgentPolicy = contextstore.PromotionTypeAgentPolicy
	StatusProposed  = contextstore.PromotionStatusProposed
	StatusApproved  = contextstore.PromotionStatusApproved
	StatusRejected  = contextstore.PromotionStatusRejected
	StatusApplied   = contextstore.PromotionStatusApplied
	StatusStale     = contextstore.PromotionStatusStale
)

type EligibleSource struct {
	Item         contextstore.ContextItem         `json:"item"`
	Aggregate    contextstore.ExperienceAggregate `json:"aggregate"`
	AllowedTypes []Type                           `json:"allowed_types"`
}

type Diagnostic struct {
	SourceID string `json:"source_id"`
	Reason   string `json:"reason"`
}
