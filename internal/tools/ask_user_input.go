//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/ergochat/readline"
	"github.com/manifoldco/promptui"
	"github.com/muesli/cancelreader"
)

// Interrupt only this prompt's reads: closing the process's stdin would poison
// future prompts. Close drains an in-flight read before releasing its poller.
type askUserInput struct {
	cancelreader.CancelReader
	mu     sync.Mutex
	closed bool
}

func (r *askUserInput) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.EOF
	}
	return r.CancelReader.Read(p)
}

func (r *askUserInput) Close() error {
	r.Cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func newAskUserInput(ctx context.Context) (io.ReadCloser, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	reader, err := cancelreader.NewReader(os.Stdin)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); reader.Cancel() })
	input := &askUserInput{CancelReader: reader}
	return input, func() {
		if !stop() {
			<-done
		}
		_ = input.Close()
		_ = reader.Close()
	}, nil
}

func runAskUserPrompt(ctx context.Context, label string) (string, error) {
	input, closeInput, err := newAskUserInput(ctx)
	if err != nil {
		return "", err
	}
	defer closeInput()
	// Own resize notification too: readline's default callback is process-global
	// and would replace an existing REPL reader's callback with this short-lived one.
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	resizeDone := make(chan struct{})
	var resizeWorkers sync.WaitGroup
	defer func() { signal.Stop(resize); close(resizeDone); resizeWorkers.Wait() }()
	// Unlike promptui's text painter, readline processes edits and painting on
	// one goroutine. Keep history disabled for potentially sensitive answers.
	prompt, err := readline.NewFromConfig(&readline.Config{
		Prompt: label + ": ", Stdin: input, Stdout: os.Stderr, Stderr: os.Stderr,
		HistoryLimit: -1, DisableAutoSaveHistory: true,
		FuncOnWidthChanged: func(update func()) {
			resizeWorkers.Go(func() {
				for {
					select {
					case <-resize:
						update()
					case <-resizeDone:
						return
					}
				}
			})
		},
	})
	if err != nil {
		return "", err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = prompt.Close() })
	defer func() {
		if !stop() {
			<-done
		}
		_ = prompt.Close()
	}()
	answer, err := prompt.ReadLine()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, readline.ErrInterrupt) {
		return "", promptui.ErrInterrupt
	}
	return answer, err
}

func runAskUserSelect(ctx context.Context, prompt *promptui.Select) (int, string, error) {
	input, closeInput, err := newAskUserInput(ctx)
	if err != nil {
		return 0, "", err
	}
	defer closeInput()
	prompt.Stdin = input
	index, answer, err := prompt.Run()
	if ctx.Err() != nil {
		return 0, "", ctx.Err()
	}
	return index, answer, err
}
