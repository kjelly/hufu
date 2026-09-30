package team

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	maxFinishRejectionRunes        = 1000
	maxRejectedFinishResponseRunes = 8000
)

// finishRejection records the finish calls the runtime refused. When the run
// later ends without an accepted finish, the terminal summary must report
// that the coordinator tried to finish and why it was refused. Saying it never
// called finish would blame the wrong party, and the report it submitted would
// be lost.
type finishRejection struct {
	count    int
	reason   string
	response string
}

// recordFinishRejection keeps the latest refusal and the report submitted with
// it. The input is the raw finish arguments.
func (c *Coordinator) recordFinishRejection(input, reason string) {
	if c == nil {
		return
	}
	var args struct {
		Response string `json:"response"`
	}
	_ = json.Unmarshal([]byte(input), &args)
	c.finishRejectionMu.Lock()
	defer c.finishRejectionMu.Unlock()
	c.finishRejections.count++
	c.finishRejections.reason = utils.TruncateRunes(utils.RedactSecrets(strings.TrimSpace(reason)), maxFinishRejectionRunes)
	if response := strings.TrimSpace(args.Response); response != "" {
		c.finishRejections.response = utils.TruncateRunes(utils.RedactSecrets(response), maxRejectedFinishResponseRunes)
	}
}

func (c *Coordinator) finishRejectionSnapshot() finishRejection {
	if c == nil {
		return finishRejection{}
	}
	c.finishRejectionMu.Lock()
	defer c.finishRejectionMu.Unlock()
	return c.finishRejections
}

// deterministicFinishReason is the continuation reason recorded when the
// runtime assembles the answer from task outputs.
func deterministicFinishReason(rejection finishRejection) string {
	if rejection.count == 0 {
		return "coordinator omitted finish after all tasks completed; deterministic summary used"
	}
	return fmt.Sprintf("finish was rejected %d time(s) after all tasks completed; deterministic summary used; last rejection: %s", rejection.count, rejection.reason)
}

// completedTasksSummaryHeader explains how the answer was produced.
func completedTasksSummaryHeader(rejection finishRejection) string {
	if rejection.count == 0 {
		return "All delegated tasks completed. The coordinator did not call finish, so this report was assembled from durable task outputs.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "All delegated tasks completed, but the runtime rejected the coordinator's finish call %d time(s), so this report was assembled from durable task outputs.\n\nLast rejection: %s\n", rejection.count, rejection.reason)
	if rejection.response != "" {
		fmt.Fprintf(&b, "\n## Coordinator report submitted with the rejected finish\n\n%s\n", rejection.response)
	}
	b.WriteString("\n## Task outputs\n")
	return b.String()
}

// acceptanceRepairUnavailableReason reports why no further work can change a
// failed acceptance check, or "" when the coordinator may still delegate
// repairs. finish is only available once a runtime workflow is done, and a
// done or failed workflow accepts no new tasks, so asking the coordinator to
// re-run tasks there only produces rejected dispatches and repeated finish
// calls.
func (c *Coordinator) acceptanceRepairUnavailableReason() string {
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return ""
	}
	switch state := c.phaseWorkflow.State(); state {
	case PhaseDone, PhaseFailed:
		return fmt.Sprintf("the runtime workflow is %s and accepts no new tasks", strings.ToLower(string(state)))
	default:
		return ""
	}
}
