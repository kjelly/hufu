package teampkg

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	MaxEntries             = 2048
	MaxFileBytes           = 8 << 20
	MaxTotalBytes          = 64 << 20
	MaxArchiveBytes        = 64 << 20
	MaxCompressionRatio    = 100
	validationErrorPrefix  = "team package validation failed"
	validationFieldArchive = "archive"
)

// ValidationError reports a package policy failure without including file
// content or a candidate secret value.
type ValidationError struct {
	Path     string
	Field    string
	Category string
}

func (e *ValidationError) Error() string {
	parts := []string{validationErrorPrefix, "category=" + e.Category}
	if e.Path != "" {
		parts = append(parts, fmt.Sprintf("path=%q", e.Path))
	}
	if e.Field != "" {
		parts = append(parts, fmt.Sprintf("field=%q", e.Field))
	}
	return strings.Join(parts, " ")
}

func validationError(path, field, category string) error {
	return &ValidationError{Path: path, Field: field, Category: category}
}

// ValidateEntryPath enforces the platform-independent V1 archive path
// contract. It accepts slash paths only and never cleans an invalid path into
// a different valid one.
func ValidateEntryPath(value string) error {
	switch {
	case value == "":
		return validationError(value, "path", "path_empty")
	case !utf8.ValidString(value):
		return validationError(value, "path", "path_invalid_utf8")
	case strings.ContainsRune(value, 0):
		return validationError(value, "path", "path_nul")
	case strings.Contains(value, `\`):
		return validationError(value, "path", "path_backslash")
	case strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//"):
		return validationError(value, "path", "path_absolute")
	case hasWindowsDrivePrefix(value):
		return validationError(value, "path", "path_windows_drive")
	case path.Clean(value) != value:
		return validationError(value, "path", "path_not_clean")
	}
	for segment := range strings.SplitSeq(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return validationError(value, "path", "path_segment")
		}
	}
	root := filepath.Join(string(filepath.Separator), "hufu-package-root")
	target := filepath.Join(root, filepath.FromSlash(value))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return validationError(value, "path", "path_escape")
	}
	return nil
}

func hasWindowsDrivePrefix(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':'
}

type pathRegistry struct {
	exact  map[string]struct{}
	folded map[string]string
}

func newPathRegistry() *pathRegistry {
	return &pathRegistry{exact: make(map[string]struct{}), folded: make(map[string]string)}
}

func (r *pathRegistry) add(value string) error {
	if err := ValidateEntryPath(value); err != nil {
		return err
	}
	if _, exists := r.exact[value]; exists {
		return validationError(value, "path", "path_duplicate")
	}
	folded := asciiFold(value)
	if _, exists := r.folded[folded]; exists {
		return validationError(value, "path", "path_case_collision")
	}
	r.exact[value] = struct{}{}
	r.folded[folded] = value
	return nil
}

func asciiFold(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for _, char := range value {
		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
		}
		result.WriteRune(char)
	}
	return result.String()
}
