package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"

	"charm.land/fantasy"
)

// ArtifactByteRange identifies a half-open interval in the original source bytes.
type ArtifactByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// ArtifactReadCoverage binds returned source bytes to an immutable artifact.
// An empty artifact still has an observation, with no non-empty ranges.
type ArtifactReadCoverage struct {
	ArtifactID string              `json:"artifact_id"`
	SHA256     string              `json:"sha256"`
	ByteSize   int64               `json:"byte_size"`
	Ranges     []ArtifactByteRange `json:"ranges"`
}

type viewReadMetadata struct {
	ArtifactReads        []string               `json:"artifact_reads"`
	ArtifactOnly         bool                   `json:"artifact_only,omitzero"`
	ArtifactReadCoverage []ArtifactReadCoverage `json:"artifact_read_coverage,omitempty"`
	ContentSHA256        string                 `json:"content_sha256,omitempty"`
}

func artifactViewResponse(content, ref string, coverage ...ArtifactReadCoverage) fantasy.ToolResponse {
	response := fantasy.NewTextResponse(content)
	setViewArtifactReads(&response, []string{ref}, true, coverage...)
	return response
}

func setViewArtifactReads(response *fantasy.ToolResponse, refs []string, artifactOnly bool, coverage ...ArtifactReadCoverage) {
	if len(refs) == 0 {
		return
	}
	raw, err := json.Marshal(viewReadMetadata{
		ArtifactReads: refs, ArtifactOnly: artifactOnly,
		ArtifactReadCoverage: coverage, ContentSHA256: artifactContentSHA256([]byte(response.Content)),
	})
	if err == nil {
		response.Metadata = string(raw)
	}
}

// ViewArtifactReads reports only artifact-only successful reads whose output was
// included in this response. Callers must first authenticate the built-in view
// tool; metadata from an arbitrary tool is not authorization evidence.
func ViewArtifactReads(response fantasy.ToolResponse) []string {
	if response.IsError || response.Metadata == "" {
		return nil
	}
	var metadata viewReadMetadata
	if json.Unmarshal([]byte(response.Metadata), &metadata, json.RejectUnknownMembers(true)) != nil {
		return nil
	}
	if !metadata.ArtifactOnly {
		return nil
	}
	return metadata.ArtifactReads
}

// ViewArtifactReadCoverage reports source byte intervals actually included in a
// successful view response, not the bytes scanned or loaded internally. Callers
// must authenticate the built-in view with IsBuiltInTool before trusting this
// metadata; an external tool's metadata is never authorization evidence.
// Changing response.Content invalidates all coverage, including empty reads.
func ViewArtifactReadCoverage(response fantasy.ToolResponse) []ArtifactReadCoverage {
	if response.IsError || response.Metadata == "" {
		return nil
	}
	var metadata viewReadMetadata
	if json.Unmarshal([]byte(response.Metadata), &metadata, json.RejectUnknownMembers(true)) != nil ||
		metadata.ContentSHA256 != artifactContentSHA256([]byte(response.Content)) {
		return nil
	}
	for i := range metadata.ArtifactReadCoverage {
		coverage := &metadata.ArtifactReadCoverage[i]
		if coverage.ArtifactID == "" || !slices.Contains(metadata.ArtifactReads, coverage.ArtifactID) ||
			!validArtifactSHA256(coverage.SHA256) || coverage.ByteSize < 0 {
			return nil
		}
		for _, r := range coverage.Ranges {
			if r.Start < 0 || r.End <= r.Start || r.End > coverage.ByteSize {
				return nil
			}
		}
		coverage.Ranges = mergeArtifactByteRanges(coverage.Ranges)
	}
	return metadata.ArtifactReadCoverage
}

func artifactContentSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validArtifactSHA256(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Verified runtime openers can expose their already-checked immutable identity
// without rehashing the whole source for every bounded read. Legacy openers are
// hashed from the same reader; those internally read bytes are not coverage.
func artifactCoverageForReader(reader io.ReadSeeker, ref string, size int64, ranges []ArtifactByteRange) (ArtifactReadCoverage, error) {
	coverage := ArtifactReadCoverage{ArtifactID: ref, ByteSize: size, Ranges: mergeArtifactByteRanges(ranges)}
	if identity, ok := reader.(interface{ ArtifactReadIdentity() (string, int64) }); ok {
		digest, byteSize := identity.ArtifactReadIdentity()
		if !validArtifactSHA256(digest) || byteSize != size {
			return ArtifactReadCoverage{}, fmt.Errorf("artifact identity does not match the source")
		}
		coverage.SHA256 = digest
		return coverage, nil
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return ArtifactReadCoverage{}, fmt.Errorf("failed to read artifact identity")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, reader)
	if err != nil || n != size {
		return ArtifactReadCoverage{}, fmt.Errorf("failed to read artifact identity")
	}
	coverage.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return coverage, nil
}

func mergeArtifactByteRanges(ranges []ArtifactByteRange) []ArtifactByteRange {
	if len(ranges) < 2 {
		return ranges
	}
	slices.SortFunc(ranges, func(a, b ArtifactByteRange) int {
		if a.Start < b.Start {
			return -1
		}
		if a.Start > b.Start {
			return 1
		}
		return 0
	})
	merged := ranges[:1]
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End {
			last.End = max(last.End, r.End)
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}
