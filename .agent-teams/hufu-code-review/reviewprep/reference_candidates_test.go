package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareReferenceCandidatesPreserveCleanupAndRequiredMentions(t *testing.T) {
	repo := newFixtureRepo(t)
	const document = "docs/reference/belief-state-validation.md"
	const prose = "The residual local scratch proposal was compared with the integrated plan and\nremoved from `docs/tmp`; that directory contains no remaining proposal files\nand no path under it is tracked.\nRead `docs/tmp` before continuing.\n已刪除 `docs/retired`。\nThe removed `legacy.OldType` is historical.\nUpdate `./missing-config.yaml` first.\nRead `../reference/missing.md` first.\n"
	writeAndCommit(t, repo, ".gitignore", "docs/tmp\n", "ignore scratch", "2025-01-02T00:00:00Z")
	writeAndCommit(t, repo, document, prose, "cleanup record", "2025-01-03T00:00:00Z")
	result, err := Prepare(t.Context(), documentationScopeConfig(repo, "HEAD"))
	if err != nil {
		t.Fatalf("cleanup record blocked preparation: %v", err)
	}
	v := result.Outputs["documentation_verification"].(documentationVerification)
	if !v.Passed || len(v.ReferenceCandidates) != 6 || len(v.Issues) != 0 {
		t.Fatalf("verification = %#v", v)
	}
	wantTargets := []string{"docs/tmp", "docs/tmp", "docs/retired", "legacy.OldType", "docs/reference/missing-config.yaml", "docs/reference/missing.md"}
	for i, c := range v.ReferenceCandidates {
		if c.ID != fmt.Sprintf("ref-%06d", i) || c.Document != document || c.Target != wantTargets[i] || c.Excerpt == "" {
			t.Fatalf("candidate %d = %#v", i, c)
		}
	}
	if v.ReferenceCandidates[0].Excerpt == v.ReferenceCandidates[1].Excerpt || v.ReferenceCandidates[3].Type != "go_symbol" {
		t.Fatal("distinct meanings or reference types lost")
	}
	for _, route := range []string{"primary", "documentation", "documentation-escalation"} {
		manifest := readManifest(t, filepath.Join(repo, "out", route, "workset-manifest.json"))
		for _, item := range manifest.Items {
			var ids []string
			if err := json.Unmarshal([]byte(item.Bindings["reference_candidate_ids"]), &ids); err != nil || ids == nil {
				t.Fatalf("invalid candidate binding: %v", err)
			}
			if item.Lens == "noop" {
				if len(ids) != 0 {
					t.Fatal("noop received candidates")
				}
				continue
			}
			if len(ids) != 6 {
				t.Fatalf("%s did not retain all semantic obligations: %v", route, ids)
			}
			for i, id := range ids {
				if id != v.ReferenceCandidates[i].ID {
					t.Fatalf("%s changed candidate identity", route)
				}
			}
			found := false
			for _, input := range item.Inputs {
				found = found || input.Description == "documentation verification report"
			}
			if !found {
				t.Fatalf("%s lacks reference evidence", route)
			}
		}
	}
}

func TestReferenceCandidateBudgetNeverSilentlyDropsMentions(t *testing.T) {
	repo := newFixtureRepo(t)
	var text strings.Builder
	for i := range 129 {
		fmt.Fprintf(&text, "Mention `docs/missing-%d`.\n", i)
	}
	writeAndCommit(t, repo, "README.md", text.String(), "many mentions", "2025-01-02T00:00:00Z")
	_, err := Prepare(t.Context(), documentationScopeConfig(repo, "HEAD"))
	if err == nil || !strings.Contains(err.Error(), "exceeds 128 reference candidates") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepeatedReferenceTextKeepsSeparateContextObligations(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "README.md", "Removed:\n`docs/tmp`\nRequired now:\n`docs/tmp`\nCompare `docs/tmp` with `docs/tmp`.\n", "distinct mentions", "2025-01-02T00:00:00Z")
	result, err := Prepare(t.Context(), documentationScopeConfig(repo, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	candidates := result.Outputs["documentation_verification"].(documentationVerification).ReferenceCandidates
	if len(candidates) != 4 {
		t.Fatalf("lost repeated mentions: %#v", candidates)
	}
	if candidates[0].AddedLineIndex == candidates[1].AddedLineIndex || candidates[2].InlineIndex == candidates[3].InlineIndex {
		t.Fatalf("lost context positions: %#v", candidates)
	}
}

func TestItemReportsDoNotExposeSiblingReferenceCandidates(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "docs/reference/a.md"), "Removed `docs/old-a`.\n")
	writeFile(t, filepath.Join(repo, "docs/reference/b.md"), "Removed `docs/old-b`.\n")
	writeFile(t, filepath.Join(repo, "README.md"), "Overview with no missing references.\n")
	commit(t, repo, "separate candidate scopes", "2025-01-02T00:00:00Z")
	config := documentationScopeConfig(repo, "HEAD")
	config.MaxPaths = 1
	result, err := Prepare(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, a := range result.Artifacts {
		declared[a.ID] = true
	}
	seenEmpty, seenCandidate := false, false
	for _, route := range []string{"primary", "documentation", "documentation-escalation"} {
		manifest := readManifest(t, filepath.Join(repo, "out", route, "workset-manifest.json"))
		for _, entry := range manifest.Items {
			count := 0
			for _, input := range entry.Inputs {
				if input.Kind != "documentation_verification" {
					continue
				}
				count++
				if !declared[input.ID] || !strings.Contains(input.Path, "/batches/"+entry.Key+"/") {
					t.Fatalf("report is shared or undeclared: %#v", input)
				}
				data, err := os.ReadFile(filepath.Join(repo, input.Path))
				if err != nil {
					t.Fatal(err)
				}
				var report struct {
					Key        string               `json:"workset_key"`
					Route      string               `json:"route"`
					IDs        []string             `json:"reference_candidate_ids"`
					Candidates []referenceCandidate `json:"reference_candidates"`
				}
				if err := json.Unmarshal(data, &report); err != nil {
					t.Fatal(err)
				}
				ids, err := json.Marshal(report.IDs)
				if err != nil {
					t.Fatal(err)
				}
				if report.Key != entry.Key || report.Route != route || report.IDs == nil || report.Candidates == nil || string(ids) != entry.Bindings["reference_candidate_ids"] {
					t.Fatalf("report differs from binding: %#v", report)
				}
				if len(report.Candidates) != len(report.IDs) || len(report.Candidates) > 1 {
					t.Fatalf("sibling inventory leaked: %#v", report)
				}
				for i, c := range report.Candidates {
					if c.ID != report.IDs[i] || !containsString(entry.TouchedPaths, c.Document) {
						t.Fatalf("foreign candidate: %#v", c)
					}
				}
				seenEmpty = seenEmpty || len(report.IDs) == 0
				seenCandidate = seenCandidate || len(report.IDs) == 1
			}
			if count != 1 {
				t.Fatalf("item %s/%s has %d reports", route, entry.Key, count)
			}
		}
	}
	if !seenEmpty || !seenCandidate {
		t.Fatal("fixture missed empty or nonempty item scope")
	}
}
