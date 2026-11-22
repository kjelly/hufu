package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestSnapshotHandoffFitsRuntimeOutputAndPassesFinalAudit(t *testing.T) {
	fixture := gateFixture(t)
	fixture.Tasks = append(fixture.Tasks[:2:2], fixture.Tasks[4:]...)
	producer, verifier, handoff := &fixture.Tasks[3], &fixture.Tasks[4], &fixture.Tasks[5]
	identity := snapshotIdentity{Revision: "frozen-head", Batch: 1, BatchCount: 1, WorksetsPerBatch: 30, FileCount: 2800}
	digest := sha256.New()
	const primaryPath = "large.go"
	for i := range identity.FileCount {
		file := snapshotInventoryFile{
			Path: fmt.Sprintf("docs/%04d-%s.md", i, strings.Repeat("inventory", 12)), Mode: "100644",
			SHA256: strings.Repeat("a", 64), Bytes: 20, Lines: 1, Disposition: "excluded", Reason: "documentation_outside_code_audit",
		}
		if i == identity.FileCount-1 {
			file.Path, file.Lines, file.Disposition, file.Reason = primaryPath, 2, "selected", ""
		}
		identity.Files = append(identity.Files, file)
		entry := raw(t, []any{file.Path, file.Mode, file.SHA256, file.Bytes})
		_, _ = digest.Write(append(entry, '\n'))
	}
	identity.Plan = &snapshotPlan{
		Worksets: []snapshotPlannedWorkset{
			{Sources: []snapshotPlannedSource{{Path: primaryPath, Spans: []snapshotSpan{{Start: 0, End: 10, StartLine: 1, EndLine: 1}}}}},
			{Sources: []snapshotPlannedSource{{Path: primaryPath, Spans: []snapshotSpan{{Start: 10, End: 20, StartLine: 2, EndLine: 2}}}}},
		},
		BatchSourceSHA256: []string{strings.Repeat("c", 64), strings.Repeat("d", 64)},
	}
	identity.Hash = hex.EncodeToString(digest.Sum(nil))
	identity.ID = "sha256:" + identity.Hash
	root := t.TempDir()
	actions := filepath.Join(root, "runtime", "runs", "run-1", "actions")
	workspace := filepath.Join(actions, "action-"+producer.ID)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	fullInventory := raw(t, identity)
	if err := os.WriteFile(filepath.Join(workspace, "inventory.json"), fullInventory, 0o600); err != nil {
		t.Fatal(err)
	}
	artifactHash := fmt.Sprintf("%x", sha256.Sum256(fullInventory))
	plan := identity.Plan
	identity.Files = nil
	identity.Plan = nil
	identity.Inventory = &snapshotInventoryReference{ID: "sha256-" + artifactHash, Path: "inventory.json", Kind: "repository_snapshot_inventory", SHA256: artifactHash, Bytes: int64(len(fullInventory))}
	scope := `{"kind":"repository_snapshot","history":"first_parent","head":"HEAD"}`
	producer.Result.RuntimeOutputs["scope"] = raw(t, map[string]any{
		"requested":            jsontext.Value(scope),
		"requested_input_hash": "sha256:" + strings.Repeat("b", 64),
		"resolved": map[string]any{
			"base": identity.Revision, "head": identity.Revision, "selected_commit_count": 1,
			"available_commit_count": 1, "history_exhausted": false, "repository_shallow": false,
		},
		"observed_budget": map[string]int{
			"changed_paths": identity.FileCount, "total_diff_bytes": 0, "total_diff_lines": 0, "workset_items": 2,
		},
		"satisfied": true, "repository_snapshot": identity,
	})
	schema, _ := contractSchema(t, "report-v2.json")
	var scopeValue map[string]any
	if err := json.Unmarshal(producer.Result.RuntimeOutputs["scope"], &scopeValue); err != nil {
		t.Fatal(err)
	}
	if err := schema.Properties["scope"].Validate(scopeValue); err != nil {
		t.Fatalf("receipt-bound scope cannot be admitted by the report contract: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing inventory", func(snapshot map[string]any) { delete(snapshot, "inventory") }},
		{"missing file count", func(snapshot map[string]any) { delete(snapshot, "file_count") }},
		{"mixed inline and referenced files", func(snapshot map[string]any) {
			snapshot["files"] = []any{map[string]any{
				"path": "code.go", "mode": "100644", "sha256": strings.Repeat("c", 64),
				"bytes": 1, "lines": 1, "disposition": "selected", "reason": "",
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := maps.Clone(scopeValue)
			snapshot := maps.Clone(value["repository_snapshot"].(map[string]any))
			tc.mutate(snapshot)
			value["repository_snapshot"] = snapshot
			if err := schema.Properties["scope"].Validate(value); err == nil {
				t.Fatal("report contract accepted an invalid snapshot representation")
			}
		})
	}
	sealFixtureOutputs(t, producer)
	var reviews []inputReview
	for i := range 2 {
		prior := fixture.Tasks[i]
		r := record{Kind: "review", Revision: identity.Revision, Key: fmt.Sprintf("unit-%04d", i), Lens: "general", Findings: []finding{}}
		item := fixtureTask(t, prior.ID, prior.ContractID, "success", payload{Record: raw(t, r)}, r, nil)
		item.Binding.Bindings["review_mode"] = "repository_snapshot"
		item.Binding.Bindings["snapshot_id"] = identity.ID
		item.Binding.Bindings["content_sha256"] = identity.Hash
		item.Binding.TouchedPaths = []string{primaryPath}
		sha := plan.BatchSourceSHA256[i]
		id := "sha256-" + sha
		item.Binding.Inputs = append(item.Binding.Inputs, struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			SHA256 string `json:"sha256"`
		}{ID: id, Kind: "review_source_snapshot", SHA256: sha})
		item.Result.FilesRead[0].Path = id
		fixture.Tasks[i] = item
		reviews = append(reviews, entry(item, r, t))
	}
	var tests map[string]any
	if err := json.Unmarshal(verifier.Result.RuntimeOutputs["targeted_go_tests"], &tests); err != nil {
		t.Fatal(err)
	}
	tests["snapshot_id"], tests["content_sha256"] = identity.ID, identity.Hash
	verifier.Result.RuntimeOutputs["targeted_go_tests"] = raw(t, tests)
	sealFixtureOutputs(t, verifier)
	teamDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := team.LoadTeam(teamDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(capability, action, taskID string) (any, error) {
		provider, ok := loaded.ProviderRegistry.Get(capability)
		if !ok {
			t.Fatalf("missing provider %s", capability)
		}
		actionID := "action-" + taskID
		workspace := filepath.Join(actions, actionID)
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatal(err)
		}
		ctx := team.WithActionEnvironment(t.Context(), team.ActionEnvironment{Workspace: workspace, RunID: "run-1", ActionInvocationID: actionID, TaskID: taskID})
		return provider.Execute(ctx, team.Action{Capability: capability, Type: action, Payload: `{"scope":` + scope + `}`})
	}
	handoff.Status = "in_progress"
	writeCheckpoint(t, root, raw(t, fixture))
	response, err := execute(handoff.ContractID, "inventory_review_handoffs", handoff.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Outputs   map[string]any               `json:"outputs"`
		Artifacts []snapshotInventoryReference `json:"artifacts"`
	}
	if err := json.Unmarshal(raw(t, response), &result); err != nil {
		t.Fatal(err)
	}
	if _, _, err := team.CanonicalizeRuntimeOutputs(result.Outputs); err != nil {
		t.Fatalf("snapshot handoff cannot be accepted by runtime: %v", err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Kind != "repository_snapshot_coverage" {
		t.Fatalf("complete coverage was not declared: %#v", result.Artifacts)
	}
	artifactPath := filepath.Join(actions, "action-"+handoff.ID, result.Artifacts[0].Path)
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var coverage snapshotCoverage
	if err := json.Unmarshal(data, &coverage); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(coverage.Completed, []string{primaryPath}) || !slices.Equal(coverage.ReviewedWorksets, []int{1, 2}) || len(coverage.WorksetPlanSHA256) != 64 {
		t.Fatalf("coverage did not include both assigned fragments: %v, %v", coverage.Completed, coverage.ReviewedWorksets)
	}
	for _, tc := range []struct {
		name       string
		change     func(*sourceSession)
		wantError  string
		wantReason string
	}{
		{"missing review", func(view *sourceSession) { view.Tasks = append(view.Tasks[:1:1], view.Tasks[2:]...) }, "no accepted review", ""},
		{"duplicate review", func(view *sourceSession) { view.Tasks[1] = view.Tasks[0] }, "duplicated snapshot workset", ""},
		{"substituted source", func(view *sourceSession) { view.Tasks[0].Binding.Inputs = view.Tasks[1].Binding.Inputs }, "did not observe its planned snapshot source", ""},
		{"finding outside fragment", func(view *sourceSession) {
			item := &view.Tasks[0]
			item.Result.Findings = []finding{{Location: primaryPath + ":2", Summary: "outside", Severity: "info", Evidence: []string{item.Binding.Inputs[0].ID}}}
			r := record{Kind: "review", Revision: identity.Revision, Key: item.Binding.Bindings["key"], Lens: "general", Findings: item.Result.Findings}
			item.Result.Payload.Value = raw(t, payload{Record: raw(t, r)})
		}, "outside the assigned snapshot fragment", ""},
		{"evidence gap in one fragment", func(view *sourceSession) {
			item := &view.Tasks[1]
			item.Result.Status = "completed_with_gaps"
			r := record{Kind: "review", Revision: identity.Revision, Key: item.Binding.Bindings["key"], Lens: "general",
				Gaps: []jsonv1.RawMessage{raw(t, map[string]string{"kind": "semantic_uncertainty"})}}
			item.Result.Payload.Value = raw(t, payload{Record: raw(t, r)})
		}, "", "review_evidence_gap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := projectedFixture(fixture, "run-1")
			if err != nil {
				t.Fatal(err)
			}
			view.Tasks[3].Workspace = workspace
			tc.change(&view)
			result, err := collectSnapshotCoverage(view)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("expected %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil || len(result.Completed) != 0 || !slices.Equal(result.Uncompleted, []snapshotUnfinished{{Path: primaryPath, Reason: tc.wantReason}}) || !slices.Equal(result.ReviewedWorksets, []int{1}) {
				t.Fatalf("unexpected coverage: %v, reviewed %v, error %v", result.Uncompleted, result.ReviewedWorksets, err)
			}
		})
	}
	t.Run("fragment outside selected batch", func(t *testing.T) {
		view, err := projectedFixture(fixture, "run-1")
		if err != nil {
			t.Fatal(err)
		}
		var split snapshotIdentity
		if err := json.Unmarshal(fullInventory, &split); err != nil {
			t.Fatal(err)
		}
		split.WorksetsPerBatch, split.BatchCount = 1, 2
		split.Plan.BatchSourceSHA256 = split.Plan.BatchSourceSHA256[:1]
		data := raw(t, split)
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "inventory.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(data))
		split.Files, split.Plan = nil, nil
		split.Inventory = &snapshotInventoryReference{ID: "sha256-" + hash, Path: "inventory.json", Kind: "repository_snapshot_inventory", SHA256: hash, Bytes: int64(len(data))}
		var scope snapshotAttestation
		if err := json.Unmarshal(view.Tasks[3].Result.RuntimeOutputs["scope"], &scope); err != nil {
			t.Fatal(err)
		}
		scope.Snapshot = &split
		view.Tasks[3].Workspace = root
		view.Tasks[3].Result.RuntimeOutputs["scope"] = raw(t, scope)
		view.Tasks = append(view.Tasks[:1:1], view.Tasks[2:]...)
		result, err := collectSnapshotCoverage(view)
		if err != nil || len(result.Completed) != 0 || !slices.Equal(result.Uncompleted, []snapshotUnfinished{{Path: primaryPath, Reason: "outside_selected_batch"}}) || !slices.Equal(result.ReviewedWorksets, []int{1}) {
			t.Fatalf("cross-batch fragment counted incorrectly: %v, reviewed %v, error %v", result.Uncompleted, result.ReviewedWorksets, err)
		}
	})
	fixture.Tasks[2] = fixtureTask(t, "7", "synthesize-review", "success", payload{
		Scope: producer.Result.RuntimeOutputs["scope"], Tests: verifier.Result.RuntimeOutputs["targeted_go_tests"],
		SnapshotCoverage: raw(t, result.Outputs["snapshot_coverage"]), Coverage: "complete", InputReview: reviews,
	}, record{}, []string{"3", "4"})
	handoff.Result.RuntimeOutputs["review_handoff_inventory"] = raw(t, result.Outputs["review_handoff_inventory"])
	handoff.Result.RuntimeOutputs["snapshot_coverage"] = raw(t, result.Outputs["snapshot_coverage"])
	handoff.Status = "done"
	sealFixtureOutputs(t, handoff)
	fixture.Tasks = append(fixture.Tasks, task{ID: "10", Status: "in_progress", ContractID: "verify-review-report", SnapshotID: fixture.SnapshotID})
	writeCheckpoint(t, root, raw(t, fixture))
	audit, err := execute("verify-review-report", "verify_review_report", "10")
	if err != nil || !bytes.Contains(raw(t, audit), []byte(`"passed":true`)) {
		t.Fatalf("final audit rejected the verified artifact: %v, %s", err, raw(t, audit))
	}
	if err := os.WriteFile(artifactPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("verify-review-report", "verify_review_report", "10"); err == nil || !strings.Contains(err.Error(), "/snapshot_coverage/inventory") {
		t.Fatalf("final audit accepted modified coverage: %v", err)
	}
}

func TestSnapshotCoverageLoadsVerifiedInventory(t *testing.T) {
	for _, mode := range []string{"reference", "legacy", "missing", "tampered", "hash", "size", "identity", "count", "traversal", "symlink", "inline_override", "unverified"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			files := []snapshotInventoryFile{
				{Path: "docs/guide.md", Mode: "100644", SHA256: strings.Repeat("a", 64), Bytes: 20, Lines: 1, Disposition: "excluded", Reason: "documentation_outside_code_audit"},
				{Path: "code.go", Mode: "100644", SHA256: strings.Repeat("b", 64), Bytes: 10, Lines: 1, Disposition: "unreviewed", Reason: "outside_selected_batch"},
			}
			digest := sha256.New()
			for _, file := range files {
				entry, err := json.Marshal([]any{file.Path, file.Mode, file.SHA256, file.Bytes})
				if err != nil {
					t.Fatal(err)
				}
				_, _ = digest.Write(append(entry, '\n'))
			}
			hash := hex.EncodeToString(digest.Sum(nil))
			full := snapshotIdentity{ID: "sha256:" + hash, Hash: hash, Revision: "frozen", Batch: 1, BatchCount: 2, WorksetsPerBatch: 30, FileCount: 2, Files: files}
			data, err := json.Marshal(full)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "inventory.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			artifactHash := hex.EncodeToString(sum[:])
			summary := full
			summary.Files = nil
			summary.Inventory = &snapshotInventoryReference{ID: "sha256-" + artifactHash, Kind: "repository_snapshot_inventory", Path: "inventory.json", SHA256: artifactHash, Bytes: int64(len(data))}
			switch mode {
			case "legacy":
				summary = full
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "tampered":
				if err := os.WriteFile(path, append(data, ' '), 0o600); err != nil {
					t.Fatal(err)
				}
			case "size":
				summary.Inventory.Bytes++
			case "hash":
				changed := append([]byte(nil), data...)
				changed[0] = ' '
				if err := os.WriteFile(path, changed, 0o600); err != nil {
					t.Fatal(err)
				}
			case "identity":
				summary.Revision = "different"
			case "count":
				summary.FileCount++
			case "traversal":
				summary.Inventory.Path = "../inventory.json"
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside.json")
				if err := os.WriteFile(outside, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "link.json")); err != nil {
					t.Fatal(err)
				}
				summary.Inventory.Path = "link.json"
			case "inline_override":
				summary.Files = files
			}
			var current sourceSession
			view, err := json.Marshal(map[string]any{"tasks": []any{
				map[string]any{"ID": "1", "contract_id": "produce-workset", "verified_outputs": mode != "unverified", "workspace": root, "typed_result": map[string]any{"runtime_outputs": map[string]any{"scope": map[string]any{"requested": map[string]string{"kind": "repository_snapshot"}, "resolved": map[string]string{"head": summary.Revision}, "satisfied": true, "repository_snapshot": summary}}}},
				map[string]any{"ID": "2", "contract_id": "verify-targeted-go-tests", "verified_outputs": true, "typed_result": map[string]any{"runtime_outputs": map[string]any{"targeted_go_tests": map[string]string{"snapshot_id": full.ID, "content_sha256": full.Hash, "reviewed_revision": full.Revision}}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(view, &current); err != nil {
				t.Fatal(err)
			}
			coverage, err := collectSnapshotCoverage(current)
			valid := mode == "reference" || mode == "legacy"
			if (err == nil) != valid {
				t.Fatalf("coverage=%+v err=%v", coverage, err)
			}
			if valid && (len(coverage.Excluded) != 1 || len(coverage.Uncompleted) != 1 || len(coverage.Expected) != 1 || coverage.Uncompleted[0].Path != "code.go") {
				t.Fatalf("inventory membership lost: %+v", coverage)
			}
		})
	}
}
