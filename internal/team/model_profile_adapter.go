package team

import "github.com/kjelly/hufu/internal/modelprofile"

// ModelContextSpecFromProfile adapts canonical model metadata to the legacy
// context admission type. Keep this conversion centralized until admission
// consumes ModelProfile directly.
func ModelContextSpecFromProfile(profile modelprofile.ModelProfile) ModelContextSpec {
	source := profile.Sources.EffectiveContext.Source
	estimated := source == modelprofile.SourceFallback
	estimator := profile.Estimator
	if estimator == "" {
		estimator = profile.Family
	}
	if estimator == "" && estimated {
		estimator = "estimated"
	}
	return ModelContextSpec{
		ModelID:             profile.ModelID,
		ContextWindow:       profile.EffectiveContext,
		ContextWindowSource: string(source),
		MaxOutputTokens:     profile.MaxOutputTokens,
		Estimator:           estimator,
		IsEstimated:         estimated,
	}
}
