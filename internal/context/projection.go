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

// RenderLegacySTMMarkdown and RenderLegacyLTMMarkdown are compatibility
// projections for the existing prompt reader.  They are generated only from
// canonical items, never read back into the repository.
func RenderLegacySTMMarkdown(items []ContextItem) string {
	return renderLegacyMarkdown(items, map[ContextKind]string{
		ContextProgress:     "# \u9032\u5ea6",
		ContextDecision:     "# \u6c7a\u7b56",
		ContextError:        "# \u932f\u8aa4",
		ContextOpenQuestion: "# \u5f85\u78ba\u8a8d",
	}, "# \u767c\u73fe")
}

func RenderLegacyLTMMarkdown(items []ContextItem) string {
	return renderLegacyMarkdown(items, nil, "# \u5e38\u898b\u6a21\u5f0f")
}

func renderLegacyMarkdown(items []ContextItem, sections map[ContextKind]string, fallback string) string {
	items = append([]ContextItem(nil), items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	bySection := map[string][]string{}
	order := []string{}
	for _, item := range items {
		section := fallback
		if item.Metadata != nil && item.Metadata["legacy_section"] != "" {
			section = item.Metadata["legacy_section"]
		} else if sections != nil && sections[item.Kind] != "" {
			section = sections[item.Kind]
		}
		if _, ok := bySection[section]; !ok {
			order = append(order, section)
		}
		entry := strings.TrimSpace(item.Content)
		entry = strings.TrimPrefix(entry, "- ")
		entry = strings.ReplaceAll(entry, "\n", "\n  ")
		bySection[section] = append(bySection[section], "- "+entry)
	}
	var b strings.Builder
	for i, section := range order {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(section)
		for _, entry := range bySection[section] {
			b.WriteString("\n")
			b.WriteString(entry)
		}
	}
	return b.String()
}

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
// overwritten by shadow data. Since WP-1 the projection is rebuilt from
// shared-scope items only (agent_id, task_id, branch_id, attempt_id are all
// NULL) so private records never leak into the shared Markdown files.
func (r *SQLiteRepository) RebuildProjection(ctx context.Context, scope Scope) error {
	items, err := r.QuerySharedProjection(ctx, scope)
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	stm := RenderSTMMarkdown(items)
	ltm := RenderLTMMarkdown(items)
	if err = atomicWrite(filepath.Join(dir, "context-stm.md"), stm); err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(dir, "context-ltm.md"), ltm); err != nil {
		return err
	}
	return nil
}
