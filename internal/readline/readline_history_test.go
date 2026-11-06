package readline

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ergoreadline "github.com/ergochat/readline"
)

func TestPromptReaderHistoryFilterRetainsInputWithoutPersistingPrivateCommands(t *testing.T) {
	const private = "private-command /sensitive/source.png"
	const ordinary = "ordinary user prompt"
	history := filepath.Join(t.TempDir(), "history")
	reader, err := newPromptReaderFromConfig(&ergoreadline.Config{
		HistoryFile: history, HistoryLimit: 1000,
		Stdin: strings.NewReader(private + "\n" + ordinary + "\n"), Stdout: io.Discard, Stderr: io.Discard,
		FuncIsTerminal: func() bool { return false },
	}, func(line string) bool { return !strings.HasPrefix(line, "private-command ") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	for _, expected := range []string{private, ordinary} {
		type readResult struct {
			line string
			err  error
		}
		read := make(chan readResult, 1)
		go func() { line, err := reader.ReadLine("> "); read <- readResult{line: line, err: err} }()
		select {
		case result := <-read:
			if result.err != nil || result.line != expected {
				t.Fatal("filter changed the actual user input", result.err)
			}
		case <-time.After(3 * time.Second):
			_ = reader.Close()
			t.Fatal("prompt reader did not return its input")
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(history)
	if err != nil || strings.Contains(string(data), private) || !strings.Contains(string(data), ordinary) {
		t.Fatal("history persisted a rejected command or lost an ordinary prompt", err)
	}
}
