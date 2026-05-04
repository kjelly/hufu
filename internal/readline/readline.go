package readline

import (
	ergoreadline "github.com/ergochat/readline"
)

type PromptReader struct {
	instance *ergoreadline.Instance
}

func NewPromptReader(historyFile string) (*PromptReader, error) {
	cfg := &ergoreadline.Config{
		Prompt:          "> ",
		HistoryFile:     historyFile,
		HistoryLimit:    1000,
		InterruptPrompt: "^C",
		EOFPrompt:       "^D",
		FuncFilterInputRune: func(r rune) (rune, bool) {
			if r == ergoreadline.CharTab {
				return ergoreadline.CharForward, true
			}
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
