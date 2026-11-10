//go:build linux || darwin

package media

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Walk with directory handles rather than Lstat/Open: substitutions cannot
// introduce a symlink between validation and opening a path component.
func openRegularFile(path string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dirs, leaf, _ := strings.CutLast(abs, "/")
	defer func() { _ = unix.Close(fd) }()
	for part := range strings.SplitSeq(strings.TrimPrefix(dirs, "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return nil, openErr
		}
		_ = unix.Close(fd)
		fd = next
	}
	fileFD, err := unix.Openat(fd, leaf, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fileFD), "media-input")
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, Reject(AccessDenied, "image source is not regular")
	}
	return f, nil
}
