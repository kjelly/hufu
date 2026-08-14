package team

import (
	"context"
	"fmt"
	"strings"
)

// ExperienceProcessor owns candidate lifecycle orchestration. Storage stays
// in the canonical context repository and worker-memory service; this seam
// deliberately does not introduce a competing candidate type or table.
type ExperienceProcessor interface {
	Prepare(context.Context, RunFinalizationInput) error
	Finalize(context.Context, RunFinalizationInput, CompletionGateDecision) error
}

type defaultExperienceProcessor struct{ c *Coordinator }

func (p *defaultExperienceProcessor) Prepare(ctx context.Context, input RunFinalizationInput) error {
	if p == nil || p.c == nil || p.c.session == nil {
		return nil
	}
	// Extraction can create only candidate records. CompletionGate's decision
	// is intentionally unavailable here, so this operation cannot confirm
	// prompt-visible knowledge by itself.
	if p.c.contextRepo != nil {
		p.c.autoExtractCanonicalLTM(ctx, input.RunID)
		return nil
	}
	// Legacy fallback: extract only from verified/completed task results in
	// the immutable finalization input, never from mutable Markdown scratchpad.
	for _, task := range input.Tasks {
		if task.Status == TaskDone && task.TypedResult != nil && task.TypedResult.Summary != "" {
			p.c.persistKnowledgeCandidate(task.TypedResult.Summary, ltmSectionPatterns, "AutoExtractLTM")
		}
	}
	return nil
}

func (p *defaultExperienceProcessor) Finalize(ctx context.Context, input RunFinalizationInput, decision CompletionGateDecision) error {
	if p == nil || p.c == nil {
		return nil
	}
	manifest := input.Evidence
	if decision.Accepted {
		var errs []string
		if err := p.c.confirmWorkerMemoryCandidates(ctx, manifest); err != nil {
			if rErr := p.c.rejectWorkerMemoryCandidates(ctx, manifest, "accepted manifest could not confirm private candidates: "+err.Error()); rErr != nil {
				errs = append(errs, "reject worker candidates: "+rErr.Error())
			}
			if rErr := p.c.rejectSharedMemoryCandidates(ctx, manifest, "accepted manifest could not confirm private candidates: "+err.Error()); rErr != nil {
				errs = append(errs, "reject shared candidates: "+rErr.Error())
			}
			if rErr := p.c.rejectRunSharedContextCandidates(ctx, manifest, "accepted manifest could not confirm private candidates: "+err.Error()); rErr != nil {
				errs = append(errs, "reject run shared context: "+rErr.Error())
			}
			errs = append(errs, "confirm worker candidates: "+err.Error())
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		if err := p.c.confirmSharedMemoryCandidates(ctx, manifest); err != nil {
			if rErr := p.c.rejectWorkerMemoryCandidates(ctx, manifest, "accepted manifest could not confirm shared candidates: "+err.Error()); rErr != nil {
				errs = append(errs, "reject worker candidates: "+rErr.Error())
			}
			if rErr := p.c.rejectSharedMemoryCandidates(ctx, manifest, "accepted manifest could not confirm shared candidates: "+err.Error()); rErr != nil {
				errs = append(errs, "reject shared candidates: "+rErr.Error())
			}
			if rErr := p.c.rejectRunSharedContextCandidates(ctx, manifest, "accepted manifest could not confirm shared candidates: "+err.Error()); rErr != nil {
				errs = append(errs, "reject run shared context: "+rErr.Error())
			}
			errs = append(errs, "confirm shared candidates: "+err.Error())
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		// Run-produced shared session context is promoted only here, from the
		// accepted finalizer, so a failed run's records can never become
		// prompt-visible knowledge.
		if err := p.c.confirmRunSharedContextCandidates(ctx, manifest); err != nil {
			if rErr := p.c.rejectRunSharedContextCandidates(ctx, manifest, "accepted manifest could not confirm run shared context: "+err.Error()); rErr != nil {
				errs = append(errs, "reject run shared context: "+rErr.Error())
			}
			errs = append(errs, "confirm run shared context: "+err.Error())
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		if p.c.contextRepo == nil {
			if err := p.c.bindCandidateLessonsToManifest(manifest); err != nil {
				if rErr := p.c.rejectWorkerMemoryCandidates(ctx, manifest, "legacy candidate manifest binding failed: "+err.Error()); rErr != nil {
					errs = append(errs, "reject worker candidates: "+rErr.Error())
				}
				errs = append(errs, "bind legacy candidates: "+err.Error())
				return fmt.Errorf("%s", strings.Join(errs, "; "))
			}
			p.c.promoteCandidateLessons(manifest)
		}
		return nil
	}
	reason := strings.Join(decision.Reasons, "; ")
	if reason == "" {
		reason = "run did not pass completion gate"
	}
	var errs []string
	if err := p.c.rejectWorkerMemoryCandidates(ctx, manifest, reason); err != nil {
		errs = append(errs, "reject worker candidates: "+err.Error())
	}
	if err := p.c.rejectSharedMemoryCandidates(ctx, manifest, reason); err != nil {
		errs = append(errs, "reject shared candidates: "+err.Error())
	}
	if err := p.c.rejectRunSharedContextCandidates(ctx, manifest, reason); err != nil {
		errs = append(errs, "reject run shared context: "+err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("finalize rejected run: %s", strings.Join(errs, "; "))
	}
	return nil
}
