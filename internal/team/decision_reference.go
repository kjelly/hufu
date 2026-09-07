package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// Reference evidence is an intentionally smaller wire protocol than the
// sealed decision packet. The producer may describe a reference class and an
// advisory source, but it cannot manufacture an ArtifactRef or any runtime
// identity.
const (
	ReferenceEvidenceSchemaVersion       = 1
	ReferenceEvidenceProducerMaxRawBytes = 64 * 1024
	ReferenceEvidenceMinEntries          = 1
	ReferenceEvidenceMaxEntries          = 8
	ReferenceEvidenceMaxDraftEntryBytes  = 16 * 1024
	ReferenceEvidenceMaxEntryBytes       = 32 * 1024
	ReferenceEvidenceMaxTotalBytes       = 64 * 1024
	ReferenceEvidenceMaxResultBytes      = 64 * 1024
	ReferenceEvidenceMaxStringBytes      = 4096
	ReferenceEvidenceMaxLimitations      = 16
	ReferenceEvidenceMaxLimitationBytes  = 1024
	ReferenceEvidenceMediaType           = "application/vnd.hufu.reference-evidence+json;v=1"
	ReferenceEvidenceResultMediaType     = "application/vnd.hufu.reference-evidence-result+json;v=1"
)

// ReferenceSourceDeclaration is an advisory producer declaration. Its values
// are retained inside the generated evidence artifact and provenance for
// audit, but none of them are used as a path, digest, media type, or artifact
// identity by the runtime.
type ReferenceSourceDeclaration struct {
	ID                      string   `json:"id,omitempty"`
	SourceID                string   `json:"source_id,omitempty"`
	SourceType              string   `json:"source_type,omitempty"`
	Name                    string   `json:"name,omitempty"`
	Title                   string   `json:"title,omitempty"`
	Publisher               string   `json:"publisher,omitempty"`
	Citation                string   `json:"citation,omitempty"`
	Description             string   `json:"description,omitempty"`
	URL                     string   `json:"url,omitempty"`
	URI                     string   `json:"uri,omitempty"`
	Locator                 string   `json:"locator,omitempty"`
	DeclaredParentSourceIDs []string `json:"declared_parent_source_ids,omitempty"`
}

// ReferenceBaseRateDraft is the producer-owned portion of one outside-view
// entry. Source is advisory metadata only; the runtime supplies the actual
// BaseRateEvidence.Source after publishing this entry to CAS.
type ReferenceBaseRateDraft struct {
	ReferenceClass string                     `json:"reference_class"`
	Metric         string                     `json:"metric"`
	SampleSize     int                        `json:"sample_size"`
	Distribution   DistributionSummary        `json:"distribution"`
	Limitations    []string                   `json:"limitations,omitempty"`
	Source         ReferenceSourceDeclaration `json:"source"`
}

// ReferenceEvidenceDraft is the only value a reference producer may return.
type ReferenceEvidenceDraft struct {
	SchemaVersion int                      `json:"schema_version"`
	Entries       []ReferenceBaseRateDraft `json:"entries"`

	// AgentID names the concrete worker capability routing resolved the
	// reference role to, if any. It is set by the runner after decoding,
	// never by the producer's own response, and is empty on the legacy
	// judge-model sidecar path. Carried into ReferenceEvidenceResult's
	// ProducerAgentID when the draft is published.
	AgentID string `json:"-"`

	// Pinned and BindingReason mirror outside-view.role.pin's effect (spec.md
	// v2 §34), set by the runner alongside AgentID, never by the producer's
	// own response. Carried into ReferenceEvidenceResult when published.
	Pinned        bool   `json:"-"`
	BindingReason string `json:"-"`
}

// ReferenceEvidenceArtifact is the fixed typed payload stored for each
// validated draft entry. It is runtime-created and therefore contains no
// producer-supplied artifact identity.
type ReferenceEvidenceArtifact struct {
	SchemaVersion int                    `json:"schema_version"`
	InvocationID  string                 `json:"invocation_id"`
	DecisionID    string                 `json:"decision_id"`
	RunID         string                 `json:"run_id,omitempty"`
	TaskID        string                 `json:"task_id,omitempty"`
	Entry         ReferenceBaseRateDraft `json:"entry"`
	PublishedAt   time.Time              `json:"published_at"`
}

// ReferenceEvidenceInvocation is the durable admission record for one
// producer call. A started invocation without a terminal event is ambiguous
// after restart and must not be replayed.
type ReferenceEvidenceInvocation struct {
	SchemaVersion    int       `json:"schema_version"`
	InvocationID     string    `json:"invocation_id"`
	InputHash        string    `json:"input_hash"`
	DecisionID       string    `json:"decision_id"`
	RunID            string    `json:"run_id,omitempty"`
	TaskID           string    `json:"task_id,omitempty"`
	Question         string    `json:"question"`
	ContractRef      string    `json:"contract_ref,omitempty"`
	ContractRevision uint64    `json:"contract_revision,omitempty"`
	StartedAt        time.Time `json:"started_at"`
}

// ReferenceEvidenceResult records only runtime-validated, runtime-published
// evidence. The refs are generated by the CAS store, never accepted from the
// producer response.
type ReferenceEvidenceResult struct {
	SchemaVersion     int                  `json:"schema_version"`
	InvocationID      string               `json:"invocation_id"`
	InputHash         string               `json:"input_hash"`
	BaseRates         []BaseRateEvidence   `json:"base_rates"`
	Artifacts         []ArtifactRef        `json:"artifacts"`
	Provenance        []EvidenceProvenance `json:"provenance"`
	CompletedAt       time.Time            `json:"completed_at"`
	ResultArtifactRef *ArtifactRef         `json:"result_artifact_ref,omitempty"`

	// ProducerAgentID names the concrete worker capability routing resolved
	// the reference role to for this invocation, copied from the validated
	// draft's AgentID. Empty on the legacy judge-model sidecar path.
	ProducerAgentID string `json:"producer_agent_id,omitempty"`

	// ProducerPinned and ProducerBindingReason copy the validated draft's
	// Pinned/BindingReason (spec.md v2 §34).
	ProducerPinned        bool   `json:"producer_pinned,omitempty"`
	ProducerBindingReason string `json:"producer_binding_reason,omitempty"`
}

// ReferenceEvidenceFailure is a terminal producer outcome. It is durable so
// Resume cannot turn a failed or uncertain call into an implicit retry.
type ReferenceEvidenceFailure struct {
	SchemaVersion int       `json:"schema_version"`
	InvocationID  string    `json:"invocation_id"`
	InputHash     string    `json:"input_hash"`
	Reason        string    `json:"reason"`
	FailedAt      time.Time `json:"failed_at"`
}

func (r ReferenceEvidenceRequest) canonicalInput() ([]byte, error) {
	copy := r
	copy.InvocationID = ""
	copy.InputHash = ""
	return json.Marshal(copy)
}

func (r ReferenceEvidenceRequest) ComputeInputHash() (string, error) {
	data, err := r.canonicalInput()
	if err != nil {
		return "", fmt.Errorf("encode reference evidence input: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (r ReferenceEvidenceRequest) Validate() error {
	if r.SchemaVersion != ReferenceEvidenceSchemaVersion {
		return fmt.Errorf("unsupported reference evidence request schema version %d", r.SchemaVersion)
	}
	if strings.TrimSpace(r.InvocationID) == "" || strings.TrimSpace(r.DecisionID) == "" {
		return fmt.Errorf("reference evidence request requires invocation and decision IDs")
	}
	if strings.TrimSpace(r.Question) == "" {
		return fmt.Errorf("reference evidence request requires a question")
	}
	hash, err := r.ComputeInputHash()
	if err != nil {
		return err
	}
	if r.InputHash != hash {
		return fmt.Errorf("reference evidence request input hash mismatch")
	}
	return nil
}

func (d ReferenceEvidenceDraft) Validate() error {
	if d.SchemaVersion != ReferenceEvidenceSchemaVersion {
		return fmt.Errorf("unsupported reference evidence schema version %d", d.SchemaVersion)
	}
	if len(d.Entries) < ReferenceEvidenceMinEntries || len(d.Entries) > ReferenceEvidenceMaxEntries {
		return fmt.Errorf("reference evidence requires %d-%d entries, got %d", ReferenceEvidenceMinEntries, ReferenceEvidenceMaxEntries, len(d.Entries))
	}
	total := 0
	for i, entry := range d.Entries {
		if err := entry.Validate(); err != nil {
			return fmt.Errorf("entries[%d]: %w", i, err)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("entries[%d]: encode: %w", i, err)
		}
		if len(encoded) > ReferenceEvidenceMaxDraftEntryBytes {
			return fmt.Errorf("entries[%d] exceeds %d bytes", i, ReferenceEvidenceMaxDraftEntryBytes)
		}
		total += len(encoded)
	}
	if total > ReferenceEvidenceMaxTotalBytes {
		return fmt.Errorf("reference evidence entries exceed %d bytes", ReferenceEvidenceMaxTotalBytes)
	}
	return nil
}

func (d ReferenceBaseRateDraft) Validate() error {
	if err := validateReferenceString("reference_class", d.ReferenceClass); err != nil {
		return err
	}
	if err := validateReferenceString("metric", d.Metric); err != nil {
		return err
	}
	if d.SampleSize < 1 {
		return fmt.Errorf("sample_size %d is not a real sample", d.SampleSize)
	}
	for name, value := range map[string]float64{
		"mean": d.Distribution.Mean, "median": d.Distribution.Median,
		"p10": d.Distribution.P10, "p90": d.Distribution.P90,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("distribution.%s is not finite", name)
		}
	}
	if err := d.Source.Validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if len(d.Limitations) > ReferenceEvidenceMaxLimitations {
		return fmt.Errorf("limitations exceed %d entries", ReferenceEvidenceMaxLimitations)
	}
	for i, limitation := range d.Limitations {
		if err := validateReferenceStringLimit(fmt.Sprintf("limitations[%d]", i), limitation, ReferenceEvidenceMaxLimitationBytes); err != nil {
			return err
		}
	}
	return nil
}

func (s ReferenceSourceDeclaration) Validate() error {
	if strings.TrimSpace(s.ID) == "" && strings.TrimSpace(s.SourceID) == "" && strings.TrimSpace(s.Name) == "" && strings.TrimSpace(s.Title) == "" && strings.TrimSpace(s.Publisher) == "" && strings.TrimSpace(s.Citation) == "" && strings.TrimSpace(s.Description) == "" && strings.TrimSpace(s.URL) == "" && strings.TrimSpace(s.URI) == "" && strings.TrimSpace(s.Locator) == "" {
		return fmt.Errorf("source declaration requires a non-empty identity")
	}
	for name, value := range map[string]string{
		"id": s.ID, "source_id": s.SourceID, "source_type": s.SourceType, "title": s.Title, "publisher": s.Publisher, "citation": s.Citation, "description": s.Description,
		"url": s.URL, "uri": s.URI, "locator": s.Locator,
	} {
		if value != "" {
			if err := validateReferenceString(name, value); err != nil {
				return err
			}
		}
	}
	if len(s.DeclaredParentSourceIDs) > ReferenceEvidenceMaxLimitations {
		return fmt.Errorf("declared_parent_source_ids exceed %d entries", ReferenceEvidenceMaxLimitations)
	}
	for i, parent := range s.DeclaredParentSourceIDs {
		if err := validateReferenceString(fmt.Sprintf("declared_parent_source_ids[%d]", i), parent); err != nil {
			return err
		}
	}
	return nil
}

func validateReferenceString(name, value string) error {
	return validateReferenceStringLimit(name, value, ReferenceEvidenceMaxStringBytes)
}

func validateReferenceStringLimit(name, value string, limit int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return nil
}

// referenceEvidenceEntryBytes is the canonical payload used for the typed CAS
// artifact. Struct JSON has deterministic field order and contains no maps.
func referenceEvidenceEntryBytes(artifact ReferenceEvidenceArtifact) ([]byte, error) {
	data, err := json.Marshal(artifact)
	if err != nil {
		return nil, fmt.Errorf("encode reference evidence entry: %w", err)
	}
	if len(data) > ReferenceEvidenceMaxEntryBytes {
		return nil, fmt.Errorf("reference evidence entry exceeds %d bytes", ReferenceEvidenceMaxEntryBytes)
	}
	return data, nil
}

func referenceEvidenceResultBytes(result ReferenceEvidenceResult) ([]byte, error) {
	// The result reference is generated by the CAS write itself and cannot be
	// part of the content-addressed envelope without creating a circular value.
	result.ResultArtifactRef = nil
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode reference evidence result: %w", err)
	}
	if len(data) > ReferenceEvidenceMaxResultBytes {
		return nil, fmt.Errorf("reference evidence result exceeds %d bytes", ReferenceEvidenceMaxResultBytes)
	}
	return data, nil
}

func decodeReferenceJSON(data []byte, target any, limit int, label string) error {
	if len(data) > limit {
		return fmt.Errorf("%s exceeds %d bytes", label, limit)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode %s: trailing JSON value", label)
		}
		return fmt.Errorf("decode %s: trailing data: %w", label, err)
	}
	return nil
}

func decodeReferenceReader(reader io.Reader, target any, limit int, label string) error {
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", label, err)
	}
	return decodeReferenceJSON(data, target, limit, label)
}

func decodeReferenceEvidenceEntry(reader io.Reader) (ReferenceEvidenceArtifact, error) {
	var artifact ReferenceEvidenceArtifact
	if err := decodeReferenceReader(reader, &artifact, ReferenceEvidenceMaxEntryBytes, "reference evidence entry"); err != nil {
		return ReferenceEvidenceArtifact{}, err
	}
	return artifact, nil
}

func decodeReferenceEvidenceResult(reader io.Reader) (ReferenceEvidenceResult, error) {
	var result ReferenceEvidenceResult
	if err := decodeReferenceReader(reader, &result, ReferenceEvidenceMaxResultBytes, "reference evidence result"); err != nil {
		return ReferenceEvidenceResult{}, err
	}
	return result, nil
}

func referenceDeclaredSourceID(source ReferenceSourceDeclaration) (string, error) {
	data, err := json.Marshal(source)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "declared-reference-" + hex.EncodeToString(sum[:]), nil
}
