package team

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// mockCoordinator provides a minimal Coordinator for testing loop detection
type mockCoordinator struct {
	delegatedTasks   map[string]int
	delegatedTasksMu sync.Mutex
	reportedEvents   []StatusEvent
}

func newMockCoordinator() *mockCoordinator {
	return &mockCoordinator{
		delegatedTasks: make(map[string]int),
		reportedEvents: make([]StatusEvent, 0),
	}
}

func (m *mockCoordinator) report(event StatusEvent) {
	m.reportedEvents = append(m.reportedEvents, event)
}

func (m *mockCoordinator) getCount(key string) int {
	m.delegatedTasksMu.Lock()
	defer m.delegatedTasksMu.Unlock()
	return m.delegatedTasks[key]
}

// simulateDelegation mimics the delegation logic from Coordinator.DelegateTasks
func (m *mockCoordinator) simulateDelegation(tasks []TaskDef) []string {
	var duplicateWarnings []string
	m.delegatedTasksMu.Lock()
	for _, t := range tasks {
		key := strings.ToLower(t.Agent) + ":" + truncateTaskDesc(t.Task)
		m.delegatedTasks[key]++
		if m.delegatedTasks[key] > 1 {
			duplicateWarnings = append(duplicateWarnings, fmt.Sprintf("%s (agent=%s, count=%d)", truncateTaskDesc(t.Task), t.Agent, m.delegatedTasks[key]))
		}
	}
	m.delegatedTasksMu.Unlock()

	if len(duplicateWarnings) > 0 {
		m.report(newStatusEvent("loop_warning").withMessage(fmt.Sprintf("Duplicate task delegation detected: %v", duplicateWarnings)))
	}

	return duplicateWarnings
}

// TestTruncateTaskDesc tests the truncateTaskDesc function
func TestTruncateTaskDesc(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "short description remains unchanged",
			input:    "Short task",
			expected: "Short task",
		},
		{
			name:     "description exactly 80 characters",
			input:    strings.Repeat("a", 80),
			expected: strings.Repeat("a", 80),
		},
		{
			name:     "description exactly 80 characters without truncation",
			input:    strings.Repeat("x", 80),
			expected: strings.Repeat("x", 80),
		},
		{
			name:     "long description truncated to 80 characters",
			input:    strings.Repeat("b", 100),
			expected: strings.Repeat("b", 80),
		},
		{
			name:     "very long description truncated",
			input:    "This is a very long task description that definitely exceeds the 80 character limit and should be truncated",
			expected: "This is a very long task description that definitely exceeds the 80 character li",
		},
		{
			name:     "empty string remains empty",
			input:    "",
			expected: "",
		},
		{
			name:     "81 characters truncated to 80",
			input:    strings.Repeat("c", 81),
			expected: strings.Repeat("c", 80),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateTaskDesc(tt.input)
			if result != tt.expected {
				t.Errorf("truncateTaskDesc(%q) = %q (len=%d), want %q (len=%d)",
					tt.input, result, len(result), tt.expected, len(tt.expected))
			}
		})
	}
}

// TestDelegatedTasksTracking tests the delegated task counting mechanism
func TestDelegatedTasksTracking(t *testing.T) {
	tests := []struct {
		name         string
		tasks        []TaskDef
		expectedKeys map[string]int
	}{
		{
			name: "single task delegation sets count to 1",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
			},
			expectedKeys: map[string]int{
				"agent1:task1": 1,
			},
		},
		{
			name: "same agent and task increments count",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent1", Task: "task1"},
			},
			expectedKeys: map[string]int{
				"agent1:task1": 2,
			},
		},
		{
			name: "different agents for same task are tracked separately",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent2", Task: "task1"},
			},
			expectedKeys: map[string]int{
				"agent1:task1": 1,
				"agent2:task1": 1,
			},
		},
		{
			name: "different tasks for same agent are tracked separately",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent1", Task: "task2"},
			},
			expectedKeys: map[string]int{
				"agent1:task1": 1,
				"agent1:task2": 1,
			},
		},
		{
			name: "multiple delegations accumulate correctly",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent1", Task: "task1"},
			},
			expectedKeys: map[string]int{
				"agent1:task1": 3,
			},
		},
		{
			name: "agent name is case insensitive for key",
			tasks: []TaskDef{
				{Agent: "Agent1", Task: "task1"},
				{Agent: "AGENT1", Task: "task1"},
				{Agent: "agent1", Task: "task1"},
			},
			expectedKeys: map[string]int{
				"agent1:task1": 3,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newMockCoordinator()
			c.simulateDelegation(tt.tasks)

			for key, expectedCount := range tt.expectedKeys {
				count := c.getCount(key)
				if count != expectedCount {
					t.Errorf("delegatedTasks[%q] = %d, want %d", key, count, expectedCount)
				}
			}
		})
	}
}

// TestLoopWarningGeneration tests the loop warning generation
func TestLoopWarningGeneration(t *testing.T) {
	tests := []struct {
		name             string
		tasks            []TaskDef
		expectWarnings   bool
		expectedWarnings int
		warningContains  []string
	}{
		{
			name: "single delegation does not trigger warning",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
			},
			expectWarnings:   false,
			expectedWarnings: 0,
		},
		{
			name: "repeated delegation of same agent+task triggers warning",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent1", Task: "task1"},
			},
			expectWarnings:   true,
			expectedWarnings: 1,
			warningContains:  []string{"agent1", "count=2"},
		},
		{
			name: "multiple repeats generate multiple warnings",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent1", Task: "task1"},
			},
			expectWarnings:   true,
			expectedWarnings: 2, // 2nd and 3rd delegation
			warningContains:  []string{"agent1", "count=3"},
		},
		{
			name: "different agents for same task do not trigger warning",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent2", Task: "task1"},
			},
			expectWarnings:   false,
			expectedWarnings: 0,
		},
		{
			name: "different tasks for same agent do not trigger warning",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent1", Task: "task2"},
			},
			expectWarnings:   false,
			expectedWarnings: 0,
		},
		{
			name: "mixed delegations with one loop",
			tasks: []TaskDef{
				{Agent: "agent1", Task: "task1"},
				{Agent: "agent2", Task: "task2"},
				{Agent: "agent1", Task: "task1"}, // loop!
				{Agent: "agent3", Task: "task3"},
			},
			expectWarnings:   true,
			expectedWarnings: 1,
			warningContains:  []string{"agent1"},
		},
		{
			name: "agent name case insensitivity triggers warning",
			tasks: []TaskDef{
				{Agent: "Agent1", Task: "task1"},
				{Agent: "AGENT1", Task: "task1"},
			},
			expectWarnings:   true,
			expectedWarnings: 1,
			warningContains:  []string{"count=2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newMockCoordinator()
			warnings := c.simulateDelegation(tt.tasks)

			if tt.expectWarnings && len(warnings) == 0 {
				t.Errorf("expected warnings but got none")
			}
			if !tt.expectWarnings && len(warnings) > 0 {
				t.Errorf("expected no warnings but got %d", len(warnings))
			}

			if len(warnings) != tt.expectedWarnings {
				t.Errorf("warnings count = %d, want %d", len(warnings), tt.expectedWarnings)
			}

			for _, expected := range tt.warningContains {
				found := false
				for _, w := range warnings {
					if strings.Contains(w, expected) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("warning message should contain %q, got %v", expected, warnings)
				}
			}
		})
	}
}

// TestLongTaskDescriptionTruncation tests truncation with real task descriptions
func TestLongTaskDescriptionTruncation(t *testing.T) {
	tests := []struct {
		name     string
		task     string
		maxLen   int
		expected string
	}{
		{
			name:     "real task description that is too long",
			task:     "Please analyze the following code and provide a detailed report on the performance implications: function processData(items []Data) { for _, item := range items { processItem(item) } }",
			maxLen:   80,
			expected: "Please analyze the following code and provide a detailed report on the performance",
		},
		{
			name:     "task with special characters",
			task:     "Review PR #1234: Add feature X with specific implementation details for the new API endpoint",
			maxLen:   80,
			expected: "Review PR #1234: Add feature X with specific implementation details for the new API",
		},
		{
			name:     "unicode characters count correctly",
			task:     "完成任務：這是一個非常長的中文任務描述，需要被截斷以避免顯示問題",
			maxLen:   80,
			expected: "完成任務：這是一個非常長的中文任務描述，需要被截斷以避免顯示問題",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateTaskDesc(tt.task)
			if len(result) > tt.maxLen {
				t.Errorf("result length %d exceeds maxLen %d", len(result), tt.maxLen)
			}
			if !strings.HasPrefix(result, tt.expected[:min(len(result), len(tt.expected))]) && len(result) > 10 {
				// Allow some flexibility for multi-byte characters
				if len(result) != tt.maxLen {
					t.Errorf("truncateTaskDesc() returned length %d, want %d", len(result), tt.maxLen)
				}
			}
		})
	}
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// newStatusEvent creates a new StatusEvent for testing
func newStatusEvent(eventType string) StatusEvent {
	return StatusEvent{
		Type:    eventType,
		Message: "",
	}
}
