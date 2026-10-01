package team

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/utils"
)

// semanticRunInputAttempts bounds how often one semantic translation is
// attempted. Each retry gets a longer timeout and, in the sidecar resolver, a
// larger output budget: the failures seen in practice were a reasoning model
// spending the classifier's 512 output tokens before it wrote the JSON, a
// cloud route exceeding the 10-second timeout, and dropped streams.
const semanticRunInputAttempts = 3

// semanticRunInputRetryProfile is the sidecar profile for a retried
// translation. The answer is still one small JSON value; the extra budget
// leaves room for a model that reasons before answering.
var semanticRunInputRetryProfile = sidecar.Profile{
	MaxOutputTokens: 2048,
	MaxInputRunes:   sidecar.ClassifierProfile.MaxInputRunes,
	Temperature:     0,
	ReasoningEffort: "low",
}

const maxRunInputResolutionFailureRunes = 500

// recordRunInputResolutionFailure notes that the request's value for an input
// could not be translated, so the team default applies. The run goes on with
// the default instead of failing on an unstable resolver model, but never
// silently: the operator is warned now, the coordinator is told, and the final
// answer carries a runtime-written notice.
func (c *Coordinator) recordRunInputResolutionFailure(name string, cause error) {
	if c == nil {
		return
	}
	reason := utils.TruncateRunes(utils.RedactSecrets(cause.Error()), maxRunInputResolutionFailureRunes)
	c.mu.Lock()
	if c.runInputResolutionFailures == nil {
		c.runInputResolutionFailures = make(map[string]string)
	}
	c.runInputResolutionFailures[name] = reason
	c.mu.Unlock()
	c.report(c.newEvent("warning").withAgent("coordinator").withTodoID(CoordTodoID).withMessage(fmt.Sprintf(
		"could not translate run input %s from the request after %d attempts (%s); the team default applies. Pass --input %s=<json> to set it explicitly.",
		name, semanticRunInputAttempts, reason, name)))
}

func (c *Coordinator) resetRunInputResolutionFailures() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.runInputResolutionFailures = nil
	c.mu.Unlock()
}

func (c *Coordinator) runInputResolutionFailure(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	reason, ok := c.runInputResolutionFailures[name]
	return reason, ok
}

// runInputResolutionNotice is the runtime-written paragraph appended to the
// final answer when a default replaced an input the request may have named,
// or "" when there is none.
func (c *Coordinator) runInputResolutionNotice() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	names := make([]string, 0, len(c.runInputResolutionFailures))
	for name := range c.runInputResolutionFailures {
		names = append(names, name)
	}
	c.mu.RUnlock()
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	values := make(map[string]string)
	if snapshot := c.RunInputSnapshot(); snapshot != nil {
		for _, input := range snapshot.Inputs {
			if redacted, err := utils.RedactJSONCompact(input.CanonicalValue); err == nil {
				values[input.Name] = string(redacted)
			}
		}
	}
	var b strings.Builder
	b.WriteString("\n\n⚠️ RUN INPUT DEFAULTED: the request could not be translated, so the team default was used instead of what the request may have asked for.")
	for _, name := range names {
		reason, _ := c.runInputResolutionFailure(name)
		fmt.Fprintf(&b, "\n- %s = %s (translation failed: %s). Rerun with --input %s=<json> if this is not the intended value.", name, utils.TruncateString(values[name], 512), reason, name)
	}
	return b.String()
}
