//go:build windows

package workspace

// Windows does not expose a portable directory fsync through os.File. File
// contents are flushed before publication and rename remains atomic.
func syncDirectory(string) error { return nil }
