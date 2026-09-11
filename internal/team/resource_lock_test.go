package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// newResourceLockTestCoordinator builds a minimal Coordinator with a real
// on-disk event store (matching the pattern other coordinator tests use,
// e.g. TestPhase1DiagnosticEventAppendFailureRetriesAtCheckpoint), so
// ValidateRequiredResourceLocks can durably append and persist.
func newResourceLockTestCoordinator(t *testing.T, dir string, specs []agent.RequiredResourceSpec) *Coordinator {
	t.Helper()
	session := &TeamSession{
		Dir:       dir,
		Workspace: dir,
		Config:    agent.TeamConfig{Name: "locked-team", RequiredResources: specs},
	}
	c := &Coordinator{session: session, sessionData: NewSession(), executionRunID: "run-1"}
	es, err := NewEventStore(dir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	t.Cleanup(func() { _ = es.Close() })
	c.eventStore = es
	return c
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestResolveRequiredResourceReadsAndHashesContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "rules content")

	spec := agent.RequiredResourceSpec{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true}
	loaded, err := ResolveRequiredResource(root, spec)
	if err != nil {
		t.Fatalf("ResolveRequiredResource() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("ResolveRequiredResource() = nil, want a resolved resource")
	}
	if loaded.Content != "rules content" {
		t.Fatalf("Content = %q, want %q", loaded.Content, "rules content")
	}
	if loaded.ByteSize != int64(len("rules content")) {
		t.Fatalf("ByteSize = %d, want %d", loaded.ByteSize, len("rules content"))
	}
	if loaded.Name != "team-rules" || loaded.Kind != agent.ResourceProjectRules {
		t.Fatalf("Name/Kind = %q/%q, want team-rules/project_rules", loaded.Name, loaded.Kind)
	}
	if loaded.SHA256 == "" {
		t.Fatal("SHA256 is empty, want a computed digest")
	}
}

func TestRequiredResourceMissingFailsAdmission(t *testing.T) {
	root := t.TempDir()
	spec := agent.RequiredResourceSpec{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "does-not-exist.md", Required: true}

	if _, err := ResolveRequiredResource(root, spec); err == nil {
		t.Fatal("ResolveRequiredResource() = nil error, want missing-resource failure")
	}
}

func TestOptionalMissingResourceIsSkippedNotFailed(t *testing.T) {
	root := t.TempDir()
	spec := agent.RequiredResourceSpec{Name: "nice-to-have", Kind: agent.ResourceProjectRules, Path: "does-not-exist.md", Required: false}

	loaded, err := ResolveRequiredResource(root, spec)
	if err != nil {
		t.Fatalf("ResolveRequiredResource() error = %v, want nil for optional missing resource", err)
	}
	if loaded != nil {
		t.Fatalf("ResolveRequiredResource() = %+v, want nil for optional missing resource", loaded)
	}
}

func TestRequiredResourceDigestMismatchFailsAdmission(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "rules content")

	spec := agent.RequiredResourceSpec{
		Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true,
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
	}
	_, err := ResolveRequiredResource(root, spec)
	if err == nil {
		t.Fatal("ResolveRequiredResource() = nil error, want sha256-mismatch failure")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("error = %v, want it to mention sha256 mismatch", err)
	}
}

func TestRequiredResourceSymlinkEscapeFails(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "secret.md"), "outside content")

	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	spec := agent.RequiredResourceSpec{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true}
	_, err := ResolveRequiredResource(root, spec)
	if err == nil {
		t.Fatal("ResolveRequiredResource() = nil error, want symlink-escape failure")
	}
	if !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("error = %v, want it to mention escaping the root", err)
	}
}

func TestResolveRequiredResourcesStopsAtFirstFailure(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "ok.md"), "fine")

	specs := []agent.RequiredResourceSpec{
		{Name: "ok", Kind: agent.ResourceProjectRules, Path: "ok.md", Required: true},
		{Name: "missing", Kind: agent.ResourceProjectRules, Path: "missing.md", Required: true},
	}
	if _, _, err := ResolveRequiredResources(root, specs); err == nil {
		t.Fatal("ResolveRequiredResources() = nil error, want failure from the missing resource")
	}
}

func TestResolveRequiredResourcesProducesMatchingSetAndLoaded(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "rules content")

	specs := []agent.RequiredResourceSpec{
		{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true, InjectInto: []string{"coordinator"}},
	}
	set, loaded, err := ResolveRequiredResources(root, specs)
	if err != nil {
		t.Fatalf("ResolveRequiredResources() error = %v", err)
	}
	if len(set.Resources) != 1 || len(loaded) != 1 {
		t.Fatalf("got %d locked resources / %d loaded, want 1/1", len(set.Resources), len(loaded))
	}
	if set.Digest == "" {
		t.Fatal("set.Digest is empty, want a computed digest")
	}
	if set.Resources[0].SHA256 != loaded[0].SHA256 {
		t.Fatalf("locked SHA256 %q != loaded SHA256 %q", set.Resources[0].SHA256, loaded[0].SHA256)
	}
}

func TestLockedResourceSetDigestExcludesLoadedAt(t *testing.T) {
	base := LockedResource{
		Name: "team-rules", Kind: agent.ResourceProjectRules, CanonicalPath: "/root/AGENTS.md",
		SHA256: "abc", ByteSize: 10, InjectInto: []string{"coordinator"},
	}
	a := base
	a.LoadedAt = time.Now()
	b := base
	b.LoadedAt = a.LoadedAt.Add(time.Hour)

	if got, want := ComputeLockedResourceSetDigest([]LockedResource{a}), ComputeLockedResourceSetDigest([]LockedResource{b}); got != want {
		t.Fatalf("digest changed with LoadedAt: %q != %q", got, want)
	}
}

func TestLockedResourceSetDigestIsOrderIndependent(t *testing.T) {
	r1 := LockedResource{Name: "a", Kind: agent.ResourceSkill, CanonicalPath: "/a", SHA256: "1", ByteSize: 1}
	r2 := LockedResource{Name: "b", Kind: agent.ResourceSkill, CanonicalPath: "/b", SHA256: "2", ByteSize: 2}

	forward := ComputeLockedResourceSetDigest([]LockedResource{r1, r2})
	reversed := ComputeLockedResourceSetDigest([]LockedResource{r2, r1})
	if forward != reversed {
		t.Fatalf("digest depends on order: %q != %q", forward, reversed)
	}
}

func TestLockedResourceSetDigestChangesWithContent(t *testing.T) {
	r1 := LockedResource{Name: "a", Kind: agent.ResourceSkill, CanonicalPath: "/a", SHA256: "1", ByteSize: 1}
	r2 := LockedResource{Name: "a", Kind: agent.ResourceSkill, CanonicalPath: "/a", SHA256: "2", ByteSize: 1}

	if ComputeLockedResourceSetDigest([]LockedResource{r1}) == ComputeLockedResourceSetDigest([]LockedResource{r2}) {
		t.Fatal("digest did not change when sha256 changed")
	}
}

// TestDefaultProfileWithoutResourcesIsCompatible is runtime invariant 10:
// legacy/default teams that never declare required-resources see no
// behavior change. At the parser level that means an empty declaration
// list round-trips to an empty, non-nil-checked TeamConfig field.
func TestDefaultProfileWithoutResourcesIsCompatible(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "team.yaml"), "name: legacy-team\n")

	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML() error = %v", err)
	}
	if len(cfg.RequiredResources) != 0 {
		t.Fatalf("RequiredResources = %+v, want empty for a team.yaml with no declaration", cfg.RequiredResources)
	}
}

func TestParseTeamYMLRequiredResources(t *testing.T) {
	dir := t.TempDir()
	content := `name: locked-team
required-resources:
  - name: team-rules
    kind: project_rules
    path: AGENTS.md
    inject-into: [coordinator, coder]
    required: true
`
	writeTestFile(t, filepath.Join(dir, "team.yaml"), content)

	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML() error = %v", err)
	}
	if len(cfg.RequiredResources) != 1 {
		t.Fatalf("RequiredResources = %+v, want 1 entry", cfg.RequiredResources)
	}
	got := cfg.RequiredResources[0]
	if got.Name != "team-rules" || got.Kind != agent.ResourceProjectRules || got.Path != "AGENTS.md" || !got.Required {
		t.Fatalf("RequiredResources[0] = %+v, want team-rules/project_rules/AGENTS.md/required", got)
	}
	if want := []string{"coordinator", "coder"}; len(got.InjectInto) != 2 || got.InjectInto[0] != want[0] || got.InjectInto[1] != want[1] {
		t.Fatalf("InjectInto = %v, want %v", got.InjectInto, want)
	}
}

func TestParseTeamYMLRejectsInvalidRequiredResource(t *testing.T) {
	dir := t.TempDir()
	content := `name: locked-team
required-resources:
  - name: team-rules
    kind: not-a-real-kind
    path: AGENTS.md
`
	writeTestFile(t, filepath.Join(dir, "team.yaml"), content)

	if _, err := parseTeamYML(dir, nil); err == nil {
		t.Fatal("parseTeamYML() = nil error, want validation failure for unknown kind")
	}
}

func TestValidateRequiredResourceLocksNoOpWithoutDeclaration(t *testing.T) {
	dir := t.TempDir()
	// Deliberately no event store attached: if the no-op short-circuit ever
	// regressed to touching EventJournal() first, this would fail with
	// "event journal is unavailable" instead of returning nil.
	c := &Coordinator{session: &TeamSession{Dir: dir, Workspace: dir, Config: agent.TeamConfig{Name: "legacy-team"}}, sessionData: NewSession()}

	if err := c.ValidateRequiredResourceLocks(context.Background(), dir); err != nil {
		t.Fatalf("ValidateRequiredResourceLocks() = %v, want nil for a team with no required-resources", err)
	}
}

func TestResourceLockEventContainsNoContent(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "AGENTS.md"), "secret rules content")
	specs := []agent.RequiredResourceSpec{{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true}}
	c := newResourceLockTestCoordinator(t, dir, specs)

	if err := c.ValidateRequiredResourceLocks(context.Background(), dir); err != nil {
		t.Fatalf("ValidateRequiredResourceLocks() error = %v", err)
	}

	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents() error = %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Type != string(EventResourceLocked) {
			continue
		}
		found = true
		if strings.Contains(string(e.Payload), "secret rules content") {
			t.Fatalf("resource_locked payload contains raw content: %s", e.Payload)
		}
		var set LockedResourceSet
		if err := json.Unmarshal(e.Payload, &set); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if len(set.Resources) != 1 || set.Resources[0].SHA256 == "" {
			t.Fatalf("payload set = %+v, want one locked resource with a digest", set)
		}
	}
	if !found {
		t.Fatal("no resource_locked event was appended")
	}
}

func TestLockedResourceSetSurvivesResume(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "AGENTS.md"), "rules content")
	specs := []agent.RequiredResourceSpec{{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true}}
	c := newResourceLockTestCoordinator(t, dir, specs)

	if err := c.ValidateRequiredResourceLocks(context.Background(), dir); err != nil {
		t.Fatalf("ValidateRequiredResourceLocks() error = %v", err)
	}

	resumed := LoadSession(dir)
	if resumed == nil || resumed.RequiredResourceLockSet == nil {
		t.Fatalf("LoadSession() RequiredResourceLockSet = %+v, want a persisted set", resumed)
	}
	if resumed.RequiredResourceLockSet.Digest == "" {
		t.Fatal("resumed RequiredResourceLockSet.Digest is empty")
	}
}

func TestLockedResourceReplayRejectsConflict(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "AGENTS.md"), "original content")
	specs := []agent.RequiredResourceSpec{{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true}}
	c := newResourceLockTestCoordinator(t, dir, specs)

	if err := c.ValidateRequiredResourceLocks(context.Background(), dir); err != nil {
		t.Fatalf("first ValidateRequiredResourceLocks() error = %v", err)
	}

	writeTestFile(t, filepath.Join(dir, "AGENTS.md"), "mutated content")

	err := c.ValidateRequiredResourceLocks(context.Background(), dir)
	if err == nil {
		t.Fatal("second ValidateRequiredResourceLocks() = nil error, want drift-detected failure")
	}
	if !strings.Contains(err.Error(), "drift detected") {
		t.Fatalf("error = %v, want it to mention drift detected", err)
	}
}

func TestLockedResourceMutationDoesNotChangeRunInput(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "AGENTS.md"), "original content")
	specs := []agent.RequiredResourceSpec{{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true}}
	c := newResourceLockTestCoordinator(t, dir, specs)

	if err := c.ValidateRequiredResourceLocks(context.Background(), dir); err != nil {
		t.Fatalf("ValidateRequiredResourceLocks() error = %v", err)
	}
	originalDigest := c.LoadedRequiredResources()[0].SHA256
	originalContent := c.LoadedRequiredResources()[0].Content

	writeTestFile(t, filepath.Join(dir, "AGENTS.md"), "mutated content")

	// The already-locked in-memory content and persisted metadata must not
	// change just because the underlying file changed after locking
	// (invariant 5); only a fresh admission attempt (tested separately)
	// detects and rejects the drift.
	if c.LoadedRequiredResources()[0].Content != originalContent {
		t.Fatalf("LoadedRequiredResources content changed after mutation: %q", c.LoadedRequiredResources()[0].Content)
	}
	var persisted *LockedResourceSet
	c.viewSessionData(func(sd *SessionData) { persisted = sd.RequiredResourceLockSet })
	if persisted.Resources[0].SHA256 != originalDigest {
		t.Fatalf("persisted SHA256 = %q, want unchanged %q", persisted.Resources[0].SHA256, originalDigest)
	}
}
