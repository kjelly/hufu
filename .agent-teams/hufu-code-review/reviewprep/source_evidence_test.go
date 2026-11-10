package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/tools"
)

func assertRequiredArtifactInputs(t *testing.T, root string, entry item) {
	t.Helper()
	if len(entry.RequiredReads) != len(entry.Inputs) {
		t.Fatalf("%s required reads do not account for every assigned artifact", entry.Key)
	}
	for index, input := range entry.Inputs {
		data, err := os.ReadFile(filepath.Join(root, input.Path))
		if err != nil {
			t.Fatal(err)
		}
		read := entry.RequiredReads[index]
		if read.ArtifactID != input.ID || read.SHA256 != sha256Hex(data) || read.SHA256 != input.SHA256 || read.ByteSize != int64(len(data)) || read.ByteSize != input.Bytes {
			t.Fatalf("%s required read identity does not match its sealed input: %#v / %#v", entry.Key, read, input)
		}
		if len(data) == 0 {
			if read.Ranges == nil || len(read.Ranges) != 0 {
				t.Fatal("empty evidence must be declared with an explicit empty range list")
			}
		} else if len(read.Ranges) != 1 || read.Ranges[0].Start != 0 || read.Ranges[0].End != int64(len(data)) {
			t.Fatalf("%s required read does not cover complete artifact: %#v", entry.Key, read)
		}
	}
}

func TestProducerRequiredReadsIncludeReferencesAndNoop(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "docs/tutorials/evidence.md", "# Evidence\nA small documentation change.\n", "documentation", "2025-01-02T00:00:00Z")
	config := sourceBudgetFixtureConfig(repo, "out")
	config.Routing = routingDocumentation
	if _, err := Prepare(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	noop := false
	for _, route := range []string{"primary", "documentation", "documentation-escalation"} {
		m := readManifest(t, filepath.Join(repo, "out", route, "workset-manifest.json"))
		for _, entry := range m.Items {
			assertRequiredArtifactInputs(t, repo, entry)
			if len(entry.Inputs) != 4 || entry.Inputs[2].Kind != "review_evidence_inventory" || entry.Inputs[3].Kind != "documentation_verification" {
				t.Fatalf("missing bound inventory/reference report: %#v", entry.Inputs)
			}
			if entry.Lens == "noop" {
				noop = true
				if entry.RequiredReads[0].ByteSize == 0 || len(entry.RequiredReads[0].Ranges) != 1 {
					t.Fatal("noop instructions are not required evidence")
				}
			}
		}
	}
	if !noop {
		t.Fatal("fixture did not exercise the no-op input contract")
	}
}

func TestLargePrimarySourcePartitionsPreserveCompleteCoverageAndDependencies(t *testing.T) {
	root, output := t.TempDir(), t.TempDir()
	var primary strings.Builder
	primary.WriteString("package sample\n\n")
	for index := range 4000 {
		fmt.Fprintf(&primary, "// declaration %d %s\nfunc Target%d() int { return %d }\n\n", index, strings.Repeat("完整上下文", 40), index, index)
	}
	data := []byte(primary.String())
	if len(data) <= 2*1024*1024 {
		t.Fatal("fixture must exceed the former unsplittable primary ceiling")
	}
	writeFile(t, filepath.Join(root, "primary.go"), string(data))
	writeFile(t, filepath.Join(root, "caller.go"), "package sample\nfunc Caller() int { return Target0() }\n")
	writeFile(t, filepath.Join(root, "primary_test.go"), "package sample\nfunc TestTarget() { _ = Target0() }\n")
	snapshot := &reviewSourceSnapshot{root: root, revision: "frozen", baseRevision: "base"}
	current := &batch{lens: "general", pathSeen: make(map[string]struct{})}
	current.add("primary.go", []byte("diff --git a/primary.go b/primary.go\n@@ -1 +1 @@\n-old\n+new\n"), 4)
	config := Config{MaxSourceSnapshotBytes: 100_000, MaxReviewInputTokens: 60_000, MaxWorksetItems: 128}
	batches, err := boundReviewBatches(output, filepath.Join(output, "out"), []*batch{current}, snapshot, config, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) < 2 || len(batches) > config.MaxWorksetItems {
		t.Fatalf("not genuinely bounded: %d partitions", len(batches))
	}
	items, err := writeItems(output, filepath.Join(output, "out"), batches, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var spans []tools.ArtifactByteRange
	for _, entry := range items {
		assertReviewInputBudget(t, output, entry, config)
		assertRequiredArtifactInputs(t, output, entry)
		bundle, err := os.ReadFile(filepath.Join(output, entry.Inputs[1].Path))
		if err != nil {
			t.Fatal(err)
		}
		inventoryData, err := os.ReadFile(filepath.Join(output, entry.Inputs[2].Path))
		if err != nil {
			t.Fatal(err)
		}
		var inventory evidenceInventory
		if err := json.Unmarshal(inventoryData, &inventory); err != nil {
			t.Fatal(err)
		}
		dependencies := make(map[string]bool)
		for _, source := range inventory.Sources {
			dependencies[source.Path] = true
			if source.Role != "primary" {
				continue
			}
			if source.SHA256 != sha256Hex(data) || source.ByteSize != int64(len(data)) || source.Truncated {
				t.Fatal("primary partition lost the original source identity or truncated its required scope")
			}
			for _, span := range source.AvailableSpans {
				if !bytes.Equal(bundle[span.BundleStart:span.BundleEnd], data[span.Start:span.End]) || !utf8.Valid(bundle[span.BundleStart:span.BundleEnd]) {
					t.Fatal("source/bundle coordinate mapping is not exact UTF-8 evidence")
				}
				if span.StartLine != bytes.Count(data[:span.Start], []byte("\n"))+1 {
					t.Fatal("source line coordinates were rebased")
				}
				spans = append(spans, tools.ArtifactByteRange{Start: span.Start, End: span.End})
			}
		}
		if !dependencies["caller.go"] || !dependencies["primary_test.go"] || !bytes.Contains(bundle, []byte("return Target0()")) || !bytes.Contains(bundle, []byte("func TestTarget")) {
			t.Fatal("source partition dropped caller or focused-test context")
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	covered := int64(0)
	overlap := false
	for _, span := range spans {
		if span.Start > covered {
			t.Fatalf("primary coverage hole [%d,%d)", covered, span.Start)
		}
		if span.Start < covered {
			overlap = true
		}
		if span.End > covered {
			covered = span.End
		}
	}
	if covered != int64(len(data)) || !overlap {
		t.Fatalf("partition union/context incomplete: covered=%d bytes=%d overlap=%v", covered, len(data), overlap)
	}
	config.MaxWorksetItems = 1
	if _, err := boundReviewBatches(output, filepath.Join(output, "rejected"), []*batch{current}, snapshot, config, "", nil); err == nil || !strings.Contains(err.Error(), "scope_too_large") {
		t.Fatalf("aggregate cap was not enforced: %v", err)
	}
}

func TestEvidenceInventoryUTF8TruncationEmptyAndUnavailable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "unicode.txt"), "世界\n")
	writeFile(t, filepath.Join(root, "empty.txt"), "")
	snapshot := &reviewSourceSnapshot{root: root, revision: "frozen", identity: &repositorySnapshotManifest{Files: []snapshotFile{{Path: "excluded.txt", SHA256: strings.Repeat("a", 64), Bytes: 20, Disposition: "excluded"}}}}
	var bundle strings.Builder
	row, used, err := snapshot.writeEvidenceSection(&bundle, "unicode.txt", "related", nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if used != 3 || !utf8.ValidString(bundle.String()) || !row.Truncated || row.OmittedBytes != 4 || len(row.AvailableSpans) != 1 || row.AvailableSpans[0].End != 3 || row.SHA256 != sha256Hex([]byte("世界\n")) {
		t.Fatalf("inexact UTF-8 truncation inventory: %#v, used=%d", row, used)
	}
	row, _, err = snapshot.writeEvidenceSection(&bundle, "empty.txt", "primary", nil, -1)
	if err != nil || row.ByteSize != 0 || row.SHA256 != sha256Hex(nil) || row.Truncated || row.Omitted || row.RequiredScopes == nil || row.AvailableSpans == nil {
		t.Fatalf("empty source contract: %#v, %v", row, err)
	}
	row, _, err = snapshot.writeEvidenceSection(&bundle, "excluded.txt", "primary", nil, -1)
	if err != nil || !row.Omitted || row.ByteSize != 20 || row.SHA256 != strings.Repeat("a", 64) || strings.Contains(bundle.String(), "absent_at_review_revision") {
		t.Fatalf("snapshot omission was misrepresented as absence: %#v %v", row, err)
	}
}

func TestRelatedPathCapRetainsOmittedDependencyInventory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "target.go"), "package sample\nfunc Target() {}\n")
	for index := range maxSnapshotRelatedPaths + 2 {
		writeFile(t, filepath.Join(root, fmt.Sprintf("caller%02d.go", index)), fmt.Sprintf("package sample\nfunc Caller%d() { Target() }\n", index))
	}
	snapshot := &reviewSourceSnapshot{root: root, revision: "frozen"}
	_, data, err := snapshot.bundleDataWithScopes("unit", []string{"target.go"}, nil, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	var inventory evidenceInventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Sources) != maxSnapshotRelatedPaths+3 {
		t.Fatal("related path cap silently discarded dependency identities")
	}
	for _, row := range inventory.Sources[maxSnapshotRelatedPaths+1:] {
		if !row.Omitted || !row.Truncated || row.ByteSize == 0 || row.SHA256 == "" || len(row.RequiredScopes) != 1 || len(row.AvailableSpans) != 0 {
			t.Fatalf("omitted dependency is not recoverable by frozen source identity: %#v", row)
		}
	}
}

func TestFrozenEvidencePersistsOriginalBytesAfterCheckoutChanges(t *testing.T) {
	root, output := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "source.txt"), "original frozen bytes\n")
	writeFile(t, filepath.Join(root, "docs/reference.md"), "Frozen dependency documentation.\n")
	snapshot := &reviewSourceSnapshot{root: root, revision: "working tree", baseRevision: "base"}
	frozen, err := snapshot.persistFrozenSource(output, filepath.Join(output, "out"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "source.txt"), "later live bytes\n")
	data, err := os.ReadFile(filepath.Join(output, snapshot.frozenRoot, "source.txt"))
	if err != nil || string(data) != "original frozen bytes\n" {
		t.Fatalf("frozen source changed: %q %v", data, err)
	}
	manifestData, err := os.ReadFile(filepath.Join(output, frozen.Path))
	if err != nil {
		t.Fatal(err)
	}
	var manifest frozenSourceManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	files := make(map[string]snapshotFile)
	for _, file := range manifest.Files {
		files[file.Path] = file
	}
	original, documentation := files["source.txt"], files["docs/reference.md"]
	if sha256Hex(manifestData) != frozen.SHA256 || original.SHA256 != sha256Hex(data) || original.Bytes != int64(len(data)) || manifest.ContentSHA256 == "" {
		t.Fatalf("frozen identity does not seal original bytes: %#v", manifest)
	}
	if documentation.Disposition != "available" || documentation.SHA256 != sha256Hex([]byte("Frozen dependency documentation.\n")) {
		t.Fatal("frozen dependency bytes were incorrectly hidden by primary-audit eligibility filters")
	}
}

func TestSupplementItemsDoNotRepublishFrozenTree(t *testing.T) {
	root, output := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "source.txt"), "same frozen source\n")
	snapshot := &reviewSourceSnapshot{root: root, revision: "frozen", skipFrozenSource: true}
	current := &batch{lens: "general", pathSeen: make(map[string]struct{})}
	current.add("source.txt", nil, 0)
	items, err := writeItems(output, filepath.Join(output, "supplement"), []*batch{current}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	assertRequiredArtifactInputs(t, output, items[0])
	if _, err := os.Stat(filepath.Join(output, "supplement", "frozen-source")); !os.IsNotExist(err) {
		t.Fatalf("bounded supplement republished the full frozen tree: %v", err)
	}
	if _, ok := items[0].Bindings["frozen_source_manifest"]; ok {
		t.Fatal("one-round supplement advertised another frozen-source grant")
	}
}

func TestRepositorySnapshotPartitionsLargePrimaryInsteadOfOmittingIt(t *testing.T) {
	repo := newFixtureRepo(t)
	var source strings.Builder
	source.WriteString("package sample\n")
	for index := range 600 {
		fmt.Fprintf(&source, "// declaration %d %s\nfunc Target%d() {}\n", index, strings.Repeat("context ", 30), index)
	}
	writeAndCommit(t, repo, "large.go", source.String(), "large source", "2025-01-02T00:00:00Z")
	config := sourceBudgetFixtureConfig(repo, "snapshot-out")
	config.Routing = routingDocumentation
	config.Scope = resolverScope{Kind: "repository_snapshot", History: "first_parent", Head: "HEAD"}
	config.MaxCommits = 0
	config.MaxSourceSnapshotBytes = 64 * 1024
	result, err := Prepare(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	scope := result.Outputs["scope"].(scopeAttestation)
	if scope.Resolved.Head == "" {
		t.Fatal("snapshot revision was not frozen")
	}
	m := readManifest(t, filepath.Join(repo, "snapshot-out", "primary", "workset-manifest.json"))
	partitions := 0
	for _, entry := range m.Items {
		for _, name := range entry.TouchedPaths {
			if name == "large.go" {
				partitions++
			}
		}
		assertReviewInputBudget(t, repo, entry, config)
	}
	if partitions < 2 {
		t.Fatal("large snapshot primary was omitted instead of partitioned")
	}
}
