package memoryconflict

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// maxConsecutiveInvalid stops a scan whose judge keeps returning invalid
// output instead of spending the whole pair budget on it.
const maxConsecutiveInvalid = 3

type ScanRepository interface {
	Iterate(context.Context, contextstore.RepositoryQuery, func(contextstore.ContextItem) error) error
	LookupPairJudgment(context.Context, string) (contextstore.PairJudgment, bool, error)
	SavePairJudgment(context.Context, contextstore.PairJudgment, string, bool) (contextstore.PairJudgment, bool, error)
}

type ScanOptions struct {
	ProjectID, TeamID string
	MaxPairs          int // judge calls per scan; default 20
	MaxPromptRunes    int // 0 disables the check
	DryRun            bool
	RetryUndetermined bool
	JudgeModel, Actor string
}

type ScanReport struct {
	EligibleItems  int          `json:"eligible_items"`
	CandidatePairs int          `json:"candidate_pairs"`
	LexicalPairs   int          `json:"lexical_pairs"`
	FilePathPairs  int          `json:"file_path_pairs"`
	VectorPairs    int          `json:"vector_pairs"`
	VectorUsed     bool         `json:"vector_used"`
	AlreadyJudged  int          `json:"already_judged"`
	Judged         int          `json:"judged"`
	Contradictions int          `json:"contradictions"`
	Undetermined   int          `json:"undetermined"`
	Skipped        int          `json:"skipped"`
	Deferred       int          `json:"deferred"`
	DryRun         bool         `json:"dry_run"`
	Diagnostics    []Diagnostic `json:"diagnostics"`
}

// Scan judges new candidate pairs and persists each judgment on its own. A
// transport error stops the scan and keeps committed judgments; invalid judge
// output is stored as undetermined so the pair is not retried every scan.
func Scan(ctx context.Context, repo ScanRepository, judge Judge, opts ScanOptions, pairOpts PairOptions) (ScanReport, error) {
	if opts.MaxPairs <= 0 {
		opts.MaxPairs = 20
	}
	report := ScanReport{DryRun: opts.DryRun, Diagnostics: []Diagnostic{}}
	now := time.Now()
	var items []contextstore.ContextItem
	err := repo.Iterate(ctx, contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: opts.ProjectID, TeamID: opts.TeamID}, Visibility: contextstore.VisibilitySubtree}, func(item contextstore.ContextItem) error {
		ok, reason := Eligible(item, opts.ProjectID, opts.TeamID, now)
		if reason != "" {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{ItemID: item.ID, Reason: reason})
		}
		if ok {
			items = append(items, item)
		}
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("load memories for conflict scan: %w", err)
	}
	report.EligibleItems = len(items)
	candidates, err := CandidatePairs(ctx, items, pairOpts)
	if err != nil {
		return report, err
	}
	report.CandidatePairs = len(candidates.Pairs)
	report.LexicalPairs, report.FilePathPairs, report.VectorPairs = candidates.LexicalPairs, candidates.FilePathPairs, candidates.VectorPairs
	report.VectorUsed = candidates.VectorUsed
	report.Diagnostics = append(report.Diagnostics, candidates.Diagnostics...)

	consecutiveInvalid := 0
	for _, pair := range candidates.Pairs {
		j := contextstore.PairJudgment{
			ProjectID: opts.ProjectID, TeamID: opts.TeamID, AgentID: pair.A.Scope.AgentID,
			ItemAID: pair.A.ID, ItemBID: pair.B.ID, ItemAContentHash: pair.A.ContentHash, ItemBContentHash: pair.B.ContentHash,
			JudgePolicyVersion: contextstore.CurrentConflictJudgePolicyVersion, JudgeModel: opts.JudgeModel,
		}
		contextstore.NormalizePairJudgment(&j)
		existing, found, lookupErr := repo.LookupPairJudgment(ctx, j.ID)
		if lookupErr != nil {
			return report, lookupErr
		}
		if found && (!opts.RetryUndetermined || existing.Verdict != contextstore.PairVerdictUndetermined) {
			report.AlreadyJudged++
			continue
		}
		if opts.MaxPromptRunes > 0 && utf8.RuneCountInString(judge.Prompt(pair.A, pair.B)) > opts.MaxPromptRunes {
			report.Skipped++
			report.Diagnostics = append(report.Diagnostics, Diagnostic{PairID: j.ID, Reason: ReasonInputTooLarge})
			continue
		}
		if report.Judged >= opts.MaxPairs {
			report.Deferred++
			continue
		}
		report.Judged++
		if opts.DryRun {
			continue
		}
		judgment, judgeErr := judge.Judge(ctx, pair.A, pair.B)
		if judgeErr != nil && !errors.Is(judgeErr, ErrInvalidJudgment) {
			return report, fmt.Errorf("judge memory pair %s: %w", j.ID, judgeErr)
		}
		if judgeErr != nil {
			j.Verdict = contextstore.PairVerdictUndetermined
			report.Undetermined++
			report.Diagnostics = append(report.Diagnostics, Diagnostic{PairID: j.ID, Reason: ReasonInvalidJudgment})
			consecutiveInvalid++
		} else {
			j.Verdict, j.Rationale = judgment.Verdict, judgment.Rationale
			consecutiveInvalid = 0
		}
		if _, _, saveErr := repo.SavePairJudgment(ctx, j, opts.Actor, true); saveErr != nil {
			return report, saveErr
		}
		if j.Verdict == contextstore.PairVerdictContradicts {
			report.Contradictions++
		}
		if consecutiveInvalid >= maxConsecutiveInvalid {
			return report, fmt.Errorf("conflict judge returned invalid output for %d consecutive pairs; last: %w", consecutiveInvalid, judgeErr)
		}
	}
	return report, nil
}
