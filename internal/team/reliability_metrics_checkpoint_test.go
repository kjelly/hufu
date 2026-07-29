package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestAcceptanceRevisionPersistsToSessionAndEventStore(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "revision"}}, sessionData: NewSession(), taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
	c.SetAcceptanceSpec(AcceptanceSpec{Commands: []string{"test -f initial"}})
	end := c.beginExecutionRun()
	c.SetAcceptanceSpecWithReason(AcceptanceSpec{Commands: []string{"test -f updated"}}, "review-fix")
	end()

	saved := LoadSession(workspace)
	if saved == nil || len(saved.AcceptanceContractRevisions) != 2 {
		t.Fatalf("acceptance revisions not persisted: %#v", saved)
	}
	if got := saved.AcceptanceContractRevisions[1].Reason; got != "review-fix" {
		t.Fatalf("revision reason = %q", got)
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == "acceptance_contract_modified" {
			var payload map[string]any
			if json.Unmarshal(event.Payload, &payload) == nil && payload["reason"] == "review-fix" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("acceptance revision event missing from event store")
	}

	restored := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "revision"}}, taskTracker: NewTaskTracker()}
	restored.SetSessionData(saved)
	if got := restored.acceptanceSpec.Commands[0]; got != "test -f updated" {
		t.Fatalf("restored acceptance command = %q", got)
	}
}

func TestContinuationCheckpointSurvivesRestartAndAbort(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "checkpoint"}}, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	c.saveContinuationCheckpoint(2, 5, "step_limit", "pending")
	restarted := &Coordinator{session: c.session, taskTracker: NewTaskTracker()}
	restarted.SetSessionData(LoadSession(workspace))
	if cp := restarted.ContinuationCheckpoint(); cp == nil || cp.Status != "pending" {
		t.Fatalf("pending checkpoint not restored: %#v", cp)
	}
	if cp := restarted.ResumeContinuationCheckpoint(); cp == nil || cp.Status != "resumed" {
		t.Fatalf("checkpoint not resumed: %#v", cp)
	}
	if restarted.continuationResume == nil || restarted.continuationResume.TurnCount != 2 {
		t.Fatalf("resume context missing original turn: %#v", restarted.continuationResume)
	}
	restarted.saveContinuationCheckpoint(3, 5, "provider", "pending")
	restarted.recordRunAborted(context.Canceled)
	restarted2 := &Coordinator{session: c.session, taskTracker: NewTaskTracker()}
	restarted2.SetSessionData(LoadSession(workspace))
	if cp := restarted2.ContinuationCheckpoint(); cp == nil || cp.Status != "aborted" || cp.TurnCount != 3 {
		t.Fatalf("aborted checkpoint not restored: %#v", cp)
	}
}

func TestCoordinatorMetricsAreExternallyQueryableAndCopied(t *testing.T) {
	c := &Coordinator{}
	c.recordRetry(FailureProtocol)
	c.recordRetry(FailureProtocol)
	c.recordRetry(FailureTimeout)
	c.recordCompaction()
	m := c.Metrics()
	if m.RetriesByFailureClass[FailureProtocol] != 2 || m.RetriesByFailureClass[FailureTimeout] != 1 || m.Compactions != 1 {
		t.Fatalf("unexpected metrics: %#v", m)
	}
	m.RetriesByFailureClass[FailureProtocol] = 99
	if got := c.Metrics().RetriesByFailureClass[FailureProtocol]; got != 2 {
		t.Fatalf("metrics map was not copied: %d", got)
	}
}

func TestAcceptanceSpecDeepCopyPreventsCallerMutation(t *testing.T) {
	workspace := t.TempDir()
	commands := []string{"test -f required-artifact"}
	artifacts := []string{"required-artifact"}
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "immutability"}}, sessionData: NewSession(), reportStatus: func(StatusEvent) {}}
	c.SetAcceptanceSpec(AcceptanceSpec{Commands: commands, RequiredArtifacts: artifacts})

	// Mutating the caller-owned backing arrays must not weaken the fixed
	// contract or rewrite its persisted revision snapshot.
	commands[0] = "true"
	artifacts[0] = "other-artifact"
	if got := c.acceptanceSpec.Commands[0]; got != "test -f required-artifact" {
		t.Fatalf("acceptance command changed through caller alias: %q", got)
	}
	if got := c.acceptanceSpec.RequiredArtifacts[0]; got != "required-artifact" {
		t.Fatalf("required artifact changed through caller alias: %q", got)
	}
	first := c.sessionData.AcceptanceContractRevisions[0]
	if first.NewSpec.Commands[0] != "test -f required-artifact" || first.NewSpec.RequiredArtifacts[0] != "required-artifact" {
		t.Fatalf("revision snapshot was mutated through caller alias: %#v", first)
	}

	nextCommands := []string{"test -f updated-artifact"}
	c.SetAcceptanceSpecWithReason(AcceptanceSpec{Commands: nextCommands}, "immutable-test")
	nextCommands[0] = "true"
	if got := c.sessionData.AcceptanceContractRevisions[1].NewSpec.Commands[0]; got != "test -f updated-artifact" {
		t.Fatalf("new revision changed through caller alias: %q", got)
	}

	c.SetAcceptanceSpecWithReason(AcceptanceSpec{Commands: []string{"true"}}, "result-copy-test")
	result, err := c.runAcceptance(context.Background())
	if err != nil {
		t.Fatalf("runAcceptance failed: %v", err)
	}
	result.Commands[0] = "false"
	if got := c.acceptanceSpec.Commands[0]; got != "true" {
		t.Fatalf("acceptance result exposed internal command slice: %q", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, sessionFile)); err != nil {
		t.Fatalf("expected isolated session artifact in temp workspace: %v", err)
	}
}
