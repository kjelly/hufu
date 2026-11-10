package team

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func dependencyViewRenderFixture() (TaskResult, DependencyResultViewSource) {
	return TaskResult{
		TaskID: "source", Agent: "producer", Attempt: 1, Status: TaskResultStatusSuccess,
		Summary:       "summary_sentinel ``` untrusted",
		Details:       strings.Repeat("unselected_details ", 4096),
		Findings:      []Finding{{Category: "correctness", Summary: "finding_sentinel", Detail: "finding_details", Severity: "high"}},
		Artifacts:     []ArtifactRef{{ID: "opaque_sentinel", Path: "private_backing_path", SHA256: strings.Repeat("a", 64), Bytes: 7, Description: "artifact_sentinel"}},
		FilesModified: []FileRef{{Path: "edited_sentinel", Purpose: "changed code"}},
		Verification:  []VerificationResult{{Command: "check", ExitCode: 0, Stdout: "stdout_sentinel", Stderr: "stderr_sentinel", Duration: 125 * time.Millisecond, Fingerprint: "verification_sentinel"}},
		Risks:         []Risk{{Description: "unselected_risk"}},
		ReceiptIDs:    []string{"unselected_receipt"},
	}, DependencyResultViewSource{BranchID: "origin", RunID: "accepted-run", TaskID: "source", Attempt: 1, OccurrenceRevision: 4, SourceEventID: "accepted-event", SourceEventSHA256: strings.Repeat("b", 64)}
}

func TestDependencyResultViewRendering(t *testing.T) {
	markers := map[string]string{"summary": "summary_sentinel", "findings": "finding_sentinel", "artifacts": "opaque_sentinel", "files_modified": "edited_sentinel", "verification": "verification_sentinel"}
	for _, field := range dependencyResultViewFields {
		t.Run(field, func(t *testing.T) {
			result, source := dependencyViewRenderFixture()
			before, err := json.Marshal(result, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			view, err := buildDependencyResultView(&DependencyResultViewSpec{Mode: "selected", Fields: []string{field}}, []TaskResult{result}, []DependencyResultViewSource{source})
			if err != nil {
				t.Fatal(err)
			}
			for candidate, marker := range markers {
				if strings.Contains(view.content, marker) != (candidate == field) {
					t.Fatalf("field %q disclosure does not match selection", candidate)
				}
			}
			for _, forbidden := range []string{"unselected_details", "unselected_risk", "unselected_receipt", "private_backing_path"} {
				if strings.Contains(view.content, forbidden) {
					t.Fatalf("view disclosed %s", forbidden)
				}
			}
			if !strings.Contains(view.content, "### Task [source] (Agent: producer, Status: success)") || !strings.Contains(view.content, "not complete upstream evidence") {
				t.Fatal("view lost its source header or evidence limitation")
			}
			if field == "summary" && !strings.Contains(view.content, "````json") {
				t.Fatal("untrusted text can close its fence")
			}
			if field == "verification" && (!strings.Contains(view.content, "stdout_sentinel") || !strings.Contains(view.content, "stderr_sentinel") || !strings.Contains(view.content, `"duration":125000000`)) {
				t.Fatal("selected verification lost recorded diagnostics or duration semantics")
			}
			if !validFullContentHash(view.attribution.PolicySHA256) || view.attribution.Sources[0].FullViewSHA256 != dependencyViewDigest([]byte(FormatDependencyResults([]TaskResult{result}))) {
				t.Fatal("view attribution does not describe the original handoff")
			}
			after, err := json.Marshal(result, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
			if err != nil || string(before) != string(after) || source.FullViewSHA256 != "" {
				t.Fatalf("renderer changed canonical source data: %v", err)
			}
			if len(view.content) >= len(FormatDependencyResults([]TaskResult{result})) {
				t.Fatal("large omitted details did not reduce presentation bytes")
			}
		})
	}
}

func TestDependencyResultViewRenderingOrderAndAttributionIsolation(t *testing.T) {
	first, firstSource := dependencyViewRenderFixture()
	second, secondSource := dependencyViewRenderFixture()
	second.TaskID, secondSource.TaskID = "another", "another"
	results, sources := []TaskResult{second, first}, []DependencyResultViewSource{secondSource, firstSource}
	policy := &DependencyResultViewSpec{Mode: "selected", Fields: []string{"verification", "summary"}}
	view, err := buildDependencyResultView(policy, results, sources)
	if err != nil {
		t.Fatal(err)
	}
	policy.Fields = []string{"summary", "verification"}
	again, err := buildDependencyResultView(policy, results, sources)
	if err != nil || view.content != again.content || !reflect.DeepEqual(view.attribution, again.attribution) {
		t.Fatalf("author ordering changed deterministic view: %v", err)
	}
	if strings.Index(view.content, "Task [another]") >= strings.Index(view.content, "Task [source]") {
		t.Fatal("renderer changed canonical source order")
	}
	clone := view.attribution.Clone()
	clone.Policy.Fields[0], clone.Sources[0].FullViewSHA256 = "findings", "changed"
	policy.Fields[0], sources[0].TaskID = "artifacts", "changed"
	if view.attribution.Policy.Fields[0] != "summary" || view.attribution.Sources[0].TaskID != "another" || !validFullContentHash(view.attribution.Sources[0].FullViewSHA256) {
		t.Fatal("attribution aliases author policy, source inputs, or another clone")
	}
	for _, full := range []*DependencyResultViewSpec{nil, {Mode: "full"}} {
		if override, err := buildDependencyResultView(full, results, nil); err != nil || override != nil {
			t.Fatalf("full mode did not preserve the original formatter: %v", err)
		}
	}
}

func TestDependencyResultViewRejectsUnavailableSources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*TaskResult, *DependencyResultViewSource)
	}{
		{"task identity", func(_ *TaskResult, s *DependencyResultViewSource) { s.TaskID = "other" }},
		{"agent identity", func(r *TaskResult, _ *DependencyResultViewSource) { r.Agent = "" }},
		{"failed result", func(r *TaskResult, _ *DependencyResultViewSource) { r.Status = "failed" }},
		{"attempt", func(_ *TaskResult, s *DependencyResultViewSource) { s.Attempt++ }},
		{"revision", func(_ *TaskResult, s *DependencyResultViewSource) { s.OccurrenceRevision = 0 }},
		{"branch", func(_ *TaskResult, s *DependencyResultViewSource) { s.BranchID = "" }},
		{"run", func(_ *TaskResult, s *DependencyResultViewSource) { s.RunID = "" }},
		{"event", func(_ *TaskResult, s *DependencyResultViewSource) { s.SourceEventID = "" }},
		{"event digest", func(_ *TaskResult, s *DependencyResultViewSource) { s.SourceEventSHA256 = "changed" }},
		{"handoff digest", func(_ *TaskResult, s *DependencyResultViewSource) { s.FullViewSHA256 = strings.Repeat("c", 64) }},
		{"structured payload", func(r *TaskResult, _ *DependencyResultViewSource) { r.StructuredPayload = &ResultPayload{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, source := dependencyViewRenderFixture()
			tc.change(&result, &source)
			view, err := buildDependencyResultView(&DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}, []TaskResult{result}, []DependencyResultViewSource{source})
			if err == nil || view != nil {
				t.Fatal("invalid source produced a selected view")
			}
		})
	}
	result, source := dependencyViewRenderFixture()
	for _, sources := range [][]DependencyResultViewSource{nil, {source, source}} {
		results := slices.Repeat([]TaskResult{result}, len(sources))
		if len(sources) == 0 {
			results = []TaskResult{result}
		}
		if view, err := buildDependencyResultView(&DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}, results, sources); err == nil || view != nil {
			t.Fatal("missing or duplicated source was accepted")
		}
	}
}
