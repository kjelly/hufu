package teampkg

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// InstallOptions selects a local package and one of Hufu's canonical team
// roots. Project-local installation is the default.
type InstallOptions struct {
	Package string
	Global  bool
	DryRun  bool
}

type InstallResult struct {
	Manifest Manifest
	Lock     Lock
	Target   string
	DryRun   bool
}

type installOperations struct {
	syncTree func(string) error
	publish  func(string, string) error
	syncDir  func(string) error
}

func defaultInstallOperations() installOperations {
	return installOperations{
		syncTree: syncStagedTree,
		publish:  publishDirectoryNoReplace,
		syncDir:  syncDirectory,
	}
}

// Install validates, stages, compiles, and atomically publishes one package.
// It never replaces an existing team directory.
func Install(options InstallOptions) (InstallResult, error) {
	return installWithOperations(options, defaultInstallOperations())
}

func installWithOperations(options InstallOptions, operations installOperations) (result InstallResult, resultErr error) {
	if strings.TrimSpace(options.Package) == "" {
		return result, errors.New("team package path is required")
	}
	archive, err := ReadArchive(options.Package)
	if err != nil {
		return result, err
	}
	if err := ValidatePackageName(archive.Manifest.Name); err != nil {
		return result, err
	}
	target, err := ResolveInstallTarget(archive.Manifest.Name, options.Global)
	if err != nil {
		return result, err
	}
	result = InstallResult{Manifest: archive.Manifest, Lock: archive.Lock, Target: target, DryRun: options.DryRun}
	if err := requireAbsentTarget(target); err != nil {
		return result, err
	}

	if options.DryRun {
		return dryRunInstall(result, archive)
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return result, fmt.Errorf("create team install root: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".hufu-team-install-*")
	if err != nil {
		return result, fmt.Errorf("create team install staging directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(staging); cleanupErr != nil {
			cleanupErr = fmt.Errorf("remove team install staging directory: %w", cleanupErr)
			if resultErr == nil {
				resultErr = cleanupErr
				return
			}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	if err := stageArchiveSources(staging, archive); err != nil {
		return result, err
	}
	if _, _, err := validateStagedArchive(staging, archive); err != nil {
		return result, fmt.Errorf("validate staged team package: %w", err)
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return result, fmt.Errorf("set staged team directory mode: %w", err)
	}
	if err := operations.syncTree(staging); err != nil {
		return result, fmt.Errorf("sync staged team package: %w", err)
	}
	if err := operations.publish(staging, target); err != nil {
		return result, fmt.Errorf("publish team package without overwrite: %w", err)
	}
	if err := operations.syncDir(parent); err != nil {
		return result, fmt.Errorf("team package was published but install root sync failed: %w", err)
	}
	return result, nil
}

func dryRunInstall(result InstallResult, archive *Archive) (dryRunResult InstallResult, resultErr error) {
	dryRunResult = result
	scratch, err := os.MkdirTemp("", "hufu-team-install-dry-run-")
	if err != nil {
		return result, fmt.Errorf("create team install dry-run scratch: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(scratch); cleanupErr != nil {
			cleanupErr = fmt.Errorf("remove team install dry-run scratch: %w", cleanupErr)
			if resultErr == nil {
				resultErr = cleanupErr
				return
			}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	root := filepath.Join(scratch, archive.Manifest.Name)
	if err := stageArchiveSources(root, archive); err != nil {
		return result, err
	}
	if _, _, err := validateStagedArchive(root, archive); err != nil {
		return result, fmt.Errorf("validate staged team package: %w", err)
	}
	return result, nil
}

// ResolveInstallTarget returns the canonical project or user team directory.
func ResolveInstallTarget(name string, global bool) (string, error) {
	if err := ValidatePackageName(name); err != nil {
		return "", err
	}
	var (
		root string
		err  error
	)
	if global {
		root, err = team.DefaultUserTeamRoot()
		if err != nil {
			return "", err
		}
	} else {
		root, err = team.DefaultProjectTeamRoot()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(root, name), nil
}

func requireAbsentTarget(target string) error {
	_, err := os.Lstat(target)
	switch {
	case err == nil:
		return fmt.Errorf("team install target %q already exists", target)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("inspect team install target: %w", err)
	}
}

func syncStagedTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("staged package entry %q is not regular", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		return errors.Join(syncErr, closeErr)
	})
	if err != nil {
		return err
	}
	slices.SortFunc(directories, func(a, b string) int {
		if compared := cmp.Compare(strings.Count(b, string(filepath.Separator)), strings.Count(a, string(filepath.Separator))); compared != 0 {
			return compared
		}
		return strings.Compare(a, b)
	})
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}
