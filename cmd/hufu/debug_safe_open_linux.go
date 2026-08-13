//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var errDebugSymlinkPath = errors.New("debug path contains symlink")

// debugOpenFile opens a regular debug input by walking every path component
// from a pinned directory descriptor. O_NOFOLLOW on every open prevents a
// concurrent replacement with a symlink between validation and reading.
func debugOpenFile(path string) (*os.File, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cleanPath := filepath.Clean(absPath)
	components := strings.Split(strings.TrimPrefix(cleanPath, string(filepath.Separator)), string(filepath.Separator))
	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for i, component := range components {
		if component == "" || component == "." {
			continue
		}
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if i < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(current, component, flags, 0)
		_ = unix.Close(current)
		if openErr != nil {
			if errors.Is(openErr, unix.ELOOP) {
				return nil, fmt.Errorf("%w: %s", errDebugSymlinkPath, path)
			}
			return nil, openErr
		}
		current = next
	}
	return os.NewFile(uintptr(current), cleanPath), nil
}
