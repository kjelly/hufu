package team

import (
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/decisionrt/settings"
	"github.com/kjelly/hufu/internal/semantic"
	"github.com/kjelly/hufu/internal/utils"
)

// prepareRecoveryValidation installs verification-only catalogs on a fresh
// dry-run projection. General planning previews retain their existing behavior;
// recovery must prove full compatibility with normal startup policy instead.
func (c *Coordinator) prepareRecoveryValidation() error {
	session := c.session
	var err error
	// An unbound lazy resolver supplies the same model identity as startup;
	// it cannot construct a sidecar or execute a decision.
	resolver := &decisionSidecarResolver{defaultModel: c.sidecarModel}
	// Compile verification-only catalogs with an unbound generator identity.
	// No provider transport is installed; hashes match normal runtime startup.
	c.decisionPrimitives, err = catalog.New(session.Config.DecisionPrimitives, catalog.WithRuntimeConfig(settings.Merge(session.GlobalDecisionRuntime, session.Config.DecisionRuntime)), catalog.WithSidecarGenerator(resolver.generator))
	if err != nil {
		return err
	}
	c.controlDecisions, err = control.New(control.Merge(session.GlobalControlDecisions, session.Config.ControlDecisions), utils.RedactSecrets, control.WithRuntimeConfig(settings.Merge(session.GlobalDecisionRuntime, session.Config.DecisionRuntime)), control.WithSidecarGenerator(resolver.generator))
	if err != nil {
		return err
	}
	c.semanticSettings, err = semantic.Merge(session.GlobalSemanticRetrieval, session.Config.SemanticRetrieval).Resolve()
	if err != nil {
		return err
	}

	c.executionPolicy, err = newExecutionPolicyState(c)
	return err
}

func validateRecoveryProofChecks(a *ResultCollectionRecoveryAssessment) error {
	// Validate each saved revision against its original proof requirements;
	// replays must never rerun today's evaluator against historical claims.
	required := map[string]bool{"target": false, "lineage": false, "canonical_replay": false, "work_sources_policy": false, "settled_decisions": false, "transcript": false}
	if a.ValidatorRevision == 2 {
		if a.TaskID != a.Checkpoint.TaskID || a.BranchID != a.Checkpoint.BranchID || a.SourceRunID != a.Checkpoint.SourceRunID {
			return errResultCollectionStale
		}
		for _, code := range []string{"canonical_target", "terminal_context", "context_manifest"} {
			required[code] = false
		}
		if a.Checkpoint.State != ResultCollectionCommitted && (a.Checkpoint.Prepared != nil || a.Checkpoint.Response != nil) {
			required["independent_claims"] = false
		}
	}
	for _, check := range a.Checks {
		if a.ValidatorRevision == 2 && check.Code == "canonical_claims" && !check.Passed && a.Disposition != RecoveryRepairClaims {
			return errResultCollectionStale
		}
		if !check.Passed && check.Code != "canonical_claims" {
			return errResultCollectionStale
		}
		if _, needed := required[check.Code]; needed && check.Passed {
			required[check.Code] = true
		}
	}
	for _, passed := range required {
		if !passed {
			return errResultCollectionStale
		}
	}
	return nil
}
