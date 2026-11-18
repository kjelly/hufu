package main

import (
	"context"
	"testing"
	"testing/synctest"
)

func TestAskUserTUIQueuedDialogHonorsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		askUserMu.Lock()
		defer askUserMu.Unlock()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan bool, 1)
		go func() {
			_, handled := handleAskUserTUI(ctx, "Queued question", "free_text", nil, false)
			done <- handled
		}()
		synctest.Wait()
		cancel()
		if <-done {
			t.Fatal("cancelled dialog was displayed")
		}
	})
}
