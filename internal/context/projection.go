package context

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RenderSTMMarkdown and RenderLTMMarkdown are human-readable projections only;
// callers must never read them back as canonical data.
func RenderSTMMarkdown(items []ContextItem) string { return renderProjection(items, "STM") }
func RenderLTMMarkdown(items []ContextItem) string { return renderProjection(items, "LTM") }

func renderProjection(items []ContextItem, name string) string {
	items = append([]ContextItem(nil), items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- Generated %s projection. Do not edit; SQLite is canonical. -->\n\n", name)
	for _, item := range items {
		fmt.Fprintf(&b, "## %s · %s\n\n", item.Kind, item.ID)
		b.WriteString(item.Content)
		b.WriteString("\n\n")
	}
	return b.String()
}

func atomicWrite(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".context-projection-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.WriteString(content); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// RebuildProjection intentionally writes side-by-side projections. During
// Phase 1 the legacy stm.md/ltm.md remain the prompt source and must not be
// overwritten by shadow data.
func (r *SQLiteRepository) RebuildProjection(ctx context.Context, scope Scope) error {
	items, err := r.Query(ctx, RepositoryQuery{Scope: scope, IncludeSuperseded: true, IncludeExpired: true, Limit: 100000})
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	if err = atomicWrite(filepath.Join(dir, "context-stm.md"), RenderSTMMarkdown(items)); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "context-ltm.md"), RenderLTMMarkdown(items))
}
