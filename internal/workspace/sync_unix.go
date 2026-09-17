//go:build !windows

package workspace

import (
	"fmt"
	"os"
)

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync %q: %w", path, err)
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync directory %q: %w", path, err)
	}
	if err = directory.Close(); err != nil {
		return fmt.Errorf("close directory %q: %w", path, err)
	}
	return nil
}
