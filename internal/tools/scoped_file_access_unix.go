//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

type scopedFileAccess struct {
	root     *os.Root
	relative string
	absolute string
}

var scopedTempSequence atomic.Uint64

func newScopedFileAccess(cfg ToolConfig, path string, write bool) (*scopedFileAccess, error) {
	if cfg.TaskPathScopeError != nil {
		return nil, cfg.TaskPathScopeError
	}
	if cfg.TaskPathScope == nil {
		return nil, nil
	}
	scope := cfg.TaskPathScope
	bounded := scope.ReadBounded
	allowed := scope.ReadPaths
	if write {
		bounded = scope.WriteBounded
		allowed = scope.WritePaths
	}
	if !bounded {
		return nil, nil
	}
	if err := validateAgentTaskPathScope(scope); err != nil {
		return nil, err
	}
	rootPath := cfg.WorkDir
	if rootPath == "" {
		var err error
		rootPath, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resource_scope_unreproducible: resolve mutable root: %w", err)
		}
	}
	rootPath, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resource_scope_unreproducible: resolve mutable root: %w", err)
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resource_scope_unreproducible: resolve mutable root: %w", err)
	}
	path = normalizeWorkspacePath(path, cfg.WorkspaceName)
	absPath, err := resolvePathWithWorkDir(path, rootPath)
	if err != nil {
		return nil, err
	}
	absPath = filepath.Clean(absPath)
	if !pathAuthorizedByTaskScope(absPath, allowed) {
		return nil, fmt.Errorf("path %q is outside the admitted task scope", path)
	}
	if err := enforceArtifactPathPolicy(canonicalPathForAuthorization(absPath), cfg); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(rootPath, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("path %q is outside mutable root", path)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resource_scope_unreproducible: open mutable root: %w", err)
	}
	return &scopedFileAccess{root: root, relative: rel, absolute: absPath}, nil
}

func pathAuthorizedByTaskScope(path string, allowed []string) bool {
	path = filepath.Clean(path)
	for _, candidate := range allowed {
		directory := strings.HasSuffix(candidate, string(filepath.Separator))
		candidate = strings.TrimSuffix(filepath.Clean(candidate), string(filepath.Separator))
		if path == candidate {
			return true
		}
		if directory {
			rel, err := filepath.Rel(candidate, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func (a *scopedFileAccess) close() {
	if a != nil && a.root != nil {
		_ = a.root.Close()
	}
}

func (a *scopedFileAccess) openRegular() (*os.File, os.FileInfo, error) {
	info, err := a.root.Lstat(a.relative)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("refusing to follow final symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("target is not a regular file")
	}
	file, err := a.root.Open(a.relative)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("file changed while it was being opened")
	}
	return file, info, nil
}

func (a *scopedFileAccess) readRegular() ([]byte, os.FileInfo, error) {
	file, info, err := a.openRegular()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	return data, info, err
}

func (a *scopedFileAccess) destinationInfo() (os.FileInfo, error) {
	info, err := a.root.Lstat(a.relative)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir() || (info.Mode()&os.ModeSymlink == 0 && !info.Mode().IsRegular()) {
		return nil, fmt.Errorf("destination is not a replaceable regular file")
	}
	return info, nil
}

func (a *scopedFileAccess) writeAtomic(ctx context.Context, data []byte, expected os.FileInfo, creationMode os.FileMode) error {
	parent := filepath.Dir(a.relative)
	if parent != "." {
		if err := a.root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	mode := creationMode.Perm()
	if expected != nil && expected.Mode().IsRegular() {
		mode = expected.Mode().Perm()
	}
	var tempName string
	var temp *os.File
	for range 100 {
		tempName = filepath.Join(parent, fmt.Sprintf(".hufu-scope-%d.tmp", scopedTempSequence.Add(1)))
		var err error
		temp, err = a.root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if temp == nil {
		return fmt.Errorf("unable to allocate scoped temporary file")
	}
	renamed := false
	defer func() {
		_ = temp.Close()
		if !renamed {
			_ = a.root.Remove(tempName)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := a.destinationInfo()
	if err != nil {
		return err
	}
	if expected == nil {
		if current != nil {
			return fmt.Errorf("destination appeared during write")
		}
	} else if current == nil || !os.SameFile(expected, current) {
		return fmt.Errorf("destination changed during write")
	}
	if err := a.root.Rename(tempName, a.relative); err != nil {
		return err
	}
	renamed = true
	return nil
}
