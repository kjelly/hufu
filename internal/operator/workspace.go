package operator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrInvalidWorkspace = errors.New("invalid operator workspace request")

func ResolveWorkspacePath(request WorkspaceRequest) (WorkspaceResolution, error) {
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	teamName := strings.TrimSpace(request.TeamName)
	if err := validateTeamName(teamName); err != nil {
		return WorkspaceResolution{}, err
	}
	projectDir, err := canonicalPath(request.ProjectDir)
	if err != nil {
		return WorkspaceResolution{}, fmt.Errorf("%w: project directory: %v", ErrInvalidWorkspace, err)
	}
	requested := strings.TrimSpace(request.RequestedPath)

	resolution := WorkspaceResolution{
		RequestedPath:      requested,
		RequestedSemantics: mode,
		ProjectDir:         projectDir,
		TeamName:           teamName,
	}
	switch mode {
	case "legacy_base", "root":
		if teamName == "" {
			return WorkspaceResolution{}, fmt.Errorf("%w: %s workspace requires a team", ErrInvalidWorkspace, mode)
		}
		base, resolveErr := resolveBasePath(requested, projectDir)
		if resolveErr != nil {
			return WorkspaceResolution{}, resolveErr
		}
		resolution.WorkspaceRoot = base
		resolution.WorkspaceExact, err = canonicalPath(filepath.Join(base, teamName))
		if err != nil {
			return WorkspaceResolution{}, fmt.Errorf("%w: team workspace: %v", ErrInvalidWorkspace, err)
		}
	case "exact":
		if requested == "" {
			return WorkspaceResolution{}, fmt.Errorf("%w: exact workspace requires a path", ErrInvalidWorkspace)
		}
		exact, resolveErr := canonicalPath(requested)
		if resolveErr != nil {
			return WorkspaceResolution{}, fmt.Errorf("%w: exact workspace: %v", ErrInvalidWorkspace, resolveErr)
		}
		resolution.WorkspaceExact = exact
		resolution.WorkspaceRoot = filepath.Dir(exact)
	case "default":
		if projectDir == "" {
			return WorkspaceResolution{}, fmt.Errorf("%w: default workspace requires a project directory", ErrInvalidWorkspace)
		}
		resolution.WorkspaceRoot, err = canonicalPath(filepath.Join(projectDir, "workspace"))
		if err != nil {
			return WorkspaceResolution{}, fmt.Errorf("%w: default workspace root: %v", ErrInvalidWorkspace, err)
		}
		resolution.WorkspaceExact = resolution.WorkspaceRoot
		if teamName != "" {
			resolution.WorkspaceExact = filepath.Join(resolution.WorkspaceRoot, teamName)
		}
		resolution.WorkspaceExact, err = canonicalPath(resolution.WorkspaceExact)
		if err != nil {
			return WorkspaceResolution{}, fmt.Errorf("%w: default workspace: %v", ErrInvalidWorkspace, err)
		}
	default:
		return WorkspaceResolution{}, fmt.Errorf("%w: unsupported mode %q", ErrInvalidWorkspace, request.Mode)
	}
	return resolution, nil
}

func resolveBasePath(requested, projectDir string) (string, error) {
	if requested != "" {
		base, err := canonicalPath(requested)
		if err != nil {
			return "", fmt.Errorf("%w: workspace root: %v", ErrInvalidWorkspace, err)
		}
		return base, nil
	}
	if projectDir == "" {
		return "", fmt.Errorf("%w: workspace root requires a path or project directory", ErrInvalidWorkspace)
	}
	return filepath.Join(projectDir, "workspace"), nil
}

func canonicalPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		return filepath.Clean(resolved), nil
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return "", resolveErr
	}
	return filepath.Clean(absolute), nil
}

func validateTeamName(name string) error {
	if name == "" {
		return nil
	}
	if filepath.IsAbs(name) || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return fmt.Errorf("%w: invalid team name %q", ErrInvalidWorkspace, name)
	}
	return nil
}
