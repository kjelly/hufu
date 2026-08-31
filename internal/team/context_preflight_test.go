package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestPrepareContextPreflightRejectsReentryWithoutBlocking(t *testing.T) {
	workspace := t.TempDir()
	tree := NewSessionTree()
	branch, err := tree.CreateRootBranch("existing-branch")
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = branch.ID
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	c, err := NewCoordinator(&TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "preflight"}}, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseContextPreflight()
	if err := c.PrepareContextPreflight(); err != nil {
		t.Fatal(err)
	}
	reentryResult := make(chan error, 1)
	go func() { reentryResult <- c.PrepareContextPreflight() }()
	var reentryErr error
	select {
	case reentryErr = <-reentryResult:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("nested context preflight blocked on the active invocation lease")
	}
	if _, ok := errors.AsType[*ContextPreflightReentryError](reentryErr); !ok {
		t.Fatalf("nested context preflight error = %v, want typed reentry error", reentryErr)
	}
	after, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveBranch != branch.ID || len(after.Branches) != len(tree.Branches) {
		t.Fatalf("preflight changed session tree: active=%q branches=%d, want active=%q branches=%d", after.ActiveBranch, len(after.Branches), branch.ID, len(tree.Branches))
	}
	for _, path := range []string{filepath.Join(workspace, "context.sqlite"), filepath.Join(workspace, "logs", "event_store.jsonl")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preflight did not create %s: %v", path, err)
		}
	}
	if _, err := c.prepareAuxiliaryPrompt(context.Background(), "team_selection", "choose a team"); err != nil {
		t.Fatal(err)
	}
	if len(c.sessionData.CoordinatorContextManifests) != 1 || c.sessionData.CoordinatorContextManifests[0].RunID != c.executionRunID {
		t.Fatalf("preflight manifest/run identity mismatch: %#v / %q", c.sessionData.CoordinatorContextManifests, c.executionRunID)
	}
	events, err := os.ReadFile(filepath.Join(workspace, "logs", "event_store.jsonl"))
	if err != nil || !strings.Contains(string(events), `"type":"context_manifest"`) || !strings.Contains(string(events), c.executionRunID) {
		t.Fatalf("preflight context manifest was not replayable: %v: %s", err, events)
	}
}
