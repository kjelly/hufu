package main

import (
	"encoding/json"
	"slices"
	"testing"
)

// Exercise the same native inventory and final audit over canonical source
// records. Empty documentation reviews, documentation authored by critic, and
// primary reviews all enter the same inventory/critique/synthesis data flow.
func TestReviewContractPipelineAccountsForMixedSources(t *testing.T) {
	for _, withFinding := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty_documentation", true: "documentation_finding"}[withFinding], func(t *testing.T) {
			current := gateFixture(t)
			doc := record{Kind: "review", Revision: "frozen-head", Key: "unit-0002", Lens: "documentation-risk", Findings: []finding{}, Gaps: []json.RawMessage{}, Invariants: []invariantAssessment{}, References: []referenceAssessment{}}
			if withFinding {
				doc.Findings = append(doc.Findings, finding{Summary: "contract promise contradicts runtime", Detail: "the observed caller rejects this state", Severity: "warning", Location: "docs/spec.md:12", Evidence: []string{"snapshot-a"}, RequiresCritique: true})
			}
			source := fixtureTask(t, "13", "review-documentation-escalation", "success", payload{Record: raw(t, doc)}, doc, nil)
			current.Tasks = append(current.Tasks, source)
			inventory, err := collectReviewHandoffs(current, "run-1")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(inventory.SourceTaskIDs, "13") || slices.Contains(inventory.RequiredCriticSources, "13") != withFinding {
				t.Fatal("inventory lost documentation source")
			}
			setFixtureInventory(t, &current)
			critique := record{Kind: "critique", InputReview: []inputReview{entry(source, doc, t)}, Decisions: []decision{}}
			if withFinding {
				critique.Decisions = append(critique.Decisions, decision{FindingIndex: 0, Verdict: "confirmed", Evidence: []string{"snapshot-a"}})
			}
			critic := fixtureTask(t, "14", "critic-review", "success", payload{Record: raw(t, critique)}, critique, []string{"13"})
			current.Tasks = append(current.Tasks, critic)
			replacePayload(t, &current.Tasks[4], func(p *payload) {
				p.InputReview = append(p.InputReview, entry(source, doc, t), entry(critic, critique, t))
			})
			current.Tasks[4].EvidenceFrom = append(current.Tasks[4].EvidenceFrom, "13", "14")
			if _, _, err := verify(current, "run-1"); err != nil {
				t.Fatal(err)
			}
			// A stale or rewritten synthesis cannot pass, even after every worker is done.
			replacePayload(t, &current.Tasks[4], func(p *payload) { p.InputReview[len(p.InputReview)-2].Hash = "substituted" })
			if _, _, err := verify(current, "run-1"); err == nil {
				t.Fatal("final audit accepted substituted source identity")
			}
		})
	}
}
