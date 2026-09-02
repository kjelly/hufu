package auditverify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// VerifyBundle verifies a portable audit bundle without needing the original
// workspace (spec.md §16.3): it extracts the archive into a temporary
// directory, verifies bundle.json's own declared file hashes, reconstructs a
// pseudo-workspace shaped exactly like a live one (logs/event_store.jsonl,
// logs/artifacts/...), and runs the exact same runWorkspaceAudit pipeline
// VerifyWorkspaceRun uses against it -- never a second verification
// algorithm for bundles.
//
// Known limitation: the bundle format does not currently include
// session_tree.json (it is not part of spec.md's bundle layout), so bundle
// verification only reconstructs the default "main" branch lineage. A run
// recorded entirely on a non-main branch will not resolve in bundle mode
// even though workspace-mode verification of the same run succeeds.
func VerifyBundle(ctx context.Context, bundlePath string, opts VerifyOptions) (*AuditVerificationResult, error) {
	return verifyBundleFile(ctx, bundlePath, "", opts)
}

func verifyBundleFile(ctx context.Context, bundlePath, expectedRunID string, opts VerifyOptions) (*AuditVerificationResult, error) {
	tmpDir, err := os.MkdirTemp("", "hufu-audit-bundle-*")
	if err != nil {
		return nil, fmt.Errorf("create extraction dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	extractDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return nil, fmt.Errorf("create extraction dir: %w", err)
	}

	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("open bundle: %w", err)
	}
	extractErr := extractTarArchive(f, extractDir)
	_ = f.Close()
	if extractErr != nil {
		return nil, fmt.Errorf("extract bundle: %w", extractErr)
	}

	manifestData, err := os.ReadFile(filepath.Join(extractDir, "bundle.json"))
	if err != nil {
		return nil, fmt.Errorf("read bundle.json: %w", err)
	}
	var manifest AuditBundleManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("decode bundle.json: %w", err)
	}
	if manifest.SchemaVersion > BundleSchemaVersion {
		return nil, fmt.Errorf("bundle schema version %d is newer than the supported version %d", manifest.SchemaVersion, BundleSchemaVersion)
	}
	if expectedRunID != "" && manifest.RunID != expectedRunID {
		return nil, fmt.Errorf("bundle run_id %q does not match expected %q", manifest.RunID, expectedRunID)
	}
	if err := manifest.VerifyHash(); err != nil {
		return failResultf(manifest.RunID, CodeBundleHashMismatch, "bundle.json hash verification failed: %v", err), nil
	}

	for _, file := range manifest.Files {
		if file.Availability != AvailabilityIncluded {
			continue
		}
		path, err := safeJoin(extractDir, file.Path)
		if err != nil {
			return failResultf(manifest.RunID, CodeBundlePathUnsafe, "bundle file %q: %v", file.Path, err), nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return failResultf(manifest.RunID, CodeBundleFileMissing, "bundle file %q is missing: %v", file.Path, err), nil
		}
		sha, size := hashBytes(data)
		if sha != file.SHA256 || size != file.Bytes {
			return failResultf(manifest.RunID, CodeBundleHashMismatch, "bundle file %q hash/size does not match bundle.json", file.Path), nil
		}
	}

	pseudoWorkspace := filepath.Join(tmpDir, "workspace")
	if err := reconstructPseudoWorkspace(extractDir, pseudoWorkspace, manifest); err != nil {
		return failResultf(manifest.RunID, CodeBundleFileMissing, "reconstruct workspace from bundle: %v", err), nil
	}

	result, _, auditErr := runWorkspaceAudit(ctx, pseudoWorkspace, manifest.RunID, opts)
	if auditErr != nil {
		return nil, fmt.Errorf("verify reconstructed bundle: %w", auditErr)
	}

	verifyWitnessLinkage(extractDir, manifest, result)
	return result, nil
}

// verifyWitnessLinkage cross-checks decision-witness.json's own hash and its
// binding to this exact bundle's run_finished/evidence hashes (spec.md §37).
// A witness that fails either check downgrades Integrity to FAIL in place; a
// bundle with no witness file is left as runWorkspaceAudit evaluated it
// (older export, or a run with no terminal event at all).
func verifyWitnessLinkage(extractDir string, manifest AuditBundleManifest, result *AuditVerificationResult) {
	data, err := os.ReadFile(filepath.Join(extractDir, "decision-witness.json"))
	if err != nil {
		return
	}
	var witness DecisionWitness
	if err := json.Unmarshal(data, &witness); err != nil {
		return
	}
	witnessErr := witness.Verify()
	reason := ""
	switch {
	case witnessErr != nil:
		reason = fmt.Sprintf("decision witness does not self-verify: %v", witnessErr)
	case witness.EventHeadHash != manifest.RunFinishedEventHash:
		reason = "decision witness event_head_hash does not match this bundle's run_finished_event_hash"
	case witness.EvidenceManifestHash != manifest.EvidenceManifestHash:
		reason = "decision witness evidence_manifest_hash does not match this bundle's evidence_manifest_hash"
	}
	if reason == "" {
		return
	}
	result.Integrity = AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
	result.addFinding(CodeBundleHashMismatch, FindingSeverityCritical, reason, "", 0, "")
	result.finalizeVerdict()
}

// reconstructPseudoWorkspace copies the bundle's canonical files into the
// exact on-disk shape team.OpenEventStore/team.NewFileArtifactStore expect,
// so runWorkspaceAudit can operate on it exactly as it would a live
// workspace.
func reconstructPseudoWorkspace(extractDir, pseudoWorkspace string, manifest AuditBundleManifest) error {
	if err := copyExtractedFile(extractDir, "events.jsonl", filepath.Join(pseudoWorkspace, eventStoreRelPath)); err != nil {
		return fmt.Errorf("reconstruct event log: %w", err)
	}
	for _, file := range manifest.Files {
		if file.Availability != AvailabilityIncluded {
			continue
		}
		switch file.Role {
		case RoleArtifactData:
			dest := filepath.Join(pseudoWorkspace, artifactDataRelDir, filepath.Base(file.Path))
			if err := copyExtractedFile(extractDir, file.Path, dest); err != nil {
				return fmt.Errorf("reconstruct artifact data %q: %w", file.Path, err)
			}
		case RoleArtifactMeta:
			dest := filepath.Join(pseudoWorkspace, artifactMetaRelDir, filepath.Base(file.Path))
			if err := copyExtractedFile(extractDir, file.Path, dest); err != nil {
				return fmt.Errorf("reconstruct artifact metadata %q: %w", file.Path, err)
			}
		}
	}
	return nil
}

func copyExtractedFile(extractDir, relPath, destPath string) error {
	safePath, err := safeJoin(extractDir, relPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(safePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(destPath, data, 0o600)
}

func failResultf(runID, code, format string, args ...any) *AuditVerificationResult {
	reason := fmt.Sprintf(format, args...)
	result := &AuditVerificationResult{SchemaVersion: AuditSchemaVersion, RunID: runID,
		Integrity: AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}}
	result.addFinding(code, FindingSeverityCritical, reason, "", 0, "")
	result.finalizeVerdict()
	return result
}
