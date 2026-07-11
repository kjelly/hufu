package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func newCapabilityCoordinator(t *testing.T, preflight []agent.CapabilityRequirement) *Coordinator {
	t.Helper()
	ws := t.TempDir()
	return &Coordinator{
		session: &TeamSession{
			Workspace: ws,
			Config: agent.TeamConfig{
				Name:      "test",
				Shell:     "sh",
				Preflight: preflight,
			},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker"},
			},
		},
		projectDir:         ws,
		taskTracker:        NewTaskTracker(),
		reportStatus:       func(StatusEvent) {},
		delegatedTasks:     make(map[string]int),
		taskResultCache:    make(map[string][]cachedTaskEntry),
		capabilityCache:    make(map[string]CapabilityResult),
		capabilityInflight: make(map[string]chan CapabilityResult),
	}
}

func TestCheckCapabilityRequirements_CachesSuccessfulProbe(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "ready.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: ws,
			Config: agent.TeamConfig{
				Name:  "test",
				Shell: "sh",
			},
		},
		projectDir:         ws,
		delegatedTasks:     make(map[string]int),
		capabilityCache:    make(map[string]CapabilityResult),
		capabilityInflight: make(map[string]chan CapabilityResult),
	}

	req := agent.CapabilityRequirement{Name: "workspace-ready", Probe: "test -f ready.txt", Timeout: 2}
	results, err := c.checkCapabilityRequirements(context.Background(), []agent.CapabilityRequirement{req})
	if err != nil {
		t.Fatalf("first probe should succeed, got %v", err)
	}
	if len(results) != 1 || !results[0].Available {
		t.Fatalf("unexpected probe result: %#v", results)
	}

	if err := os.Remove(filepath.Join(ws, "ready.txt")); err != nil {
		t.Fatal(err)
	}
	results, err = c.checkCapabilityRequirements(context.Background(), []agent.CapabilityRequirement{req})
	if err != nil {
		t.Fatalf("cached probe should still succeed, got %v", err)
	}
	if len(results) != 1 || !results[0].Available {
		t.Fatalf("unexpected cached probe result: %#v", results)
	}
}

func TestExecuteTasksBlocksOnMissingCapability(t *testing.T) {
	c := newCapabilityCoordinator(t, []agent.CapabilityRequirement{
		{Name: "workspace-ready", Probe: "test -f ready.txt", Timeout: 2},
	})
	var mu sync.Mutex
	var events []string
	c.SetStatusReporter(func(event StatusEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event.Type)
	})

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "worker", Goal: "check environment", Requires: []string{"workspace-ready"}},
	})
	if err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("expected capability failure, got %v", err)
	}

	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("expected 1 todo item, got %d", len(items))
	}
	if items[0].Status != TaskBlocked {
		t.Fatalf("expected task to be blocked, got %s", items[0].Status)
	}
	if items[0].Detail == "" {
		t.Fatal("expected blocked task detail to explain the missing capability")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, eventType := range events {
		if eventType == "start" {
			t.Fatal("capability failure should stop before worker start")
		}
	}
}

func TestCapabilityBlockedErrorHelper(t *testing.T) {
	res := CapabilityResult{Name: "libvirt", Reason: "capability \"libvirt\" unavailable"}
	if _, ok := isCapabilityBlockedError(capabilityBlockedError{Result: res}); !ok {
		t.Fatal("expected capability blocked error to be recognized")
	}
	if _, ok := isCapabilityBlockedError(errors.New("boom")); ok {
		t.Fatal("non-capability errors should not be recognized")
	}
}
