package team

import (
	"context"
	"slices"
	"testing"

	"github.com/anomalyco/hufu/internal/tools"
)

// TestWithEffectiveToolsAllowed_DefaultHelperBashPreservesRuntimePermissions
// catches the fast-path regression where a Helper exposed its declared tools
// to the model but never received them in the runtime permission context.
func TestWithEffectiveToolsAllowed_DefaultHelperBashPreservesRuntimePermissions(t *testing.T) {
	session, err := LoadDefaultTeam(t.TempDir(), nil, "bash")
	if err != nil {
		t.Fatalf("LoadDefaultTeam: %v", err)
	}

	ctx := (&Coordinator{session: session}).withEffectiveToolsAllowed(context.Background(), session.Agents["helper"])
	allowed := tools.GetToolsAllowed(ctx)
	for _, want := range []string{"view", "bash", "wait_for"} {
		if !slices.Contains(allowed, want) {
			t.Fatalf("runtime allowlist = %v, missing %q", allowed, want)
		}
	}
}
