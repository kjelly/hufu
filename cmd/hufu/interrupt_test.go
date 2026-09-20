package main

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolvedGracefulWrapUpTimeout(t *testing.T) {
	tests := []struct {
		name          string
		cli           time.Duration
		workerSeconds int64
		configured    string
		want          time.Duration
		wantErr       string
	}{
		{name: "safe default", want: 15 * time.Minute},
		{name: "short worker keeps default", workerSeconds: 60, want: 15 * time.Minute},
		{name: "long worker extends grace", workerSeconds: 20 * 60, want: 21 * time.Minute},
		{name: "config duration", configured: "45m", want: 45 * time.Minute},
		{name: "config seconds", configured: "900", want: 15 * time.Minute},
		{name: "CLI wins", cli: 2 * time.Hour, workerSeconds: 60, configured: "bad", want: 2 * time.Hour},
		{name: "negative CLI", cli: -time.Second, wantErr: "must be positive"},
		{name: "invalid config", configured: "eventually", wantErr: "invalid graceful-wrap-up-timeout"},
		{name: "zero config", configured: "0s", wantErr: "must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvedGracefulWrapUpTimeout(tt.cli, tt.workerSeconds, tt.configured)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve timeout: %v", err)
			}
			if got != tt.want {
				t.Fatalf("timeout = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestInterruptWatchdogUsesConfiguredGrace(t *testing.T) {
	injector := newPromptInjector(nil)
	activeCoord := new(activeCoordinator)
	sigIntCh := make(chan os.Signal, 1)
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() { close(cancelled) })
	}
	exited := make(chan int, 1)

	cleanup := setupInterruptHandlerFromChannel(
		injector,
		activeCoord,
		nil,
		cancel,
		nil,
		func(code int) { exited <- code },
		20*time.Millisecond,
		sigIntCh,
		nil,
		func() {},
	)
	defer cleanup()

	sigIntCh <- os.Interrupt
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not cancel after configured grace period")
	}
	if !injector.IsWrapUpRequested() {
		t.Fatal("first interrupt did not request graceful wrap-up")
	}
	select {
	case code := <-exited:
		t.Fatalf("force exit ran before its grace period, code %d", code)
	default:
	}
}

func TestSecondInterruptCancelsBeforeGraceExpires(t *testing.T) {
	injector := newPromptInjector(nil)
	activeCoord := new(activeCoordinator)
	sigIntCh := make(chan os.Signal, 2)
	cancelled := make(chan struct{})
	var cancelOnce sync.Once

	cleanup := setupInterruptHandlerFromChannel(
		injector,
		activeCoord,
		nil,
		func() { cancelOnce.Do(func() { close(cancelled) }) },
		nil,
		func(int) {},
		time.Hour,
		sigIntCh,
		nil,
		func() {},
	)
	defer cleanup()

	sigIntCh <- os.Interrupt
	sigIntCh <- os.Interrupt
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("second interrupt did not cancel immediately")
	}
}
