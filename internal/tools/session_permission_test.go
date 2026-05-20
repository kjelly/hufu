package tools

import (
	"context"
	"testing"
)

func TestCheckToolPermission_SessionDecision(t *testing.T) {
	ctx := context.Background()

	// 1. Without allowlist, tool is denied
	allowed, askUser, err := CheckToolPermission(ctx, "fetch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed || askUser {
		t.Errorf("expected allowed=false, askUser=false, got allowed=%v, askUser=%v", allowed, askUser)
	}

	// 2. Set "Always Deny" via session perms
	perms := map[string]bool{"fetch": false}
	ctx = context.WithValue(ctx, AgentToolsSessionPermissionsKey, perms)

	allowed, askUser, err = CheckToolPermission(ctx, "fetch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed || askUser {
		t.Errorf("expected allowed=false, askUser=false (denied), got allowed=%v, askUser=%v", allowed, askUser)
	}

	// 3. Set "Always Allow" via session perms
	perms["fetch"] = true
	allowed, askUser, err = CheckToolPermission(ctx, "fetch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed || askUser {
		t.Errorf("expected allowed=true, askUser=false, got allowed=%v, askUser=%v", allowed, askUser)
	}
}

func TestToolPermissionCallback(t *testing.T) {
	called := false
	var resultName string
	var resultAllowed bool
	
	cb := func(name string, allowed bool) {
		called = true
		resultName = name
		resultAllowed = allowed
	}
	
	ctx := context.WithValue(context.Background(), ToolPermissionCallbackKey, ToolPermissionCallback(cb))
	
	// Simulate using the callback
	if injected, ok := ctx.Value(ToolPermissionCallbackKey).(ToolPermissionCallback); ok {
		injected("bash", false)
	}
	
	if !called {
		t.Error("callback was not called")
	}
	if resultName != "bash" || resultAllowed != false {
		t.Errorf("wrong callback data: name=%q, allowed=%v", resultName, resultAllowed)
	}
}
