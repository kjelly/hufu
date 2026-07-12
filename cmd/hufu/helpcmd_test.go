package main

import "testing"

func TestFlagGroupsContainOutputControls(t *testing.T) {
	found := false
	for _, name := range flagGroups["output"] {
		if name == "event-format" {
			found = true
		}
	}
	if !found {
		t.Error("output group must include event-format")
	}
}
