package team

import (
	json "encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestRetiredDistillationStreamsLargeHistoryAndRejectsCorruption(t *testing.T) {
	workspace := t.TempDir()
	sd := NewSession()
	sd.Tasks = []*TodoItem{{ID: "1", Agent: "worker", Status: TaskDone, Desc: "finished", Output: "verified-rule-sentinel"}}
	if err := SaveSession(workspace, sd); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	payload, err := json.Marshal(map[string]string{"id": "1", "status": "in_progress", "desc": "finished", "output": strings.Repeat("x", 1<<20)})
	if err != nil {
		t.Fatal(err)
	}
	for range 9 {
		if _, err = es.AppendPersisted(RunEvent{Type: "task_started", TaskID: "1", Actor: "runtime", Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = es.AppendPersisted(RunEvent{Type: "task_completed", TaskID: "1", Actor: "runtime", Payload: []byte(`{"id":"1","status":"done","desc":"finished","agent":"worker","output":"verified-rule-sentinel"}`)}); err != nil {
		t.Fatal(err)
	}
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	before, err := SnapshotRetiredWorkspace(t.Context(), workspace, scope)
	if err != nil || !strings.Contains(before.Content, "verified-rule-sentinel") || len(before.Content) > 4096 {
		t.Fatalf("snapshot bytes=%d err=%v", len(before.Content), err)
	}
	if _, err = es.AppendPersisted(RunEvent{Type: "model_diagnostic", Actor: "runtime", Payload: []byte(`{"diagnostic":"changed"}`)}); err != nil {
		t.Fatal(err)
	}
	after, err := SnapshotRetiredWorkspace(t.Context(), workspace, scope)
	if err != nil || before.Fingerprint == after.Fingerprint {
		t.Fatalf("historical evidence change was not bound: %v", err)
	}
	store, err := runtimestore.Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err = store.DB().ExecContext(t.Context(), `UPDATE events SET payload = ? WHERE sequence = 1`, []byte(`{"diagnostic":"tampered"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = SnapshotRetiredWorkspace(t.Context(), workspace, scope); err == nil {
		t.Fatal("corrupted historical payload authorized distillation")
	}
}

func TestRetiredDistillationDistinguishesHistoricalFailuresFromCurrentWork(t *testing.T) {
	workspace := t.TempDir()
	sd := NewSession()
	sd.Tasks = []*TodoItem{{ID: "current", Agent: "worker", Status: TaskDone, Desc: "finished"}}
	if err := SaveSession(workspace, sd); err != nil {
		t.Fatal(err)
	}
	st := NewSessionTree()
	st.Branches["main"].State.TaskPlan = []*TodoItem{{ID: "current", Status: TaskPending}}
	st.Branches["historical"] = &SessionBranch{ID: "historical", Name: "historical", State: BranchState{TaskPlan: []*TodoItem{{ID: "current", Status: TaskError}}}}
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	for _, id := range []string{"historical", "current"} {
		payload, err := json.Marshal(map[string]string{"id": id, "status": "error", "desc": "failed check", "output": "historical-diagnostic-sentinel"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = es.AppendPersisted(RunEvent{Type: "task_failed", TaskID: id, Actor: "runtime", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := SnapshotRetiredWorkspace(t.Context(), workspace, contextstore.Scope{ProjectID: "project", TeamID: "team"})
		if err != nil || snapshot.Protected != (id == "current") || !strings.Contains(snapshot.Content, "historical-diagnostic-sentinel") {
			t.Fatalf("id=%s protected=%v err=%v", id, snapshot.Protected, err)
		}
	}
}

func TestRetiredDistillationSnapshotPreservesMemoryScopeAndDetectsChanges(t *testing.T) {
	for _, mode := range []string{"shared", "session", "branch", "task", "attempt", "private", "expiring", "pinned"} {
		t.Run(mode, func(t *testing.T) {
			protected := mode == "private" || mode == "expiring" || mode == "pinned"
			workspace := t.TempDir()
			sd := NewSession()
			sd.Tasks = []*TodoItem{{ID: "1", Agent: "worker", Status: TaskDone, Desc: "finished", Output: "useful result", Goal: "goal-sentinel", Constraints: "constraint-sentinel", VerifyMode: "observation", VerifyResult: &VerificationResult{Command: "observed-check", ExitCode: 2, Stdout: "diagnostic-sentinel"}}}
			if err := SaveSession(workspace, sd); err != nil {
				t.Fatal(err)
			}
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
			switch mode {
			case "private":
				scope.AgentID = "private-agent"
			case "session":
				scope.SessionID = "old-session"
			case "branch":
				scope.BranchID = "old-branch"
			case "task":
				scope.TaskID = "old-task"
			case "attempt":
				scope.AttemptID = "old-attempt"
			}
			item := contextstore.ContextItem{ID: "memory", Kind: contextstore.ContextPattern, Content: "memory-sentinel", Scope: scope, Lifecycle: contextstore.LifecycleConfirmed, Confidence: 1}
			if mode == "expiring" {
				item.ExpiresAt = new(time.Now().Add(24 * time.Hour))
			}
			if mode == "pinned" {
				item.Pinned = true
			}
			if err = repo.Append(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			_ = repo.Close()
			snapshot, err := SnapshotRetiredWorkspace(t.Context(), workspace, contextstore.Scope{ProjectID: "project", TeamID: "team"})
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Protected != protected {
				t.Fatalf("protected=%v mode=%s", snapshot.Protected, mode)
			}
			for _, value := range []string{"goal-sentinel", "constraint-sentinel", "diagnostic-sentinel", `"exit_code":2`} {
				if !strings.Contains(snapshot.Content, value) {
					t.Fatalf("model source omitted canonical task evidence %s", value)
				}
			}
			if strings.Contains(snapshot.Content, "memory-sentinel") == protected {
				t.Fatal("private memory leaked or shared memory was omitted")
			}
			sd.Tasks[0].Output = "new evidence"
			if err = SaveSession(workspace, sd); err != nil {
				t.Fatal(err)
			}
			changed, err := SnapshotRetiredWorkspace(t.Context(), workspace, contextstore.Scope{ProjectID: "project", TeamID: "team"})
			if err != nil {
				t.Fatal(err)
			}
			if changed.Fingerprint == snapshot.Fingerprint {
				t.Fatal("snapshot failed to bind source revision")
			}
		})
	}
}
