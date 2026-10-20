package paired

import (
	"path"
	"strings"
)

// SafeRelativePath accepts only normalized slash-separated paths. This check
// is lexical; filesystem callers must additionally reject symlink components.
func SafeRelativePath(value string) bool {
	return value != "" && value != "." && value != ".." && path.Clean(value) == value &&
		!strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "../") &&
		!strings.ContainsAny(value, "\\:\x00")
}
