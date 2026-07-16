package tui

import "os"

func osWriteFile(path string, content []byte, perm os.FileMode) error {
	return os.WriteFile(path, content, perm)
}
