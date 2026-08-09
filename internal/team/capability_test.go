package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
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
	var failureEvents []StatusEvent
	c.SetStatusReporter(func(event StatusEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event.Type)
		if event.Type == "failure" {
			failureEvents = append(failureEvents, event)
		}
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
	if items[0].FailureEvent == nil || items[0].FailureEvent.FailureClass != FailurePolicy || items[0].FailureEvent.Phase == "" || items[0].FailureEvent.RetryDisposition != NeedsHuman {
		t.Fatalf("capability preflight failure event = %#v", items[0].FailureEvent)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, eventType := range events {
		if eventType == "start" {
			t.Fatal("capability failure should stop before worker start")
		}
	}
	if len(failureEvents) != 1 || failureEvents[0].TodoID != items[0].ID {
		t.Fatalf("structured capability failure reporter events = %#v", failureEvents)
	}
	if payload, ok := failureEvents[0].Data["failure_event"].(*FailureEventPayload); !ok || payload.FailureClass != FailurePolicy {
		t.Fatalf("reporter failure payload = %#v", failureEvents[0].Data)
	}
}

func TestExecuteTasksUnknownCapabilityPersistsStructuredFailureEvent(t *testing.T) {
	c := newCapabilityCoordinator(t, nil)
	var failureEvent StatusEvent
	c.SetStatusReporter(func(event StatusEvent) {
		if event.Type == "failure" {
			failureEvent = event
		}
	})
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "unknown capability", Requires: []string{"missing-capability"}}})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("unknown capability execution error = %v, want terminal task failure", err)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskBlocked {
		t.Fatalf("unknown capability items = %+v", items)
	}
	event := items[0].FailureEvent
	if event == nil || event.FailureClass != FailurePolicy || event.Phase == "" || event.RetryDisposition != NeedsHuman {
		t.Fatalf("unknown capability failure event = %#v", event)
	}
	if failureEvent.TodoID != items[0].ID {
		t.Fatalf("unknown capability reporter event = %#v", failureEvent)
	}
}

func TestRunAgentsToolCapabilityReferencesAreSchemaBoundAndRejectedBeforeTodoCreation(t *testing.T) {
	tests := []struct {
		name          string
		preflight     []agent.CapabilityRequirement
		wantRequires  bool
		wantEnum      []string
		invalidInput  string
		wantErrorPart string
	}{
		{
			name:          "no configured capabilities omits requires and guides interactive work",
			invalidInput:  `{"tasks":[{"agent":"worker","goal":"prepare environment","requires":["interactive"]}]}`,
			wantErrorPart: "execution.kind=interactive",
		},
		{
			name: "configured capabilities become an enum",
			preflight: []agent.CapabilityRequirement{
				{Name: "ssh-ready", Probe: "true"},
				{Name: "libvirt", Probe: "true"},
			},
			wantRequires:  true,
			wantEnum:      []string{"libvirt", "ssh-ready"},
			invalidInput:  `{"tasks":[{"agent":"worker","goal":"prepare environment","requires":["interactive"]}]}`,
			wantErrorPart: "valid names are: libvirt, ssh-ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCapabilityCoordinator(t, tt.preflight)
			tool := &runAgentsTool{coordinator: c}
			props := tool.Info().Parameters["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
			requires, hasRequires := props["requires"]
			if hasRequires != tt.wantRequires {
				t.Fatalf("requires present = %t, want %t", hasRequires, tt.wantRequires)
			}
			if tt.wantRequires {
				items := requires.(map[string]any)["items"].(map[string]any)
				enum := items["enum"].([]string)
				if strings.Join(enum, ",") != strings.Join(tt.wantEnum, ",") {
					t.Fatalf("requires enum = %v, want %v", enum, tt.wantEnum)
				}
			}

			response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: tt.invalidInput})
			if err != nil {
				t.Fatalf("tool run returned transport error: %v", err)
			}
			if !response.IsError || !strings.Contains(response.Content, tt.wantErrorPart) {
				t.Fatalf("invalid capability response = %#v, want error containing %q", response, tt.wantErrorPart)
			}
			if items := c.taskTracker.TodoList().Items(); len(items) != 0 {
				t.Fatalf("invalid coordinator input created todo items: %#v", items)
			}
		})
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
