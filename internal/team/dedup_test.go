package team

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTaskDefUnmarshalJSON tests backward compatibility with legacy "task" field
func TestTaskDefUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		jsonData string
		wantGoal string
	}{
		{
			name:     "legacy task field maps to Goal",
			jsonData: `{"task": "do something", "agent": "dev"}`,
			wantGoal: "do something",
		},
		{
			name:     "goal field takes precedence over task",
			jsonData: `{"task": "old task", "goal": "new goal", "agent": "dev"}`,
			wantGoal: "new goal",
		},
		{
			name:     "goal field works without task",
			jsonData: `{"goal": "just goal", "agent": "dev"}`,
			wantGoal: "just goal",
		},
		{
			name:     "empty task and goal",
			jsonData: `{"agent": "dev"}`,
			wantGoal: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var task TaskDef
			err := task.UnmarshalJSON([]byte(tt.jsonData))
			if err != nil {
				t.Fatalf("UnmarshalJSON() error = %v", err)
			}
			if task.Goal != tt.wantGoal {
				t.Errorf("Goal = %q, want %q", task.Goal, tt.wantGoal)
			}
		})
	}
}

// TestTaskDefUnmarshalJSONInvalid tests error handling
func TestTaskDefUnmarshalJSONInvalid(t *testing.T) {
	var task TaskDef
	err := task.UnmarshalJSON([]byte(`{invalid json}`))
	if err == nil {
		t.Error("UnmarshalJSON() should return error for invalid JSON")
	}
}

// TestDetectTaskCycle tests cycle detection in task dependencies
func TestDetectTaskCycle(t *testing.T) {
	tests := []struct {
		name  string
		tasks []TaskDef
		want  bool
	}{
		{
			name: "no dependencies - no cycle",
			tasks: []TaskDef{
				{Goal: "task1"},
				{Goal: "task2"},
			},
			want: false,
		},
		{
			name: "linear dependency - no cycle",
			tasks: []TaskDef{
				{Goal: "task1"},
				{Goal: "task2", DependsOn: []int{0}},
				{Goal: "task3", DependsOn: []int{1}},
			},
			want: false,
		},
		{
			name: "self-loop - cycle detected",
			tasks: []TaskDef{
				{Goal: "task1", DependsOn: []int{0}},
			},
			want: true,
		},
		{
			name: "simple cycle - cycle detected",
			tasks: []TaskDef{
				{Goal: "task1", DependsOn: []int{1}},
				{Goal: "task2", DependsOn: []int{0}},
			},
			want: true,
		},
		{
			name: "three-node cycle - cycle detected",
			tasks: []TaskDef{
				{Goal: "task1", DependsOn: []int{2}},
				{Goal: "task2", DependsOn: []int{0}},
				{Goal: "task3", DependsOn: []int{1}},
			},
			want: true,
		},
		{
			name: "diamond dependency - no cycle",
			tasks: []TaskDef{
				{Goal: "task1"},
				{Goal: "task2", DependsOn: []int{0}},
				{Goal: "task3", DependsOn: []int{0}},
				{Goal: "task4", DependsOn: []int{1, 2}},
			},
			want: false,
		},
		{
			name: "out-of-bounds dependency - ignored",
			tasks: []TaskDef{
				{Goal: "task1", DependsOn: []int{99}},
			},
			want: false,
		},
		{
			name: "negative dependency - ignored",
			tasks: []TaskDef{
				{Goal: "task1", DependsOn: []int{-1}},
			},
			want: false,
		},
		{
			name: "complex graph with cycle",
			tasks: []TaskDef{
				{Goal: "task1"},
				{Goal: "task2", DependsOn: []int{0}},
				{Goal: "task3", DependsOn: []int{1}},
				{Goal: "task4", DependsOn: []int{2, 0}},
				{Goal: "task5", DependsOn: []int{3}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTaskCycle(tt.tasks)
			if got != tt.want {
				t.Errorf("detectTaskCycle() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLookupTaskCache tests the task cache lookup functionality
func TestLookupTaskCache(t *testing.T) {
	tests := []struct {
		name       string
		cacheSetup func() map[string][]cachedTaskEntry
		agentKey   string
		newTask    string
		cacheGen   int64
		wantFound  bool
		wantOutput string
		sidecarHit bool
	}{
		{
			name: "exact match in current generation",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{
					"agent1": {
						{taskDesc: "do something", output: "result1", generation: 1},
					},
				}
			},
			agentKey:   "agent1",
			newTask:    "do something",
			cacheGen:   1,
			wantFound:  true,
			wantOutput: "result1",
		},
		{
			name: "exact match across generations",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{
					"agent1": {
						{taskDesc: "do something", output: "old result", generation: 0},
					},
				}
			},
			agentKey:   "agent1",
			newTask:    "do something",
			cacheGen:   1,
			wantFound:  true,
			wantOutput: "old result",
		},
		{
			name: "no match - empty cache",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{}
			},
			agentKey:  "agent1",
			newTask:   "do something",
			cacheGen:  1,
			wantFound: false,
		},
		{
			name: "no match - different agent",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{
					"agent2": {
						{taskDesc: "do something", output: "result", generation: 1},
					},
				}
			},
			agentKey:  "agent1",
			newTask:   "do something",
			cacheGen:  1,
			wantFound: false,
		},
		{
			name: "no match - different task",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{
					"agent1": {
						{taskDesc: "do something else", output: "result", generation: 1},
					},
				}
			},
			agentKey:  "agent1",
			newTask:   "do something",
			cacheGen:  1,
			wantFound: false,
		},
		{
			name: "normalized match - extra whitespace",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{
					"agent1": {
						{taskDesc: "do  something", output: "result", generation: 1},
					},
				}
			},
			agentKey:   "agent1",
			newTask:    "do something",
			cacheGen:   1,
			wantFound:  true,
			wantOutput: "result",
		},
		{
			name: "normalized match - case insensitive",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{
					"agent1": {
						{taskDesc: "DO SOMETHING", output: "result", generation: 1},
					},
				}
			},
			agentKey:   "agent1",
			newTask:    "do something",
			cacheGen:   1,
			wantFound:  true,
			wantOutput: "result",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Coordinator{
				taskResultCache:   tt.cacheSetup(),
				taskResultCacheMu: sync.RWMutex{},
				cacheGeneration:   atomic.Int64{},
			}
			c.cacheGeneration.Store(tt.cacheGen)

			got, found := c.lookupTaskCache(context.Background(), tt.agentKey, tt.newTask)
			if found != tt.wantFound {
				t.Errorf("lookupTaskCache() found = %v, want %v", found, tt.wantFound)
			}
			if found && got != tt.wantOutput {
				t.Errorf("lookupTaskCache() output = %q, want %q", got, tt.wantOutput)
			}
		})
	}
}

// TestLookupTaskCacheAllGenerations tests semantic duplicate detection
func TestLookupTaskCacheAllGenerations(t *testing.T) {
	tests := []struct {
		name       string
		cacheSetup func() map[string][]cachedTaskEntry
		agentKey   string
		newTask    string
		wantFound  bool
	}{
		{
			name: "exact match across all generations",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{
					"agent1": {
						{taskDesc: "task from gen 0", output: "result0", generation: 0},
						{taskDesc: "task from gen 1", output: "result1", generation: 1},
					},
				}
			},
			agentKey:  "agent1",
			newTask:   "task from gen 0",
			wantFound: true,
		},
		{
			name: "no match - empty cache",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{}
			},
			agentKey:  "agent1",
			newTask:   "do something",
			wantFound: false,
		},
		{
			name: "normalized match - whitespace differences",
			cacheSetup: func() map[string][]cachedTaskEntry {
				return map[string][]cachedTaskEntry{
					"agent1": {
						{taskDesc: "do   something", output: "result", generation: 1},
					},
				}
			},
			agentKey:  "agent1",
			newTask:   "do something",
			wantFound: true,
		},
		{
			name: "limits to last 100 entries",
			cacheSetup: func() map[string][]cachedTaskEntry {
				entries := make([]cachedTaskEntry, 150)
				for i := 0; i < 150; i++ {
					entries[i] = cachedTaskEntry{
						taskDesc:   "task " + string(rune('A'+i%26)),
						output:     "result",
						generation: int64(i / 50),
					}
				}
				// The match should be in the last 100
				entries[149] = cachedTaskEntry{
					taskDesc:   "find me",
					output:     "found!",
					generation: 2,
				}
				return map[string][]cachedTaskEntry{
					"agent1": entries,
				}
			},
			agentKey:  "agent1",
			newTask:   "find me",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Coordinator{
				taskResultCache:   tt.cacheSetup(),
				taskResultCacheMu: sync.RWMutex{},
			}

			gotOutput, gotDesc, found := c.lookupTaskCacheAllGenerations(context.Background(), tt.agentKey, tt.newTask)
			if found != tt.wantFound {
				t.Errorf("lookupTaskCacheAllGenerations() found = %v, want %v", found, tt.wantFound)
			}
			if tt.wantFound && gotOutput == "" {
				t.Error("lookupTaskCacheAllGenerations() should return non-empty output on hit")
			}
			if tt.wantFound && gotDesc == "" {
				t.Error("lookupTaskCacheAllGenerations() should return non-empty task description on hit")
			}
		})
	}
}

// TestStoreTaskCache tests caching of task results
func TestStoreTaskCache(t *testing.T) {
	c := &Coordinator{
		taskResultCache:   make(map[string][]cachedTaskEntry),
		taskResultCacheMu: sync.RWMutex{},
		cacheGeneration:   atomic.Int64{},
	}
	c.cacheGeneration.Store(5)

	c.storeTaskCache("agent1", "test task", "test result")

	entries := c.taskResultCache["agent1"]
	if len(entries) != 1 {
		t.Fatalf("storeTaskCache() should add 1 entry, got %d", len(entries))
	}

	entry := entries[0]
	if entry.taskDesc != "test task" {
		t.Errorf("taskDesc = %q, want %q", entry.taskDesc, "test task")
	}
	if entry.output != "test result" {
		t.Errorf("output = %q, want %q", entry.output, "test result")
	}
	if entry.generation != 5 {
		t.Errorf("generation = %d, want %d", entry.generation, 5)
	}
}

// TestStoreTaskCacheMaxEntries tests that cache is limited to max entries
func TestStoreTaskCacheMaxEntries(t *testing.T) {
	c := &Coordinator{
		taskResultCache:   make(map[string][]cachedTaskEntry),
		taskResultCacheMu: sync.RWMutex{},
		cacheGeneration:   atomic.Int64{},
	}
	c.cacheGeneration.Store(1)

	// Add more than maxTaskCacheEntries
	for i := 0; i < maxTaskCacheEntries+10; i++ {
		c.storeTaskCache("agent1", "task "+string(rune('A'+i)), "result")
	}

	entries := c.taskResultCache["agent1"]
	if len(entries) > maxTaskCacheEntries {
		t.Errorf("cache size = %d, should be <= %d", len(entries), maxTaskCacheEntries)
	}

	// Verify oldest entries were removed (should start from index 10)
	if len(entries) > 0 && entries[0].taskDesc != "task "+string(rune('A'+10)) {
		t.Errorf("oldest entry should have been removed, got %q", entries[0].taskDesc)
	}
}

// TestCheckDuplicateTasksComprehensive tests comprehensive duplicate detection scenarios
func TestCheckDuplicateTasksComprehensive(t *testing.T) {
	tests := []struct {
		name              string
		previousTasks     map[string]int
		batchTasks        []TaskDef
		expectExactDup    bool
		expectBatchDup    bool
		expectSemanticDup bool
	}{
		{
			name:          "exact duplicate from previous round",
			previousTasks: map[string]int{"agent1:task1": 2},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
			},
			expectExactDup: true,
		},
		{
			name:          "duplicate within batch",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
			},
			expectBatchDup: true,
		},
		{
			name:          "constraints included in dedup key",
			previousTasks: map[string]int{"agent1:task1 constraints: use python": 1},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1", Constraints: "use python"},
			},
			expectExactDup: true,
		},
		{
			name:          "different constraints = different task",
			previousTasks: map[string]int{"agent1:task1 constraints: use python": 1},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1", Constraints: "use go"},
			},
			expectExactDup: false,
		},
		{
			name:          "case insensitive agent name",
			previousTasks: map[string]int{"agent1:task1": 1},
			batchTasks: []TaskDef{
				{Agent: "AGENT1", Goal: "task1"},
			},
			expectExactDup: true,
		},
		{
			name:          "whitespace normalized in task",
			previousTasks: map[string]int{"agent1:task 1": 1},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task   1"},
			},
			expectExactDup: true,
		},
		{
			name:          "multiple agents same task - not duplicate",
			previousTasks: map[string]int{"agent1:task1": 1},
			batchTasks: []TaskDef{
				{Agent: "agent2", Goal: "task1"},
			},
			expectExactDup: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Coordinator{
				delegatedTasks:   make(map[string]int),
				delegatedTasksMu: sync.Mutex{},
			}
			for k, v := range tt.previousTasks {
				c.delegatedTasks[k] = v
			}

			warnings, duplicates := c.checkDuplicateTasks(context.Background(), tt.batchTasks)

			hasExactDup := false
			hasBatchDup := false
			for _, w := range warnings {
				if strings.Contains(w, "EXACT DUPLICATE:") && !strings.Contains(w, "in batch") {
					hasExactDup = true
				}
				if strings.Contains(w, "in batch") {
					hasBatchDup = true
				}
			}

			if tt.expectExactDup && !hasExactDup {
				t.Errorf("expected exact duplicate warning, got: %v", warnings)
			}
			if tt.expectBatchDup && !hasBatchDup {
				t.Errorf("expected batch duplicate warning, got: %v", warnings)
			}

			// Verify duplicates map is populated correctly
			if tt.expectExactDup || tt.expectBatchDup {
				if len(duplicates) == 0 {
					t.Error("duplicates map should contain entries")
				}
			}
		})
	}
}

// TestCheckDuplicateTasksGlobalCountUpdate tests that global counts are updated correctly
func TestCheckDuplicateTasksGlobalCountUpdate(t *testing.T) {
	c := &Coordinator{
		delegatedTasks:   make(map[string]int),
		delegatedTasksMu: sync.Mutex{},
	}

	tasks := []TaskDef{
		{Agent: "agent1", Goal: "task1"},
		{Agent: "agent1", Goal: "task2"},
	}

	c.checkDuplicateTasks(context.Background(), tasks)

	// Verify non-duplicate tasks increment the global count
	c.delegatedTasksMu.Lock()
	count1 := c.delegatedTasks["agent1:task1"]
	count2 := c.delegatedTasks["agent1:task2"]
	c.delegatedTasksMu.Unlock()

	if count1 != 1 {
		t.Errorf("task1 count = %d, want 1", count1)
	}
	if count2 != 1 {
		t.Errorf("task2 count = %d, want 1", count2)
	}

	// Run again - should increment counts
	_, duplicates2 := c.checkDuplicateTasks(context.Background(), tasks)

	c.delegatedTasksMu.Lock()
	count1Again := c.delegatedTasks["agent1:task1"]
	count2Again := c.delegatedTasks["agent1:task2"]
	c.delegatedTasksMu.Unlock()

	if count1Again != 1 {
		t.Errorf("task1 count after 2nd run = %d, want 1 (duplicate not incremented)", count1Again)
	}
	if count2Again != 1 {
		t.Errorf("task2 count after 2nd run = %d, want 1 (duplicate not incremented)", count2Again)
	}

	// Verify duplicates are marked
	if !duplicates2[0] || !duplicates2[1] {
		t.Error("duplicate tasks should be marked in duplicates map")
	}
}

// TestTaskTimingReset tests the taskTiming reset functionality
func TestTaskTimingReset(t *testing.T) {
	tt := &taskTiming{}
	tt.reset()

	tt.mu.Lock()
	defer tt.mu.Unlock()

	if tt.taskStart.IsZero() {
		t.Error("taskStart should be set after reset")
	}
	if tt.toolTime != 0 {
		t.Errorf("toolTime = %v, want 0", tt.toolTime)
	}
	if !tt.counting {
		t.Error("counting should be true after reset")
	}
}

// TestTaskTimingSnapshot tests the taskTiming snapshot functionality
func TestTaskTimingSnapshot(t *testing.T) {
	tt := &taskTiming{}
	tt.reset()

	// Let some time pass
	time.Sleep(10 * time.Millisecond)

	duration, modelTime, _ := tt.snapshot()

	if duration == 0 {
		t.Error("duration should be non-zero after reset and sleep")
	}
	// modelTime is duration - toolTime, so it should be around 10ms if counting
	if modelTime == 0 {
		t.Errorf("modelTime = %v, want > 0 (counting is on by default)", modelTime)
	}
}

// TestTaskTimingBeginEndTool tests tool timing tracking
func TestTaskTimingBeginEndTool(t *testing.T) {
	tt := &taskTiming{}
	tt.reset()

	tt.beginTool()
	time.Sleep(10 * time.Millisecond)
	tt.endTool()

	_, _, toolTime := tt.snapshot()

	if toolTime == 0 {
		t.Error("toolTime should be non-zero after beginTool/endTool")
	}
}

// TestTaskTimingBeginToolWhenNotCounting tests that beginTool is ignored when not counting
func TestTaskTimingBeginToolWhenNotCounting(t *testing.T) {
	tt := &taskTiming{counting: false}
	tt.beginTool()
	time.Sleep(10 * time.Millisecond)
	tt.endTool()

	_, _, toolTime := tt.snapshot()

	if toolTime != 0 {
		t.Errorf("toolTime = %v, want 0 (not counting)", toolTime)
	}
}

// TestTaskTimingMultipleTools tests multiple tool calls
func TestTaskTimingMultipleTools(t *testing.T) {
	tt := &taskTiming{}
	tt.reset()

	for i := 0; i < 3; i++ {
		tt.beginTool()
		time.Sleep(5 * time.Millisecond)
		tt.endTool()
	}

	_, _, toolTime := tt.snapshot()

	if toolTime == 0 {
		t.Error("toolTime should accumulate across multiple tool calls")
	}
}

// TestFormatTaskResults tests task result formatting
func TestFormatTaskResults(t *testing.T) {
	tests := []struct {
		name     string
		results  []agentTaskResult
		total    int
		dupWarns []string
		wantErr  bool
		check    func(t *testing.T, output string)
	}{
		{
			name: "successful tasks",
			results: []agentTaskResult{
				{agentName: "agent1", output: "result1"},
				{agentName: "agent2", output: "result2"},
			},
			total:   2,
			wantErr: false,
			check: func(t *testing.T, output string) {
				if !strings.Contains(output, "2/2 tasks completed successfully") {
					t.Errorf("output should contain success summary")
				}
			},
		},
		{
			name: "failed tasks",
			results: []agentTaskResult{
				{agentName: "agent1", err: newTestError("error1")},
				{agentName: "agent2", err: newTestError("error2")},
			},
			total:   2,
			wantErr: true,
			check: func(t *testing.T, output string) {
				if !strings.Contains(output, "0/2 tasks completed successfully") {
					t.Errorf("output should contain failure summary, got %q", output)
				}
			},
		},
		{
			name: "mixed results",
			results: []agentTaskResult{
				{agentName: "agent1", output: "result1"},
				{agentName: "agent2", err: newTestError("error2")},
			},
			total:   2,
			wantErr: false,
			check: func(t *testing.T, output string) {
				if !strings.Contains(output, "1/2 tasks completed successfully") {
					t.Errorf("output should contain partial success summary")
				}
				if !strings.Contains(output, "1 failed") {
					t.Errorf("output should mention failed count")
				}
			},
		},
		{
			name: "plan submitted",
			results: []agentTaskResult{
				{agentName: "agent1", planText: "Plan details", todoID: "1"},
			},
			total:   1,
			wantErr: false,
			check: func(t *testing.T, output string) {
				if !strings.Contains(output, "PLAN SUBMITTED") {
					t.Errorf("output should indicate plan was submitted")
				}
				if !strings.Contains(output, "**Todo ID**: 1") {
					t.Errorf("output should include todo ID, got %q", output)
				}
			},
		},
		{
			name: "with duplicate warnings",
			results: []agentTaskResult{
				{agentName: "agent1", output: "result1"},
			},
			total:    1,
			dupWarns: []string{"EXACT DUPLICATE: task1"},
			wantErr:  false,
			check: func(t *testing.T, output string) {
				if !strings.Contains(output, "Warning") {
					t.Errorf("output should contain warning section")
				}
				if !strings.Contains(output, "EXACT DUPLICATE: task1") {
					t.Errorf("output should include duplicate warning")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := formatTaskResults(tt.results, tt.total, tt.dupWarns)
			if (err != nil) != tt.wantErr {
				t.Errorf("formatTaskResults() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil {
				if !strings.Contains(err.Error(), "all") {
					t.Errorf("error message should contain 'all', got %v", err)
				}
			}
			if tt.check != nil {
				tt.check(t, output)
			}
		})
	}
}

// TestFormatTaskResultsSeparators tests that results are properly separated
func TestFormatTaskResultsSeparators(t *testing.T) {
	results := []agentTaskResult{
		{agentName: "agent1", output: "result1"},
		{agentName: "agent2", output: "result2"},
		{agentName: "agent3", output: "result3"},
	}

	output, _ := formatTaskResults(results, 3, nil)

	// Should have separators between results
	if !strings.Contains(output, "---") {
		t.Error("output should contain separators between results")
	}

	// Should have agent headers
	if !strings.Contains(output, "## Agent: agent1") {
		t.Error("output should contain agent headers")
	}
}

func newTestError(msg string) error {
	return errors.New(msg)
}
