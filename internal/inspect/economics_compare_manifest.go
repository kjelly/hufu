package inspect

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxEconomicsComparisonManifestBytes = 1 << 20
	maxEconomicsComparisonRuns          = 100
	maxEconomicsComparisonGroups        = 10
)

type EconomicsComparisonManifest struct {
	SchemaVersion int                     `json:"schema_version"`
	WorkloadID    string                  `json:"workload_id"`
	Runs          []EconomicsRunSelection `json:"runs"`
}

type EconomicsRunSelection struct {
	Group     string `json:"group"`
	Workspace string `json:"workspace"`
	BranchID  string `json:"branch_id"`
	RunID     string `json:"run_id"`
}

type comparisonRunIdentity struct{ workspace, branch, run string }

// Shape, size and group checks finish before any workspace is inspected.
// Binding subsequently verifies canonical branch identities.
func loadEconomicsComparisonManifest(ctx context.Context, path string) (EconomicsComparisonManifest, error) {
	var manifest EconomicsComparisonManifest
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return manifest, fmt.Errorf("%w: manifest path: %v", ErrInvalidQuery, err)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return manifest, fmt.Errorf("%w: open manifest: %v", ErrInvalidQuery, err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxEconomicsComparisonManifestBytes+1))
	if err != nil {
		return manifest, fmt.Errorf("%w: read manifest: %v", ErrInvalidQuery, err)
	}
	if len(raw) > maxEconomicsComparisonManifestBytes {
		return manifest, fmt.Errorf("%w: manifest exceeds 1 MiB", ErrInvalidQuery)
	}
	if err := json.Unmarshal(raw, &manifest, json.RejectUnknownMembers(true)); err != nil {
		return manifest, fmt.Errorf("%w: manifest JSON: %v", ErrInvalidQuery, err)
	}
	if err := validateEconomicsComparisonManifest(manifest); err != nil {
		return manifest, err
	}
	identities := make(map[comparisonRunIdentity]bool, len(manifest.Runs))
	for i := range manifest.Runs {
		if err := ctx.Err(); err != nil {
			return manifest, err
		}
		selection := &manifest.Runs[i]
		workspace := selection.Workspace
		if !filepath.IsAbs(workspace) {
			workspace = filepath.Join(filepath.Dir(absolute), workspace)
		}
		canonical, err := filepath.EvalSymlinks(workspace)
		if err != nil {
			return manifest, fmt.Errorf("%w: selection %d workspace: %v", ErrNotFound, i+1, err)
		}
		canonical, err = filepath.Abs(canonical)
		if err != nil {
			return manifest, fmt.Errorf("%w: selection %d workspace: %v", ErrInvalidQuery, i+1, err)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return manifest, fmt.Errorf("%w: selection %d workspace: %v", ErrNotFound, i+1, err)
		}
		if !info.IsDir() {
			return manifest, fmt.Errorf("%w: selection %d workspace is not a directory", ErrNotFound, i+1)
		}
		selection.Workspace = filepath.Clean(canonical)
		key := comparisonRunIdentity{selection.Workspace, selection.BranchID, selection.RunID}
		if identities[key] {
			return manifest, fmt.Errorf("%w: selection %d duplicates a canonical workspace/branch/run", ErrInvalidQuery, i+1)
		}
		identities[key] = true
	}
	return manifest, ctx.Err()
}

func validateEconomicsComparisonManifest(manifest EconomicsComparisonManifest) error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("%w: manifest schema_version must be 1", ErrInvalidQuery)
	}
	if manifest.WorkloadID == "" || strings.TrimSpace(manifest.WorkloadID) != manifest.WorkloadID {
		return fmt.Errorf("%w: workload_id must be nonempty without surrounding whitespace", ErrInvalidQuery)
	}
	if len(manifest.Runs) < 2 || len(manifest.Runs) > maxEconomicsComparisonRuns {
		return fmt.Errorf("%w: manifest requires 2 to 100 runs", ErrInvalidQuery)
	}
	groups := make(map[string]bool)
	for i, selection := range manifest.Runs {
		for _, field := range []struct{ name, value string }{{"group", selection.Group}, {"branch_id", selection.BranchID}, {"run_id", selection.RunID}} {
			if field.value == "" || strings.TrimSpace(field.value) != field.value {
				return fmt.Errorf("%w: selection %d %s must be nonempty without surrounding whitespace", ErrInvalidQuery, i+1, field.name)
			}
		}
		if strings.TrimSpace(selection.Workspace) == "" {
			return fmt.Errorf("%w: selection %d workspace is required", ErrInvalidQuery, i+1)
		}
		groups[selection.Group] = true
	}
	if len(groups) < 2 || len(groups) > maxEconomicsComparisonGroups {
		return fmt.Errorf("%w: manifest requires 2 to 10 groups", ErrInvalidQuery)
	}
	return nil
}
