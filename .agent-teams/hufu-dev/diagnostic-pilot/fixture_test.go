package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/belief"
	"github.com/kjelly/hufu/internal/team"
)

func TestFixtureProviderRegisteredInputsAndObjectiveVerification(t *testing.T) {
	p, _, _, err := loadOfflineInputs()
	if err != nil {
		t.Fatal(err)
	}
	for ref, f := range p.bundles {
		before, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, probe := range []string{"check_source", "check_environment"} {
			input := probeInput{ref, probe}
			payload, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			action := team.Action{Capability: fixtureCapability, Type: probe, Payload: string(payload)}
			result, err := p.Execute(t.Context(), action)
			if err != nil {
				t.Fatal(err)
			}
			m := result.(team.ActionResult).Outputs["measurement"].(measurement)
			if err := verifyMeasurement(f, input, m); err != nil {
				t.Fatal(err)
			}
			forged := m
			forged.Outcome = "unknown"
			if verifyMeasurement(f, input, forged) == nil {
				t.Fatal("trusted provider outcome without objective comparison")
			}
			forged = m
			forged.Digest = strings.Repeat("0", 64)
			if verifyMeasurement(f, input, forged) == nil {
				t.Fatal("trusted provider digest without recomputation")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := p.Execute(ctx, action); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation=%v", err)
			}
			ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
			defer cancel()
			if _, err := p.Execute(ctx, action); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout=%v", err)
			}
		}
		after, err := json.Marshal(p.bundles[ref])
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("provider modified the subject")
		}
	}
	for _, payload := range []string{
		`{"fixture_ref":"../fixture.json","experiment_id":"check_source"}`,
		`{"fixture_ref":"/etc/passwd","experiment_id":"check_source"}`,
		`{"fixture_ref":"` + strings.Repeat("0", 64) + `","experiment_id":"check_source"}`,
		`{"fixture_ref":null,"experiment_id":"check_source"}`,
		`{"experiment_id":"check_source","command":"true"}`,
	} {
		if err := p.Validate(team.Action{Capability: fixtureCapability, Type: "check_source", Payload: payload}); err == nil {
			t.Fatalf("accepted nonregistered input %s", payload)
		}
	}
}

func TestFixtureStrictSizeSchemaAndNoOracleLeak(t *testing.T) {
	data, err := resources.ReadFile("fixtures/fail-pass.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range [][2]string{
		{`"schema_version": 1`, `"schema_version": 2`},
		{`"scope": "single_fault_fixture_v1"`, `"scope": "double_fault"`},
		{`"actual": 3`, `"actual": null`},
		{`"actual": 3`, `"actual": 1000001`},
		{`"actual": 3`, `"actual": 3, "oracle": "source_fault"`},
		{`"required_version": "v1"`, `"required_version": null`},
	} {
		bad := strings.Replace(string(data), mutation[0], mutation[1], 1)
		if bad == string(data) {
			t.Fatal("mutation did not apply")
		}
		if _, err := decodeFixture([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := decodeFixture(append(data, []byte(`{}`)...)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	if _, err := decodeFixture([]byte(strings.Repeat(" ", maxBundleBytes+1))); err == nil {
		t.Fatal("accepted oversized bundle")
	}
	p, err := newFixtureProvider(data)
	if err != nil {
		t.Fatal(err)
	}
	clear(data)
	if len(p.bundles) != 1 {
		t.Fatal("registry did not retain its own immutable input")
	}
}

func TestPilotGoldenScoresAndAllOutcomePairs(t *testing.T) {
	p, model, policy, err := loadOfflineInputs()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := belief.ModelDigest(model)
	if err != nil {
		t.Fatal(err)
	}
	epoch := belief.Epoch{ID: "test", ModelDigest: digest, Scope: belief.Scope{RunID: "run", BranchID: "main", TaskID: "owner", Attempt: 1}, SubjectRevision: "subject", EnvironmentRevision: "environment"}
	choice, err := belief.RankDiagnostic(model, epoch, nil, policy)
	if err != nil || choice.CandidateID != "check_source" {
		t.Fatalf("choice=%+v %v", choice, err)
	}
	wantScores := map[string]string{"check_source": "7009/31200", "check_environment": "5521/28800"}
	for _, score := range choice.Scores {
		if score.Numerator+"/"+score.Denominator != wantScores[score.CandidateID] {
			t.Fatalf("wrong golden score: %+v", score)
		}
	}
	wants := map[string][]belief.Mass{
		"fixture_fail_pass": {{HypothesisID: "environment_fault", Units: 6818182}, {HypothesisID: "other_fault", Units: 72727273}, {HypothesisID: "source_fault", Units: 920454545}},
		"fixture_pass_fail": {{HypothesisID: "environment_fault", Units: 867857143}, {HypothesisID: "other_fault", Units: 114285714}, {HypothesisID: "source_fault", Units: 17857143}},
		"fixture_pass_pass": {{HypothesisID: "environment_fault", Units: 135000000}, {HypothesisID: "other_fault", Units: 640000000}, {HypothesisID: "source_fault", Units: 225000000}},
		"fixture_fail_fail": {{HypothesisID: "environment_fault", Units: 337500000}, {HypothesisID: "other_fault", Units: 100000000}, {HypothesisID: "source_fault", Units: 562500000}},
	}
	for ref, f := range p.bundles {
		gini, err := runOfflineTrace(t.Context(), p, ref, model, policy)
		if err != nil {
			t.Fatal(err)
		}
		baselinePolicy := policy
		baselinePolicy.Algorithm = belief.DiagnosticFixedOrder
		baseline, err := runOfflineTrace(t.Context(), p, ref, model, baselinePolicy)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(gini.Actions, []string{"check_source", "check_environment"}) || !slices.Equal(baseline.Actions, []string{"check_environment", "check_source"}) || gini.Cost != 2 || baseline.Cost != 2 {
			t.Fatalf("incorrect bounded trace: %+v %+v", gini, baseline)
		}
		if !slices.Equal(gini.Distribution, wants[f.ID]) || !slices.Equal(gini.Distribution, baseline.Distribution) {
			t.Fatalf("posterior mismatch for %s: %+v", f.ID, gini.Distribution)
		}
		wantStop := "round_limit"
		if f.ID == "fixture_fail_pass" {
			wantStop = "threshold_reached"
		}
		if gini.StopReason != wantStop || baseline.StopReason != wantStop {
			t.Fatalf("wrong stop precedence: %+v %+v", gini, baseline)
		}
		one := policy
		one.Limits.MaxCostUnits = 1
		trace, err := runOfflineTrace(t.Context(), p, ref, model, one)
		if err != nil || trace.Cost != 1 || len(trace.Actions) != 1 || trace.StopReason != "budget_exhausted" || len(trace.Distribution) != 3 {
			t.Fatalf("last evidence lost at budget boundary: %+v %v", trace, err)
		}
		one.Limits.MaxCostUnits, one.Limits.MaxAttempts = 2, 1
		trace, err = runOfflineTrace(t.Context(), p, ref, model, one)
		if err != nil || len(trace.Actions) != 1 || trace.StopReason != "budget_exhausted" {
			t.Fatalf("independent attempt cap ignored: %+v %v", trace, err)
		}
	}
}

func TestPairedCorpusIsFiniteReproducibleAndDoesNotClaimSavings(t *testing.T) {
	first, err := evaluate(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := evaluate(t.Context(), io.Discard)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("corpus or trace digests are not deterministic")
	}
	if first.Cases != 100000 || len(first.Traces) != 8 || !first.Synthetic || !belief.ValidDiagnosticDigest(first.CorpusDigest) {
		t.Fatalf("invalid corpus: %+v", first)
	}
	for _, summary := range first.Strategies {
		if summary.ExpandedActions != 200000 || summary.ExpandedCost != 200000 || summary.Stops["threshold_reached"] != 44000 || summary.Stops["round_limit"] != 56000 {
			t.Fatalf("incorrect expanded metrics: %+v", summary)
		}
	}
	var report bytes.Buffer
	if err := run(t.Context(), []string{"--evaluate"}, strings.NewReader(""), &report, io.Discard); err != nil {
		t.Fatal(err)
	}
	var decoded evaluationReport
	if err := json.Unmarshal(report.Bytes(), &decoded); err != nil || !reflect.DeepEqual(first, decoded) {
		t.Fatalf("CLI did not return the canonical report: %v", err)
	}
	if err := run(t.Context(), nil, strings.NewReader(strings.Repeat(" ", 32769)), io.Discard, io.Discard); err == nil {
		t.Fatal("oversized protocol input accepted")
	}
}

func TestOfflineTraceIdentityPinsModelAndPolicyVersions(t *testing.T) {
	p, model, policy, err := loadOfflineInputs()
	if err != nil {
		t.Fatal(err)
	}
	for ref := range p.bundles {
		original, err := runOfflineTrace(t.Context(), p, ref, model, policy)
		if err != nil {
			t.Fatal(err)
		}
		model.Version = "new-version"
		changedModel, err := runOfflineTrace(t.Context(), p, ref, model, policy)
		if err != nil || changedModel.Digest == original.Digest || changedModel.ModelDigest == original.ModelDigest || changedModel.Revisions != original.Revisions {
			t.Fatal("trace did not bind the model version independently of revisions")
		}
		policy.Version = "new-policy"
		changedPolicy, err := runOfflineTrace(t.Context(), p, ref, model, policy)
		if err != nil || changedPolicy.Digest == changedModel.Digest || changedPolicy.PolicyDigest == changedModel.PolicyDigest {
			t.Fatal("trace did not bind the policy version")
		}
		break
	}
}
