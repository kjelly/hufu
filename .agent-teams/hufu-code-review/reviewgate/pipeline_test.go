package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestLegacyAcceptedEvidenceTypoRejectsSupplementHandoff(t *testing.T) {
	current := gateFixture(t)
	source := &current.Tasks[0]
	replacePayload(t, source, func(p *payload) {
		var review record
		if err := json.Unmarshal(p.Record, &review); err != nil {
			t.Fatal(err)
		}
		review.Findings[0].Evidence[0] = "snaphsot-a"
		p.Record = raw(t, review)
	})
	view, err := projectedFixture(current, "run-1")
	if err != nil || !view.Tasks[0].VerifiedResult {
		t.Fatalf("legacy accepted source projection: %v", err)
	}
	if _, err := collectSupplementRequests(view); err == nil || !strings.Contains(err.Error(), `task 3 /record/findings/0/evidence/0: reference "snaphsot-a"`) {
		t.Fatalf("downstream did not reject the original citation: %v", err)
	}
}

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
			} else {
				doc.Key = "unit-0000"
			}
			source := fixtureTask(t, "13", "review-documentation-escalation", "success", payload{Record: raw(t, doc)}, doc, nil)
			var direct []byte
			if !withFinding {
				// The runtime regression requires byte-for-byte equality with
				// this complete direct submission before report binding.
				direct = directCritiqueFixture(t, &source)
			}
			current.Tasks = append(current.Tasks, source)
			inventory, err := fixtureCollectReviewHandoffs(current, "run-1")
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
			if direct != nil {
				critic.Result.Payload.Value = direct
				critic.Result.Payload.Hash = fmt.Sprintf("%x", sha256.Sum256(direct))
			}
			current.Tasks = append(current.Tasks, critic)
			var inputs []inputReview
			for _, item := range []task{source, critic} {
				var accepted payload
				if err := json.Unmarshal(item.Result.Payload.Value, &accepted); err != nil {
					t.Fatal(err)
				}
				inputs = append(inputs, inputReview{TaskID: item.ID, Hash: item.Result.Payload.Hash, Record: accepted.Record})
			}
			replacePayload(t, &current.Tasks[4], func(p *payload) {
				p.InputReview = append(p.InputReview, inputs...)
			})
			current.Tasks[4].EvidenceFrom = append(current.Tasks[4].EvidenceFrom, "13", "14")
			if _, _, err := fixtureVerify(current, "run-1"); err != nil {
				t.Fatal(err)
			}
			// A stale or rewritten synthesis cannot pass, even after every worker is done.
			replacePayload(t, &current.Tasks[4], func(p *payload) { p.InputReview[len(p.InputReview)-2].Hash = "substituted" })
			if _, _, err := fixtureVerify(current, "run-1"); err == nil {
				t.Fatal("final audit accepted substituted source identity")
			}
		})
	}
}

func directCritiqueFixture(t *testing.T, source *task) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/direct-empty-critique.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err = canonicalJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		AuditComplete bool `json:"audit_complete"`
		Record        record
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.AuditComplete || len(result.Record.InputReview) != 1 {
		t.Fatal("expected an assembled result with one accepted source")
	}
	input := result.Record.InputReview[0]
	canonical, err := canonicalJSON(raw(t, map[string]json.RawMessage{"record": input.Record}))
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(canonical))
	if source.ID != input.TaskID || digest != input.Hash {
		t.Fatal("direct critique source identity does not match the accepted source")
	}
	source.Result.Payload.Value, source.Result.Payload.Hash = canonical, digest
	return data
}
