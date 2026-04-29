package readline

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNewPromptReader tests the NewPromptReader function
func TestNewPromptReader(t *testing.T) {
	tests := []struct {
		name    string
		history string
		wantErr bool
	}{
		{
			name:    "create with empty history file",
			history: "",
			wantErr: false,
		},
		{
			name:    "create with valid history file path",
			history: "/tmp/test-history",
			wantErr: false,
		},
		{
			name:    "create with nonexistent directory",
			history: "/nonexistent/dir/history",
			wantErr: false, // Implementation creates directories automatically
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := NewPromptReader(tt.history)

			if (err != nil) != tt.wantErr {
				t.Errorf("NewPromptReader() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && reader == nil {
				t.Error("NewPromptReader() returned nil reader without error")
			}

			if !tt.wantErr {
				defer reader.Close()
			}
		})
	}
}

// TestPromptReaderReadLine tests the ReadLine method
func TestPromptReaderReadLine(t *testing.T) {
	// This test requires interactive input, so we skip it in automated tests
	t.Skip("TestPromptReaderReadLine requires interactive input")
}

// TestPromptReaderClose tests the Close method
func TestPromptReaderClose(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, "history")

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}

	err = reader.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

// TestNewPromptReaderWithNonExistentDir tests NewPromptReader with a directory that doesn't exist
func TestNewPromptReaderWithNonExistentDir(t *testing.T) {
	nonExistentDir := "/nonexistent/directory/that/does/not/exist/history"
	_, err := NewPromptReader(nonExistentDir)

	// Implementation creates directories automatically, so no error expected
	if err != nil {
		t.Errorf("NewPromptReader() unexpected error = %v", err)
	}
}

// TestNewPromptReaderWithValidDir tests NewPromptReader with a valid directory
func TestNewPromptReaderWithValidDir(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, "history")

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}

	if reader == nil {
		t.Error("NewPromptReader() returned nil reader")
	}

	// Verify the history file was created
	if _, err := os.Stat(historyFile); os.IsNotExist(err) {
		t.Error("NewPromptReader() did not create history file")
	}

	reader.Close()
}

// TestPromptReaderMultipleClose tests calling Close multiple times
func TestPromptReaderMultipleClose(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, "history")

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}

	// First close
	err1 := reader.Close()

	// Second close
	err2 := reader.Close()

	// Both closes should not panic
	if err1 != nil {
		t.Logf("First close error: %v", err1)
	}
	if err2 != nil {
		t.Logf("Second close error: %v", err2)
	}
}

// TestPromptReaderConfig tests that the reader is created with correct configuration
func TestPromptReaderConfig(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, "history")

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}
	defer reader.Close()

	// The reader should be created successfully
	if reader == nil {
		t.Error("NewPromptReader() returned nil reader")
	}
}

// TestPromptReaderWithSpecialCharacters tests history file with special characters in path
func TestPromptReaderWithSpecialCharacters(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, "history-with-special-chars_123")

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}
	defer reader.Close()

	if reader == nil {
		t.Error("NewPromptReader() returned nil reader")
	}
}

// TestPromptReaderWithLongPath tests history file with a long path
func TestPromptReaderWithLongPath(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a long path
	longDir := tmpDir
	for i := 0; i < 5; i++ {
		longDir = filepath.Join(longDir, "long-directory-name")
	}
	historyFile := filepath.Join(longDir, "history")

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}
	defer reader.Close()

	if reader == nil {
		t.Error("NewPromptReader() returned nil reader")
	}
}

// TestPromptReaderWithEmptyString tests NewPromptReader with empty string
func TestPromptReaderWithEmptyString(t *testing.T) {
	reader, err := NewPromptReader("")
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}
	defer reader.Close()

	if reader == nil {
		t.Error("NewPromptReader() returned nil reader")
	}
}

// TestPromptReaderConcurrentClose tests calling Close from multiple goroutines
func TestPromptReaderConcurrentClose(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, "history")

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}

	done := make(chan bool)

	// Start multiple goroutines trying to close
	for i := 0; i < 10; i++ {
		go func() {
			defer func() {
				done <- true
			}()
			_ = reader.Close()
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestPromptReaderWithExistingHistoryFile tests NewPromptReader with an existing history file
func TestPromptReaderWithExistingHistoryFile(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, "history")

	// Create an existing history file with some content
	existingContent := "line1\nline2\nline3\n"
	if err := os.WriteFile(historyFile, []byte(existingContent), 0o644); err != nil {
		t.Fatalf("failed to create existing history file: %v", err)
	}

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}
	defer reader.Close()

	if reader == nil {
		t.Error("NewPromptReader() returned nil reader")
	}

	// Verify the file still exists
	if _, err := os.Stat(historyFile); os.IsNotExist(err) {
		t.Error("NewPromptReader() removed existing history file")
	}
}

// TestPromptReaderWithUnicodeInPath tests history file with unicode characters in path
func TestPromptReaderWithUnicodeInPath(t *testing.T) {
	tmpDir := t.TempDir()
	historyFile := filepath.Join(tmpDir, "历史文件") // Chinese characters

	reader, err := NewPromptReader(historyFile)
	if err != nil {
		t.Fatalf("NewPromptReader() error = %v", err)
	}
	defer reader.Close()

	if reader == nil {
		t.Error("NewPromptReader() returned nil reader")
	}
}
