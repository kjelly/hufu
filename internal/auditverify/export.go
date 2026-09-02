package auditverify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/kjelly/hufu/internal/team"
)

// ExportOptions configures hufu audit export.
type ExportOptions struct {
	// ArtifactMode is ArtifactModeReferenced (default) or
	// ArtifactModeMetadataOnly (spec.md §23.3).
	ArtifactMode string
}

// eventStoreRelPath and artifact store paths mirror the on-disk convention
// team.OpenEventStore/team.NewFileArtifactStore use (workspace/logs/...).
// They are re-derived here, rather than imported, because team does not
// export them; auditverify only ever reads this layout, never writes to it.
const (
	eventStoreRelPath  = "logs/event_store.jsonl"
	artifactDataRelDir = "logs/artifacts/data"
	artifactMetaRelDir = "logs/artifacts/meta"
)

// ExportRun builds a portable, self-verifying audit bundle for runID and
// writes it to outputPath (spec.md §22). It follows the crash-safe sequence
// spec.md §54 requires: build the complete archive in a ".tmp" file, verify
// that exact archive by extracting and re-auditing it, and only then rename
// it into place. If self-verification fails, outputPath is never created or
// modified.
func ExportRun(ctx context.Context, workspace, runID, outputPath string, opts ExportOptions) error {
	if opts.ArtifactMode == "" {
		opts.ArtifactMode = ArtifactModeReferenced
	}
	if opts.ArtifactMode != ArtifactModeReferenced && opts.ArtifactMode != ArtifactModeMetadataOnly {
		return fmt.Errorf("unknown artifact mode %q", opts.ArtifactMode)
	}

	verification, projection, err := runWorkspaceAudit(ctx, workspace, runID, VerifyOptions{})
	if err != nil {
		return fmt.Errorf("verify run before export: %w", err)
	}
	if projection == nil || projection.runResult == nil {
		return fmt.Errorf("run %q has no canonical terminal state to export: %s", runID, verification.Integrity.Reason)
	}

	witness, err := buildRunWitness(runID, verification, projection)
	if err != nil {
		return fmt.Errorf("build decision witness: %w", err)
	}

	entries, files, err := collectBundleEntries(workspace, projection, witness, opts)
	if err != nil {
		return fmt.Errorf("collect bundle files: %w", err)
	}

	manifest := AuditBundleManifest{
		RunID:                runID,
		CreatedFromWorkspace: workspace,
		RunFinishedEventID:   projection.terminalEvent.ID,
		RunFinishedEventHash: projection.terminalEvent.Hash,
		DecisionWitnessHash:  witness.WitnessHash,
		Files:                files,
	}
	if projection.runResult.EvidenceManifest != nil {
		manifest.EvidenceManifestHash = projection.runResult.EvidenceManifest.ManifestHash
	}
	if err := manifest.Seal(); err != nil {
		return fmt.Errorf("seal bundle manifest: %w", err)
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bundle manifest: %w", err)
	}
	entries = append(entries, bundleFileEntry{path: "bundle.json", data: manifestData})
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })

	return writeBundleAtomically(ctx, outputPath, entries, manifest, verification, opts)
}

// collectBundleEntries gathers every bundle member except bundle.json itself
// (whose hash depends on the rest) and returns both the raw entries to
// archive and their AuditBundleFile records.
func collectBundleEntries(workspace string, projection *runProjection, witness *DecisionWitness, opts ExportOptions) ([]bundleFileEntry, []AuditBundleFile, error) {
	var entries []bundleFileEntry
	var files []AuditBundleFile
	add := func(path, role string, data []byte, availability string) {
		entries = append(entries, bundleFileEntry{path: path, data: data})
		sha, size := hashBytes(data)
		files = append(files, AuditBundleFile{Path: path, SHA256: sha, Bytes: size, Role: role, Availability: availability})
	}

	witnessData, err := json.MarshalIndent(witness, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal decision witness: %w", err)
	}
	add("decision-witness.json", RoleDecisionWitness, witnessData, AvailabilityIncluded)

	runResultData, err := json.MarshalIndent(projection.runResult, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal run result: %w", err)
	}
	add("run-result.json", RoleRunResult, runResultData, AvailabilityIncluded)

	eventsData, err := os.ReadFile(filepath.Join(workspace, eventStoreRelPath))
	if err != nil {
		return nil, nil, fmt.Errorf("read canonical event log: %w", err)
	}
	add("events.jsonl", RoleEvents, eventsData, AvailabilityIncluded)

	add("README.txt", RoleReadme, []byte(bundleReadme(projection.runResult.RunID)), AvailabilityIncluded)

	manifest := projection.runResult.EvidenceManifest
	if manifest != nil {
		manifestData, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return nil, nil, fmt.Errorf("marshal evidence manifest: %w", err)
		}
		add("evidence-manifest.json", RoleEvidenceManifest, manifestData, AvailabilityIncluded)

		seenArtifacts := make(map[string]bool)
		for _, ref := range manifest.ArtifactRefs {
			if ref.ID == "" || seenArtifacts[ref.ID] {
				continue
			}
			seenArtifacts[ref.ID] = true
			metaPath := filepath.Join(workspace, artifactMetaRelDir, ref.ID+".json")
			metaData, err := os.ReadFile(metaPath)
			if err != nil {
				return nil, nil, fmt.Errorf("read artifact metadata %q: %w", ref.ID, err)
			}
			add(filepath.ToSlash(filepath.Join("artifacts", "meta", ref.ID+".json")), RoleArtifactMeta, metaData, AvailabilityIncluded)

			dataArchivePath := filepath.ToSlash(filepath.Join("artifacts", "data", ref.ID))
			if opts.ArtifactMode == ArtifactModeMetadataOnly {
				// spec.md §23.3: declare the artifact's identity and digest
				// without including its bytes. There is nothing to archive, so
				// this goes straight into files, not entries.
				files = append(files, AuditBundleFile{
					Path: dataArchivePath, SHA256: ref.SHA256, Bytes: ref.Bytes,
					Role: RoleArtifactData, Availability: AvailabilityHashOnly,
				})
				continue
			}
			dataPath := filepath.Join(workspace, artifactDataRelDir, ref.ID)
			data, err := os.ReadFile(dataPath)
			if err != nil {
				return nil, nil, fmt.Errorf("read artifact data %q: %w", ref.ID, err)
			}
			add(dataArchivePath, RoleArtifactData, data, AvailabilityIncluded)
		}
	}

	seenReceipts := make(map[string]bool)
	for _, item := range projection.tasks {
		if item == nil {
			continue
		}
		for i := range item.ExecutionReceipts {
			receipt := item.ExecutionReceipts[i]
			hash, err := HashExecutionReceipt(receipt)
			if err != nil {
				return nil, nil, fmt.Errorf("hash receipt for task %q attempt %d: %w", item.ID, receipt.Attempt, err)
			}
			if seenReceipts[hash] {
				continue
			}
			seenReceipts[hash] = true
			receiptData, err := json.MarshalIndent(NormalizeExecutionReceipt(receipt), "", "  ")
			if err != nil {
				return nil, nil, fmt.Errorf("marshal receipt for task %q attempt %d: %w", item.ID, receipt.Attempt, err)
			}
			add(filepath.ToSlash(filepath.Join("receipts", hash+".json")), RoleReceipt, receiptData, AvailabilityIncluded)
		}
	}

	return entries, files, nil
}

func bundleReadme(runID string) string {
	return fmt.Sprintf(`This is a hufu audit bundle for run %s.

It is self-contained: verify it on any machine, without the original
workspace, with:

    hufu audit verify --bundle <this-file>

bundle.json lists every file in this bundle with its SHA-256 digest and role.
decision-witness.json explains which acceptance criteria and tasks justified
the run's outcome. events.jsonl is the run's complete canonical event log
(hash chain included); evidence-manifest.json and artifacts/ are its sealed
evidence manifest and referenced artifacts, when the run produced one.
`, runID)
}

// writeBundleAtomically implements spec.md §54's crash-safe export sequence:
// write to a randomized ".tmp" sibling of outputPath, self-verify that exact
// archive by extracting and re-auditing it, and only then rename it into
// place. A failure at any step -- including, for a "referenced" mode export,
// the bundle reproducing a different verdict than the live workspace
// verification just computed -- leaves outputPath untouched.
//
// A "metadata-only" export deliberately omits artifact bytes, so its bundle
// verification is expected to diverge on the Evidence dimension (there is no
// artifact-store implementation, canonical or otherwise, that can verify
// digests it was never given); only "referenced" mode's self-check requires
// an exact verdict match.
func writeBundleAtomically(ctx context.Context, outputPath string, entries []bundleFileEntry, manifest AuditBundleManifest, liveVerification *AuditVerificationResult, opts ExportOptions) error {
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return fmt.Errorf("generate temp suffix: %w", err)
	}
	tmpPath := fmt.Sprintf("%s.tmp.%s", outputPath, hex.EncodeToString(randBytes))

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temp bundle: %w", err)
	}
	if err := writeTarArchive(f, entries); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write bundle archive: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temp bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp bundle: %w", err)
	}

	bundleVerification, err := verifyBundleFile(ctx, tmpPath, manifest.RunID, VerifyOptions{})
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exported bundle failed self-verification, not publishing it: %w", err)
	}
	if opts.ArtifactMode == ArtifactModeReferenced &&
		(bundleVerification.Verdict != liveVerification.Verdict || bundleVerification.DerivedOutcome != liveVerification.DerivedOutcome) {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exported bundle reproduces verdict=%s/outcome=%s but live verification was verdict=%s/outcome=%s, not publishing it",
			bundleVerification.Verdict, bundleVerification.DerivedOutcome, liveVerification.Verdict, liveVerification.DerivedOutcome)
	}
	if opts.ArtifactMode == ArtifactModeMetadataOnly && bundleVerification.Integrity.Status == AuditDimensionFail {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("exported bundle failed integrity self-verification: %s", bundleVerification.Integrity.Reason)
	}

	if err := os.Rename(tmpPath, outputPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish bundle: %w", err)
	}
	if err := team.SyncDir(dir); err != nil {
		return fmt.Errorf("sync output directory: %w", err)
	}
	return nil
}
