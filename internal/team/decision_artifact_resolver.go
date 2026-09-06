package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Artifact resolution for decision outcome verification.
//
// Outcome evidence is only evidence if the artifact it cites exists; this
// adapts the workspace artifact store to the resolver the outcome path
// checks against. Split out of decision_index.go unchanged.
type fileArtifactResolver struct {
	byDigest map[string]ArtifactRef
}

// NewArtifactResolver builds a resolver from a set of known artifacts.
func NewArtifactResolver(artifacts []ArtifactRef) ArtifactResolver {
	byDigest := make(map[string]ArtifactRef, len(artifacts))
	for _, artifact := range artifacts {
		if digest := strings.TrimSpace(artifact.SHA256); digest != "" {
			byDigest[digest] = artifact
		}
	}
	return fileArtifactResolver{byDigest: byDigest}
}

// ResolveDigest implements ArtifactResolver.
func (r fileArtifactResolver) ResolveDigest(sha256 string) (ArtifactRef, bool) {
	artifact, ok := r.byDigest[strings.TrimSpace(sha256)]
	return artifact, ok
}

// WorkspaceArtifacts lists every artifact the workspace's content-addressed
// store holds, so outcome evidence can be checked against real bytes.
func WorkspaceArtifacts(workspace string) ([]ArtifactRef, error) {
	root := filepath.Join(workspace, logsDir, "artifacts", "meta")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("decision index: reading artifact metadata in %s: %w", root, err)
	}

	var artifacts []ArtifactRef
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		var ref ArtifactRef
		if err := json.Unmarshal(data, &ref); err != nil {
			continue
		}
		if strings.TrimSpace(ref.SHA256) != "" {
			artifacts = append(artifacts, ref)
		}
	}
	sort.Slice(artifacts, func(a, b int) bool { return artifacts[a].SHA256 < artifacts[b].SHA256 })
	return artifacts, nil
}

// CheckAssumption records an operator's assumption check, the third status
// source (spec §18.1). It appends a superseding row; the DecisionRecord the row
// points at is never edited.
//
// An operator may only report supported or contradicted, exactly like the other
// two sources: `unknown` is the absence of a check and `stale` is the runtime's
// own conclusion.
