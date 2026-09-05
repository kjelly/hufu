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

	ordered := append([]EvidenceProvenance(nil), sources...)
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
		parents := append([]string(nil), source.ParentSourceIDs...)
		sort.Strings(parents)
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
	out := make([]EvidenceProvenance, 0, len(artifacts))
	for _, artifact := range artifacts {
		id := artifact.ID
		if id == "" {
			id = artifact.Path
		}
		if id == "" {
			id = artifact.SHA256
		}
		if id == "" {
			continue
		}
		out = append(out, EvidenceProvenance{
			SourceID:    id,
			SourceType:  EvidenceSourceArtifact,
			ContentHash: artifact.SHA256,
		})
	}
	return out
}
