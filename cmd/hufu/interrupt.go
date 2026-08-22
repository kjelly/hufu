package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/kjelly/hufu/internal/tools"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

const emergencyFinalizationTimeout = 1500 * time.Millisecond

// processExit is an injectable seam for shutdown tests. Production retains
// the historical os.Exit behavior.
var processExit = os.Exit

// setupInterruptHandler installs the SIGINT / Ctrl+C handler that
// drives the wrap-up / force-quit two-stage shutdown. Returns a
// cleanup function that tears down the signal handler. The signal
// goroutine calls cancelFn on the second Ctrl+C to stop in-flight work.
func setupInterruptHandler(injector *promptInjector, activeCoord *activeCoordinator, loadedTeamsPointer *map[string]*teamContext, cancelFn context.CancelFunc) func() {
	return setupInterruptHandlerWithHooks(injector, activeCoord, loadedTeamsPointer, cancelFn, func() {
		emergencyFinalizeCoordinator(activeCoord)
	}, processExit)
}

func setupInterruptHandlerWithHooks(injector *promptInjector, activeCoord *activeCoordinator, loadedTeamsPointer *map[string]*teamContext, cancelFn context.CancelFunc, emergencyFinalize func(), exit func(int)) func() {
	// The map is intentionally retained in the compatibility signature, but
	// emergency shutdown must never iterate it while segment loading may still
	// be publishing teams. The atomic active coordinator is the stable snapshot.
	_ = loadedTeamsPointer
	sigIntCh := make(chan os.Signal, 1)
	signal.Notify(sigIntCh, os.Interrupt)
	sigIntDone := make(chan struct{})

	var watchdog *time.Timer
	stopWatchdog := func() {
		if watchdog != nil {
			watchdog.Stop()
			watchdog = nil
		}
	}

	go func() {
		defer close(sigIntDone)
		first := true
		for range sigIntCh {
			if first {
				tools.RequestInteractiveAbort()
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
					fmt.Fprintf(os.Stderr, "  Cancelling in-flight operations (up to 8s grace period)...\n")
					fmt.Fprintf(os.Stderr, "  Press Ctrl+\\\\ (SIGQUIT) to dump stack if still stuck\n")
				}
				stopWatchdog()
				watchdog = time.AfterFunc(8*time.Second, func() {
					fmt.Fprintf(os.Stderr, "\n%s Operations did not cancel within 8s. Forcing exit.\n",
						errStyle.Render("⚠"))
					if emergencyFinalize != nil {
						emergencyFinalize()
					}
					if exit != nil {
						exit(130)
					}
				})
				cancelFn()
			}
		}
	}()

	return func() {
		stopWatchdog()
		signal.Stop(sigIntCh)
		close(sigIntCh)
		<-sigIntDone
	}
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
