package context

import (
	"testing"
	"time"
)

func TestIsCurrentPersistentKnowledge(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	base := ContextItem{Lifecycle: LifecycleConfirmed, Scope: Scope{ProjectID: "p", TeamID: "t"}, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	cases := []struct {
		name string
		edit func(*ContextItem)
		want bool
	}{
		{name: "current persistent", want: true},
		{name: "persistent tier metadata", edit: func(i *ContextItem) { i.Metadata = map[string]string{"memory_tier": "persistent"} }, want: true},
		{name: "agent scope allowed", edit: func(i *ContextItem) { i.Scope.AgentID = "worker" }, want: true},
		{name: "future expiry", edit: func(i *ContextItem) { i.ExpiresAt = &future }, want: true},
		{name: "valid until is not consulted", edit: func(i *ContextItem) { i.ValidUntil = &past }, want: true},
		{name: "candidate", edit: func(i *ContextItem) { i.Lifecycle = LifecycleCandidate }},
		{name: "superseded", edit: func(i *ContextItem) { i.SupersededBy = "newer" }},
		{name: "session scope", edit: func(i *ContextItem) { i.Scope.SessionID = "s" }},
		{name: "task scope", edit: func(i *ContextItem) { i.Scope.TaskID = "task" }},
		{name: "expired", edit: func(i *ContextItem) { i.ExpiresAt = &past }},
		{name: "session lifetime", edit: func(i *ContextItem) { i.Metadata = map[string]string{"memory_lifetime": "session"} }},
		{name: "no metadata", edit: func(i *ContextItem) { i.Metadata = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := base
			item.Metadata = map[string]string{"memory_lifetime": "persistent"}
			if tc.edit != nil {
				tc.edit(&item)
			}
			if got := IsCurrentPersistentKnowledge(item, now); got != tc.want {
				t.Fatalf("IsCurrentPersistentKnowledge = %v, want %v", got, tc.want)
			}
		})
	}
}
