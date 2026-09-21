package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

const invocationIdentityHeartbeatInterval = 5 * time.Minute

// cliRunIdentity is the terminal CLI projection of one durable coordinator
// invocation. A single hufu command may allocate more than one run ID when it
// switches teams, directly invokes an agent and synthesizes the result, or
// accepts an injected continuation.
type cliRunIdentity struct {
	Team  string `json:"team"`
	RunID string `json:"run_id"`
}

func bindInvocationIdentity(loadedTeams map[string]*teamContext, invocationID string) {
	for _, tc := range loadedTeams {
		if tc != nil && tc.coordinator != nil {
			tc.coordinator.SetInvocationID(invocationID)
		}
	}
}

func snapshotExecutionRunCounts(loadedTeams map[string]*teamContext) map[string]int {
	counts := make(map[string]int, len(loadedTeams))
	for name, tc := range loadedTeams {
		if tc == nil || tc.coordinator == nil {
			continue
		}
		counts[name] = len(tc.coordinator.ExecutionRunIDs())
	}
	return counts
}

func collectExecutionRunIdentities(loadedTeams map[string]*teamContext, priorCounts map[string]int) []cliRunIdentity {
	names := slices.Sorted(maps.Keys(loadedTeams))

	var identities []cliRunIdentity
	for _, name := range names {
		tc := loadedTeams[name]
		if tc == nil || tc.coordinator == nil {
			continue
		}
		runIDs := tc.coordinator.ExecutionRunIDs()
		start := priorCounts[name]
		if start < 0 || start > len(runIDs) {
			start = 0
		}
		teamName := strings.TrimSpace(tc.teamName)
		if teamName == "" {
			teamName = name
		}
		for _, runID := range runIDs[start:] {
			if runID = strings.TrimSpace(runID); runID != "" {
				identities = append(identities, cliRunIdentity{Team: teamName, RunID: runID})
			}
		}
	}
	return identities
}

// emitInvocationIdentity publishes the stable parent identity even in
// quiet/no-summary modes. Text output goes to stderr so stdout remains the
// final answer, while JSONL mode preserves its one-object-per-line framing.
func emitInvocationIdentity(w io.Writer, invocationID string) {
	invocationID = strings.TrimSpace(invocationID)
	if invocationID == "" {
		return
	}
	if opts.eventFormat == "jsonl" {
		writeJSONLStatusEvent(w, jsonStatusEvent{
			Type: "invocation_identity", InvocationID: invocationID,
			Time: time.Now().UTC().Format(time.RFC3339Nano),
		})
		return
	}
	if activeTUIProgram.Load() != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "Invocation ID: %s\n", invocationID)
}

func emitExecutionIdentity(w io.Writer, invocationID string, identities []cliRunIdentity) {
	emitInvocationIdentity(w, invocationID)
	for _, identity := range identities {
		if opts.eventFormat == "jsonl" {
			event := jsonStatusEvent{
				Type:         "run_identity",
				Team:         identity.Team,
				InvocationID: invocationID,
				RunID:        identity.RunID,
				Time:         time.Now().UTC().Format(time.RFC3339Nano),
			}
			writeJSONLStatusEvent(w, event)
			continue
		}
		if activeTUIProgram.Load() != nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "Run ID: %s (team: %s)\n", identity.RunID, identity.Team)
	}
}

// startInvocationIdentityHeartbeat emits immediately and then periodically
// until stopped. The synchronous stop waits for the goroutine so the terminal
// identity block cannot race a late heartbeat.
func startInvocationIdentityHeartbeat(w io.Writer, invocationID string, interval time.Duration) func() {
	if interval <= 0 || strings.TrimSpace(invocationID) == "" {
		emitInvocationIdentity(w, invocationID)
		return func() {}
	}
	ticker := time.NewTicker(interval)
	return startInvocationIdentityHeartbeatWithTicks(w, invocationID, ticker.C, ticker.Stop)
}

func startInvocationIdentityHeartbeatWithTicks(w io.Writer, invocationID string, ticks <-chan time.Time, stopTicks func()) func() {
	emitInvocationIdentity(w, invocationID)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		defer stopTicks()
		for {
			select {
			case _, ok := <-ticks:
				if !ok {
					return
				}
				emitInvocationIdentity(w, invocationID)
			case <-done:
				return
			}
		}
	}()
	return sync.OnceFunc(func() {
		close(done)
		<-stopped
	})
}
