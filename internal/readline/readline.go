package readline

import (
	ergoreadline "github.com/ergochat/readline"
)

type PromptReader struct {
	instance *ergoreadline.Instance
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

	instance, err := ergoreadline.NewFromConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &PromptReader{instance: instance}, nil
}

func (r *PromptReader) ReadLine(prompt string) (string, error) {
	r.instance.SetPrompt(prompt)
	line, err := r.instance.ReadLine()
	if err != nil {
		return "", err
	}
	return line, nil
}

func (r *PromptReader) Close() error {
	if r.instance != nil {
		return r.instance.Close()
	}
	return nil
}
