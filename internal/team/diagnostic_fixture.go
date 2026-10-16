package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"

	"github.com/kjelly/hufu/internal/belief"
)

const (
	DiagnosticFixtureVersion  = "diagnostic_offline_fixture_v1"
	maxDiagnosticFixtureBytes = belief.MaxModelBytes + 2*belief.MaxPolicyBytes
)

// DiagnosticFixture freezes synthetic inputs for bookkeeping tests. Its
// measurements are fixture data, never authenticated runtime receipts or grants.
// No execution interface is accepted by the diagnostic journal or projector.
type DiagnosticFixture struct {
	SchemaVersion int                            `json:"schema_version"`
	Version       string                         `json:"version"`
	Epoch         belief.Epoch                   `json:"epoch"`
	Revisions     belief.DiagnosticRevisions     `json:"revisions"`
	Model         belief.Model                   `json:"model"`
	Policy        belief.DiagnosticPolicy        `json:"policy"`
	Measurements  []DiagnosticFixtureMeasurement `json:"measurements"`
}

type DiagnosticFixtureMeasurement struct {
	ExperimentID string `json:"experiment_id"`
	OutcomeID    string `json:"outcome_id"`
	Digest       string `json:"measurement_digest"`
}

func validateDiagnosticFixture(f DiagnosticFixture) error {
	if f.SchemaVersion != 1 || f.Version != DiagnosticFixtureVersion || !f.Model.Synthetic {
		return fmt.Errorf("diagnostic shadow requires a versioned synthetic offline fixture")
	}
	if err := belief.ValidateDiagnosticPolicy(f.Model, f.Policy); err != nil {
		return err
	}
	if _, err := belief.Project(f.Model, f.Epoch, nil); err != nil {
		return err
	}
	if !belief.ValidDiagnosticDigest(f.Revisions.Subject) || !belief.ValidDiagnosticDigest(f.Revisions.Source) || !belief.ValidDiagnosticDigest(f.Revisions.Environment) || f.Epoch.SubjectRevision != f.Revisions.Subject || f.Epoch.EnvironmentRevision != f.Revisions.Environment || f.Epoch.Scope.SessionID == "" || len(f.Epoch.Scope.SessionID) > 128 {
		return fmt.Errorf("invalid diagnostic fixture revision or session binding")
	}
	if len(f.Measurements) != len(f.Policy.Candidates) {
		return fmt.Errorf("diagnostic fixture must cover every policy experiment")
	}
	seen := map[string]bool{}
	digests := map[string]bool{}
	for _, m := range f.Measurements {
		if seen[m.ExperimentID] || digests[m.Digest] || !belief.ValidDiagnosticDigest(m.Digest) || !slices.Contains([]string{"pass", "fail"}, m.OutcomeID) || !slices.ContainsFunc(f.Policy.Candidates, func(c belief.DiagnosticCandidate) bool { return c.ExperimentID == m.ExperimentID }) {
			return fmt.Errorf("invalid diagnostic fixture measurement")
		}
		seen[m.ExperimentID] = true
		digests[m.Digest] = true
		if _, err := belief.Project(f.Model, f.Epoch, []belief.Observation{{ID: "fixture-validation", ExperimentID: m.ExperimentID, OutcomeID: m.OutcomeID, SourceID: m.Digest}}); err != nil {
			return err
		}
	}
	return nil
}

func diagnosticFixtureBytes(f DiagnosticFixture) ([]byte, string, error) {
	if err := validateDiagnosticFixture(f); err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(f, json.Deterministic(true))
	if err != nil || len(data) > maxDiagnosticFixtureBytes {
		return nil, "", fmt.Errorf("diagnostic fixture cannot be encoded within its size limit")
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func loadDiagnosticFixture(ctx context.Context, store ArtifactStore, opened diagnosticEpisodeOpened) (*DiagnosticFixture, string) {
	if store == nil {
		return nil, "fixture_unavailable"
	}
	ref, err := store.Resolve(ctx, opened.FixtureRef)
	if err != nil || ref.Kind != "diagnostic_fixture" || ref.SHA256 != opened.FixtureDigest {
		return nil, "fixture_unavailable"
	}
	reader, err := store.Open(ctx, ref.ID)
	if err != nil {
		return nil, "fixture_unavailable"
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, maxDiagnosticFixtureBytes+1))
	if err != nil || len(data) > maxDiagnosticFixtureBytes {
		return nil, "fixture_invalid"
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != opened.FixtureDigest {
		return nil, "fixture_digest_mismatch"
	}
	var fixture DiagnosticFixture
	if err := json.Unmarshal(data, &fixture, json.RejectUnknownMembers(true)); err != nil || validateDiagnosticFixture(fixture) != nil {
		return nil, "fixture_invalid"
	}
	modelDigest, _ := belief.ModelDigest(fixture.Model)
	policyDigest, _ := belief.DiagnosticPolicyDigest(fixture.Policy)
	if fixture.Epoch != opened.Epoch || fixture.Revisions != opened.Revisions || modelDigest != opened.ModelDigest || policyDigest != opened.PolicyDigest || fixture.Policy.Limits.MaxEvents != opened.MaxEvents {
		return nil, "fixture_binding_mismatch"
	}
	return &fixture, ""
}

func diagnosticFixtureObservation(f DiagnosticFixture, candidateID string) (belief.Observation, error) {
	i := slices.IndexFunc(f.Policy.Candidates, func(c belief.DiagnosticCandidate) bool { return c.ID == candidateID })
	if i < 0 {
		return belief.Observation{}, fmt.Errorf("unknown diagnostic fixture candidate")
	}
	experiment := f.Policy.Candidates[i].ExperimentID
	i = slices.IndexFunc(f.Measurements, func(m DiagnosticFixtureMeasurement) bool { return m.ExperimentID == experiment })
	m := f.Measurements[i]
	id := beliefIdentity("hufu-diagnostic-fixture-observation-v1", []string{f.Epoch.Scope.BranchID, f.Epoch.ID, experiment})
	return belief.Observation{ID: id, ExperimentID: experiment, OutcomeID: m.OutcomeID, SourceID: m.Digest}, nil
}
