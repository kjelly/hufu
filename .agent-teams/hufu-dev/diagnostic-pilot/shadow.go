package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/kjelly/hufu/internal/belief"
	"github.com/kjelly/hufu/internal/team"
)

const (
	shadowRunID     = "run-diagnostic-fixture"
	shadowSessionID = "session-diagnostic-fixture"
)

type fixtureEventJournal struct{ store *team.EventStore }

func (j fixtureEventJournal) Append(ctx context.Context, event team.RunEvent) (team.RunEvent, error) {
	return j.store.AppendPersistedContext(ctx, event)
}
func (j fixtureEventJournal) AppendAtHead(ctx context.Context, event team.RunEvent, head string) (team.RunEvent, error) {
	return j.store.AppendPersistedAtHead(ctx, event, head)
}
func (j fixtureEventJournal) ReadEvents(ctx context.Context) ([]team.RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return j.store.ReadEvents()
}
func (j fixtureEventJournal) VerifyHashChain(context.Context) error { return j.store.VerifyHashChain() }

// journalFixture computes fixed fixture comparisons directly. Shadow never
// calls ActionProvider.Execute, a verifier command, a model or the network.
func journalFixture(name string) (team.DiagnosticFixture, error) {
	var result team.DiagnosticFixture
	if !slices.Contains([]string{"pass-pass", "pass-fail", "fail-pass", "fail-fail"}, name) {
		return result, fmt.Errorf("unknown embedded shadow fixture")
	}
	p, model, policy, err := loadOfflineInputs()
	if err != nil {
		return result, err
	}
	data, err := resources.ReadFile("fixtures/" + name + ".json")
	if err != nil {
		return result, err
	}
	f, err := decodeFixture(data)
	if err != nil {
		return result, err
	}
	ref, err := contentDigest("diagnostic-fixture-v1", f)
	if err != nil {
		return result, err
	}
	f, ok := p.bundles[ref]
	if !ok {
		return result, fmt.Errorf("shadow fixture is not registered")
	}
	source, err := contentDigest("diagnostic-source-v1", f.Vectors)
	if err != nil {
		return result, err
	}
	provider, err := embeddedDigest("fixture.go")
	if err != nil {
		return result, err
	}
	adapter, err := embeddedDigest("adapter.json")
	if err != nil {
		return result, err
	}
	environment, err := contentDigest("diagnostic-environment-v1", struct {
		Manifest environment `json:"environment"`
		Provider string      `json:"provider_version"`
		Source   string      `json:"provider_source_digest"`
		Adapter  string      `json:"adapter_artifact_digest"`
	}{f.Environment, "fixture_design_v1", provider, adapter})
	if err != nil {
		return result, err
	}
	modelDigest, err := belief.ModelDigest(model)
	if err != nil {
		return result, err
	}
	result = team.DiagnosticFixture{SchemaVersion: 1, Version: team.DiagnosticFixtureVersion, Model: model, Policy: policy,
		Epoch:     belief.Epoch{ID: "fixture-" + ref, ModelDigest: modelDigest, Scope: belief.Scope{RunID: shadowRunID, SessionID: shadowSessionID, BranchID: "main", TaskID: "fixture-owner", Attempt: 1}, SubjectRevision: ref, EnvironmentRevision: environment},
		Revisions: belief.DiagnosticRevisions{Subject: ref, Source: source, Environment: environment}, Measurements: []team.DiagnosticFixtureMeasurement{}}
	for _, candidate := range policy.Candidates {
		m, err := measure(f, probeInput{FixtureRef: ref, ExperimentID: candidate.ExperimentID})
		if err != nil {
			return result, err
		}
		result.Measurements = append(result.Measurements, team.DiagnosticFixtureMeasurement{ExperimentID: m.ExperimentID, OutcomeID: m.Outcome, Digest: m.Digest})
	}
	return result, nil
}

// refuseNonFixtureWorkspace prevents this harness from appending to a live
// Hufu workspace. The journal, rather than a side file, identifies its purpose.
func refuseNonFixtureWorkspace(workspace string) error {
	_, err := os.Stat(filepath.Join(workspace, "runtime.sqlite"))
	if os.IsNotExist(err) {
		entries, readErr := os.ReadDir(workspace)
		if os.IsNotExist(readErr) || readErr == nil && len(entries) == 0 {
			return nil
		}
		return fmt.Errorf("shadow requires an empty or existing offline fixture workspace")
	}
	if err != nil {
		return err
	}
	store, err := team.OpenEventStoreReadOnly(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.RunID != shadowRunID || event.SessionID != shadowSessionID || event.BranchID != "main" || !slices.Contains([]string{team.EventDiagnosticEpisodeOpened, team.EventDiagnosticEpisodeShadowed, team.EventDiagnosticEpisodeObserved}, event.Type) {
			return fmt.Errorf("shadow refuses a workspace containing non-fixture runtime events")
		}
	}
	return nil
}

func runFixtureShadow(ctx context.Context, workspace, name string) (team.DiagnosticEpisodeState, error) {
	var result team.DiagnosticEpisodeState
	f, err := journalFixture(name)
	if err != nil {
		return result, err
	}
	if err := refuseNonFixtureWorkspace(workspace); err != nil {
		return result, err
	}
	lease, err := team.AcquireDecisionWriterLease(ctx, workspace, "main")
	if err != nil {
		return result, err
	}
	defer func() { _ = lease.Close() }()
	store, err := team.NewEventStore(workspace, shadowRunID, shadowSessionID)
	if err != nil {
		return result, err
	}
	defer func() { _ = store.Close() }()
	// Recheck under ownership; a competing writer may have committed meanwhile.
	if err := refuseNonFixtureWorkspace(workspace); err != nil {
		return result, err
	}
	tree, exists, err := team.LoadSavedSessionTree(workspace)
	if err != nil {
		return result, err
	}
	if !exists {
		tree = team.NewSessionTree()
		if err := team.SaveSessionTree(workspace, tree); err != nil {
			return result, err
		}
	}
	if tree.ActiveBranch != "main" || len(tree.Branches) != 1 {
		return result, fmt.Errorf("offline shadow harness requires an unforked main branch")
	}
	artifacts, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		return result, err
	}
	journal, err := team.NewDiagnosticJournal(fixtureEventJournal{store}, artifacts, tree, "main", shadowRunID, shadowSessionID)
	if err != nil {
		return result, err
	}
	if err := journal.OpenFixture(ctx, f); err != nil {
		return result, err
	}
	for {
		views, err := journal.Replay(ctx)
		if err != nil {
			return result, err
		}
		i := slices.IndexFunc(views, func(v team.DiagnosticEpisodeState) bool { return v.Epoch.ID == f.Epoch.ID })
		if i < 0 {
			return result, fmt.Errorf("offline fixture episode is missing")
		}
		result = views[i]
		if result.Status == "stopped" {
			return result, nil
		}
		if err := journal.AdvanceFixture(ctx, f.Epoch.ID, result.CursorID); err != nil {
			return result, err
		}
	}
}
