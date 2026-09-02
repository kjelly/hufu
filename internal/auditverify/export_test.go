package auditverify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC-8: export, then verify --bundle in a separate empty directory, with no
// access to the original workspace.
func TestExportThenVerifyBundleRoundTrip(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	bundlePath := filepath.Join(t.TempDir(), "run-audit.tar")

	if err := ExportRun(context.Background(), fx.workspace, fx.runID, bundlePath, ExportOptions{}); err != nil {
		t.Fatalf("ExportRun: %v", err)
	}
	if info, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("stat exported bundle: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle perm = %v, want 0600", info.Mode().Perm())
	}

	// A brand new, empty directory: nothing here references fx.workspace.
	emptyDir := t.TempDir()
	movedBundle := filepath.Join(emptyDir, "moved.tar")
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(movedBundle, data, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := VerifyBundle(context.Background(), movedBundle, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if result.Verdict != AuditVerdictPass {
		t.Fatalf("bundle verdict = %#v, want pass", result)
	}
}

func TestExportRunUnknownRunIsError(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	bundlePath := filepath.Join(t.TempDir(), "run-audit.tar")
	if err := ExportRun(context.Background(), fx.workspace, "run-does-not-exist", bundlePath, ExportOptions{}); err == nil {
		t.Fatal("expected an error exporting an unknown run")
	}
	if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
		t.Fatal("a failed export must not leave a bundle file behind")
	}
}

func TestExportRunMetadataOnlyOmitsArtifactBytes(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	bundlePath := filepath.Join(t.TempDir(), "run-audit.tar")
	if err := ExportRun(context.Background(), fx.workspace, fx.runID, bundlePath, ExportOptions{ArtifactMode: ArtifactModeMetadataOnly}); err != nil {
		t.Fatalf("ExportRun metadata-only: %v", err)
	}

	extractDir := t.TempDir()
	f, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := extractTarArchive(f, extractDir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "artifacts", "data", fx.artifactID)); !os.IsNotExist(err) {
		t.Fatal("metadata-only export must not include artifact data bytes")
	}
	if _, err := os.Stat(filepath.Join(extractDir, "artifacts", "meta", fx.artifactID+".json")); err != nil {
		t.Fatalf("metadata-only export must still include artifact metadata: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(extractDir, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest AuditBundleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	// bundle.json must still declare the artifact's identity and digest
	// (spec.md §23.3), just with availability "hash_only" and no archived
	// bytes to back it.
	found := false
	for _, file := range manifest.Files {
		if file.Role == RoleArtifactData {
			found = true
			if file.Availability != AvailabilityHashOnly {
				t.Fatalf("artifact data file availability = %q, want hash_only", file.Availability)
			}
			if file.SHA256 == "" {
				t.Fatal("hash_only artifact data file must still declare its digest")
			}
		}
	}
	if !found {
		t.Fatal("bundle.json does not declare the artifact data file at all in metadata-only mode")
	}
}

// Tamper Case A: modify run-result.json after export -> bundle file hash
// mismatch is caught.
func TestVerifyBundleRejectsTamperedRunResultFile(t *testing.T) {
	bundlePath := exportFixtureBundle(t)
	tamperTarEntry(t, bundlePath, "run-result.json", func(data []byte) []byte {
		return append(data, []byte(`// tampered`)...)
	})
	result, err := VerifyBundle(context.Background(), bundlePath, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if result.Verdict != AuditVerdictFail {
		t.Fatalf("tampered run-result.json verdict = %#v, want fail", result)
	}
}

// Tamper Case B: modify events.jsonl (recomputing only the bundle file hash,
// as a real attacker would, but not the event hash chain) -> event chain
// verification fails even though the bundle's own file-hash bookkeeping is
// internally consistent.
func TestVerifyBundleRejectsTamperedEventsWithRecomputedBundleHash(t *testing.T) {
	bundlePath := exportFixtureBundle(t)
	tamperTarEntryAndRehash(t, bundlePath, "events.jsonl", func(data []byte) []byte {
		// A trailing blank line is not a meaningful tamper (JSONL scanners
		// skip blank lines); corrupt actual event content instead.
		return []byte(strings.Replace(string(data), `"run_started"`, `"tampered_type"`, 1))
	})
	result, err := VerifyBundle(context.Background(), bundlePath, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if result.Verdict != AuditVerdictFail {
		t.Fatalf("tampered events.jsonl verdict = %#v, want fail", result)
	}
}

// Tamper Case C: modify an artifact's bytes and recompute only the bundle's
// own file hash for it -> evidence artifact digest verification fails.
func TestVerifyBundleRejectsTamperedArtifactWithRecomputedBundleHash(t *testing.T) {
	fx := buildCompletedRunFixture(t)
	bundlePath := filepath.Join(t.TempDir(), "run-audit.tar")
	if err := ExportRun(context.Background(), fx.workspace, fx.runID, bundlePath, ExportOptions{}); err != nil {
		t.Fatalf("ExportRun: %v", err)
	}
	tamperTarEntryAndRehash(t, bundlePath, filepath.Join("artifacts", "data", fx.artifactID), func([]byte) []byte {
		return []byte("tampered artifact bytes")
	})
	result, err := VerifyBundle(context.Background(), bundlePath, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if result.Verdict != AuditVerdictFail {
		t.Fatalf("tampered artifact verdict = %#v, want fail", result)
	}
}

// Tamper Case D: modify decision-witness.json and recompute only the
// bundle's own file hash for it -> witness linkage verification fails.
func TestVerifyBundleRejectsTamperedWitnessWithRecomputedBundleHash(t *testing.T) {
	bundlePath := exportFixtureBundle(t)
	tamperTarEntryAndRehash(t, bundlePath, "decision-witness.json", func(data []byte) []byte {
		var witness DecisionWitness
		if err := json.Unmarshal(data, &witness); err != nil {
			t.Fatalf("decode witness: %v", err)
		}
		witness.Outcome = "tampered"
		out, err := json.MarshalIndent(witness, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return out
	})
	result, err := VerifyBundle(context.Background(), bundlePath, VerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if result.Verdict != AuditVerdictFail {
		t.Fatalf("tampered witness verdict = %#v, want fail", result)
	}
}

// exportFixtureBundle exports buildCompletedRunFixture's run and returns the
// path to the resulting bundle file, in its own fresh temp dir.
func exportFixtureBundle(t *testing.T) string {
	t.Helper()
	fx := buildCompletedRunFixture(t)
	bundlePath := filepath.Join(t.TempDir(), "run-audit.tar")
	if err := ExportRun(context.Background(), fx.workspace, fx.runID, bundlePath, ExportOptions{}); err != nil {
		t.Fatalf("ExportRun: %v", err)
	}
	return bundlePath
}

// tamperTarEntry rewrites one tar entry's bytes in place, without touching
// bundle.json's declared hash for it -- simulating an attacker who edits a
// bundle member but does not (or cannot) also forge a matching bundle.json.
func tamperTarEntry(t *testing.T, bundlePath, entryPath string, mutate func([]byte) []byte) {
	t.Helper()
	rewriteTarArchive(t, bundlePath, func(entries []bundleFileEntry) []bundleFileEntry {
		for i := range entries {
			if entries[i].path == entryPath {
				entries[i].data = mutate(entries[i].data)
			}
		}
		return entries
	})
}

// tamperTarEntryAndRehash rewrites one tar entry's bytes AND updates
// bundle.json's declared hash/size for that entry to match -- simulating an
// attacker capable of recomputing the bundle's own bookkeeping, so the only
// thing that can still catch the tamper is the canonical proof chain itself
// (event hash chain, evidence manifest verify, or witness linkage).
func tamperTarEntryAndRehash(t *testing.T, bundlePath, entryPath string, mutate func([]byte) []byte) {
	t.Helper()
	rewriteTarArchive(t, bundlePath, func(entries []bundleFileEntry) []bundleFileEntry {
		var manifest *AuditBundleManifest
		manifestIdx := -1
		for i := range entries {
			if entries[i].path == "bundle.json" {
				manifestIdx = i
				var m AuditBundleManifest
				if err := json.Unmarshal(entries[i].data, &m); err != nil {
					t.Fatalf("decode bundle.json: %v", err)
				}
				manifest = &m
			}
		}
		if manifest == nil {
			t.Fatal("bundle.json entry not found")
		}
		for i := range entries {
			if entries[i].path == entryPath {
				entries[i].data = mutate(entries[i].data)
				sha, size := hashBytes(entries[i].data)
				for j := range manifest.Files {
					if manifest.Files[j].Path == entryPath {
						manifest.Files[j].SHA256 = sha
						manifest.Files[j].Bytes = size
					}
				}
			}
		}
		if err := manifest.Seal(); err != nil {
			t.Fatalf("reseal tampered manifest: %v", err)
		}
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		entries[manifestIdx].data = data
		return entries
	})
}

// rewriteTarArchive extracts bundlePath into memory, applies mutate to the
// full entry set, and writes the result back over bundlePath.
func rewriteTarArchive(t *testing.T, bundlePath string, mutate func([]bundleFileEntry) []bundleFileEntry) {
	t.Helper()
	extractDir := t.TempDir()
	f, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := extractTarArchive(f, extractDir); err != nil {
		_ = f.Close()
		t.Fatalf("extract for tampering: %v", err)
	}
	_ = f.Close()

	var entries []bundleFileEntry
	_ = filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(extractDir, path)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		entries = append(entries, bundleFileEntry{path: filepath.ToSlash(rel), data: data})
		return nil
	})

	entries = mutate(entries)

	out, err := os.OpenFile(bundlePath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	if err := writeTarArchive(out, entries); err != nil {
		t.Fatalf("rewrite tar: %v", err)
	}
}
