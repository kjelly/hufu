//go:build !linux && !darwin

package main

import (
	"errors"
	"fmt"
	"os"
)

var errDebugSymlinkPath = errors.New("debug path contains symlink")

// Debug bundles fail closed on platforms without the descriptor-walking
// implementation rather than falling back to a racy pathname read.
func debugOpenFile(path string) (*os.File, error) {
	return nil, fmt.Errorf("secure debug file opening is unavailable on this platform: %s", path)
}
