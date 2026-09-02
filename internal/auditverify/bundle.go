package auditverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// BundleSchemaVersion is the schema version for AuditBundleManifest.
const BundleSchemaVersion = 1

// BundleType identifies bundle.json's document kind.
const BundleType = "hufu-audit-bundle"

// Bundle file availability values (spec.md §24).
const (
	AvailabilityIncluded        = "included"
	AvailabilityHashOnly        = "hash_only"
	AvailabilityOmittedByPolicy = "omitted_by_policy"
	AvailabilityUnavailable     = "unavailable"
)

// Artifact export modes (spec.md §23.3).
const (
	ArtifactModeReferenced   = "referenced"
	ArtifactModeMetadataOnly = "metadata-only"
)

// Bundle member roles, used by AuditBundleFile.Role.
const (
	RoleDecisionWitness  = "decision_witness"
	RoleRunResult        = "run_result"
	RoleEvidenceManifest = "evidence_manifest"
	RoleEvents           = "events"
	RoleReceipt          = "receipt"
	RoleArtifactMeta     = "artifact_meta"
	RoleArtifactData     = "artifact_data"
	RoleReadme           = "readme"
)

// AuditBundleFile is one member of an exported bundle (spec.md §9.3, §24).
type AuditBundleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`

	Role string `json:"role"`

	Availability string `json:"availability"`
}

// AuditBundleManifest is bundle.json (spec.md §9.2).
type AuditBundleManifest struct {
	SchemaVersion int    `json:"schema_version"`
	BundleType    string `json:"bundle_type"`

	RunID string `json:"run_id"`

	CreatedFromWorkspace string `json:"created_from_workspace,omitempty"`
	SourceHufuVersion    string `json:"source_hufu_version,omitempty"`

	RunFinishedEventID   string `json:"run_finished_event_id"`
	RunFinishedEventHash string `json:"run_finished_event_hash"`

	EvidenceManifestHash string `json:"evidence_manifest_hash,omitempty"`
	DecisionWitnessHash  string `json:"decision_witness_hash,omitempty"`

	Files []AuditBundleFile `json:"files"`

	BundleHash string `json:"bundle_hash"`
}

// HashInput returns the normalized projection of m that BundleHash is
// computed over (spec.md §10): BundleHash cleared, Files sorted by Path, and
// CreatedFromWorkspace cleared since an absolute workspace path is
// informational only and must not affect a hash meant to be reproducible on
// another machine.
func (m AuditBundleManifest) HashInput() AuditBundleManifest {
	normalized := m
	normalized.BundleHash = ""
	normalized.CreatedFromWorkspace = ""
	normalized.Files = append([]AuditBundleFile(nil), m.Files...)
	sort.Slice(normalized.Files, func(i, j int) bool { return normalized.Files[i].Path < normalized.Files[j].Path })
	return normalized
}

// Seal computes BundleHash = SHA256(json.Marshal(m.HashInput())).
func (m *AuditBundleManifest) Seal() error {
	if m == nil {
		return fmt.Errorf("nil bundle manifest")
	}
	m.SchemaVersion = BundleSchemaVersion
	m.BundleType = BundleType
	data, err := json.Marshal(m.HashInput())
	if err != nil {
		return fmt.Errorf("marshal bundle manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	m.BundleHash = hex.EncodeToString(sum[:])
	return nil
}

// VerifyHash recomputes BundleHash and reports whether it still matches.
func (m AuditBundleManifest) VerifyHash() error {
	if m.BundleHash == "" {
		return fmt.Errorf("bundle manifest is unsealed")
	}
	data, err := json.Marshal(m.HashInput())
	if err != nil {
		return fmt.Errorf("marshal bundle manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != m.BundleHash {
		return fmt.Errorf("bundle hash mismatch")
	}
	return nil
}

// FileByPath returns the declared file entry for path, if any.
func (m AuditBundleManifest) FileByPath(path string) (AuditBundleFile, bool) {
	for _, f := range m.Files {
		if f.Path == path {
			return f, true
		}
	}
	return AuditBundleFile{}, false
}

func hashBytes(data []byte) (string, int64) {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data))
}
