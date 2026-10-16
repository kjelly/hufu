package main

import (
	"fmt"
	"io"

	"github.com/kjelly/hufu/internal/belief"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func renderInspectDiagnosticEpisodes(writer io.Writer, data inspectpkg.BeliefsData) error {
	for _, episode := range data.Episodes {
		if _, err := fmt.Fprintf(writer, "Episode %s: task=%s mode=%s source=%s status=%s validation=%s\n  Fixture: %s digest=%s\n  Model: %s policy=%s\n  Revisions: subject=%s source=%s environment=%s\n  Cursor: %s hash=%s events=%d replay=%s\n", safeOverviewValue(episode.Epoch.ID), safeOverviewValue(episode.Epoch.Scope.TaskID), safeOverviewValue(episode.Mode), safeOverviewValue(episode.Source), safeOverviewValue(episode.Status), safeOverviewValue(episode.Validation), safeOverviewValue(episode.FixtureRef.ID), safeOverviewValue(episode.FixtureDigest), safeOverviewValue(episode.ModelDigest), safeOverviewValue(episode.PolicyDigest), safeOverviewValue(episode.Revisions.Subject), safeOverviewValue(episode.Revisions.Source), safeOverviewValue(episode.Revisions.Environment), safeOverviewValue(episode.CursorID), safeOverviewValue(episode.CursorHash), episode.EventCount, safeOverviewValue(valueOrUnavailable(episode.ReplayDigest))); err != nil {
			return err
		}
		for _, decision := range episode.Decisions {
			if _, err := fmt.Fprintf(writer, "  Shadow: generation=%d candidate=%s stop=%s state=%s\n", decision.Generation, safeOverviewValue(valueOrUnavailable(decision.Choice.CandidateID)), safeOverviewValue(valueOrUnavailable(decision.Choice.StopReason)), safeOverviewValue(decision.StateDigest)); err != nil {
				return err
			}
			for _, score := range decision.Choice.Scores {
				if _, err := fmt.Fprintf(writer, "    Score: %s gain_per_cost=%s/%s cost=%d excluded=%s\n", safeOverviewValue(score.CandidateID), safeOverviewValue(score.Numerator), safeOverviewValue(score.Denominator), score.CostUnits, safeOverviewValue(valueOrUnavailable(score.Excluded))); err != nil {
					return err
				}
			}
		}
		for _, observation := range episode.Observations {
			if _, err := fmt.Fprintf(writer, "  Fixture observation: %s experiment=%s outcome=%s measurement=%s\n", safeOverviewValue(observation.ID), safeOverviewValue(observation.ExperimentID), safeOverviewValue(observation.OutcomeID), safeOverviewValue(observation.SourceID)); err != nil {
				return err
			}
		}
		if episode.State == nil {
			if _, err := fmt.Fprintf(writer, "  Belief: unavailable (%s); decisions above are historical records\n", safeOverviewValue(episode.Unavailable)); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(writer, "  Belief: %s digest=%s synthetic=%t\n", safeOverviewValue(string(episode.State.Status)), safeOverviewValue(episode.State.Digest), episode.State.Synthetic); err != nil {
			return err
		}
		if episode.State.Distribution != nil {
			for _, mass := range *episode.State.Distribution {
				if _, err := fmt.Fprintf(writer, "  %s: %d.%09d\n", safeOverviewValue(mass.HypothesisID), mass.Units/belief.Scale, mass.Units%belief.Scale); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
