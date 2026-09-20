package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

const (
	emergencyFinalizationTimeout = 1500 * time.Millisecond
	defaultGracefulWrapUpTimeout = 15 * time.Minute
	gracefulWrapUpSafetyMargin   = time.Minute
	forceQuitGracePeriod         = 8 * time.Second
)

// processExit is an injectable seam for shutdown tests. Production retains
// the historical os.Exit behavior.
var processExit = os.Exit

// setupInterruptHandler installs the SIGINT / Ctrl+C handler that
// drives the wrap-up / force-quit two-stage shutdown. Returns a
// cleanup function that tears down the signal handler. The signal
// goroutine calls cancelFn on the second Ctrl+C to stop in-flight work.
func setupInterruptHandler(injector *promptInjector, activeCoord *activeCoordinator, loadedTeamsPointer *map[string]*teamContext, cancelFn context.CancelFunc) func() {
	gracefulTimeout, err := resolvedGracefulWrapUpTimeout(opts.gracefulWrapUpTimeout, opts.timeoutOverride, config.LoadConfig().GracefulWrapUpTimeout)
	if err != nil {
		// Run flag validation reports this before execution. Keep shutdown safe if
		// a non-standard caller installs the handler without validating first.
		gracefulTimeout = defaultGracefulWrapUpTimeout
	}
	return setupInterruptHandlerWithTimeout(injector, activeCoord, loadedTeamsPointer, cancelFn, gracefulTimeout)
}

func setupInterruptHandlerWithTimeout(injector *promptInjector, activeCoord *activeCoordinator, loadedTeamsPointer *map[string]*teamContext, cancelFn context.CancelFunc, gracefulTimeout time.Duration) func() {
	return setupInterruptHandlerWithHooksAndTimeout(injector, activeCoord, loadedTeamsPointer, cancelFn, func() {
		emergencyFinalizeCoordinator(activeCoord)
	}, processExit, gracefulTimeout)
}

func setupInterruptHandlerWithHooksAndTimeout(injector *promptInjector, activeCoord *activeCoordinator, loadedTeamsPointer *map[string]*teamContext, cancelFn context.CancelFunc, emergencyFinalize func(), exit func(int), gracefulTimeout time.Duration) func() {
	sigIntCh := make(chan os.Signal, 1)
	signal.Notify(sigIntCh, os.Interrupt)
	return setupInterruptHandlerFromChannel(injector, activeCoord, loadedTeamsPointer, cancelFn, emergencyFinalize, exit, gracefulTimeout, sigIntCh, func() {
		signal.Stop(sigIntCh)
	}, tools.RequestInteractiveAbort)
}

func setupInterruptHandlerFromChannel(injector *promptInjector, activeCoord *activeCoordinator, loadedTeamsPointer *map[string]*teamContext, cancelFn context.CancelFunc, emergencyFinalize func(), exit func(int), gracefulTimeout time.Duration, sigIntCh chan os.Signal, stopSignals func(), requestInteractiveAbort func()) func() {
	// The map is intentionally retained in the compatibility signature, but
	// emergency shutdown must never iterate it while segment loading may still
	// be publishing teams. The atomic active coordinator is the stable snapshot.
	_ = loadedTeamsPointer
	sigIntDone := make(chan struct{})

	var watchdog *time.Timer
	var watchdogMu sync.Mutex
	watchdogsClosed := false
	stopWatchdog := func() {
		watchdogMu.Lock()
		defer watchdogMu.Unlock()
		watchdogsClosed = true
		if watchdog != nil {
			watchdog.Stop()
			watchdog = nil
		}
	}
	replaceWatchdog := func(delay time.Duration, callback func()) {
		watchdogMu.Lock()
		defer watchdogMu.Unlock()
		if watchdogsClosed {
			return
		}
		if watchdog != nil {
			watchdog.Stop()
		}
		watchdog = time.AfterFunc(delay, callback)
	}
	forceExit := func() {
		fmt.Fprintf(os.Stderr, "\n%s Operations did not cancel within %s. Forcing exit.\n",
			errStyle.Render("⚠"), forceQuitGracePeriod)
		if emergencyFinalize != nil {
			emergencyFinalize()
		}
		if exit != nil {
			exit(130)
		}
	}

	go func() {
		defer close(sigIntDone)
		first := true
		for range sigIntCh {
			if first {
				if requestInteractiveAbort != nil {
					requestInteractiveAbort()
				}
				if p := activeTUIProgram.Load(); p != nil {
					p.Send(tuipkg.WrapUpMsg{})
				} else {
					logCancelSource("sigint", "wrap-up requested")
					fmt.Fprintf(os.Stderr, "\n%s Wrapping up... (press Ctrl+C again to force quit)\n", boldStyle.Render("⏹"))
					if c := activeCoord.Load(); c != nil {
						c.SetWrapUp()
					}
					injector.injectWrapUp()
				}
				replaceWatchdog(gracefulTimeout, func() {
					fmt.Fprintf(os.Stderr, "\n%s Graceful wrap-up exceeded %s; cancelling in-flight operations.\n",
						errStyle.Render("⚠"), gracefulTimeout)
					recordRunCancellation(activeCoord, team.RunCancellationGracefulTimeout, "runtime_watchdog", gracefulTimeout)
					cancelFn()
					replaceWatchdog(forceQuitGracePeriod, forceExit)
				})
				first = false
			} else {
				if activeTUIProgram.Load() == nil {
					currentStatus := "unknown"
					if c := activeCoord.Load(); c != nil {
						currentStatus = c.GetCurrentStatus()
					}
					logCancelSource("sigint", "force quit requested")
					fmt.Fprintf(os.Stderr, "\n%s Force quit requested\n", errStyle.Render("✗"))
					fmt.Fprintf(os.Stderr, "  Current: %s\n", currentStatus)
					fmt.Fprintf(os.Stderr, "  Cancelling in-flight operations (up to %s grace period)...\n", forceQuitGracePeriod)
					fmt.Fprintf(os.Stderr, "  Press Ctrl+\\\\ (SIGQUIT) to dump stack if still stuck\n")
				}
				recordRunCancellation(activeCoord, team.RunCancellationOperatorForce, "operator_sigint", 0)
				replaceWatchdog(forceQuitGracePeriod, forceExit)
				cancelFn()
			}
		}
	}()

	return func() {
		stopWatchdog()
		if stopSignals != nil {
			stopSignals()
		}
		close(sigIntCh)
		<-sigIntDone
	}
}

func recordRunCancellation(activeCoord *activeCoordinator, reason team.RunCancellationReason, source string, gracefulTimeout time.Duration) {
	if activeCoord == nil {
		return
	}
	coordinator := activeCoord.Load()
	if coordinator == nil {
		return
	}
	if err := coordinator.RecordRunCancellation(reason, source, gracefulTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "\n%s Could not persist cancellation cause: %v\n", errStyle.Render("⚠"), err)
	}
}

func resolvedGracefulWrapUpTimeout(cli time.Duration, workerTimeoutSeconds int64, configured string) (time.Duration, error) {
	if cli < 0 {
		return 0, fmt.Errorf("--graceful-wrap-up-timeout must be positive")
	}
	if cli > 0 {
		return cli, nil
	}

	configured = strings.TrimSpace(configured)
	if configured != "" {
		duration, err := time.ParseDuration(configured)
		if err != nil {
			seconds, secondsErr := strconv.ParseInt(configured, 10, 64)
			if secondsErr != nil || seconds <= 0 {
				return 0, fmt.Errorf("invalid graceful-wrap-up-timeout %q: use a positive duration such as 15m or 1h", configured)
			}
			if seconds > int64(time.Duration(1<<63-1)/time.Second) {
				return 0, fmt.Errorf("graceful-wrap-up-timeout %q is too large", configured)
			}
			duration = time.Duration(seconds) * time.Second
		}
		if duration <= 0 {
			return 0, fmt.Errorf("graceful-wrap-up-timeout must be positive")
		}
		return duration, nil
	}

	resolved := defaultGracefulWrapUpTimeout
	if workerTimeoutSeconds > int64((time.Duration(1<<63-1)-gracefulWrapUpSafetyMargin)/time.Second) {
		return 0, fmt.Errorf("--timeout is too large to derive a graceful wrap-up timeout")
	}
	if workerTimeoutSeconds > 0 {
		workerGrace := time.Duration(workerTimeoutSeconds)*time.Second + gracefulWrapUpSafetyMargin
		if workerGrace > resolved {
			resolved = workerGrace
		}
	}
	return resolved, nil
}

func emergencyFinalizeCoordinator(activeCoord *activeCoordinator) {
	if activeCoord == nil {
		return
	}
	c := activeCoord.Load()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), emergencyFinalizationTimeout)
	defer cancel()
	if err := c.EmergencyFinalizeRun(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\n%s Emergency terminal persistence failed: %v\n", errStyle.Render("⚠"), err)
	}
}

func logCancelSource(source, detail string) {
	if detail == "" {
		stderrLog("\n%s [cancel] source=%s\n", boldStyle.Render("⏹"), source)
		return
	}
	stderrLog("\n%s [cancel] source=%s detail=%s\n", boldStyle.Render("⏹"), source, detail)
}
