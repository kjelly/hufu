package readline

import (
	ergoreadline "github.com/ergochat/readline"
)

type PromptReader struct {
	instance      *ergoreadline.Instance
	historyFilter func(string) bool
}

// AutoCompleter is implemented by callers that want to provide tab
// completion in the prompt. The Do method receives the current line and
// cursor position; it returns candidate completions (each is the text
// that should replace the partial word) and the length of the shared
// prefix (in bytes) that the user has already typed.
type AutoCompleter interface {
	Do(line []rune, pos int) (newLine [][]rune, length int)
}

func NewPromptReader(historyFile string) (*PromptReader, error) {
	return NewPromptReaderWithCompleter(historyFile, nil)
}

// NewPromptReaderWithCompleter creates a PromptReader with optional tab
// completion. Pass nil for completer to disable.
func NewPromptReaderWithCompleter(historyFile string, completer AutoCompleter) (*PromptReader, error) {
	return NewPromptReaderWithHistoryFilter(historyFile, completer, nil)
}

// NewPromptReaderWithHistoryFilter excludes rejected lines from both in-memory
// and persisted history. The caller still receives the original input.
func NewPromptReaderWithHistoryFilter(historyFile string, completer AutoCompleter, filter func(string) bool) (*PromptReader, error) {
	cfg := &ergoreadline.Config{
		Prompt:          "> ",
		HistoryFile:     historyFile,
		HistoryLimit:    1000,
		InterruptPrompt: "^C",
		EOFPrompt:       "^D",
		AutoComplete:    completer,
		FuncFilterInputRune: func(r rune) (rune, bool) {
			return r, true
		},
	}

	return newPromptReaderFromConfig(cfg, filter)
}

func newPromptReaderFromConfig(cfg *ergoreadline.Config, filter func(string) bool) (*PromptReader, error) {
	if filter != nil {
		cfg.DisableAutoSaveHistory = true
	}
	instance, err := ergoreadline.NewFromConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &PromptReader{instance: instance, historyFilter: filter}, nil
}

func (r *PromptReader) ReadLine(prompt string) (string, error) {
	r.instance.SetPrompt(prompt)
	line, err := r.instance.ReadLine()
	if err != nil {
		return "", err
	}
	if r.historyFilter != nil && r.historyFilter(line) {
		// Match readline's ordinary autosave behavior: a history-write failure
		// does not discard the user's successfully read prompt.
		_ = r.instance.SaveToHistory(line)
	}
	return line, nil
}

func (r *PromptReader) Close() error {
	if r.instance != nil {
		return r.instance.Close()
	}
	return nil
}
