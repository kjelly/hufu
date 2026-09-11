package team

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// ComputeLockedResourceSetDigest produces a deterministic digest over a
// locked-resource set's canonical identity fields (spec.md §7):
// kind, name, canonical_path, sha256, byte_size, inject_targets.
//
// LoadedAt is deliberately excluded — it is a timestamp, not deterministic
// across runs, and including it would make every resume compute a
// different digest for an unchanged lock. SnapshotRef is excluded by
// default too, to stay compatible with sets computed before that field
// existed. Resources are sorted by Name first so declaration order in
// team.yaml never changes the digest.
func ComputeLockedResourceSetDigest(resources []LockedResource) string {
	sorted := append([]LockedResource(nil), resources...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	for _, r := range sorted {
		b.WriteString(string(r.Kind))
		b.WriteByte(',')
		b.WriteString(r.Name)
		b.WriteByte(',')
		b.WriteString(r.CanonicalPath)
		b.WriteByte(',')
		b.WriteString(r.SHA256)
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(r.ByteSize, 10))
		b.WriteByte(',')
		b.WriteString(strings.Join(r.InjectInto, "|"))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
