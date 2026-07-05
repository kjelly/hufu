package team

// Reflexion-to-LTM: when a failed task is rescued by a reflection hint (or
// exhausts its retries), the lesson is persisted to long-term memory so
// future runs benefit from it instead of rediscovering the same failure.

import (
	"fmt"
	"log"
	"strings"

	"github.com/anomalyco/hufu/internal/utils"
)

// reflectionHeader prefixes every reflection hint appended to a retry prompt.
// Shared with reflectOnFailure so persistReflexionLesson can strip it.
const reflectionHeader = "\n\n## Reflection on Previous Failure\n\n"

// formatReflexionLesson renders a single-line LTM lesson describing how a task
// failed and, when rescued, what fixed it. Each part is truncated so the whole
// lesson fits within the 200-rune cap that formatLTMEntry enforces.
func formatReflexionLesson(agentName, goal, failure, hint string, rescued bool) string {
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	goal = utils.TruncateRunes(collapse(goal), 50)
	failure = utils.TruncateRunes(collapse(failure), 60)
	if rescued {
		hint = utils.TruncateRunes(collapse(hint), 60)
		return fmt.Sprintf("agent %s: %q failed (%s); fixed by: %s", agentName, goal, failure, hint)
	}
	return fmt.Sprintf("agent %s: %q fails: %s — avoid this approach", agentName, goal, failure)
}

// persistReflexionLesson appends the lesson to LTM using the same
// classify → dedup → prune → truncate recipe as the worker-facing LTM tools.
// Errors are logged and swallowed: memory persistence must never fail a task.
func (c *Coordinator) persistReflexionLesson(lesson string) {
	lesson = strings.TrimSpace(lesson)
	if lesson == "" || c.session == nil {
		return
	}
	section := ClassifyLTMEntry(lesson, "error")
	if section == "" {
		section = ltmSectionIssues
	}

	c.ltmWriteMu.Lock()
	defer c.ltmWriteMu.Unlock()

	workspace := c.session.Workspace
	existingLTM := LoadLTM(workspace, c.session.Config.Name)
	entry := formatLTMEntry(lesson)
	if hasLTREntry(ParseSTMSections(existingLTM), section, entry) {
		return
	}
	newLTM := appendSTMEntry(existingLTM, entry, section)
	if err := SaveLTM(workspace, c.session.Config.Name, TruncateLTM(PruneLTM(newLTM))); err != nil {
		log.Printf("warning: reflexion lesson LTM write failed: %v", err)
	}
}

func (c *Coordinator) persistReflexionLessonAsync(agentName, goal, failure, hint string, rescued bool) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] persistReflexionLessonAsync recovered: %v", r)
			}
		}()
		c.persistReflexionLesson(formatReflexionLesson(agentName, goal, failure, hint, rescued))
	}()
}
