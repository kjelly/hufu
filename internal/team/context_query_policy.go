package team

import (
	"context"
	json "encoding/json/v2"
	"fmt"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// ExecutionContextQueryPolicySnapshot pins the bounded algorithm. Nil means
// legacy, including snapshots written before query routing was introduced.
type ExecutionContextQueryPolicySnapshot struct {
	Mode          string                   `json:"mode"`
	PolicyVersion string                   `json:"policy_version"`
	Limits        ContextQueryLimits       `json:"limits"`
	Fusion        ContextQueryFusionPolicy `json:"fusion"`
	PolicyHash    string                   `json:"policy_hash"`
}

type ContextQueryFusionPolicy struct {
	PrimaryWeight      float64 `json:"primary_weight"`
	SupplementalWeight float64 `json:"supplemental_weight"`
	RankConstant       int     `json:"rank_constant"`
	ScoreScale         float64 `json:"score_scale"`
}

type ContextQueryLimits struct {
	ConstraintRunes   int   `json:"constraint_runes"`
	VerificationRunes int   `json:"verification_runes"`
	DependencyRunes   int   `json:"dependency_runes"`
	Dependencies      int   `json:"dependencies"`
	Paths             int   `json:"paths"`
	PathRunes         int   `json:"path_runes"`
	SupplementalRunes int   `json:"supplemental_runes"`
	TotalRunes        int   `json:"total_runes"`
	ExactTerms        int   `json:"exact_terms"`
	SourceRows        int   `json:"source_rows"`
	Scopes            int   `json:"scopes"`
	TimeoutMS         int64 `json:"timeout_ms"`
}

func activeContextQueryPolicy() *ExecutionContextQueryPolicySnapshot {
	p := &ExecutionContextQueryPolicySnapshot{Mode: "active", PolicyVersion: ContextQueryPolicyVersion,
		Limits: ContextQueryLimits{ConstraintRunes: queryConstraintRunes, VerificationRunes: queryVerificationRunes,
			DependencyRunes: queryDependencyRunes, Dependencies: queryDependencyCount, Paths: queryPathCount,
			PathRunes: queryPathRunes, SupplementalRunes: querySupplementalRunes, TotalRunes: queryTotalRunes,
			ExactTerms: contextstore.SupplementalMaxExactTerms, SourceRows: contextstore.SupplementalMaxSourceRows,
			Scopes: 3, TimeoutMS: contextQuerySupplementalTimeout.Milliseconds()},
		Fusion: ContextQueryFusionPolicy{PrimaryWeight: contextQueryPrimaryWeight, SupplementalWeight: contextQuerySupplementalWeight,
			RankConstant: contextQueryRankConstant, ScoreScale: contextQueryScoreScale}}
	data, err := json.Marshal(p, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	p.PolicyHash = contextQueryDigest(string(data))
	return p
}

func contextQueryPolicyForMode(mode string) *ExecutionContextQueryPolicySnapshot {
	if mode == "active" {
		return activeContextQueryPolicy()
	}
	return nil
}

func validateContextQueryPolicy(p *ExecutionContextQueryPolicySnapshot) error {
	if p != nil && *p != *activeContextQueryPolicy() {
		return fmt.Errorf("execution context query policy version, limits or hash is unsupported")
	}
	return nil
}

func (c *Coordinator) withContextQueryPolicy(ctx context.Context) context.Context {
	if _, explicit := ctx.Value(contextQueryEvaluationKey{}).(string); explicit {
		return ctx
	}
	if c != nil && c.executionPolicy != nil && c.executionPolicy.snapshot.ContextQuery != nil {
		return context.WithValue(ctx, contextQueryEvaluationKey{}, "candidate")
	}
	return ctx
}
