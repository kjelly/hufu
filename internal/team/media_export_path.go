package team

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Pin each directory while checking its inode. A writable parent can be
// renamed concurrently, but a symlink cannot redirect a subsequent open.
func imageExportDirectory(destination string) (*os.Root, string, error) {
	if strings.TrimSpace(destination) == "" || strings.HasSuffix(destination, string(os.PathSeparator)) {
		return nil, "", fmt.Errorf("an explicit destination filename is required")
	}
	abs, err := filepath.Abs(destination)
	if err != nil {
		return nil, "", err
	}
	volumeRoot := filepath.VolumeName(abs) + string(os.PathSeparator)
	root, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, "", err
	}
	success := false
	defer func() {
		if !success {
			_ = root.Close()
		}
	}()
	parent := strings.TrimPrefix(filepath.Dir(abs), volumeRoot)
	for _, component := range strings.Split(parent, string(os.PathSeparator)) {
		if component == "" {
			continue
		}
		info, err := root.Lstat(component)
		if err != nil {
			return nil, "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, "", fmt.Errorf("destination parent must be a real directory")
		}
		child, err := root.OpenRoot(component)
		if err != nil {
			return nil, "", err
		}
		opened, err := child.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = child.Close()
			return nil, "", fmt.Errorf("destination parent changed while opening")
		}
		_ = root.Close()
		root = child
	}
	name := filepath.Base(abs)
	if name == "." || name == string(os.PathSeparator) {
		return nil, "", fmt.Errorf("destination must name a new file")
	}
	success = true
	return root, name, nil
}

func writeExclusiveImage(ctx context.Context, destination string, source io.Reader) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, name, err := imageExportDirectory(destination)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	defer func() {
		closeErr := file.Close()
		if resultErr == nil {
			resultErr = closeErr
		}
		if resultErr != nil {
			// Remove only this newly created inode, through its pinned parent.
			if current, err := root.Lstat(name); err == nil && os.SameFile(info, current) {
				_ = root.Remove(name)
			}
		}
	}()
	if _, err := io.Copy(file, imageExportReader{ctx: ctx, source: source}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return file.Sync()
}

type imageExportReader struct {
	ctx    context.Context
	source io.Reader
}

func (r imageExportReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buffer)
}
