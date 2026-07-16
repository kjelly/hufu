package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
	tuipkg "github.com/anomalyco/hufu/internal/tui"
)

// setupInterruptHandler installs the SIGINT / Ctrl+C handler that
// drives the wrap-up / force-quit two-stage shutdown. Returns a
// cleanup function that tears down the signal handler. The signal
// goroutine calls cancelFn on the second Ctrl+C to stop in-flight work.
func setupInterruptHandler(injector *promptInjector, activeCoord *activeCoordinator, loadedTeamsPointer *map[string]*teamContext, cancelFn context.CancelFunc) func() {
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
					for _, tc := range *loadedTeamsPointer {
						if tc == nil {
							continue
						}
						if tc.session != nil && tc.session.Workspace != "" {
							fmt.Fprintf(os.Stderr, "  Session: %s\n", tc.session.Workspace)
							if tc.sessionData != nil {
								_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
								_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
							}
						}
					}
					os.Exit(130)
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

func logCancelSource(source, detail string) {
	if detail == "" {
		stderrLog("\n%s [cancel] source=%s\n", boldStyle.Render("⏹"), source)
		return
	}
	stderrLog("\n%s [cancel] source=%s detail=%s\n", boldStyle.Render("⏹"), source, detail)
}
