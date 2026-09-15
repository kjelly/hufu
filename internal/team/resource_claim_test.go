package team

import (
	"reflect"
	"testing"
)

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

func TestWorkspaceResourceClaimNormalization(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		wantPath string
		wantOK   bool
		wantErr  bool
	}{
		{name: "file", resource: "workspace:path:docs/a.md", wantPath: "docs/a.md", wantOK: true},
		{name: "directory marker", resource: "workspace:path:docs/", wantPath: "docs", wantOK: true},
		{name: "whole root", resource: "workspace:path:.", wantPath: ".", wantOK: true},
		{name: "generic", resource: "database:production"},
		{name: "empty", resource: "workspace:path:", wantErr: true},
		{name: "absolute", resource: "workspace:path:/etc/passwd", wantErr: true},
		{name: "parent", resource: "workspace:path:../secret", wantErr: true},
		{name: "dot segment", resource: "workspace:path:docs/./a.md", wantErr: true},
		{name: "backslash", resource: `workspace:path:docs\a.md`, wantErr: true},
		{name: "unknown reserved namespace", resource: "workspace:other:x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseWorkspacePathResource(tt.resource)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseWorkspacePathResource(%q) error = %v, wantErr %t", tt.resource, err, tt.wantErr)
			}
			if got != tt.wantPath || ok != tt.wantOK {
				t.Fatalf("ParseWorkspacePathResource(%q) = (%q, %t), want (%q, %t)", tt.resource, got, ok, tt.wantPath, tt.wantOK)
			}
		})
	}
}

func TestNewWorkspacePathResourceClaim(t *testing.T) {
	claim, err := NewWorkspacePathResourceClaim("docs/", "")
	if err != nil {
		t.Fatalf("NewWorkspacePathResourceClaim: %v", err)
	}
	want := ResourceClaim{Resource: "workspace:path:docs", Mode: ResourceExclusive}
	if claim != want {
		t.Fatalf("claim = %#v, want %#v", claim, want)
	}
	if _, err := NewWorkspacePathResourceClaim("docs", ResourceClaimMode("unknown")); err == nil {
		t.Fatal("unknown claim mode was accepted")
	}
}

func TestNormalizeResourceClaimsCoalescesStrongestMode(t *testing.T) {
	claims, err := normalizeResourceClaims([]ResourceClaim{
		{Resource: "workspace:path:docs/", Mode: ResourceRead},
		{Resource: "repo", Mode: ResourceRead},
		{Resource: "workspace:path:docs", Mode: ResourceWrite},
		{Resource: ""},
	})
	if err != nil {
		t.Fatalf("normalizeResourceClaims: %v", err)
	}
	want := []ResourceClaim{
		{Resource: "repo", Mode: ResourceRead},
		{Resource: "workspace:path:docs", Mode: ResourceWrite},
	}
	if !reflect.DeepEqual(claims, want) {
		t.Fatalf("claims = %#v, want %#v", claims, want)
	}
}

func TestWorkspaceResourceClaimHierarchy(t *testing.T) {
	tests := []struct {
		name  string
		left  ResourceClaim
		right ResourceClaim
		want  bool
	}{
		{name: "same reads", left: ResourceClaim{Resource: "workspace:path:docs", Mode: ResourceRead}, right: ResourceClaim{Resource: "workspace:path:docs", Mode: ResourceRead}},
		{name: "parent reader child writer", left: ResourceClaim{Resource: "workspace:path:docs", Mode: ResourceRead}, right: ResourceClaim{Resource: "workspace:path:docs/a.md", Mode: ResourceWrite}, want: true},
		{name: "child writer parent writer", left: ResourceClaim{Resource: "workspace:path:internal/team/a.go", Mode: ResourceWrite}, right: ResourceClaim{Resource: "workspace:path:internal/team", Mode: ResourceWrite}, want: true},
		{name: "disjoint writers", left: ResourceClaim{Resource: "workspace:path:docs", Mode: ResourceWrite}, right: ResourceClaim{Resource: "workspace:path:internal", Mode: ResourceWrite}},
		{name: "segment boundary", left: ResourceClaim{Resource: "workspace:path:docs", Mode: ResourceWrite}, right: ResourceClaim{Resource: "workspace:path:docs2/a.md", Mode: ResourceWrite}},
		{name: "whole root", left: ResourceClaim{Resource: "workspace:path:.", Mode: ResourceRead}, right: ResourceClaim{Resource: "workspace:path:docs/a.md", Mode: ResourceWrite}, want: true},
		{name: "generic distinct namespace", left: ResourceClaim{Resource: "repo", Mode: ResourceWrite}, right: ResourceClaim{Resource: "workspace:path:repo", Mode: ResourceWrite}},
		{name: "malformed fails closed", left: ResourceClaim{Resource: "workspace:other:x", Mode: ResourceRead}, right: ResourceClaim{Resource: "repo", Mode: ResourceRead}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resourceClaimConflicts(tt.left, tt.right); got != tt.want {
				t.Fatalf("resourceClaimConflicts(%#v, %#v) = %t, want %t", tt.left, tt.right, got, tt.want)
			}
		})
	}
}

func TestValidateResourceClaimsRejectsMalformedClaim(t *testing.T) {
	for _, task := range []TaskDef{
		{Resources: []ResourceClaim{{Resource: "workspace:other:x", Mode: ResourceRead}}},
		{Resources: []ResourceClaim{{Resource: "repo", Mode: ResourceClaimMode("shared")}}},
	} {
		if err := validateResourceClaims([]TaskDef{task}); err == nil {
			t.Fatalf("validateResourceClaims(%#v) accepted malformed claim", task.Resources)
		}
	}
}
