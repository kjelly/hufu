package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type stateRootEnvironment struct {
	getenv      func(string) string
	userHomeDir func() (string, error)
}

func DefaultStateRoot() (string, error) {
	environment := stateRootEnvironment{
		getenv:      os.Getenv,
		userHomeDir: os.UserHomeDir,
	}
	return defaultStateRootPlatform(environment)
}

func stateRootForOS(goos string, environment stateRootEnvironment) (string, error) {
	if override := environment.getenv("HUFU_STATE_HOME"); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("HUFU_STATE_HOME must be an absolute path: %q", override)
		}
		return canonicalPath(override)
	}

	var root string
	switch goos {
	case "linux":
		if xdg := strings.TrimSpace(environment.getenv("XDG_STATE_HOME")); xdg != "" && filepath.IsAbs(xdg) {
			root = filepath.Join(xdg, "hufu")
		} else {
			home, err := environment.userHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home directory: %w", err)
			}
			root = filepath.Join(home, ".local", "state", "hufu")
		}
	case "darwin":
		home, err := environment.userHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		root = filepath.Join(home, "Library", "Application Support", "hufu", "state")
	case "windows":
		localAppData := strings.TrimSpace(environment.getenv("LOCALAPPDATA"))
		if localAppData == "" {
			return "", fmt.Errorf("LOCALAPPDATA is not set")
		}
		if !filepath.IsAbs(localAppData) && filepath.VolumeName(localAppData) == "" {
			return "", fmt.Errorf("LOCALAPPDATA must be an absolute path: %q", localAppData)
		}
		root = filepath.Join(localAppData, "hufu", "state")
	default:
		home, err := environment.userHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		root = filepath.Join(home, ".local", "state", "hufu")
	}
	return canonicalPath(root)
}
