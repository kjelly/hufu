package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestInventoryIncludesDocumentationReviewPerformedByCritic(t *testing.T) {
	fixture := gateFixture(t)
	doc := record{Kind: "review", Revision: "frozen-head", Key: "unit-0000", Lens: "documentation-risk", Findings: []finding{
		{Summary: "Undefined recovery status", Detail: "Machine consumers cannot interpret it", Severity: "warning", RequiresCritique: true, Evidence: []string{"snapshot-a"}},
	}, Gaps: []json.RawMessage{}}
	item := fixtureTask(t, "13", "review-documentation-escalation", "success", payload{Record: raw(t, doc)}, doc, nil)
	// Real checkpoints identify this worker as critic. It is still a review.
	encoded := raw(t, map[string]any{"Agent": "critic"})
	if err := json.Unmarshal(encoded, &item); err != nil {
		t.Fatal(err)
	}
	fixture.Tasks = append(fixture.Tasks, item)
	inventory, err := collectReviewHandoffs(fixture, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(inventory.SourceTaskIDs, []string{"3", "4", "13"}) || !slices.Equal(inventory.RequiredCriticSources, []string{"3", "4", "13"}) {
		t.Fatalf("documentation critic was omitted: %+v", inventory)
	}
	if inventory.Sources[2].PayloadHash != item.Result.Payload.Hash || inventory.Sources[2].FindingCount != 1 {
		t.Fatalf("source identity not preserved: %+v", inventory.Sources[2])
	}
	if _, _, err := verify(fixture, "run-1"); err == nil || !strings.Contains(err.Error(), "unassigned required critique") {
		t.Fatalf("missing independent documentation critique accepted: %v", err)
	}
}

func TestInventoryRejectsUnacceptedOrSubstitutedHandoffs(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*session)
	}{
		{"pending", func(s *session) { s.Tasks[0].Status = "in_progress" }},
		{"stale_run", func(s *session) { s.Tasks[0].Receipt.RunID = "old-run" }},
		{"payload_hash", func(s *session) { s.Tasks[0].Result.Payload.Hash = strings.Repeat("0", 64) }},
		{"item_identity", func(s *session) { s.Tasks[0].Binding.Bindings["key"] = "other-item" }},
		{"duplicate", func(s *session) { s.Tasks = append(s.Tasks, s.Tasks[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := gateFixture(t)
			test.edit(&fixture)
			if _, err := collectReviewHandoffs(fixture, "run-1"); err == nil {
				t.Fatal("invalid review handoff accepted")
			}
		})
	}
}

func TestInventoryVerificationRejectsOmittedSource(t *testing.T) {
	fixture := gateFixture(t)
	var inventory handoffInventory
	if err := json.Unmarshal(fixture.Tasks[7].Result.RuntimeOutputs["review_handoff_inventory"], &inventory); err != nil {
		t.Fatal(err)
	}
	inventory.RequiredCriticSources = inventory.RequiredCriticSources[:1]
	fixture.Tasks[7].Result.RuntimeOutputs["review_handoff_inventory"] = raw(t, inventory)
	if err := verifyHandoffInventory(fixture, "run-1"); err == nil {
		t.Fatal("inventory omitted a required source")
	}
}

func TestEmbeddedInventoryActionUsesCurrentCheckpoint(t *testing.T) {
	teamDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := team.LoadTeam(teamDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := loaded.ProviderRegistry.Get("inventory-review-handoffs")
	if !ok {
		t.Fatal("inventory provider is not registered")
	}
	workspace, _ := actionFixture(t)
	root, _, err := checkpointRoot(workspace, "run-1", "action-1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var current session
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatal(err)
	}
	current.Tasks[len(current.Tasks)-1].ContractID = "inventory-review-handoffs"
	if err := os.WriteFile(filepath.Join(root, "session.json"), raw(t, current), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := team.WithActionEnvironment(t.Context(), team.ActionEnvironment{Workspace: workspace, RunID: "run-1", ActionInvocationID: "action-1", TaskID: "8"})
	result, err := provider.Execute(ctx, team.Action{Capability: "inventory-review-handoffs", Type: "inventory_review_handoffs", Payload: `{"scope":{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}}`})
	if err != nil {
		t.Fatalf("configured embedded inventory: %v", err)
	}
	var envelope struct {
		Outputs map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(raw(t, result), &envelope); err != nil {
		t.Fatal(err)
	}
	var inventory handoffInventory
	if err := json.Unmarshal(envelope.Outputs["review_handoff_inventory"], &inventory); err != nil {
		t.Fatal(err)
	}
	if !inventory.Passed || !slices.Equal(inventory.RequiredCriticSources, []string{"3", "4"}) {
		t.Fatalf("incorrect native inventory: %+v", inventory)
	}
}
