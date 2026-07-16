package team

import (
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func newMemoryTestCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	return &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "test"},
		},
	}
}

// A real run's LTM accumulated 8 entries in one section, newest-first (entry
// n is always more recent than entry n+1, since persisting prepends). Both
// prompt-injection paths used to keep entries[len-3:] — the 3 OLDEST
// survivors — which silently hid the freshest lessons (including one from
// the very same run) from every agent that read them.
func TestBuildLTMContextKeepsMostRecentEntries(t *testing.T) {
	c := newMemoryTestCoordinator(t)
	content := "# 常見模式\n" +
		"- [2026-07-13] newest lesson\n" +
		"- [2026-07-13] second newest\n" +
		"- [2026-07-12] third newest\n" +
		"- [2026-07-12] oldest lesson 1\n" +
		"- [2026-07-12] oldest lesson 2\n"
	if err := SaveLTM(c.session.Workspace, c.session.Config.Name, content); err != nil {
		t.Fatalf("SaveLTM: %v", err)
	}

	got := c.buildLTMContext()
	for _, want := range []string{"newest lesson", "second newest", "third newest"} {
		if !strings.Contains(got, want) {
			t.Errorf("buildLTMContext() missing recent entry %q:\n%s", want, got)
		}
	}
	for _, avoid := range []string{"oldest lesson 1", "oldest lesson 2"} {
		if strings.Contains(got, avoid) {
			t.Errorf("buildLTMContext() should have dropped the older entry %q:\n%s", avoid, got)
		}
	}
}

func TestBuildMemorySuffixKeepsMostRecentLTMEntries(t *testing.T) {
	c := newMemoryTestCoordinator(t)
	content := "# 常見模式\n" +
		"- [2026-07-13] newest lesson\n" +
		"- [2026-07-13] second newest\n" +
		"- [2026-07-12] third newest\n" +
		"- [2026-07-12] oldest lesson\n"
	if err := SaveLTM(c.session.Workspace, c.session.Config.Name, content); err != nil {
		t.Fatalf("SaveLTM: %v", err)
	}

	got := c.buildMemorySuffix("worker")
	if !strings.Contains(got, "newest lesson") {
		t.Errorf("buildMemorySuffix() missing the most recent LTM entry:\n%s", got)
	}
	if strings.Contains(got, "oldest lesson") {
		t.Errorf("buildMemorySuffix() should have dropped the oldest LTM entry:\n%s", got)
	}
}
