package team

import (
	"context"
	"sync"
	"testing"
)

func TestCheckDuplicateTasks_SameBatch(t *testing.T) {
	c := &Coordinator{
		delegatedTasks:   make(map[string]int),
		delegatedTasksMu: sync.Mutex{},
		taskTracker:      NewTaskTracker(),
	}

	tests := []struct {
		name             string
		previousTasks    map[string]int
		batchTasks       []TaskDef
		expectWarnings   bool
		expectedDupCount int
		warningContains  string
	}{
		{
			name:          "two identical tasks in batch - first should not be duplicate",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
			},
			expectWarnings:   true,
			expectedDupCount: 1,
			warningContains:  "in batch",
		},
		{
			name:          "three identical tasks in batch - first two should not be duplicate",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
			},
			expectWarnings:   true,
			expectedDupCount: 2,
			warningContains:  "in batch",
		},
		{
			name:          "identical task was already delegated in previous round",
			previousTasks: map[string]int{"agent1:task1": 1},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
			},
			expectWarnings:   true,
			expectedDupCount: 1,
			warningContains:  "EXACT DUPLICATE:",
		},
		{
			name:          "different tasks for same agent - no duplicates",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task2"},
			},
			expectWarnings:   false,
			expectedDupCount: 0,
		},
		{
			name:          "same task different agents - no duplicates",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent2", Goal: "task1"},
			},
			expectWarnings:   false,
			expectedDupCount: 0,
		},
		{
			name:          "batch duplicate with constraints - should detect",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1", Constraints: "use python"},
				{Agent: "agent1", Goal: "task1", Constraints: "use python"},
			},
			expectWarnings:   true,
			expectedDupCount: 1,
			warningContains:  "in batch",
		},
		{
			name:          "mixed: one previous round dup, one in-batch dup",
			previousTasks: map[string]int{"agent1:task1": 1},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
			},
			expectWarnings:   true,
			expectedDupCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.delegatedTasks = make(map[string]int)
			for k, v := range tt.previousTasks {
				c.delegatedTasks[k] = v
			}

			warnings, duplicates, suppressed := c.checkDuplicateTasks(context.Background(), tt.batchTasks)

			if tt.expectWarnings && len(warnings) == 0 {
				t.Errorf("expected warnings but got none")
			}
			if !tt.expectWarnings && len(warnings) > 0 {
				t.Errorf("expected no warnings but got %d: %v", len(warnings), warnings)
			}

			actualDupCount := 0
			for _, isDup := range duplicates {
				if isDup {
					actualDupCount++
				}
			}
			if actualDupCount != tt.expectedDupCount {
				t.Errorf("duplicate count = %d, want %d", actualDupCount, tt.expectedDupCount)
			}
			if len(suppressed) != 0 {
				t.Errorf("expected no suppressed duplicates, got %d", len(suppressed))
			}

			if tt.warningContains != "" && len(warnings) > 0 {
				found := false
				for _, w := range warnings {
					if containsString(w, tt.warningContains) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("warning should contain %q, got %v", tt.warningContains, warnings)
				}
			}
		})
	}
}

func TestCheckDuplicateTasks_SuppressesExistingActiveTodo(t *testing.T) {
	c := &Coordinator{
		delegatedTasks:   make(map[string]int),
		delegatedTasksMu: sync.Mutex{},
		taskTracker:      NewTaskTracker(),
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "helper",
		Desc:  "Investigate DHCP on enp4s0",
	}})
	c.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskInProgress, "")

	warnings, duplicates, suppressed := c.checkDuplicateTasks(context.Background(), []TaskDef{{
		Agent: "helper",
		Goal:  "Investigate DHCP on enp4s0",
	}})

	if !duplicates[0] {
		t.Fatal("expected duplicate to be suppressed")
	}
	if suppressed[0] == nil || suppressed[0].Item.ID != items[0].ID {
		t.Fatalf("expected suppression to reference existing task %s", items[0].ID)
	}
	if len(warnings) == 0 || !containsString(warnings[0], "SUPPRESSED DUPLICATE") {
		t.Fatalf("expected suppression warning, got %v", warnings)
	}
}

func TestCheckDuplicateTasks_SuppressesPermissionBlockedFailure(t *testing.T) {
	c := &Coordinator{
		delegatedTasks:   make(map[string]int),
		delegatedTasksMu: sync.Mutex{},
		taskTracker:      NewTaskTracker(),
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "helper",
		Desc:  "Run bash diagnostic command",
	}})
	c.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskError, "tool 'bash' is not permitted. Add 'bash' to tools.allowed in team.yaml to enable.")

	warnings, duplicates, suppressed := c.checkDuplicateTasks(context.Background(), []TaskDef{{
		Agent: "helper",
		Goal:  "Run bash diagnostic command",
	}})

	if !duplicates[0] {
		t.Fatal("expected blocked failure to suppress duplicate task")
	}
	if suppressed[0] == nil || suppressed[0].Item.ID != items[0].ID {
		t.Fatalf("expected suppression to reference existing task %s", items[0].ID)
	}
	if len(warnings) == 0 || !containsString(warnings[0], "SUPPRESSED DUPLICATE") {
		t.Fatalf("expected suppression warning, got %v", warnings)
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
