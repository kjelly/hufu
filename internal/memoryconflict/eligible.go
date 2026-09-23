// Package memoryconflict finds candidate pairs of persistent memories that
// may contradict each other and records a model judgment for each pair. It
// never changes a memory: resolution stays with the operator (supersede or
// dismiss).
package memoryconflict

import (
	"slices"
	"strings"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

// KnowledgeKinds are the only kinds compared for contradictions.
var KnowledgeKinds = []contextstore.ContextKind{
	contextstore.ContextDecision, contextstore.ContextConvention,
	contextstore.ContextArchitecture, contextstore.ContextPattern,
	contextstore.ContextInstruction, contextstore.ContextRequirement,
}

// Diagnostic explains an item, pair, or scan-level condition that was
// skipped. It never carries memory content.
type Diagnostic struct {
	ItemID string `json:"item_id,omitempty"`
	PairID string `json:"pair_id,omitempty"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

const (
	ReasonSecretLikeContent = "secret_like_content"
	ReasonFilePathTooCommon = "file_path_too_common"
	ReasonVectorUnavailable = "vector_unavailable"
	ReasonInputTooLarge     = "input_too_large"
	ReasonInvalidJudgment   = "invalid_judgment"
)

// Eligible reports whether item is compared for contradictions. A
// secret-like item is excluded with reason secret_like_content; every other
// exclusion has an empty reason.
func Eligible(item contextstore.ContextItem, projectID, teamID string, now time.Time) (bool, string) {
	if item.Scope.ProjectID != projectID || item.Scope.TeamID != teamID || !contextstore.IsCurrentPersistentKnowledge(item, now) {
		return false, ""
	}
	if !slices.Contains(KnowledgeKinds, item.Kind) {
		return false, ""
	}
	if utils.RedactSecrets(item.Content) != item.Content || strings.Contains(item.Content, "[REDACTED]") || strings.Contains(item.Content, "<REDACTED:") {
		return false, ReasonSecretLikeContent
	}
	return true, ""
}
