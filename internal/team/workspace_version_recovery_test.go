package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

var errSimulatedCrash = errors.New("simulated crash")

// crashAt makes the named stage fail once, simulating a process that dies
// right after that stage.
func crashAt(t *testing.T, stage string) {
	t.Helper()
	previous := workspaceVersionStageHook
	workspaceVersionStageHook = func(got string) error {
		if got == stage {
			return errSimulatedCrash
		}
		return nil
	}
	t.Cleanup(func() { workspaceVersionStageHook = previous })
}

func noCrash() { workspaceVersionStageHook = nil }

func recoverFixture(t *testing.T, f *versionFixture) WorkspaceRecoveryReport {
	t.Helper()
	noCrash()
	f.reopen()
	report, err := RecoverWorkspaceOperations(context.Background(), f.ops())
	if err != nil {
		t.Fatalf("RecoverWorkspaceOperations: %v", err)
	}
	if incomplete, _ := f.store.IncompleteOperations(context.Background()); len(incomplete) != 0 {
		t.Fatalf("operations left incomplete: %#v", incomplete)
	}
	return report
}

// TestWorkspaceCheckoutCrashRecovery covers IT8: a checkout that dies after
// materialization is completed forward on the next start.
func TestWorkspaceCheckoutCrashRecovery(t *testing.T) {
	f := newVersionFixture(t)
	f.write("a.txt", "main")
	mustFork(t, f, "exp", "")
	f.write("a.txt", "exp")
	mustCheckout(t, f, "main")

	crashAt(t, "checkout:materialized")
	if _, err := f.ops().Checkout(context.Background(), "exp"); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("err = %v, want the simulated crash", err)
	}
	if f.read("a.txt") != "exp" {
		t.Fatal("files were not materialized before the crash point")
	}
	if tree, _ := LoadSessionTree(f.control); tree.ActiveBranch != "main" {
		t.Fatalf("active branch saved before the crash point: %s", tree.ActiveBranch)
	}
	report := recoverFixture(t, f)
	if len(report.Completed) != 1 || f.tree.ActiveBranch != "exp" || f.read("a.txt") != "exp" {
		t.Fatalf("report = %#v active = %s", report, f.tree.ActiveBranch)
	}
	state, _, _ := f.store.GetSubjectState(context.Background(), f.subject)
	if state.MaterializedBranchID != "exp" {
		t.Fatalf("materialized projection = %#v", state)
	}
}

// TestWorkspaceForkCrashRecovery covers IT9 and the §18.3 table.
func TestWorkspaceForkCrashRecovery(t *testing.T) {
	tests := []struct {
		name        string
		stage       string
		wantRemoved bool
		wantActive  string
	}{
		{name: "before the child commit event", stage: "fork:child_saved", wantRemoved: true, wantActive: "main"},
		{name: "after the child commit event", stage: "fork:event_committed", wantActive: "exp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVersionFixture(t)
			f.write("a.txt", "main")
			crashAt(t, tt.stage)
			if _, err := f.ops().Fork(context.Background(), "exp", ""); !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("err = %v, want the simulated crash", err)
			}
			report := recoverFixture(t, f)
			_, exists := f.tree.Branches["exp"]
			if exists == tt.wantRemoved || f.tree.ActiveBranch != tt.wantActive {
				t.Fatalf("exp exists=%v active=%s report=%#v", exists, f.tree.ActiveBranch, report)
			}
			if tt.wantRemoved {
				if len(report.Removed) != 1 || len(report.Failed) != 1 {
					t.Fatalf("report = %#v", report)
				}
				// The fork can simply be run again.
				mustFork(t, f, "exp", "")
				return
			}
			if f.head("exp").RootTreeHash != f.head("main").RootTreeHash {
				t.Fatal("recovered child does not share the parent tree")
			}
		})
	}
}

// TestWorkspaceHistoricalForkCrashAfterEventMaterializes: a historical fork
// that dies after its commit event is completed forward, including the
// materialization of the older snapshot.
func TestWorkspaceHistoricalForkCrashAfterEventMaterializes(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("v.txt", "old")
	if _, _, err := f.ops().SnapshotNow(ctx); err != nil {
		t.Fatal(err)
	}
	marker := f.event("main", "test_marker")
	f.write("v.txt", "new")
	crashAt(t, "fork:event_committed")
	if _, err := f.ops().Fork(ctx, "past", marker); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("err = %v", err)
	}
	recoverFixture(t, f)
	if f.tree.ActiveBranch != "past" || f.read("v.txt") != "old" {
		t.Fatalf("active = %s, v.txt = %q", f.tree.ActiveBranch, f.read("v.txt"))
	}
}

// TestWorkspaceRestoreCrashRecovery: a restore whose node is committed is
// materialized on recovery.
func TestWorkspaceRestoreCrashRecovery(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.txt", "v1")
	first, _, err := f.ops().SnapshotNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.write("a.txt", "v2")
	crashAt(t, "restore:event_committed")
	if _, _, err = f.ops().Restore(ctx, first.ID); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("err = %v", err)
	}
	if f.read("a.txt") != "v2" {
		t.Fatal("restore materialized before its crash point")
	}
	report := recoverFixture(t, f)
	if len(report.Completed) != 1 || f.read("a.txt") != "v1" || f.head("main").Reason != versionstore.SnapshotRestore {
		t.Fatalf("report = %#v a.txt = %q", report, f.read("a.txt"))
	}
}

func hashTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		rel, _ := filepath.Rel(root, path)
		lines = append(lines, rel+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// TestWorkspaceCheckoutLeavesGitAlone covers IT5: hufu never runs a Git
// command that changes HEAD, the branch, or the index, and never writes .git.
func TestWorkspaceCheckoutLeavesGitAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary unavailable")
	}
	f := newVersionFixture(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = f.subject
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "trunk")
	f.write("base.txt", "base")
	git("add", "base.txt")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "feature")
	f.write("X.txt", "staged")
	git("add", "X.txt")
	f.write("Y.txt", "dirty")

	gitBefore := hashTree(t, filepath.Join(f.subject, ".git"))
	headBefore, branchBefore := git("rev-parse", "HEAD"), git("branch", "--show-current")

	mustFork(t, f, "exp", "")
	f.write("Y.txt", "dirty on exp")
	mustCheckout(t, f, "main")
	mustCheckout(t, f, "exp")

	if got := hashTree(t, filepath.Join(f.subject, ".git")); got != gitBefore {
		t.Fatal(".git bytes changed")
	}
	if git("rev-parse", "HEAD") != headBefore || git("branch", "--show-current") != branchBefore {
		t.Fatal("Git HEAD or branch changed")
	}
	if staged := git("diff", "--cached", "--name-only"); staged != "X.txt" {
		t.Fatalf("index changed: staged = %q", staged)
	}
	if f.read("Y.txt") != "dirty on exp" {
		t.Fatal("working tree was not versioned")
	}
}
