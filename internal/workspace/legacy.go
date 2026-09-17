package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

type LegacyWorkspaceError struct {
	TeamName string
	Sources  []string
}

func (e *LegacyWorkspaceError) Error() string {
	return fmt.Sprintf("legacy workspace exists for team %q at %s; migrate it first with: hufu workspace migrate --team %s", e.TeamName, strings.Join(e.Sources, ", "), e.TeamName)
}

// FindLegacyTeamSources performs the resolver's read-only legacy lookup. It
// returns canonical, sorted, deduplicated team directories and never creates
// the state root, registry, or workspace paths.
func FindLegacyTeamSources(startDir, subjectRoot, teamName string) ([]string, error) {
	team, err := NormalizeTeamName(teamName)
	if err != nil {
		return nil, err
	}
	start, err := canonicalPath(startDir)
	if err != nil {
		return nil, err
	}
	candidates := []string{filepath.Join(start, "workspace"), filepath.Join(subjectRoot, "workspace")}
	var sources []string
	for _, candidate := range candidates {
		root, rootErr := CanonicalExistingDirectory(candidate)
		if rootErr != nil {
			if !errors.Is(rootErr, os.ErrNotExist) {
				return nil, rootErr
			}
			continue
		}
		sourcePath := filepath.Join(root, team)
		info, statErr := os.Lstat(sourcePath)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect legacy workspace %q: %w", sourcePath, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("legacy_source_invalid: %q must be a directory and not a symlink", sourcePath)
		}
		canonical, canonicalErr := canonicalPath(sourcePath)
		if canonicalErr != nil {
			return nil, canonicalErr
		}
		if !slices.Contains(sources, canonical) {
			sources = append(sources, canonical)
		}
	}
	sort.Strings(sources)
	return sources, nil
}

func rejectLegacyWorkspace(startDir, subjectRoot, teamName string) error {
	sources, err := FindLegacyTeamSources(startDir, subjectRoot, teamName)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return nil
	}
	return &LegacyWorkspaceError{TeamName: teamName, Sources: sources}
}
