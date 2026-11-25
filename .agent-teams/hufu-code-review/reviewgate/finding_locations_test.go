package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/template"
)

func TestCanonicalDraftFindingsSnapOnlyObservedSyntax(t *testing.T) {
	root := t.TempDir()
	const revision = "frozen-head"
	const name = "pkg/sample.go"
	// The complete file opened two scopes before this assigned fragment.
	bundle := []byte("\t\tprintln(1)\n\t}\n}\n")
	put := func(kind string, data []byte) sourceArtifactRef {
		t.Helper()
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		id := "sha256-" + hash
		if err := os.WriteFile(filepath.Join(root, id), data, 0o600); err != nil {
			t.Fatal(err)
		}
		return sourceArtifactRef{ID: id, Kind: kind, SHA256: hash, Bytes: int64(len(data)), TaskID: "producer"}
	}
	ref := put("review_source_snapshot", bundle)
	inventory := put("review_evidence_inventory", raw(t, map[string]any{
		"review_revision": revision,
		"sources": []any{map[string]any{"path": name, "role": "primary", "available_spans": []any{
			map[string]any{"bundle_start": 0, "bundle_end": len(bundle), "start_line": 4, "end_line": 6, "go_syntax_lines": []int{4}, "first_full_line": 4, "last_full_line": 6},
		}}},
	}))
	tasks := make(map[string]sourceTask)
	records := make(map[string]record)
	current := sourceSession{ArtifactDataDirectory: root, Tasks: []sourceTask{{ID: "producer", VerifiedOutputs: true}}}
	for _, tc := range []struct{ id, path, severity, contract string }{
		{"a", "./pkg/sample.go:2", "warning", "review-primary-workset"},
		{"b", "/checkout/pkg/sample.go: 5", "warning", "review-supplement-workset"},
		{"c", "pkg/sample.go:4", "info", "review-primary-workset"},
	} {
		r := record{Kind: "review", Revision: revision, Findings: []finding{{
			Location: tc.path, Summary: "same judgment", Severity: tc.severity, SourceExcerpt: " println(1) ", Evidence: []string{ref.ID},
		}}}
		if tc.id == "c" {
			r.Findings[0].SourceExcerpt = ""
		}
		item := fixtureTask(t, tc.id, tc.contract, "success", payload{Record: raw(t, r)}, r, nil)
		item.Binding.Inputs, item.Binding.TouchedPaths = []sourceArtifactRef{ref, inventory}, []string{name}
		item.Result.FilesRead[0].Path, item.Result.FilesRead[1].Path = ref.ID, inventory.ID
		task := projectedTask(item)
		tasks[tc.id], records[tc.id] = task, r
	}
	groups, err := canonicalDraftFindings(current, tasks, records)
	if err != nil || len(groups) != 2 || groups[0].Location != name+":4" || len(groups[0].Sources) != 2 ||
		groups[1].Location != name+":4" || len(groups[1].Sources) != 1 ||
		records["a"].Findings[0].Location != "./pkg/sample.go:2" || records["b"].Findings[0].Location != "/checkout/pkg/sample.go: 5" {
		t.Fatalf("canonical groups = %+v, %v", groups, err)
	}
	supplement := records["b"]
	supplement.Findings = slices.Clone(supplement.Findings)
	supplement.Findings[0].Location = name + ":7"
	supplement.Findings[0].SourceExcerpt = ""
	if _, err := resolveDraftLocations(current, tasks["b"], supplement); err == nil {
		t.Fatal("supplement accepted an unobserved line")
	}
	unanchored := records["c"]
	unanchored.Findings[0].Location = name + ":999999"
	if _, err := resolveDraftLocations(current, tasks["c"], unanchored); err == nil {
		t.Fatal("location outside observed lines passed without an excerpt")
	}
	unanchored.Findings[0].Location = name + ":4"
	for _, tc := range []struct {
		line int
		want bool
	}{{5, true}, {3, false}} {
		var inv findingEvidenceInventory
		data, err := os.ReadFile(filepath.Join(root, inventory.ID))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &inv); err != nil {
			t.Fatal(err)
		}
		if line, err := snapFindingLine(&inv, bundle, name, tc.line, "}"); (err == nil) != tc.want || tc.want && line != tc.line {
			t.Fatalf("closing token at %d: line=%d, %v", tc.line, line, err)
		}
	}
	overlappingDiff := put("review_diff", []byte("diff --git a/pkg/sample.go b/pkg/sample.go\n--- a/pkg/sample.go\n+++ b/pkg/sample.go\n@@ -4 +4 @@\n-\t\tprintln(0)\n+\t\tprintln(1)\n"))
	overlappingTask := tasks["a"]
	binding, result := *overlappingTask.Binding, *overlappingTask.Result
	binding.Inputs = []sourceArtifactRef{ref, inventory, overlappingDiff}
	result.FilesRead = append(slices.Clone(result.FilesRead), result.FilesRead[0])
	result.FilesRead[2].Path = overlappingDiff.ID
	overlappingTask.Binding, overlappingTask.Result = &binding, &result
	overlappingReview := record{Kind: "review", Revision: revision, Findings: []finding{
		{Location: name + ":4", SourceSide: "head", Summary: "same judgment", SourceExcerpt: "println(1)", Evidence: []string{overlappingDiff.ID}},
		{Location: name + ":4", SourceSide: "head", Summary: "same judgment", Evidence: []string{overlappingDiff.ID}},
		{Location: name + ":4", SourceSide: "old", Summary: "same judgment", SourceExcerpt: "println(0)", Evidence: []string{overlappingDiff.ID}},
		{Location: name + ":4", SourceSide: "old", Summary: "same judgment", Evidence: []string{overlappingDiff.ID}},
	}}
	overlap, err := canonicalDraftFindings(current, map[string]sourceTask{"a": overlappingTask}, map[string]record{"a": overlappingReview})
	if err != nil || len(overlap) != 2 || overlap[0].SourceSide != "head" || overlap[1].SourceSide != "old" ||
		len(overlap[0].Sources) != 2 || len(overlap[1].Sources) != 2 || overlap[0].Location != overlap[1].Location {
		t.Fatalf("overlapping coordinates = %+v, %v", overlap, err)
	}
	overlappingReview.Findings[3].SourceSide = ""
	if _, err := resolveDraftLocations(current, overlappingTask, overlappingReview); err == nil {
		t.Fatal("ambiguous coordinates were assigned a side without evidence")
	}
	overlappingReview.Findings[3].SourceExcerpt = "println(0)"
	resolved, err := resolveDraftLocations(current, overlappingTask, overlappingReview)
	if err != nil || resolved.Findings[3].SourceSide != "old" {
		t.Fatalf("exact old excerpt did not disambiguate the coordinate: %+v, %v", resolved, err)
	}
	parentOne, parentTwo := strings.Repeat("a", 40), strings.Repeat("b", 40)
	multipleParents := put("review_diff", []byte(strings.Join([]string{
		"diff --git a/pkg/sample.go b/pkg/sample.go", "hufu-old-revision " + parentOne,
		"--- a/pkg/sample.go", "+++ b/pkg/sample.go", "@@ -4 +4 @@", "-println(0)", "+println(1)",
		"diff --git a/pkg/sample.go b/pkg/sample.go", "hufu-old-revision " + parentTwo,
		"--- a/pkg/sample.go", "+++ b/pkg/sample.go", "@@ -4 +4 @@", "-println(2)", "+println(1)", "",
	}, "\n")))
	binding.Inputs[2], result.FilesRead[2].Path = multipleParents, multipleParents.ID
	parentsReview := record{Kind: "review", Revision: revision, Findings: []finding{
		{Location: name + ":4", SourceSide: "old", SourceRevision: parentOne, Summary: "same judgment", Evidence: []string{multipleParents.ID}},
		{Location: name + ":2", SourceSide: "old", SourceExcerpt: "println(2)", Summary: "same judgment", Evidence: []string{multipleParents.ID}},
		{Location: name + ":6", SourceSide: "old", SourceRevision: parentTwo, SourceExcerpt: "println(2)", Summary: "same judgment", Evidence: []string{multipleParents.ID}},
	}}
	parents, err := canonicalDraftFindings(current, map[string]sourceTask{"a": overlappingTask}, map[string]record{"a": parentsReview})
	if err != nil || len(parents) != 2 || parents[0].SourceRevision != parentOne || parents[1].SourceRevision != parentTwo ||
		parents[1].Location != name+":4" || len(parents[1].Sources) != 2 ||
		parentsReview.Findings[1].Location != name+":2" || parentsReview.Findings[2].Location != name+":6" {
		t.Fatalf("findings at separate parent revisions = %+v, %v", parents, err)
	}
	parentsReview.Findings[0].SourceRevision = ""
	if _, err := resolveDraftLocations(current, overlappingTask, parentsReview); err == nil {
		t.Fatal("ambiguous parent revision admitted")
	}
	parentsReview.Findings[0].SourceRevision = strings.Repeat("c", 40)
	if _, err := resolveDraftLocations(current, overlappingTask, parentsReview); err == nil {
		t.Fatal("unobserved parent revision admitted")
	}
	repeated := put("review_source_snapshot", []byte("func f() {\nprintln(1)\n_ = 0\nprintln(2)\nprintln(1)\nprintln(2)\n}\n"))
	repeatedInventory := put("review_evidence_inventory", raw(t, map[string]any{
		"review_revision": revision,
		"sources": []any{map[string]any{"path": name, "role": "primary", "available_spans": []any{
			map[string]any{"bundle_start": 0, "bundle_end": repeated.Bytes, "start_line": 8, "end_line": 14,
				"go_syntax_lines": []int{8, 9, 10, 11, 12, 13}, "first_full_line": 8, "last_full_line": 14},
		}}},
	}))
	repeatedDiff := put("review_diff", []byte(strings.Join([]string{
		"diff --git a/pkg/sample.go b/pkg/sample.go", "hufu-old-revision " + parentOne,
		"--- a/pkg/sample.go", "+++ b/pkg/sample.go", "@@ -8,7 +8 @@",
		"-func f() {", "-println(1)", "-_ = 0", "-println(2)", "-println(1)", "-println(2)", "-}", "+func f() {}", "",
	}, "\n")))
	binding.Inputs = []sourceArtifactRef{repeated, repeatedInventory, repeatedDiff}
	for i, ref := range binding.Inputs {
		result.FilesRead[i].Path = ref.ID
	}
	for _, side := range []string{"head", "old"} {
		for _, tc := range []struct{ draft, excerpt, want string }{
			{"10", "println(1)", "9"},
			{"12", "println(1)", "12"},
			{"12", "println(2)", ""},
		} {
			observed := record{Kind: "review", Revision: revision, Findings: []finding{{
				Location: name + ":" + tc.draft, SourceSide: side, SourceExcerpt: tc.excerpt, Evidence: []string{repeatedDiff.ID},
			}}}
			got, err := canonicalDraftFindings(current, map[string]sourceTask{"a": overlappingTask}, map[string]record{"a": observed})
			if tc.want == "" {
				if err == nil {
					t.Fatalf("%s source accepted an equal-distance tie: %+v", side, got)
				}
			} else if err != nil || len(got) != 1 || got[0].Location != name+":"+tc.want || got[0].SourceSide != side {
				t.Fatalf("%s source at %s did not select %s: %+v %v", side, tc.draft, tc.want, got, err)
			}
		}
	}
	const removed = "pkg/removed file.go"
	removedDiff := put("review_diff", []byte("diff --git a/pkg/removed file.go b/pkg/removed file.go\ndeleted file mode 100644\n--- a/pkg/removed file.go\t\n+++ /dev/null\n@@ -1,3 +0,0 @@\n-package sample\n---- a/wrong.go\n-println(2)\n"))
	removedReview := record{Kind: "review", Revision: revision, Findings: []finding{
		{Location: removed + ":3", SourceExcerpt: "println(2)", Evidence: []string{removedDiff.ID}},
		{Location: removed + ":2", Evidence: []string{removedDiff.ID}},
	}}
	item := fixtureTask(t, "deleted", "review-primary-workset", "success", payload{Record: raw(t, removedReview)}, removedReview, nil)
	item.Binding.Inputs = []sourceArtifactRef{ref, inventory, removedDiff}
	item.Binding.TouchedPaths = []string{removed}
	item.Result.FilesRead[0].Path, item.Result.FilesRead[1].Path = ref.ID, inventory.ID
	deletedTask := projectedTask(item)
	if _, err := resolveDraftLocations(current, deletedTask, removedReview); err == nil {
		t.Fatal("unobserved diff admitted a deleted line")
	}
	deletedTask.Result.FilesRead = append(deletedTask.Result.FilesRead, deletedTask.Result.FilesRead[0])
	deletedTask.Result.FilesRead[2].Path = removedDiff.ID
	if _, err := resolveDraftLocations(current, deletedTask, removedReview); err != nil {
		t.Fatalf("authenticated deletion rejected: %v", err)
	}
	removedReview.Findings[1].Location = removed + ":9"
	if _, err := resolveDraftLocations(current, deletedTask, removedReview); err == nil {
		t.Fatal("out-of-hunk deletion admitted")
	}
	removedReview.Findings[1].Location = removed + ":2"
	if err := os.WriteFile(filepath.Join(root, removedDiff.ID), []byte("changed patch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDraftLocations(current, deletedTask, removedReview); err == nil {
		t.Fatal("modified diff admitted")
	}
	if err := os.WriteFile(filepath.Join(root, ref.ID), bytes.Repeat([]byte("x"), len(bundle)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalDraftFindings(current, tasks, records); err == nil {
		t.Fatal("modified source admitted")
	}
}

func TestHumanReportRendersCanonicalLocationAndPreservesDraft(t *testing.T) {
	_, _, report := schemaFixture()
	report["supplement_outcomes"] = []any{}
	revision := strings.Repeat("a", 40)
	report["canonical_findings"] = []any{map[string]any{"location": "team.yaml:21", "source_side": "old", "source_revision": revision, "requires_critique": true,
		"sources": []any{map[string]any{"task_id": "4", "finding_index": 0}}}}
	_, document := contractSchema(t, "report-v3.json")
	renderer, err := template.New("report").Option("missingkey=error").Parse(document["x-hufu-final-report"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := renderer.Execute(&output, report); err != nil {
		t.Fatal(err)
	}
	main, audit, _ := strings.Cut(output.String(), "## 完整審查證據（稽核附錄）")
	findings, verdicts, _ := strings.Cut(main, "## Critic 判定與後續處理")
	location := "位置：team.yaml:21（刪除側 " + revision + "）"
	if !strings.Contains(findings, location) || !strings.Contains(verdicts, location) ||
		strings.Contains(main, "team.yaml:20") || !strings.Contains(audit, "team.yaml:20") {
		t.Fatalf("canonical and original locations not both preserved: %s", output.String())
	}
}
