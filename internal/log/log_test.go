package log

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

var testMu sync.Mutex

// captureOutput swaps the writer for a buffer and disables quiet/TUI suppression
// for the duration of the test. Callers must invoke the returned restore func
// (typically via defer) to put the logger back the way it was.
func captureOutput(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	testMu.Lock()
	var buf bytes.Buffer
	SetWriter(&buf)
	prevQuiet := IsQuiet()
	prevTUI := tuiActive.Load()
	SetQuiet(false)
	SetTUIActive(false)
	return &buf, func() {
		SetWriter(io.Discard) // ensure subsequent tests don't share our buffer
		SetQuiet(prevQuiet)
		SetTUIActive(prevTUI)
		// Reset to default stderr for safety
		SetWriter(nil)
		testMu.Unlock()
	}
}

func TestPrintAndPrintf(t *testing.T) {
	buf, restore := captureOutput(t)
	defer restore()
	Print("hello", " ", "world\n")
	Printf("count=%d\n", 7)
	if got := buf.String(); !strings.Contains(got, "hello world") || !strings.Contains(got, "count=7") {
		t.Fatalf("expected output to contain 'hello world' and 'count=7', got %q", got)
	}
}

func TestPrintRespectsQuiet(t *testing.T) {
	buf, restore := captureOutput(t)
	defer restore()
	SetQuiet(true)
	Print("should be suppressed\n")
	Printf("also suppressed %d\n", 42)
	SetQuiet(false)
	if got := buf.String(); strings.Contains(got, "suppressed") {
		t.Fatalf("quiet mode should suppress output, got %q", got)
	}
}

func TestPrintRespectsTUIActive(t *testing.T) {
	buf, restore := captureOutput(t)
	defer restore()
	SetTUIActive(true)
	Print("should be suppressed\n")
	SetTUIActive(false)
	if got := buf.String(); strings.Contains(got, "suppressed") {
		t.Fatalf("TUI active should suppress output, got %q", got)
	}
}

func TestSetWriterNilRestoresStderr(t *testing.T) {
	// Just verify SetWriter(nil) doesn't panic and we can set a new writer after.
	SetWriter(nil)
	SetWriter(nil)
}
