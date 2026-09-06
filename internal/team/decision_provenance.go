package team

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Evidence provenance and independence grouping
// (docs/hufu-decision-aware-runtime-spec.md §28).
//
// V1 is deliberately advisory. The runtime groups only what it can derive
// itself — identical content, a shared registrable domain, and parent links a
// retrieval adapter recorded at fetch time. Model-declared parentage is stored
// but never used for grouping, because the same specification that asks for
// independence also says self-description is not trusted evidence. Blocking on
// a number the runtime cannot honestly compute would be worse than reporting it.

// EvidenceIndependence is the computed view over a decision's sources.
type EvidenceIndependence struct {
	SourceCount            int
	IndependenceGroupCount int
	// Groups maps source ID to its assigned group ID, for reporting.
	Groups map[string]string
	// SharedOriginWarnings names each group that holds more than one source.
	SharedOriginWarnings []string
}

// RuntimeEvidenceMetadata is populated only after an artifact or retrieval has
// been resolved by the runtime. It deliberately has no model-declared parent
// field: declarations remain auditable in EvidenceProvenance but never become
// grouping authority.
type RuntimeEvidenceMetadata struct {
	Artifact ArtifactRef
	Origin   ArtifactOriginMetadata
}

// TrustedProvenanceFromRuntimeMetadata derives the only provenance eligible
// for independence grouping. The artifact must carry the opaque ID and digest
// returned by ArtifactStore.Resolve; caller-provided paths and declarations are
// not a substitute for this metadata.
func TrustedProvenanceFromRuntimeMetadata(metadata []RuntimeEvidenceMetadata) []EvidenceProvenance {
	byID := make(map[string]EvidenceProvenance, len(metadata))
	for _, value := range metadata {
		artifact := value.Artifact
		if strings.TrimSpace(artifact.ID) == "" || strings.TrimSpace(artifact.SHA256) == "" {
			continue
		}
		sourceID := artifact.ID
		sourceType := EvidenceSourceArtifact
		retrievalURL := strings.TrimSpace(value.Origin.RetrievalURL)
		if parsed, err := url.Parse(retrievalURL); err == nil && parsed.Scheme != "" && parsed.Hostname() != "" {
			sourceID = parsed.String()
			sourceType = EvidenceSourceURL
		}
		parents := normalizeProvenanceIDs(value.Origin.ParentSourceIDs)
		candidate := EvidenceProvenance{
			SourceID: sourceID, SourceType: sourceType, ContentHash: artifact.SHA256,
			ParentSourceIDs: parents,
		}
		if existing, ok := byID[sourceID]; ok {
			// The same source can be referenced by several evidence roles. Keep
			// every runtime edge while choosing scalar metadata deterministically.
			existing.ParentSourceIDs = normalizeProvenanceIDs(append(existing.ParentSourceIDs, candidate.ParentSourceIDs...))
			if canonicalProvenanceKey(candidate) < canonicalProvenanceKey(existing) {
				existing.SourceType = candidate.SourceType
				existing.ContentHash = candidate.ContentHash
			}
			byID[sourceID] = existing
			continue
		}
		byID[sourceID] = candidate
	}
	out := make([]EvidenceProvenance, 0, len(byID))
	for _, source := range byID {
		out = append(out, source)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })
	return out
}

// unionFind assigns sources to groups deterministically.
type unionFind struct{ parent map[string]string }

func newUnionFind(ids []string) *unionFind {
	uf := &unionFind{parent: make(map[string]string, len(ids))}
	for _, id := range ids {
		uf.parent[id] = id
	}
	return uf
}

func (u *unionFind) find(id string) string {
	root, ok := u.parent[id]
	if !ok {
		u.parent[id] = id
		return id
	}
	for root != u.parent[root] {
		root = u.parent[root]
	}
	// Path compression keeps repeated lookups cheap without changing results.
	for u.parent[id] != root {
		next := u.parent[id]
		u.parent[id] = root
		id = next
	}
	return root
}

func (u *unionFind) union(a, b string) {
	rootA, rootB := u.find(a), u.find(b)
	if rootA == rootB {
		return
	}
	// Always keep the byte-wise smaller root so grouping is deterministic
	// regardless of the order sources arrive in.
	if rootB < rootA {
		rootA, rootB = rootB, rootA
	}
	u.parent[rootB] = rootA
}

// GroupEvidence computes independence groups from provenance records.
func GroupEvidence(sources []EvidenceProvenance, policy EvidenceIndependencePolicy) EvidenceIndependence {
	result := EvidenceIndependence{Groups: map[string]string{}}
	if len(sources) == 0 {
		return result
	}

	byID := make(map[string]EvidenceProvenance, len(sources))
	for _, source := range sources {
		id := strings.TrimSpace(source.SourceID)
		if id == "" {
			continue
		}
		source.SourceID = id
		// A duplicate source is still one source; stable canonical selection
		// prevents input order from changing grouping or warning output.
		if existing, ok := byID[id]; ok {
			// Normalize deterministically but retain duplicate runtime edges.
			existing.ParentSourceIDs = normalizeProvenanceIDs(append(existing.ParentSourceIDs, source.ParentSourceIDs...))
			if canonicalProvenanceKey(source) < canonicalProvenanceKey(existing) {
				existing.SourceType = source.SourceType
				existing.ContentHash = source.ContentHash
			}
			byID[id] = existing
		} else {
			byID[id] = source
		}
	}
	ordered := make([]EvidenceProvenance, 0, len(byID))
	for _, source := range byID {
		ordered = append(ordered, source)
	}
	if len(ordered) == 0 {
		return result
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SourceID < ordered[j].SourceID })

	ids := make([]string, 0, len(ordered))
	for _, source := range ordered {
		ids = append(ids, source.SourceID)
	}
	uf := newUnionFind(ids)

	// Rule 1: identical content is not two confirmations.
	byContent := map[string]string{}
	// Rule 2: one publisher speaking twice is not two publishers.
	byDomain := map[string]string{}
	for _, source := range ordered {
		if hash := strings.TrimSpace(source.ContentHash); hash != "" {
			if first, ok := byContent[hash]; ok {
				uf.union(first, source.SourceID)
			} else {
				byContent[hash] = source.SourceID
			}
		}
		if source.SourceType == EvidenceSourceURL {
			if domain := registrableDomain(source.SourceID); domain != "" {
				if first, ok := byDomain[domain]; ok {
					uf.union(first, source.SourceID)
				} else {
					byDomain[domain] = source.SourceID
				}
			}
		}
	}

	// Rule 3: parent links the retrieval adapter recorded. DeclaredParentSourceIDs
	// is intentionally not consulted.
	for _, source := range ordered {
		parents := normalizeProvenanceIDs(source.ParentSourceIDs)
		for _, parent := range parents {
			if parent == "" {
				continue
			}
			uf.union(source.SourceID, parent)
		}
	}

	members := map[string][]string{}
	for _, id := range ids {
		root := uf.find(id)
		result.Groups[id] = root
		members[root] = append(members[root], id)
	}

	roots := make([]string, 0, len(members))
	for root := range members {
		roots = append(roots, root)
	}
	sort.Strings(roots)

	result.SourceCount = len(ids)
	result.IndependenceGroupCount = len(roots)
	if policy.WarnSharedOrigin {
		for _, root := range roots {
			if len(members[root]) > 1 {
				sort.Strings(members[root])
				result.SharedOriginWarnings = append(result.SharedOriginWarnings,
					fmt.Sprintf("%d sources share origin %s: %s",
						len(members[root]), root, strings.Join(members[root], ", ")))
			}
		}
	}
	return result
}

func canonicalProvenanceKey(source EvidenceProvenance) string {
	parents := normalizeProvenanceIDs(source.ParentSourceIDs)
	return source.SourceType + "\x00" + source.ContentHash + "\x00" + strings.Join(parents, "\x00")
}

// normalizeProvenanceIDs trims and sorts edge IDs while intentionally retaining
// duplicates. Sorting makes grouping/replay stable; dropping duplicates would
// destroy provenance fidelity.
func normalizeProvenanceIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			normalized = append(normalized, id)
		}
	}
	sort.Strings(normalized)
	return normalized
}

// registrableDomain extracts eTLD+1 from a URL-shaped source ID. An unparsable
// value groups by nothing rather than guessing.
func registrableDomain(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	domain, err := publicsuffix.EffectiveTLDPlusOne(parsed.Hostname())
	if err != nil {
		return parsed.Hostname()
	}
	return domain
}

// ProvenanceFromArtifacts derives provenance for the packet's artifacts. The
// content digest is the identity: the same bytes at two paths are one source.
func ProvenanceFromArtifacts(artifacts []ArtifactRef) []EvidenceProvenance {
	metadata := make([]RuntimeEvidenceMetadata, 0, len(artifacts))
	for _, artifact := range artifacts {
		if strings.TrimSpace(artifact.ID) != "" && strings.TrimSpace(artifact.SHA256) != "" {
			metadata = append(metadata, RuntimeEvidenceMetadata{Artifact: artifact})
		}
	}
	return TrustedProvenanceFromRuntimeMetadata(metadata)
}
