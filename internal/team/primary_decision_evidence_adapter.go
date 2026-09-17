package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
)

// PrimaryEvidenceCandidatesFromSources resolves and verifies runtime-owned
// source records before producing collector candidates. Facts and typed values
// are read from immutable artifacts rather than accepted as caller prose.
func PrimaryEvidenceCandidatesFromSources(ctx context.Context, store ArtifactStore, records []PrimaryEvidenceSourceRecord) ([]PrimaryEvidenceCandidate, error) {
	if store == nil {
		return nil, fmt.Errorf("primary evidence source adapter requires an artifact store")
	}
	result := make([]PrimaryEvidenceCandidate, 0, len(records))
	for index, record := range records {
		resolved, err := store.Resolve(ctx, record.SourceArtifact)
		if err != nil {
			return nil, fmt.Errorf("resolve primary evidence source %d: %w", index, err)
		}
		if resolved.ByteSize < 0 || uint64(resolved.ByteSize) > primaryEvidenceDefaultSourceBytes {
			return nil, fmt.Errorf("primary evidence source %d exceeds source scan limit", index)
		}
		ref, err := decisionArtifactRefFromArtifact(resolved)
		if err != nil {
			return nil, fmt.Errorf("primary evidence source %d: %w", index, err)
		}

		format, content, sourceKind, err := readPrimaryEvidenceSource(ctx, store, resolved, record.JSONPointer, record.SourceKind)
		if err != nil {
			return nil, fmt.Errorf("read primary evidence source %d: %w", index, err)
		}
		if record.BaseRate {
			if sourceKind != "base_rate" || format != "json" {
				return nil, fmt.Errorf("primary evidence source %d has inconsistent base-rate typing", index)
			}
			if err := validatePrimaryEvidenceBaseRate(content); err != nil {
				return nil, fmt.Errorf("primary evidence source %d: %w", index, err)
			}
		}
		if record.Assumption {
			if sourceKind != "assumption" || format != "json" {
				return nil, fmt.Errorf("primary evidence source %d has inconsistent assumption typing", index)
			}
			value, decodeErr := decodeUniqueJSON([]byte(content))
			if decodeErr != nil {
				return nil, fmt.Errorf("primary evidence source %d: %w", index, decodeErr)
			}
			if _, assumptionErr := primaryEvidenceAssumption(value); assumptionErr != nil {
				return nil, fmt.Errorf("primary evidence source %d: %w", index, assumptionErr)
			}
		}
		roots := []string(nil)
		if record.ProvenanceAuthority != "model_reported" {
			roots = []string{"artifact:" + resolved.SHA256}
			if resolver, ok := store.(TrustedArtifactMetadataResolver); ok {
				metadata, metadataErr := resolver.TrustedArtifactMetadata(ctx, resolved)
				if metadataErr != nil {
					return nil, fmt.Errorf("resolve trusted provenance for source %d: %w", index, metadataErr)
				}
				if len(metadata.ParentSourceIDs) > 0 {
					roots = append([]string(nil), metadata.ParentSourceIDs...)
				}
			}
		}
		result = append(result, PrimaryEvidenceCandidate{
			Source: PrimaryEvidenceSourceV1{
				SourceKind: sourceKind, Task: clonePrimaryEvidenceTaskSource(record.Task), SourceArtifact: ref,
				JSONPointer: record.JSONPointer, SourceEventID: record.SourceEventID,
				ProducerAgentID: cloneStringPointer(record.ProducerAgentID), ModelIdentity: cloneStringPointer(record.ModelIdentity),
				ProviderIdentity: cloneStringPointer(record.ProviderIdentity), DerivedParentItemIDs: slices.Clone(record.DerivedParentItemIDs),
				ProvenanceAuthority: record.ProvenanceAuthority,
			},
			ContentFormat: format, Content: content, Verification: record.Verification,
			MandatoryReasons: slices.Clone(record.MandatoryReasons), EpistemicStatus: record.EpistemicStatus,
			RootIdentities: roots, SourceEventOrder: record.SourceEventOrder, BaseRate: record.BaseRate, Assumption: record.Assumption,
		})
	}
	return result, nil
}

func readPrimaryEvidenceSource(ctx context.Context, store ArtifactStore, ref ArtifactRef, pointer, sourceKind string) (string, string, string, error) {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(ref.MediaType, ";")[0]))
	isJSON := mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
	isText := strings.HasPrefix(mediaType, "text/")
	if !isJSON && !isText {
		if pointer != "" || sourceKind == "base_rate" || sourceKind == "assumption" || sourceKind == "fact" || sourceKind == "assertion" {
			return "", "", "", fmt.Errorf("unsupported media type %q for typed evidence", ref.MediaType)
		}
		metadata, err := CanonicalDecisionJSON(map[string]any{"media_type": ref.MediaType, "sha256": ref.SHA256, "size_bytes": ref.ByteSize})
		return "json", string(metadata), "artifact_metadata", err
	}

	reader, err := store.Open(ctx, ref.ID)
	if err != nil {
		return "", "", "", err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, primaryEvidenceDefaultSourceBytes+1))
	if err != nil {
		return "", "", "", err
	}
	if len(data) > primaryEvidenceDefaultSourceBytes {
		return "", "", "", ErrDecisionEvidenceScanLimit
	}
	if isJSON {
		value, err := decodeUniqueJSON(data)
		if err != nil {
			return "", "", "", err
		}
		value, err = resolveJSONPointer(value, pointer)
		if err != nil {
			return "", "", "", fmt.Errorf("resolve JSON pointer %q: %w", pointer, err)
		}
		canonical, err := canonicalPrimaryEvidenceJSON(value)
		if err != nil {
			return "", "", "", err
		}
		return "json", string(canonical), sourceKind, nil
	}
	if pointer != "" {
		return "", "", "", fmt.Errorf("text evidence cannot use JSON pointer %q", pointer)
	}
	return "text", string(data), sourceKind, nil
}

// AdaptPrimaryDecisionEvidence builds the legacy engine packet without
// changing its V1 material hash algorithm. Every view and nested typed source
// is resolved through the artifact store before entering the packet.
func AdaptPrimaryDecisionEvidence(ctx context.Context, store ArtifactStore, evidence *PrimaryDecisionEvidenceV1, input PrimaryEvidencePacketInput) (DecisionEvidencePacket, error) {
	if err := ValidatePrimaryDecisionEvidence(evidence); err != nil {
		return DecisionEvidencePacket{}, err
	}
	if store == nil {
		return DecisionEvidencePacket{}, fmt.Errorf("primary evidence packet adapter requires an artifact store")
	}
	packet := DecisionEvidencePacket{
		ID: input.ID, Question: input.Question, Options: slices.Clone(input.Options), Criteria: slices.Clone(input.Criteria),
		Facts: make(map[string]any), RequestContractRef: input.RequestContractRef,
		AvailableMetadata: map[string]any{
			"primary_evidence_identity": map[string]any{
				"logical_run_id": evidence.LogicalRunID, "branch_id": evidence.BranchID, "generation": evidence.Generation,
				"requirement_digest": evidence.RequirementDigest, "support_revision_digest": evidence.SupportRevisionDigest,
				"source_index_digest": evidence.SourceIndexDigest,
			},
			"primary_evidence_coverage": evidence.Coverage,
			"primary_evidence_budget":   evidence.Budget,
		},
	}
	baseRateIDs := sliceSet(evidence.BaseRateItemIDs)
	assumptionIDs := sliceSet(evidence.AssumptionItemIDs)
	for _, item := range evidence.Items {
		artifact, err := resolveDecisionArtifactRef(ctx, store, item.ViewRef, "decision-view")
		if err != nil {
			return DecisionEvidencePacket{}, fmt.Errorf("resolve evidence view %s: %w", item.ItemID, err)
		}
		packet.Artifacts = append(packet.Artifacts, artifact)
		value, err := primaryEvidenceItemValue(item)
		if err != nil {
			return DecisionEvidencePacket{}, fmt.Errorf("decode evidence item %s: %w", item.ItemID, err)
		}
		if item.Source.SourceKind == "fact" {
			packet.Facts[item.ItemID+"#"+item.Source.JSONPointer] = value
		}
		if _, ok := baseRateIDs[item.ItemID]; ok {
			rate, rateErr := primaryEvidenceBaseRate(value)
			if rateErr != nil {
				return DecisionEvidencePacket{}, fmt.Errorf("adapt base rate %s: %w", item.ItemID, rateErr)
			}
			resolvedSource, resolveErr := resolveDecisionArtifactRef(ctx, store, rate.Source, "base-rate-source")
			if resolveErr != nil {
				return DecisionEvidencePacket{}, fmt.Errorf("resolve base rate source %s: %w", item.ItemID, resolveErr)
			}
			rate.Value.Source = resolvedSource
			packet.BaseRates = append(packet.BaseRates, rate.Value)
		}
		if _, ok := assumptionIDs[item.ItemID]; ok {
			assumption, assumptionErr := primaryEvidenceAssumption(value)
			if assumptionErr != nil {
				return DecisionEvidencePacket{}, fmt.Errorf("adapt assumption %s: %w", item.ItemID, assumptionErr)
			}
			for index, ref := range assumption.EvidenceRefs {
				resolvedRef, resolveErr := store.Resolve(ctx, ref)
				if resolveErr != nil {
					return DecisionEvidencePacket{}, fmt.Errorf("resolve assumption %s evidence %d: %w", item.ItemID, index, resolveErr)
				}
				assumption.EvidenceRefs[index] = resolvedRef
			}
			packet.Assumptions = append(packet.Assumptions, assumption)
		}
		provenance := EvidenceProvenance{SourceID: item.ItemID, SourceType: EvidenceSourceArtifact, IndependenceGroup: item.Independence.GroupID, ContentHash: item.Source.SourceArtifact.SHA256}
		if item.Source.ProvenanceAuthority == "model_reported" {
			provenance.DeclaredParentSourceIDs = slices.Clone(item.Source.DerivedParentItemIDs)
		} else {
			provenance.ParentSourceIDs = slices.Clone(item.Source.DerivedParentItemIDs)
		}
		packet.Provenance = append(packet.Provenance, provenance)
	}
	if len(packet.Facts) == 0 {
		packet.Facts = nil
	}
	if err := packet.Validate(); err != nil {
		return DecisionEvidencePacket{}, err
	}
	return packet, nil
}

type adaptedPrimaryBaseRate struct {
	Value  BaseRateEvidence
	Source DecisionArtifactRef
}

func primaryEvidenceBaseRate(value any) (adaptedPrimaryBaseRate, error) {
	data, err := canonicalPrimaryEvidenceJSON(value)
	if err != nil {
		return adaptedPrimaryBaseRate{}, err
	}
	var wire struct {
		ReferenceClass string              `json:"reference_class"`
		Metric         string              `json:"metric"`
		SampleSize     uint64              `json:"sample_size"`
		Distribution   DistributionSummary `json:"distribution"`
		Source         DecisionArtifactRef `json:"source"`
		Limitations    []string            `json:"limitations"`
	}
	if err := decodeStrictJSON(data, &wire); err != nil {
		return adaptedPrimaryBaseRate{}, err
	}
	if wire.SampleSize > math.MaxInt {
		return adaptedPrimaryBaseRate{}, fmt.Errorf("sample size exceeds int range")
	}
	valueOut := BaseRateEvidence{
		ReferenceClass: wire.ReferenceClass, Metric: wire.Metric, SampleSize: int(wire.SampleSize),
		Distribution: wire.Distribution, Limitations: slices.Clone(wire.Limitations),
		Source: ArtifactRef{ID: wire.Source.ID, SHA256: wire.Source.SHA256, Bytes: int64(wire.Source.SizeBytes), ByteSize: int64(wire.Source.SizeBytes), MediaType: wire.Source.MediaType},
	}
	if err := validateBaseRate(valueOut); err != nil {
		return adaptedPrimaryBaseRate{}, err
	}
	return adaptedPrimaryBaseRate{Value: valueOut, Source: wire.Source}, nil
}

func primaryEvidenceAssumption(value any) (DecisionAssumption, error) {
	data, err := canonicalPrimaryEvidenceJSON(value)
	if err != nil {
		return DecisionAssumption{}, err
	}
	var assumption DecisionAssumption
	if err := decodeStrictJSON(data, &assumption); err != nil {
		return DecisionAssumption{}, err
	}
	if canonicalString(assumption.ID) == "" || canonicalString(assumption.Statement) == "" || !ValidAssumptionStatus(assumption.EffectiveStatus()) {
		return DecisionAssumption{}, fmt.Errorf("invalid typed assumption")
	}
	return assumption, nil
}

func primaryEvidenceItemValue(item PrimaryEvidenceItemV1) (any, error) {
	if item.ContentFormat == "text" {
		return item.Content, nil
	}
	return decodeUniqueJSON([]byte(item.Content))
}

func resolveDecisionArtifactRef(ctx context.Context, store ArtifactStore, ref DecisionArtifactRef, role string) (ArtifactRef, error) {
	resolved, err := store.Resolve(ctx, ArtifactRef{ID: ref.ID, SHA256: ref.SHA256, ByteSize: int64(ref.SizeBytes), Bytes: int64(ref.SizeBytes), MediaType: ref.MediaType})
	if err != nil {
		return ArtifactRef{}, err
	}
	if resolved.SHA256 != ref.SHA256 || resolved.ByteSize < 0 || uint64(resolved.ByteSize) != ref.SizeBytes || resolved.MediaType != ref.MediaType {
		return ArtifactRef{}, fmt.Errorf("resolved artifact identity does not match decision reference")
	}
	resolved.Role = role
	return resolved, nil
}

func decisionArtifactRefFromArtifact(ref ArtifactRef) (DecisionArtifactRef, error) {
	if ref.ByteSize < 0 {
		return DecisionArtifactRef{}, fmt.Errorf("artifact has a negative byte size")
	}
	result := DecisionArtifactRef{ID: ref.ID, SHA256: ref.SHA256, MediaType: ref.MediaType, SizeBytes: uint64(ref.ByteSize)}
	if err := validateDecisionArtifactRef(result); err != nil {
		return DecisionArtifactRef{}, err
	}
	return result, nil
}

func clonePrimaryEvidenceTaskSource(value *PrimaryEvidenceTaskSourceV1) *PrimaryEvidenceTaskSourceV1 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func sliceSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func canonicalPrimaryEvidenceJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode canonical evidence JSON: %w", err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func decodeUniqueJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeUniqueJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func decodeUniqueJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		result := make(map[string]any)
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return nil, keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := result[key]; duplicate {
				return nil, fmt.Errorf("duplicate JSON object key %q", key)
			}
			value, valueErr := decodeUniqueJSONValue(decoder)
			if valueErr != nil {
				return nil, valueErr
			}
			result[key] = value
		}
		if closing, closingErr := decoder.Token(); closingErr != nil || closing != json.Delim('}') {
			return nil, fmt.Errorf("invalid JSON object terminator")
		}
		return result, nil
	case '[':
		result := make([]any, 0)
		for decoder.More() {
			value, valueErr := decodeUniqueJSONValue(decoder)
			if valueErr != nil {
				return nil, valueErr
			}
			result = append(result, value)
		}
		if closing, closingErr := decoder.Token(); closingErr != nil || closing != json.Delim(']') {
			return nil, fmt.Errorf("invalid JSON array terminator")
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func sortDecisionArtifactRefs(values []DecisionArtifactRef) {
	sort.Slice(values, func(i, j int) bool {
		left, right := values[i], values[j]
		return left.SHA256 < right.SHA256 || left.SHA256 == right.SHA256 && left.ID < right.ID
	})
}

func sortedDecisionArtifactRefs(values []DecisionArtifactRef) []DecisionArtifactRef {
	result := slices.Clone(values)
	sortDecisionArtifactRefs(result)
	return slices.Compact(result)
}

func sortedUniquePrimaryEvidenceStrings(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}
