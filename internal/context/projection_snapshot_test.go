package context

// WP-0 — Shared STM/LTM projection snapshot tests.
//
// These tests pin the current behavior of RebuildProjection and the legacy
// Markdown renderers (RenderLegacySTMMarkdown / RenderLegacyLTMMarkdown) so
// that WP-1's shared-only projection query does not silently change what
// appears in the human-readable projections.
//
// Key behaviors fixed here:
//   - RebuildProjection writes context-stm.md and context-ltm.md from ALL
//     items in scope (including agent-scoped ones, per the current wildcard).
//   - Legacy stm.md / ltm-team.md files are never overwritten by
//     RebuildProjection (it writes side-by-side canonical projections).
//   - RenderLegacySTMMarkdown groups items by kind into the existing STM
//     section headings.
//   - RenderLegacyLTMMarkdown uses the fallback section for non-mapped kinds.
//   - Projection content is redacted (secrets never appear in projections).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectionSnapshot_RebuildWritesAllItemsInScope(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"}
	if err := repo.Append(ctx,
		ContextItem{ID: "dec-1", Kind: ContextDecision, Content: "decided to use SQLite", Scope: scope, Priority: PriorityHigh},
		ContextItem{ID: "prog-1", Kind: ContextProgress, Content: "migration step completed", Scope: scope, Priority: PriorityNormal},
		ContextItem{ID: "err-1", Kind: ContextError, Content: "connection refused on retry", Scope: scope, Priority: PriorityNormal},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := repo.RebuildProjection(ctx, scope); err != nil {
		t.Fatalf("RebuildProjection: %v", err)
	}
	stm, err := os.ReadFile(filepath.Join(dir, "context-stm.md"))
	if err != nil {
		t.Fatalf("read context-stm.md: %v", err)
	}
	// All three items must appear in the canonical projection.
	for _, want := range []string{"decided to use SQLite", "migration step completed", "connection refused on retry"} {
		if !strings.Contains(string(stm), want) {
			t.Errorf("context-stm.md missing %q:\n%s", want, stm)
		}
	}
}

func TestProjectionSnapshot_RebuildDoesNotOverwriteLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "proj", TeamID: "team"}
	if err := repo.Append(ctx, ContextItem{ID: "canon", Kind: ContextProgress, Content: "canonical entry", Scope: scope}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	legacySTM := "# 進度\n- legacy STM entry that must survive"
	legacyLTM := "# 專案慣例\n- legacy LTM entry that must survive"
	if err := os.WriteFile(filepath.Join(dir, "stm.md"), []byte(legacySTM), 0o644); err != nil {
		t.Fatalf("write stm.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ltm-team.md"), []byte(legacyLTM), 0o644); err != nil {
		t.Fatalf("write ltm-team.md: %v", err)
	}
	if err := repo.RebuildProjection(ctx, scope); err != nil {
		t.Fatalf("RebuildProjection: %v", err)
	}
	for name, want := range map[string]string{"stm.md": legacySTM, "ltm-team.md": legacyLTM} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("RebuildProjection overwrote legacy %s:\ngot:  %q\nwant: %q", name, got, want)
		}
	}
}

func TestProjectionSnapshot_LegacySTMGroupsByKind(t *testing.T) {
	items := []ContextItem{
		{ID: "d", Kind: ContextDecision, Content: "use WAL mode", Scope: Scope{ProjectID: "p"}, CreatedAt: time.Now()},
		{ID: "p", Kind: ContextProgress, Content: "step one done", Scope: Scope{ProjectID: "p"}, CreatedAt: time.Now()},
		{ID: "e", Kind: ContextError, Content: "timeout on step two", Scope: Scope{ProjectID: "p"}, CreatedAt: time.Now()},
		{ID: "q", Kind: ContextOpenQuestion, Content: "should we shard?", Scope: Scope{ProjectID: "p"}, CreatedAt: time.Now()},
	}
	stm := RenderLegacySTMMarkdown(items)
	for _, want := range []string{"# 進度", "# 決策", "# 錯誤", "# 待確認"} {
		if !strings.Contains(stm, want) {
			t.Errorf("legacy STM missing section %q:\n%s", want, stm)
		}
	}
	for _, want := range []string{"use WAL mode", "step one done", "timeout on step two", "should we shard?"} {
		if !strings.Contains(stm, want) {
			t.Errorf("legacy STM missing content %q:\n%s", want, stm)
		}
	}
}

func TestProjectionSnapshot_LegacyLTMUsesFallbackSection(t *testing.T) {
	items := []ContextItem{
		{ID: "pat", Kind: ContextPattern, Content: "retry with backoff", Scope: Scope{ProjectID: "p"}, CreatedAt: time.Now()},
		{ID: "arch", Kind: ContextArchitecture, Content: "layered context store", Scope: Scope{ProjectID: "p"}, CreatedAt: time.Now()},
	}
	ltm := RenderLegacyLTMMarkdown(items)
	// The fallback section heading for LTM is "# 常見模式".
	if !strings.Contains(ltm, "# 常見模式") {
		t.Errorf("legacy LTM missing fallback section:\n%s", ltm)
	}
	for _, want := range []string{"retry with backoff", "layered context store"} {
		if !strings.Contains(ltm, want) {
			t.Errorf("legacy LTM missing content %q:\n%s", want, ltm)
		}
	}
}

func TestProjectionSnapshot_LegacySTMRespectsMetadataSection(t *testing.T) {
	items := []ContextItem{
		{ID: "custom", Kind: ContextObservation, Content: "custom section entry", Scope: Scope{ProjectID: "p"}, Metadata: map[string]string{"legacy_section": "# 自訂"}, CreatedAt: time.Now()},
	}
	stm := RenderLegacySTMMarkdown(items)
	if !strings.Contains(stm, "# 自訂") {
		t.Errorf("legacy STM should respect metadata legacy_section:\n%s", stm)
	}
	if !strings.Contains(stm, "custom section entry") {
		t.Errorf("legacy STM missing custom entry:\n%s", stm)
	}
}

func TestProjectionSnapshot_RedactedContentInProjection(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "proj"}
	if err := repo.Append(ctx, ContextItem{
		ID: "secret-item", Kind: ContextObservation,
		Content: "found Authorization: Bearer sk-leaked1234567890abcdef in logs",
		Scope:   scope,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := repo.RebuildProjection(ctx, scope); err != nil {
		t.Fatalf("RebuildProjection: %v", err)
	}
	stm, err := os.ReadFile(filepath.Join(dir, "context-stm.md"))
	if err != nil {
		t.Fatalf("read context-stm.md: %v", err)
	}
	if strings.Contains(string(stm), "sk-leaked1234567890abcdef") {
		t.Errorf("projection leaked secret:\n%s", stm)
	}
}

func TestProjectionSnapshot_CanonicalProjectionHeader(t *testing.T) {
	items := []ContextItem{
		{ID: "x", Kind: ContextDecision, Content: "test decision", Scope: Scope{ProjectID: "p"}, CreatedAt: time.Now()},
	}
	stm := RenderSTMMarkdown(items)
	if !strings.Contains(stm, "Generated STM projection") {
		t.Errorf("canonical STM projection missing generated header:\n%s", stm)
	}
	if !strings.Contains(stm, "Do not edit; SQLite is canonical") {
		t.Errorf("canonical STM projection missing canonical warning:\n%s", stm)
	}
}

func TestProjectionSnapshot_RebuildProjectionIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "proj", TeamID: "team"}
	if err := repo.Append(ctx, ContextItem{ID: "stable", Kind: ContextDecision, Content: "stable decision", Scope: scope}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := repo.RebuildProjection(ctx, scope); err != nil {
		t.Fatalf("first RebuildProjection: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "context-stm.md"))
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	if err := repo.RebuildProjection(ctx, scope); err != nil {
		t.Fatalf("second RebuildProjection: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "context-stm.md"))
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("RebuildProjection is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestProjectionSnapshot_Contract_SharedOnlyProjection is the WP-1
// contract: RebuildProjection for a shared (empty AgentID) scope must not
// include agent-scoped private items in the projection.
func TestProjectionSnapshot_Contract_SharedOnlyProjection(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()
	ctx := context.Background()
	shared := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess"}
	agentA := Scope{ProjectID: "proj", TeamID: "team", SessionID: "sess", AgentID: "agent-a"}
	if err := repo.Append(ctx,
		ContextItem{ID: "shared-item", Kind: ContextDecision, Content: "shared decision visible to all", Scope: shared},
		ContextItem{ID: "private-item", Kind: ContextObservation, Content: "agent-a private observation", Scope: agentA},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := repo.RebuildProjection(ctx, shared); err != nil {
		t.Fatalf("RebuildProjection: %v", err)
	}
	stm, err := os.ReadFile(filepath.Join(dir, "context-stm.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(stm), "shared decision visible to all") {
		t.Errorf("shared projection missing shared item:\n%s", stm)
	}
	if strings.Contains(string(stm), "agent-a private observation") {
		t.Errorf("shared projection leaked private item:\n%s", stm)
	}
}
