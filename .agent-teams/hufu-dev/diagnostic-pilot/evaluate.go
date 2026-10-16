package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/belief"
	"github.com/kjelly/hufu/internal/team"
)

type offlineTrace struct {
	FixtureRef   string                     `json:"fixture_ref"`
	Algorithm    string                     `json:"algorithm"`
	ModelDigest  string                     `json:"model_digest"`
	PolicyDigest string                     `json:"policy_digest"`
	Revisions    belief.DiagnosticRevisions `json:"revisions"`
	Actions      []string                   `json:"actions"`
	Cost         uint64                     `json:"cost_units"`
	StopReason   string                     `json:"stop_reason"`
	Distribution []belief.Mass              `json:"distribution"`
	Decisions    []belief.DiagnosticChoice  `json:"decisions"`
	Digest       string                     `json:"digest"`
}

type strategySummary struct {
	ExpandedActions uint64         `json:"expanded_actions"`
	ExpandedCost    uint64         `json:"expanded_cost_units"`
	Stops           map[string]int `json:"stops"`
}

type evaluationReport struct {
	SchemaVersion int                        `json:"schema_version"`
	Synthetic     bool                       `json:"synthetic"`
	Claim         string                     `json:"claim"`
	Cases         int                        `json:"cases"`
	CorpusDigest  string                     `json:"corpus_digest"`
	ModelDigest   string                     `json:"model_digest"`
	PolicyDigest  string                     `json:"policy_digest"`
	Provenance    string                     `json:"provenance_digest"`
	Provider      string                     `json:"provider_source_digest"`
	Generator     string                     `json:"generator_source_digest"`
	Adapter       string                     `json:"adapter_artifact_digest"`
	Traces        map[string]offlineTrace    `json:"traces"`
	Strategies    map[string]strategySummary `json:"strategies"`
}

func loadOfflineInputs() (*fixtureProvider, belief.Model, belief.DiagnosticPolicy, error) {
	var bundles [][]byte
	for _, name := range []string{"pass-pass", "pass-fail", "fail-pass", "fail-fail"} {
		data, err := resources.ReadFile("fixtures/" + name + ".json")
		if err != nil {
			return nil, belief.Model{}, belief.DiagnosticPolicy{}, err
		}
		bundles = append(bundles, data)
	}
	p, err := newFixtureProvider(bundles...)
	if err != nil {
		return nil, belief.Model{}, belief.DiagnosticPolicy{}, err
	}
	data, err := resources.ReadFile("model.json")
	if err != nil {
		return nil, belief.Model{}, belief.DiagnosticPolicy{}, err
	}
	model, err := belief.DecodeModel(data)
	if err != nil {
		return nil, model, belief.DiagnosticPolicy{}, err
	}
	data, err = resources.ReadFile("policy.json")
	if err != nil {
		return nil, model, belief.DiagnosticPolicy{}, err
	}
	policy, err := belief.DecodeDiagnosticPolicy(data)
	return p, model, policy, err
}

func runOfflineTrace(ctx context.Context, p *fixtureProvider, ref string, model belief.Model, policy belief.DiagnosticPolicy) (offlineTrace, error) {
	trace := offlineTrace{FixtureRef: ref, Algorithm: policy.Algorithm, Actions: []string{}, Decisions: []belief.DiagnosticChoice{}}
	f, ok := p.bundles[ref]
	if !ok {
		return trace, fmt.Errorf("unregistered trace fixture")
	}
	modelDigest, err := belief.ModelDigest(model)
	if err != nil {
		return trace, err
	}
	trace.ModelDigest = modelDigest
	trace.PolicyDigest, err = belief.DiagnosticPolicyDigest(policy)
	if err != nil {
		return trace, err
	}
	sourceDigest, err := contentDigest("diagnostic-source-v1", f.Vectors)
	if err != nil {
		return trace, err
	}
	providerDigest, err := embeddedDigest("fixture.go")
	if err != nil {
		return trace, err
	}
	adapterDigest, err := embeddedDigest("adapter.json")
	if err != nil {
		return trace, err
	}
	environmentDigest, err := contentDigest("diagnostic-environment-v1", struct {
		Manifest environment `json:"environment"`
		Provider string      `json:"provider_version"`
		Source   string      `json:"provider_source_digest"`
		Adapter  string      `json:"adapter_artifact_digest"`
	}{f.Environment, "fixture_design_v1", providerDigest, adapterDigest})
	if err != nil {
		return trace, err
	}
	revisions := belief.DiagnosticRevisions{Subject: ref, Source: sourceDigest, Environment: environmentDigest}
	trace.Revisions = revisions
	epoch := belief.Epoch{ID: "offline-" + ref, ModelDigest: modelDigest, Scope: belief.Scope{RunID: "offline", BranchID: "offline", TaskID: "owner", Attempt: 1}, SubjectRevision: ref, EnvironmentRevision: environmentDigest}
	observations := []belief.Observation{}
	for {
		if err := ctx.Err(); err != nil {
			return trace, err
		}
		choice, err := belief.RankDiagnostic(model, epoch, observations, policy)
		if err != nil {
			return trace, err
		}
		trace.Decisions = append(trace.Decisions, choice)
		if choice.StopReason != "" {
			trace.StopReason = choice.StopReason
			break
		}
		index := slices.IndexFunc(policy.Candidates, func(c belief.DiagnosticCandidate) bool { return c.ID == choice.CandidateID })
		candidate := policy.Candidates[index]
		if len(trace.Actions) >= policy.Limits.MaxAttempts || trace.Cost+candidate.CostUnits > policy.Limits.MaxCostUnits {
			trace.StopReason = "budget_exhausted"
			break
		}
		input := probeInput{FixtureRef: ref, ExperimentID: candidate.ExperimentID}
		payload, err := json.Marshal(input)
		if err != nil {
			return trace, err
		}
		result, err := p.Execute(ctx, team.Action{Capability: fixtureCapability, Type: candidate.ExperimentID, Payload: string(payload)})
		if err != nil {
			return trace, err
		}
		envelope, ok := result.(team.ActionResult)
		if !ok {
			return trace, fmt.Errorf("invalid action result envelope")
		}
		m, ok := envelope.Outputs["measurement"].(measurement)
		if !ok {
			return trace, fmt.Errorf("missing typed measurement")
		}
		if err := verifyMeasurement(f, input, m); err != nil {
			return trace, err
		}
		// These are explicitly synthetic harness identities, never runtime events.
		expected := belief.DiagnosticSource{EpisodeID: epoch.ID, DecisionID: candidate.ID, ExecutionID: "execution-" + candidate.ID, ChildTaskID: "child-" + candidate.ID, Occurrence: 1, InvocationID: "invocation-" + candidate.ID, ReceiptKey: "receipt-" + candidate.ID, EventID: "event-" + candidate.ID, EventHash: m.Digest, Cursor: uint64(len(observations) + 1), Adapter: "diagnostic_measurement_v1", ExperimentID: candidate.ExperimentID}
		observation, err := belief.CheckDiagnosticEvidence(epoch, expected, revisions, belief.DiagnosticEvidence{SchemaVersion: 1, Source: expected, Revisions: revisions, Outcome: m.Outcome, Measurement: m.Digest})
		if err != nil {
			return trace, err
		}
		before, err := belief.Project(model, epoch, observations)
		if err != nil {
			return trace, err
		}
		observations = append(observations, observation)
		state, err := belief.Project(model, epoch, observations)
		if err != nil {
			return trace, err
		}
		if state.Distribution == nil {
			return trace, fmt.Errorf("offline projection unavailable")
		}
		trace.Actions = append(trace.Actions, candidate.ID)
		trace.Cost += candidate.CostUnits
		trace.Distribution = *state.Distribution
		if slices.Equal(*before.Distribution, *state.Distribution) {
			trace.StopReason = "no_progress"
			break
		}
	}
	trace.Digest, err = contentDigest("diagnostic-offline-trace-v1", trace)
	return trace, err
}

type pairedCase struct {
	CaseID          int    `json:"case_id"`
	Oracle          string `json:"oracle"`
	SourceSeed      int    `json:"seed_source"`
	EnvironmentSeed int    `json:"seed_environment"`
	GiniTrace       string `json:"gini_trace"`
	BaselineTrace   string `json:"baseline_trace"`
}

// evaluate expands a finite paired corpus using eight cached deterministic
// traces. This avoids pretending to perform 400,000 distinct measurements.
// Optional JSONL rows refer to the full traces included in the report.
func evaluate(ctx context.Context, rows io.Writer) (evaluationReport, error) {
	report := evaluationReport{SchemaVersion: 1, Synthetic: true, Claim: "Synthetic mechanism check only; no real-world accuracy, calibration or savings claim.", Traces: map[string]offlineTrace{}, Strategies: map[string]strategySummary{}}
	p, model, policy, err := loadOfflineInputs()
	if err != nil {
		return report, err
	}
	report.ModelDigest, err = belief.ModelDigest(model)
	if err != nil {
		return report, err
	}
	report.PolicyDigest, err = belief.DiagnosticPolicyDigest(policy)
	if err != nil {
		return report, err
	}
	provenance, err := resources.ReadFile("provenance.json")
	if err != nil {
		return report, err
	}
	sum := sha256.Sum256(provenance)
	report.Provenance = hex.EncodeToString(sum[:])
	for name, target := range map[string]*string{"fixture.go": &report.Provider, "evaluate.go": &report.Generator, "adapter.json": &report.Adapter} {
		*target, err = embeddedDigest(name)
		if err != nil {
			return report, err
		}
	}
	pairs := map[string][2]offlineTrace{}
	for ref, f := range p.bundles {
		gini, err := runOfflineTrace(ctx, p, ref, model, policy)
		if err != nil {
			return report, err
		}
		baselinePolicy := policy
		baselinePolicy.Algorithm = belief.DiagnosticFixedOrder
		baseline, err := runOfflineTrace(ctx, p, ref, model, baselinePolicy)
		if err != nil {
			return report, err
		}
		if !slices.Equal(gini.Distribution, baseline.Distribution) || gini.Cost != baseline.Cost || len(gini.Actions) != 2 || len(baseline.Actions) != 2 {
			return report, fmt.Errorf("paired baseline invariant failed")
		}
		key := "pass-pass"
		if f.Vectors[0].Actual != f.Vectors[0].Expected {
			key = "fail-pass"
		}
		if f.Environment.Required != f.Environment.Observed {
			key = stringsReplaceLastPass(key)
		}
		pairs[key] = [2]offlineTrace{gini, baseline}
		report.Traces[gini.Digest], report.Traces[baseline.Digest] = gini, baseline
	}
	for _, algorithm := range []string{belief.DiagnosticGiniAlgorithm, belief.DiagnosticFixedOrder} {
		report.Strategies[algorithm] = strategySummary{Stops: map[string]int{}}
	}
	corpusHash := sha256.New()
	output := io.Writer(corpusHash)
	if rows != nil {
		output = io.MultiWriter(corpusHash, rows)
	}
	// Independent seed grids, repeated in the declared 5:3:2 prior ratio.
	for _, latent := range []struct {
		id                           string
		repeats, source, environment int
	}{{"source_fault", 5, 90, 10}, {"environment_fault", 3, 10, 90}, {"other_fault", 2, 20, 20}} {
		for range latent.repeats {
			for sourceSeed := range 100 {
				for environmentSeed := range 100 {
					if err := ctx.Err(); err != nil {
						return report, err
					}
					key := "pass-pass"
					if sourceSeed < latent.source {
						key = "fail-pass"
					}
					if environmentSeed < latent.environment {
						key = stringsReplaceLastPass(key)
					}
					pair := pairs[key]
					row := pairedCase{report.Cases, latent.id, sourceSeed, environmentSeed, pair[0].Digest, pair[1].Digest}
					data, err := json.Marshal(row, json.Deterministic(true))
					if err != nil {
						return report, err
					}
					if _, err := output.Write(append(data, '\n')); err != nil {
						return report, err
					}
					report.Cases++
					for _, trace := range pair {
						summary := report.Strategies[trace.Algorithm]
						summary.ExpandedActions += uint64(len(trace.Actions))
						summary.ExpandedCost += trace.Cost
						summary.Stops[trace.StopReason]++
						report.Strategies[trace.Algorithm] = summary
					}
				}
			}
		}
	}
	report.CorpusDigest = hex.EncodeToString(corpusHash.Sum(nil))
	return report, nil
}

func stringsReplaceLastPass(key string) string {
	prefix, _ := strings.CutSuffix(key, "pass")
	return prefix + "fail"
}
