package tools

import "testing"

func TestFormatDenialError_EmptyReason(t *testing.T) {
	got := formatDenialError("bash", "")
	want := "user denied permission for tool 'bash'"
	if got != want {
		t.Errorf("formatDenialError(\"bash\", \"\") = %q, want %q", got, want)
	}
}

func TestFormatDenialError_WithReason(t *testing.T) {
	got := formatDenialError("bash", "please don't delete files")
	want := "user denied permission for tool 'bash'. Reason: please don't delete files"
	if got != want {
		t.Errorf("formatDenialError = %q, want %q", got, want)
	}
}

func TestFormatDenialError_TrimsWhitespace(t *testing.T) {
	got := formatDenialError("bash", "  trimmed reason  ")
	want := "user denied permission for tool 'bash'. Reason: trimmed reason"
	if got != want {
		t.Errorf("formatDenialError with whitespace = %q, want %q", got, want)
	}
}

func TestFormatDenialError_FirstLineOnly(t *testing.T) {
	got := formatDenialError("bash", "first line\nsecond line\nthird")
	want := "user denied permission for tool 'bash'. Reason: first line"
	if got != want {
		t.Errorf("formatDenialError with newlines = %q, want %q", got, want)
	}
}

func TestFormatDenialError_WhitespaceBecomesEmpty(t *testing.T) {
	got := formatDenialError("bash", "   \n  \n  ")
	want := "user denied permission for tool 'bash'"
	if got != want {
		t.Errorf("formatDenialError with whitespace-only = %q, want %q", got, want)
	}
}
