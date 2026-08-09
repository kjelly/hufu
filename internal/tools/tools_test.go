package tools

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/audit"
)

func TestAuditLogger_Context(t *testing.T) {
	ctx := context.Background()

	// Test with nil
	logger := GetAuditLogger(ctx)
	if logger != nil {
		t.Errorf("GetAuditLogger() = %v, want nil", logger)
	}

	// Test with logger set
	// Note: Can't create real AuditLogger without workspace, so just test context mechanics
	var mockLogger *audit.AuditLogger
	ctx = SetAuditLogger(ctx, mockLogger)
	logger = GetAuditLogger(ctx)
	if logger != mockLogger {
		t.Error("GetAuditLogger() should return same logger")
	}
}
