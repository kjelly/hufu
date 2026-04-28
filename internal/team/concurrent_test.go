package team

import (
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestTodoListConcurrentAddBatch(t *testing.T) {
	tl := &TodoList{}
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tl.AddBatch([]struct {
				Agent string
				Desc  string
			}{{Agent: "agent", Desc: "task"}})
		}(i)
	}
	wg.Wait()

	all := tl.Items()
	if len(all) != 100 {
		t.Errorf("expected 100 items after concurrent adds, got %d", len(all))
	}

	seen := map[string]bool{}
	for _, item := range all {
		if seen[item.ID] {
			t.Errorf("duplicate ID: %s", item.ID)
		}
		seen[item.ID] = true
	}
}

func TestTodoListConcurrentUpdateStatus(t *testing.T) {
	tl := &TodoList{}
	items := tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
		{Agent: "a", Desc: "t1"},
		{Agent: "b", Desc: "t2"},
		{Agent: "c", Desc: "t3"},
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			tl.UpdateStatus(items[0].ID, TaskInProgress, "")
		}()
		go func() {
			defer wg.Done()
			tl.UpdateStatus(items[1].ID, TaskDone, "done")
		}()
		go func() {
			defer wg.Done()
			tl.UpdateStatus(items[2].ID, TaskError, "err")
		}()
	}
	wg.Wait()

	all := tl.Items()
	if all[0].Status != TaskInProgress {
		t.Errorf("item 1 status unexpected: %s", all[0].Status)
	}
	if all[1].Status != TaskDone {
		t.Errorf("item 2 status unexpected: %s", all[1].Status)
	}
	if all[2].Status != TaskError {
		t.Errorf("item 3 status unexpected: %s", all[2].Status)
	}
	if all[2].Detail != "err" {
		t.Errorf("item 3 detail unexpected: %s", all[2].Detail)
	}
}

func TestTodoListConcurrentReadWrite(t *testing.T) {
	tl := &TodoList{}
	tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{{Agent: "a", Desc: "initial"}})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			tl.AddBatch([]struct {
				Agent string
				Desc  string
			}{{Agent: "a", Desc: "concurrent"}})
		}(i)
		go func() {
			defer wg.Done()
			_ = tl.Items()
		}()
	}
	wg.Wait()

	all := tl.Items()
	if len(all) != 51 {
		t.Errorf("expected 51 items (1 initial + 50 concurrent), got %d", len(all))
	}
}

func TestTodoListConcurrentClearAndRead(t *testing.T) {
	tl := &TodoList{}
	tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
		{Agent: "a", Desc: "t1"},
		{Agent: "b", Desc: "t2"},
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			tl.Clear()
		}()
		go func() {
			defer wg.Done()
			_ = tl.Items()
		}()
	}
	wg.Wait()
}

func TestCoordinatorConversationHistoryConcurrent(t *testing.T) {
	c := &Coordinator{
		session:    &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		agentCache: make(map[string]fantasy.Agent),
		taskTracker: NewTaskTracker(),
		reportStatus: func(event StatusEvent) {},
	}
	c.conversationHistory = make([]fantasy.Message, 0)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.conversationHistoryMu.Lock()
			snapshot := make([]fantasy.Message, len(c.conversationHistory))
			copy(snapshot, c.conversationHistory)
			c.conversationHistoryMu.Unlock()
		}()
		go func() {
			defer wg.Done()
			c.conversationHistoryMu.Lock()
			c.conversationHistory = append(c.conversationHistory, fantasy.NewUserMessage("test"))
			if len(c.conversationHistory) > maxConversationHistory {
				trimmed := make([]fantasy.Message, maxConversationHistory)
				copy(trimmed, c.conversationHistory[len(c.conversationHistory)-maxConversationHistory:])
				c.conversationHistory = trimmed
			}
			c.conversationHistoryMu.Unlock()
		}()
	}
	wg.Wait()
}

func TestCoordinatorWrapUpConcurrent(t *testing.T) {
	c := &Coordinator{
		session:    &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		agentCache: make(map[string]fantasy.Agent),
		taskTracker: NewTaskTracker(),
		reportStatus: func(event StatusEvent) {},
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.SetWrapUp()
		}()
		go func() {
			defer wg.Done()
			_ = c.IsWrapUp()
		}()
	}
	wg.Wait()

	if !c.IsWrapUp() {
		t.Error("expected wrapUp to be set after concurrent SetWrapUp calls")
	}
}