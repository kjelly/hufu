//go:build linux || darwin
// +build linux darwin

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func resolvePathWithWorkDir(path, workDir string) (string, error) {
	path = expandUserPath(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	baseDir := workDir
	if baseDir == "" {
		var err error
		baseDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get working directory: %w", err)
		}
	}
	return filepath.Clean(filepath.Join(baseDir, path)), nil
}

// expandUserPath resolves the common path spellings agents use in tool
// arguments. Tool arguments are not shell commands, so $HOME and ~ must be
// expanded explicitly rather than relying on a shell to do it.
func expandUserPath(path string) string {
	path = os.ExpandEnv(path)
	if strings.HasPrefix(path, "~") && (path == "~" || strings.HasPrefix(path, "~/")) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

func resolveAndValidatePath(path, workDir string) (string, error) {
	absPath, err := resolvePathWithWorkDir(path, workDir)
	if err != nil {
		return "", err
	}
	projectDir := workDir
	if projectDir == "" {
		projectDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get working directory: %w", err)
		}
	}
	projectDir = filepath.Clean(projectDir)

	evaluatedProjDir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve project directory: %w", err)
	}
	evaluatedProjDir = filepath.Clean(evaluatedProjDir)

	evaluatedPath, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		if !strings.HasPrefix(evaluatedPath, evaluatedProjDir+string(filepath.Separator)) && evaluatedPath != evaluatedProjDir {
			return "", fmt.Errorf("path '%s' is outside the project directory", path)
		}
		return evaluatedPath, nil
	}

	evaluatedDir, err := filepath.EvalSymlinks(filepath.Dir(absPath))
	if err != nil {
		return "", fmt.Errorf("path '%s' is invalid or cannot be resolved: %w", path, err)
	}

	if !strings.HasPrefix(evaluatedDir, evaluatedProjDir+string(filepath.Separator)) && evaluatedDir != evaluatedProjDir {
		return "", fmt.Errorf("path '%s' is outside the project directory", path)
	}
	return filepath.Join(evaluatedDir, filepath.Base(absPath)), nil
}

func IsPathAllowed(absPath string, allowedPaths []string) bool {
	return isPathAllowed(absPath, allowedPaths)
}

func isPathAllowed(absPath string, allowedPaths []string) bool {
	evalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		evalPath = absPath
	}
	evalPath = filepath.Clean(evalPath)

	if cwd, err := os.Getwd(); err == nil && isPathWithinDir(evalPath, cwd) {
		return true
	}

	checkPath := evalPath

	for _, allowed := range allowedPaths {
		evalAllowed, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			evalAllowed = allowed
		}
		evalAllowed = filepath.Clean(evalAllowed)

		if checkPath == evalAllowed || strings.HasPrefix(checkPath, evalAllowed+string(filepath.Separator)) {
			return true
		}

		parentDir := filepath.Dir(absPath)
		evalParent, err := filepath.EvalSymlinks(parentDir)
		if err == nil {
			evalParent = filepath.Clean(evalParent)
			if evalParent == evalAllowed || strings.HasPrefix(evalParent, evalAllowed+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func isWritePathAllowed(absPath string, allowedWritePaths []string) bool {
	evalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		evalPath = absPath
	}
	evalPath = filepath.Clean(evalPath)

	for _, allowed := range allowedWritePaths {
		evalAllowed, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			evalAllowed = allowed
		}
		evalAllowed = filepath.Clean(evalAllowed)

		if evalPath == evalAllowed || strings.HasPrefix(evalPath, evalAllowed+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func isPathWithinDir(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	evalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		evalPath = path
	}
	evalPath = filepath.Clean(evalPath)

	evalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		evalDir = dir
	}
	evalDir = filepath.Clean(evalDir)

	if evalPath == evalDir {
		return true
	}
	rel, err := filepath.Rel(evalDir, evalPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func checkPathOrConsent(path, workDir, operation string, cfg ToolConfig) (string, error) {
	path = normalizeWorkspacePath(path, cfg.WorkspaceName)
	absPath, err := resolvePathWithWorkDir(path, workDir)
	if err != nil {
		return "", err
	}
	canonicalPath := canonicalPathForAuthorization(absPath)
	if err := enforceArtifactPathPolicy(canonicalPath, cfg); err != nil {
		return "", err
	}

	if isPathAllowed(canonicalPath, cfg.AllowedPaths) {
		return absPath, nil
	}

	if cfg.PathConsent != nil {
		result, suggestion, err := cfg.PathConsent.AskConsent(absPath, operation, cfg.ToolName, path)
		if err != nil {
			return "", pathDenied(cfg, fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", path, err))
		}
		switch result {
		case ConsentOnce, ConsentAlways:
			evalPath, evalErr := filepath.EvalSymlinks(absPath)
			if evalErr == nil {
				evalPath = filepath.Clean(evalPath)
				if evalPath != absPath && !isPathAllowed(evalPath, cfg.AllowedPaths) {
					return "", pathDenied(cfg, fmt.Errorf("path '%s' resolves to '%s' which is outside allowed paths", path, evalPath))
				}
			}
			return absPath, nil
		default:
			return "", pathDenied(cfg, formatPathConsentDenied(path, suggestion))
		}
	}

	return "", pathDenied(cfg, fmt.Errorf("path '%s' is outside allowed paths", path))
}

func resolveAndValidatePathWithConsent(path string, cfg ToolConfig) (string, error) {
	path = normalizeWorkspacePath(path, cfg.WorkspaceName)
	absPath, err := resolvePathWithWorkDir(path, cfg.WorkDir)
	if err != nil {
		return "", err
	}
	canonicalPath := canonicalPathForAuthorization(absPath)
	if err := enforceArtifactPathPolicy(canonicalPath, cfg); err != nil {
		return "", err
	}

	if isPathAllowed(canonicalPath, cfg.AllowedPaths) {
		// resolveAndValidatePath only knows about cfg.WorkDir (the project
		// directory) — calling it here silently discarded the AllowedPaths
		// check that just passed, rejecting any agent-level allowed-paths
		// entry outside the project dir (e.g. a /tmp scratch directory) with
		// a misleading "outside the project directory" from write/edit even
		// though bash/sudo happily use the same path. Validate against the
		// allowlist that actually matched, with the same symlink-aware logic.
		return resolveAndValidatePathForAllowedPath(absPath, cfg.WorkDir, cfg.AllowedPaths)
	}

	if cfg.PathConsent != nil {
		result, suggestion, err := cfg.PathConsent.AskConsent(absPath, "edit", cfg.ToolName, path)
		if err != nil {
			return "", pathDenied(cfg, fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", path, err))
		}
		switch result {
		case ConsentOnce, ConsentAlways:
			projectDir := cfg.WorkDir
			if projectDir == "" {
				projectDir, _ = os.Getwd()
			}
			// The consent applies to the directory that was approved. Add that
			// directory only for this validation pass so edit/write tools retain
			// their symlink-aware validation without rejecting a path the user
			// explicitly approved (including a persisted approval).
			consentedPaths := append([]string{}, cfg.AllowedPaths...)
			consentedPaths = append(consentedPaths, dirOfPath(absPath))
			return resolveAndValidatePathForAllowedPath(absPath, projectDir, consentedPaths)
		default:
			return "", pathDenied(cfg, formatPathConsentDenied(path, suggestion))
		}
	}

	return "", pathDenied(cfg, fmt.Errorf("path '%s' is outside allowed paths", path))
}

// canonicalPathForAuthorization resolves the target itself when it exists and
// otherwise resolves its parent. This gives path policy one canonical value
// for ordinary files, symlinks, and new write targets.
func canonicalPathForAuthorization(path string) string {
	path = filepath.Clean(path)
	if evaluated, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(evaluated)
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(filepath.Clean(parent), filepath.Base(path))
	}
	return path
}

func enforceArtifactPathPolicy(canonicalPath string, cfg ToolConfig) error {
	if isArtifactPathBlocked(canonicalPath, cfg.ArtifactPathPolicy) {
		return fmt.Errorf("path %q is a runtime-managed artifact path; use the authorized opaque artifact_ref instead", canonicalPath)
	}
	return nil
}

// EnforceArtifactPathPolicy applies the shared canonical path check to a
// runtime-owned path operation outside a built-in tool. Callers must invoke it
// before inspecting or materializing the path; opaque artifact_ref resolution
// remains a separate capability.
func EnforceArtifactPathPolicy(path string, policy *ArtifactPathPolicy) error {
	if policy == nil {
		return nil
	}
	return enforceArtifactPathPolicy(canonicalPathForAuthorization(path), ToolConfig{ArtifactPathPolicy: policy})
}

// isArtifactPathBlocked is shared by direct path authorization and recursive
// traversal. Canonicalizing both sides prevents symlink aliases from exposing
// a blocked artifact root or descendant.
func isArtifactPathBlocked(path string, policy *ArtifactPathPolicy) bool {
	if policy == nil {
		return false
	}
	canonicalPath := canonicalPathForAuthorization(path)
	for _, blocked := range policy.BlockedPaths {
		blocked = canonicalPathForAuthorization(blocked)
		if canonicalPath == blocked || strings.HasPrefix(canonicalPath, blocked+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func resolveAndValidateWritePathWithConsent(path string, cfg ToolConfig) (string, error) {
	writeCfg := cfg
	writeCfg.AllowedPaths = cfg.AllowedWritePaths
	if len(writeCfg.AllowedPaths) == 0 {
		writeCfg.AllowedPaths = cfg.AllowedPaths
	}
	resolved, err := resolveAndValidatePathWithConsent(path, writeCfg)
	if err != nil {
		return "", err
	}
	if err := checkExecutionRootWrite(resolved, cfg); err != nil {
		return "", err
	}
	return resolved, nil
}

func formatPathConsentDenied(path, suggestion string) error {
	if suggestion == "" {
		return fmt.Errorf("path '%s' is outside allowed paths — access denied by user", path)
	}
	return fmt.Errorf("path '%s' is outside allowed paths; user suggested '%s', retry the command using that path instead", path, suggestion)
}

func resolveAndValidatePathForAllowedPath(absPath, projectDir string, allowedPaths []string) (string, error) {
	evaluatedAbsPath, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		return validatedPathInAllowedDirs(evaluatedAbsPath, allowedPaths)
	}

	parentDir := filepath.Dir(absPath)
	evaluatedParent, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		return "", fmt.Errorf("path '%s' is invalid or cannot be resolved: %w", absPath, err)
	}

	result, err := validatedPathInAllowedDirs(evaluatedParent, allowedPaths)
	if err != nil {
		return "", err
	}
	return filepath.Join(result, filepath.Base(absPath)), nil
}

func validatedPathInAllowedDirs(evaluatedPath string, allowedPaths []string) (string, error) {
	evaluatedPath = filepath.Clean(evaluatedPath)
	for _, allowed := range allowedPaths {
		evalAllowed, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			evalAllowed = allowed
		}
		evalAllowed = filepath.Clean(evalAllowed)

		if evaluatedPath == evalAllowed || strings.HasPrefix(evaluatedPath, evalAllowed+string(filepath.Separator)) {
			return evaluatedPath, nil
		}
	}
	return "", fmt.Errorf("path is outside allowed directories")
}
