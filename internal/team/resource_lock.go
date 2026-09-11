package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// LockedResource is the durable, metadata-only record of one resolved
// required resource (runtime invariant 9: event/report record metadata,
// never content). SnapshotRef points at the full-content snapshot in the
// existing ArtifactStore (internal/team/evidence_store.go); resolving and
// writing that snapshot is PR-2 scope, so ResolveRequiredResource leaves it
// zero-valued for now. InjectInto is copied from the originating
// RequiredResourceSpec so bind/inject can act on a LockedResource alone,
// without joining back to the original declared-spec list.
type LockedResource struct {
	Name          string
	Kind          agent.RequiredResourceKind
	CanonicalPath string
	SHA256        string
	ByteSize      int64
	LoadedAt      time.Time
	SnapshotRef   ArtifactRef
	InjectInto    []string
}

// LoadedResource exists only in the memory of a single run: it is never
// persisted and never placed in an event payload. It carries the exact
// content read during resolution so bind/inject can use it directly without
// re-reading the file (runtime invariant 11 — re-reading at inject time
// would reopen the TOCTOU window this whole mechanism exists to close).
type LoadedResource struct {
	LockedResource
	Content string
}

// LockedResourceSet is the persisted form of every resource locked for one
// run (see resource_lock_digest.go for Digest's computation).
type LockedResourceSet struct {
	SchemaVersion int
	Resources     []LockedResource
	Digest        string
}

// ResolveRequiredResource resolves, canonicalizes, reads, and hashes one
// declared resource relative to rootDir, following spec.md's
// resolve→canonicalize→read→hash→authorize lifecycle (authorize/lock/
// persist/bind-inject are the caller's responsibility in later PRs). It
// returns (nil, nil) only when the resource is missing and spec.Required is
// false; every other outcome is either a *LoadedResource or a non-nil
// error — resolution is fail-closed by default.
func ResolveRequiredResource(rootDir string, spec agent.RequiredResourceSpec) (*LoadedResource, error) {
	if strings.TrimSpace(spec.Path) == "" {
		return nil, fmt.Errorf("resource %q: path must not be empty", spec.Name)
	}

	canonicalRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolving root %q: %w", rootDir, err)
	}

	joined := filepath.Join(canonicalRoot, spec.Path)
	canonicalPath, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if os.IsNotExist(err) {
			if !spec.Required {
				return nil, nil
			}
			return nil, fmt.Errorf("required resource %q missing: %s", spec.Name, spec.Path)
		}
		return nil, fmt.Errorf("resolving resource %q: %w", spec.Name, err)
	}

	rel, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("resource %q: %s escapes root %s via symlink or path traversal", spec.Name, spec.Path, rootDir)
	}

	content, err := os.ReadFile(canonicalPath)
	if err != nil {
		return nil, fmt.Errorf("reading resource %q: %w", spec.Name, err)
	}

	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	if declared := strings.TrimSpace(spec.SHA256); declared != "" && !strings.EqualFold(declared, digest) {
		return nil, fmt.Errorf("resource %q: sha256 mismatch: declared %s, actual %s", spec.Name, declared, digest)
	}

	locked := LockedResource{
		Name:          spec.Name,
		Kind:          spec.Kind,
		CanonicalPath: canonicalPath,
		SHA256:        digest,
		ByteSize:      int64(len(content)),
		LoadedAt:      time.Now().UTC(),
		InjectInto:    append([]string(nil), spec.InjectInto...),
	}
	return &LoadedResource{LockedResource: locked, Content: string(content)}, nil
}

// ResolveRequiredResources resolves every declared resource under rootDir
// and returns the durable LockedResourceSet alongside the in-memory
// LoadedResource values bind/inject needs for Content. Admission is
// all-or-nothing: it stops at the first failure rather than collecting
// partial results.
func ResolveRequiredResources(rootDir string, specs []agent.RequiredResourceSpec) (*LockedResourceSet, []*LoadedResource, error) {
	loaded := make([]*LoadedResource, 0, len(specs))
	locked := make([]LockedResource, 0, len(specs))
	for _, spec := range specs {
		lr, err := ResolveRequiredResource(rootDir, spec)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving required resource %q: %w", spec.Name, err)
		}
		if lr == nil {
			continue // optional and missing
		}
		loaded = append(loaded, lr)
		locked = append(locked, lr.LockedResource)
	}
	set := &LockedResourceSet{
		SchemaVersion: 1,
		Resources:     locked,
		Digest:        ComputeLockedResourceSetDigest(locked),
	}
	return set, loaded, nil
}
