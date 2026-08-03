package team

import "testing"

func TestResourceClaimModes(t *testing.T) {
	read := []ResourceClaim{{Resource: "repo", Mode: ResourceRead}}
	if claimsConflict(read, read) {
		t.Fatal("read/read claims must be compatible")
	}
	if !claimsConflict(read, []ResourceClaim{{Resource: "repo", Mode: ResourceWrite}}) {
		t.Fatal("read/write claims must conflict")
	}
	if !claimsConflict([]ResourceClaim{{Resource: "repo"}}, read) {
		t.Fatal("default claim mode must be exclusive")
	}
	if !claimsConflict([]ResourceClaim{{Resource: "repo", Mode: ResourceExclusive}}, []ResourceClaim{{Resource: "repo", Mode: ResourceExclusive}}) {
		t.Fatal("exclusive claims must conflict")
	}
}

func TestDAGSchedulerResourceConflict(t *testing.T) {
	c := &Coordinator{maxConcurrent: 2}
	tasks := []TaskDef{
		{Resources: []ResourceClaim{{Resource: "vm", Mode: ResourceExclusive}}},
		{Resources: []ResourceClaim{{Resource: "vm", Mode: ResourceWrite}}},
		{Resources: []ResourceClaim{{Resource: "repo", Mode: ResourceRead}}},
	}
	s := newDAGScheduler(c, tasks, nil, nil)
	s.activeResources[0] = resourceClaims(tasks[0])
	if !s.resourceConflict(1) {
		t.Fatal("write task was not blocked by active exclusive claim")
	}
	if s.resourceConflict(2) {
		t.Fatal("independent read task was incorrectly blocked")
	}
}
