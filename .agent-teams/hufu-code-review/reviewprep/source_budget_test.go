package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/golangruntime"
)

func TestPrepareBoundsSourceSnapshotInputs(t *testing.T) {
	for _, route := range []string{"", routingDocumentation} {
		for _, limit := range []string{"source_bytes", "input_tokens"} {
			t.Run(route+"/"+limit, func(t *testing.T) {
				repo := newLargeSourceBudgetRepo(t)
				config := sourceBudgetFixtureConfig(repo, "out")
				config.Routing = route
				config.MaxSourceSnapshotBytes = 1_000_000
				config.MaxReviewInputTokens = defaultMaxReviewInputTokens
				if limit == "source_bytes" {
					config.MaxSourceSnapshotBytes = 20_000
				} else {
					config.MaxReviewInputTokens = 10_000
				}
				if _, err := Prepare(t.Context(), config); err != nil {
					t.Fatal(err)
				}
				manifestPath := filepath.Join(repo, "out", "workset-manifest.json")
				if route != "" {
					manifestPath = filepath.Join(repo, "out", "primary", "workset-manifest.json")
				}
				m := readManifest(t, manifestPath)
				rebuilt := make(map[string][]byte)
				for _, entry := range m.Items {
					assertReviewInputBudget(t, repo, entry, config)
					if len(entry.TouchedPaths) != 1 {
						t.Fatalf("large sources must be partitioned by path: %#v", entry)
					}
					collectBudgetedPrimary(t, repo, entry, rebuilt)
					diff, err := os.ReadFile(filepath.Join(repo, entry.Inputs[0].Path))
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(diff, []byte("return 1")) {
						t.Fatalf("source split lost the changed hunk: %s", diff)
					}
				}
				for _, file := range []string{"src/Alpha.go", "src/Beta.go"} {
					want, err := os.ReadFile(filepath.Join(repo, file))
					if err != nil || !bytes.Equal(rebuilt[file], want) {
						t.Fatalf("partitioning changed or lost primary bytes for %s: %v", file, err)
					}
				}
				// Repacking uses stable input ordering and final partition keys.
				before := mustJSON(t, m)
				if err := os.RemoveAll(filepath.Join(repo, "out")); err != nil {
					t.Fatal(err)
				}
				if _, err := Prepare(t.Context(), config); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, mustJSON(t, readManifest(t, manifestPath))) {
					t.Fatal("source budgeting changed manifest keys, ordering, or artifact hashes on identical inputs")
				}
			})
		}
	}
}

func collectBudgetedPrimary(t *testing.T, root string, entry item, rebuilt map[string][]byte) {
	t.Helper()
	var bundle []byte
	var inventory evidenceInventory
	for _, artifact := range entry.Inputs {
		switch artifact.Kind {
		case "review_source_snapshot", "review_evidence_inventory":
			data, err := os.ReadFile(filepath.Join(root, artifact.Path))
			if err != nil {
				t.Fatal(err)
			}
			if artifact.Kind == "review_source_snapshot" {
				bundle = data
			} else if err := json.Unmarshal(data, &inventory); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, source := range inventory.Sources {
		if source.Role != "primary" {
			continue
		}
		if rebuilt[source.Path] == nil {
			rebuilt[source.Path] = make([]byte, source.ByteSize)
		}
		for _, span := range source.AvailableSpans {
			copy(rebuilt[source.Path][span.Start:span.End], bundle[span.BundleStart:span.BundleEnd])
		}
	}
}

func TestPrepareSourceBudgetFailsBeforePublishingOversizedItems(t *testing.T) {
	for _, limit := range []string{"single_source", "single_hunk", "total_items"} {
		t.Run(limit, func(t *testing.T) {
			repo := newLargeSourceBudgetRepo(t)
			config := sourceBudgetFixtureConfig(repo, "out")
			want := "workset_item_too_large"
			switch limit {
			case "single_source":
				config.MaxSourceSnapshotBytes = 400
			case "single_hunk":
				config.MaxReviewInputTokens = 300
			case "total_items":
				config.MaxSourceSnapshotBytes = 20_000
				config.MaxWorksetItems = 1
				want = "scope_too_large"
			}
			_, err := Prepare(t.Context(), config)
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "remediation:") {
				t.Fatalf("Prepare error = %v, want %s with remediation", err, want)
			}
			if _, err := os.Stat(filepath.Join(repo, "out")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("input budget rejection published partial worksets: %v", err)
			}
		})
	}
}

func TestRoutedSourceBudgetSplitsDocumentationPartitions(t *testing.T) {
	for _, directory := range []string{"docs/tutorials", "docs/reference"} {
		t.Run(directory, func(t *testing.T) {
			repo := newFixtureRepo(t)
			for _, name := range []string{"alpha", "beta"} {
				writeFile(t, filepath.Join(repo, directory, name+".md"), strings.Repeat("Unchanged documentation context.\n", 350)+"\nVersion zero.\n")
			}
			commit(t, repo, "documentation baseline", "2025-01-02T00:00:00Z")
			for _, name := range []string{"alpha", "beta"} {
				writeFile(t, filepath.Join(repo, directory, name+".md"), strings.Repeat("Unchanged documentation context.\n", 350)+"\nVersion one.\n")
			}
			commit(t, repo, "tiny documentation changes", "2025-01-03T00:00:00Z")
			config := sourceBudgetFixtureConfig(repo, "out")
			config.Routing = routingDocumentation
			config.MaxSourceSnapshotBytes = 20_000
			if _, err := Prepare(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			for _, route := range []string{"primary", "documentation", "documentation-escalation"} {
				m := readManifest(t, filepath.Join(repo, "out", route, "workset-manifest.json"))
				isReview := directory == "docs/reference" && route != "documentation" || directory == "docs/tutorials" && route == "documentation"
				if isReview && len(m.Items) != 2 {
					t.Fatalf("%s produced %d documentation items, want 2", route, len(m.Items))
				}
				for _, entry := range m.Items {
					assertReviewInputBudget(t, repo, entry, config)
					if entry.Bindings["route"] != route || entry.Bindings["reference_candidate_ids"] != "[]" {
						t.Fatalf("source split lost route/candidate binding: %#v", entry.Bindings)
					}
					if isReview && (len(entry.TouchedPaths) != 1 || entry.Lens == "noop") {
						t.Fatalf("%s source partition = %#v", route, entry)
					}
				}
			}
		})
	}
}

func TestPrepareCapsRelatedSourceWithoutTruncatingPrimary(t *testing.T) {
	repo := newFixtureRepo(t)
	primary := "package sample\n\nfunc ReviewedTarget() int { return 0 }\n"
	caller := "package sample\n\nfunc Caller() int { return ReviewedTarget() }\n" + strings.Repeat("// related evidence padding\n", 4000)
	writeFile(t, filepath.Join(repo, "src/target.go"), primary)
	writeFile(t, filepath.Join(repo, "src/caller.go"), caller)
	commit(t, repo, "baseline with a large caller", "2025-01-02T00:00:00Z")
	primary = strings.ReplaceAll(primary, "return 0", "return 1")
	writeAndCommit(t, repo, "src/target.go", primary, "change target", "2025-01-03T00:00:00Z")
	config := sourceBudgetFixtureConfig(repo, "out")
	if _, err := Prepare(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	m := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	entry := m.Items[0]
	assertReviewInputBudget(t, repo, entry, config)
	data, err := os.ReadFile(filepath.Join(repo, entry.Inputs[1].Path))
	if err != nil {
		t.Fatal(err)
	}
	sections := strings.Split(string(data), "\n## ")
	if len(sections) != 3 || !strings.Contains(sections[1], primary) || strings.Contains(sections[1], "content_truncated") {
		t.Fatalf("primary evidence was truncated: %s", data[:500])
	}
	if !strings.Contains(sections[2], "content_truncated: true") || !strings.Contains(sections[2], "omitted_bytes:") || !strings.Contains(sections[2], "return ReviewedTarget()") {
		t.Fatal("related caller lacks its explicit evidence limitation or useful prefix")
	}
	if entry.SourceSnapshotBytes > maxRelatedSnapshotContentBytes+4096 {
		t.Fatalf("related content escaped its separate byte budget: %d", entry.SourceSnapshotBytes)
	}
}

func TestSourceBudgetSplitsSinglePathHunksWithoutLosingDiff(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "source.txt"), "small source\n")
	snapshot := &reviewSourceSnapshot{root: root, revision: "frozen", baseRevision: "frozen"}
	current := &batch{lens: "general", pathSeen: make(map[string]struct{})}
	for _, marker := range []string{"first", "second"} {
		data := []byte("@@ " + marker + " @@\n" + strings.Repeat("+payload\n", 100))
		current.add("source.txt", data, bytes.Count(data, []byte("\n")))
	}
	source, inventory, err := snapshot.bundleDataWithScopes("out/batches/unit-0000", current.paths, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{MaxSourceSnapshotBytes: 10_000, MaxReviewInputTokens: reviewInputFramingTokens + estimateReviewInputTokens(len(source)) + estimateReviewInputTokens(len(inventory)) + estimateReviewInputTokens(len(current.parts[0].diff)) + 16}
	result, err := boundReviewBatches(root, filepath.Join(root, "out"), []*batch{current}, snapshot, config, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || !bytes.Equal(append(bytes.Clone(result[0].diff.Bytes()), result[1].diff.Bytes()...), current.diff.Bytes()) {
		t.Fatal("splitting an oversized single-path input lost or duplicated a diff hunk")
	}
}

func TestEmbeddedRuntimeEnforcesSourceSnapshotBudget(t *testing.T) {
	repo := newLargeSourceBudgetRepo(t)
	scope := lastNScope(1)
	payload := wireConfig{Scope: &scope, Routing: routingDocumentation, MaxSourceSnapshotBytes: 20_000, MaxReviewInputTokens: 12_000}
	request := mustJSON(t, actionRequest{Type: "prepare_review_workset", Payload: string(mustJSON(t, payload))})
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	program, err := golangruntime.Prepare(source, ".")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	env := append(os.Environ(), "HUFU_REPOSITORY="+repo, "HUFU_WORKSPACE="+workspace)
	result, err := golangruntime.Execute(t.Context(), "", program, request, env, 1<<20, 16<<10)
	if err != nil {
		t.Fatalf("execute source-aware producer: %v; stderr=%s", err, result.Stderr)
	}
	m := readManifest(t, filepath.Join(workspace, "workset", "primary", "workset-manifest.json"))
	if len(m.Items) != 2 {
		t.Fatalf("embedded provider produced %d items, want 2 source-bounded items", len(m.Items))
	}
	config := Config{MaxSourceSnapshotBytes: payload.MaxSourceSnapshotBytes, MaxReviewInputTokens: payload.MaxReviewInputTokens}
	for _, entry := range m.Items {
		assertReviewInputBudget(t, workspace, entry, config)
		assertFrozenEvidenceIntegrity(t, workspace, entry)
	}
}

func assertFrozenEvidenceIntegrity(t *testing.T, workspace string, entry item) {
	t.Helper()
	var inventory evidenceInventory
	var bundle []byte
	for _, input := range entry.Inputs {
		data, err := os.ReadFile(filepath.Join(workspace, input.Path))
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) != input.Bytes || sha256Hex(data) != input.SHA256 {
			t.Fatalf("artifact integrity mismatch: %s", input.Path)
		}
		switch input.Kind {
		case "review_source_snapshot":
			bundle = data
		case "review_evidence_inventory":
			if err := json.Unmarshal(data, &inventory); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(inventory.Sources) == 0 || len(bundle) == 0 {
		t.Fatal("missing frozen evidence")
	}
	for _, row := range inventory.Sources {
		data, err := os.ReadFile(filepath.Join(workspace, entry.Bindings["frozen_source_root"], row.Path))
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != "present" || row.ByteSize != int64(len(data)) || row.SHA256 != sha256Hex(data) {
			t.Fatalf("corrupt source identity: %#v", row)
		}
		available := int64(0)
		for _, span := range row.AvailableSpans {
			if span.Start < 0 || span.Start >= span.End || span.End > int64(len(data)) || span.BundleStart < 0 || span.BundleStart >= span.BundleEnd || span.BundleEnd > int64(len(bundle)) {
				t.Fatalf("invalid source span: %#v", span)
			}
			if !bytes.Equal(data[span.Start:span.End], bundle[span.BundleStart:span.BundleEnd]) {
				t.Fatalf("source bytes changed: %s", row.Path)
			}
			available += span.End - span.Start
		}
		if row.OmittedBytes != int64(len(data))-available {
			t.Fatalf("corrupt omitted bytes: %#v", row)
		}
	}
}

func TestDecodeWireConfigSourceBudgets(t *testing.T) {
	config, err := decodeWireConfig(`{"max_source_snapshot_bytes":12345,"max_review_input_tokens":6789}`)
	if err != nil || config.MaxSourceSnapshotBytes != 12345 || config.MaxReviewInputTokens != 6789 {
		t.Fatalf("source budget wire config = %#v, %v", config, err)
	}
	for _, payload := range []string{`{"max_source_snapshot_bytes":-1}`, `{"max_review_input_tokens":-1}`} {
		config, err := decodeWireConfig(payload)
		if err != nil {
			t.Fatal(err)
		}
		applyConfigDefaults(&config)
		if err := validateConfig(config); err == nil {
			t.Fatalf("negative source budget was accepted: %s", payload)
		}
	}
}

func newLargeSourceBudgetRepo(t *testing.T) string {
	t.Helper()
	repo := newFixtureRepo(t)
	for _, name := range []string{"Alpha", "Beta"} {
		text := "package sample\n\n" + strings.Repeat("// unchanged source context line\n", 225) + "\nfunc " + name + "() int { return 0 }\n"
		writeFile(t, filepath.Join(repo, "src", name+".go"), text)
	}
	commit(t, repo, "large unchanged source baseline", "2025-01-02T00:00:00Z")
	for _, name := range []string{"Alpha", "Beta"} {
		path := filepath.Join(repo, "src", name+".go")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, strings.ReplaceAll(string(data), "return 0", "return 1"))
	}
	commit(t, repo, "tiny changes to large sources", "2025-01-03T00:00:00Z")
	return repo
}

func sourceBudgetFixtureConfig(repo, output string) Config {
	config := fixtureConfig(repo, output)
	config.Scope = lastNScope(1)
	applyConfigDefaults(&config)
	return config
}

func assertReviewInputBudget(t *testing.T, root string, entry item, config Config) {
	t.Helper()
	tokens := reviewInputFramingTokens
	for _, input := range entry.Inputs {
		data, err := os.ReadFile(filepath.Join(root, input.Path))
		if err != nil {
			t.Fatal(err)
		}
		tokens += estimateReviewInputTokens(len(data))
		if input.Kind == "review_source_snapshot" && (len(data) != entry.SourceSnapshotBytes || len(data) > config.MaxSourceSnapshotBytes) {
			t.Fatalf("%s source bytes = %d, recorded %d, limit %d", entry.Key, len(data), entry.SourceSnapshotBytes, config.MaxSourceSnapshotBytes)
		}
	}
	if tokens != entry.EstimatedReviewInputTokens || tokens > config.MaxReviewInputTokens {
		t.Fatalf("%s input tokens = %d, recorded %d, limit %d", entry.Key, tokens, entry.EstimatedReviewInputTokens, config.MaxReviewInputTokens)
	}
}

func TestReviewInputBudgetCountsDenseUTF8Payload(t *testing.T) {
	root := t.TempDir()
	body := strings.Repeat("!@#$%^&*(){}[]:;?<>_+日本語🥾🧪\n", 500)
	writeFile(t, filepath.Join(root, "source.txt"), body)
	snapshot := &reviewSourceSnapshot{root: root, revision: "frozen", baseRevision: "base"}
	current := &batch{lens: "general", pathSeen: make(map[string]struct{})}
	current.add("source.txt", nil, 0)
	config := Config{MaxSourceSnapshotBytes: defaultMaxSourceSnapshotBytes, MaxReviewInputTokens: defaultMaxReviewInputTokens}
	bundle, inventory, err := snapshot.bundleDataWithScopes("out/batches/unit-0000", current.paths, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	rawBytes := len(bundle) + len(inventory)
	if rawBytes < config.MaxReviewInputTokens || rawBytes/2 > config.MaxReviewInputTokens {
		t.Fatalf("fixture does not test byte-fallback risk: %d bytes", rawBytes)
	}
	partitions, err := boundReviewBatches(root, filepath.Join(root, "out"), []*batch{current}, snapshot, config, "", nil)
	if err != nil || len(partitions) < 2 {
		t.Fatalf("dense input was not split: %d partitions, %v", len(partitions), err)
	}
	rebuilt := make([]byte, len(body))
	for _, item := range partitions {
		if len(item.sourceBundle)+len(item.evidenceInventory)+item.diff.Len()+reviewInputFramingTokens > config.MaxReviewInputTokens {
			t.Fatal("partition exceeded the budget after serializing its artifacts")
		}
		var evidence evidenceInventory
		if err := json.Unmarshal(item.evidenceInventory, &evidence); err != nil {
			t.Fatal(err)
		}
		for _, span := range evidence.Sources[0].AvailableSpans {
			copy(rebuilt[span.Start:span.End], item.sourceBundle[span.BundleStart:span.BundleEnd])
		}
	}
	if string(rebuilt) != body {
		t.Fatal("partitioning changed or lost source bytes")
	}
}

func TestSplitDiffFragmentPreservesDeletedLineNumbers(t *testing.T) {
	header := "diff --git a/removed.go b/removed.go\n--- a/removed.go\n+++ /dev/null\n"
	body := "-" + strings.Repeat("a", 90) + "\n-" + strings.Repeat("b", 90) + "\n-" + strings.Repeat("c", 90) + "\n-" + strings.Repeat("d", 90) + "\n"
	b := &batch{paths: []string{"removed.go"}, pathSeen: map[string]struct{}{"removed.go": {}}}
	b.diff.WriteString(header + "@@ -20,4 +0,0 @@\n" + body)
	left, right := splitDiffFragment(b)
	if left == nil || right == nil || !strings.HasPrefix(left.diff.String(), header+"@@ -20,3 +0,0 @@\n") ||
		!strings.HasPrefix(right.diff.String(), header+"@@ -23,1 +0,0 @@\n-") {
		t.Fatalf("fragment headers did not preserve original deleted coordinates: left=%v right=%v", left, right)
	}
}
