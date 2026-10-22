//go:build !unix

package team

import (
	"fmt"
	"os"
)

func openBoundedArtifact(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact must be a regular file")
	}
	return os.Open(path)
}
