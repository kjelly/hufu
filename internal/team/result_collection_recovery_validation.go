package team

import (
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/semantic"
	"github.com/kjelly/hufu/internal/utils"
)

// prepareRecoveryValidation installs verification-only catalogs on a fresh
// dry-run projection. General planning previews retain their existing behavior;
// recovery must prove full compatibility with normal startup policy instead.
func (c *Coordinator) prepareRecoveryValidation() error {
	session := c.session
	shared, err := session.ResolveDecisionRuntime()
	if err != nil {
		return err
	}
	// An unbound lazy resolver supplies the same model identity as startup;
	// it cannot construct a sidecar or execute a decision.
	resolver := &decisionSidecarResolver{defaultModel: c.sidecarModel}
	// Compile verification-only catalogs with an unbound generator identity.
	// No provider transport is installed; hashes match normal runtime startup.
	c.decisionPrimitives, err = catalog.New(session.Config.DecisionPrimitives, catalog.WithRuntimeConfig(shared), catalog.WithSidecarGenerator(resolver.generator))
	if err != nil {
		return err
	}
	c.controlDecisions, err = control.New(control.Merge(session.GlobalControlDecisions, session.Config.ControlDecisions).PolicyOnly(), utils.RedactSecrets, control.WithRuntimeConfig(shared), control.WithSidecarGenerator(resolver.generator))
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
	if a.ValidatorRevision >= 2 {
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
	if a.ValidatorRevision >= 8 && a.Checkpoint.State != ResultCollectionCommitted && (a.Checkpoint.Prepared != nil || a.Checkpoint.Response != nil) {
		required["runtime_inputs"] = false
	}
	if a.ValidatorRevision >= 10 && acceptedRepairProof(a.Checkpoint) != nil && a.Checkpoint.State != ResultCollectionCommitted {
		required["accepted_repair_source"], required["accepted_repair_binding"], required["evidence_keys"] = false, false, false
		if a.ValidatorRevision >= 11 {
			required["claims_context"] = false
		}
		if a.ValidatorRevision >= 14 && a.Checkpoint.ContractUpgrade.SourceOrigin != nil {
			required["accepted_origin"] = false
		}
	}
	if a.ValidatorRevision >= 3 {
		required["execution_ended"] = false
	}
	if a.ValidatorRevision >= 4 {
		required["workflow_recovery"] = false
	}
	if a.ValidatorRevision >= 7 && a.Checkpoint.LegacySubmission != nil && a.Checkpoint.Response == nil {
		required["legacy_submission"] = false
	}
	if a.Disposition == RecoverySelectReferences {
		required["reference_selection"], required["canonical_claims"] = false, false
		required["independent_claims"], required["runtime_inputs"] = false, false
		if a.ValidatorRevision >= 13 && len(acceptedRepairProof(a.Checkpoint).PathPointers) > 0 {
			required["workset_paths"] = false
		}
	}
	for _, check := range a.Checks {
		if a.ValidatorRevision >= 2 && check.Code == "canonical_claims" && !check.Passed && a.Disposition != RecoveryRepairClaims {
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
