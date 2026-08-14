package team

// Reflexion-to-LTM: when a failed task is rescued by a reflection hint (or
// exhausts its retries), the lesson is persisted to long-term memory so
// future runs benefit from it instead of rediscovering the same failure.

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

// reflectionHeader prefixes every reflection hint appended to a retry prompt.
// Shared with reflectOnFailure so persistReflexionLesson can strip it.
const reflectionHeader = "\n\n## Reflection on Previous Failure\n\n"

// formatReflexionLesson renders a single-line LTM lesson describing how a task
// failed and, when rescued, what fixed it. Each part is truncated so the whole
// lesson fits within the 200-rune cap that formatLTMEntry enforces.
//
// verifyPolarityBug marks the isUnfixableVerifyFailure case: the task's own
// actions succeeded, only its (coordinator-assigned, worker-immutable) verify
// command had the wrong exit-code polarity for a cleanup/delete check. The
// generic "avoid this approach" framing would be actively misleading there —
// it tells future runs to avoid a task shape that was never actually wrong —
// so this gets a distinct lesson pointing at the verify command instead.
func formatReflexionLesson(agentName, goal, failure, hint string, rescued, verifyPolarityBug bool) string {
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	goal = utils.TruncateRunes(collapse(goal), 50)
	failure = utils.TruncateRunes(collapse(failure), 60)
	if verifyPolarityBug {
		return fmt.Sprintf("agent %s: %q — actions succeeded but the verify command had wrong exit-code polarity (checked EXISTS instead of GONE); fix the verify command's polarity, the task itself is fine", agentName, goal)
	}
	if rescued {
		hint = utils.TruncateRunes(collapse(hint), 60)
		return fmt.Sprintf("agent %s: %q failed (%s); fixed by: %s", agentName, goal, failure, hint)
	}
	return fmt.Sprintf("agent %s: %q fails: %s — avoid this approach", agentName, goal, failure)
}

// persistReflexionLesson stores a candidate only. Candidate lessons are not
// prompt-visible knowledge until CompletionGate promotes them after an
// accepted run with complete evidence.
func (c *Coordinator) persistReflexionLesson(lesson string) {
	lesson = strings.TrimSpace(lesson)
	if lesson == "" || c.session == nil {
		return
	}
	c.persistKnowledgeCandidate(lesson, ltmSectionIssues, "reflexion")
}

func (c *Coordinator) persistPrivateReflexionLesson(agentName, taskID, lesson string) {
	lesson = strings.TrimSpace(lesson)
	if lesson == "" || c == nil || c.session == nil {
		return
	}
	if c.contextRepo == nil || c.workerMemorySvc == nil {
		log.Printf("warning: canonical context unavailable; refusing private reflexion candidate")
		return
	}
	def := c.agentDefByName(agentName)
	if def == nil || def.Memory.Mode != agent.WorkerMemoryPersistent || strings.TrimSpace(def.MemoryID) == "" || strings.TrimSpace(taskID) == "" {
		return
	}
	runID := c.executionRunID
	if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		runID = c.taskTracker.TodoList().RunID()
	}
	if strings.TrimSpace(runID) == "" {
		return
	}
	scope := resolveWorkerScope(c.contextScope(), def, c.activeBranchID())
	item, err := c.workerMemorySvc.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: def.MemoryID,
		Scope:    scope,
		Content:  lesson,
		Category: "lesson",
		Tier:     "persistent",
		RunID:    runID,
		TaskID:   taskID,
		Source:   "reflexion",
	})
	if err != nil {
		log.Printf("warning: private reflexion candidate write failed: %v", err)
		return
	}
	_ = c.emitEvent("worker_memory_candidate_saved", "coordinator", taskID, map[string]interface{}{
		"worker_id": def.MemoryID, "item_id": item.ID, "tier": "persistent", "run_id": runID, "task_id": taskID,
	})
}

func (c *Coordinator) persistKnowledgeCandidate(lesson, section, source string) {
	c.persistKnowledgeCandidateWithEvidence(lesson, section, source, nil)
}

// persistKnowledgeCandidateWithEvidence records the source evidence used to
// derive reusable knowledge. A candidate may be proposed before a run is
// accepted, but its derivation must remain inspectable and it is only promoted
// by the evidence-manifest transaction in CompletionGate.
func (c *Coordinator) persistKnowledgeCandidateWithEvidence(lesson, section, source string, evidence []contextstore.EvidenceRef) {
	lesson = strings.TrimSpace(lesson)
	if lesson == "" || c == nil || c.session == nil {
		return
	}
	if strings.TrimSpace(section) == "" {
		section = ltmSectionIssues
	}
	if c.contextRepo != nil {
		runID := c.executionRunID
		if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
			runID = c.taskTracker.TodoList().RunID()
		}
		item, err := NewSharedMemoryService(c.contextRepo).Propose(context.Background(), SharedMemoryProposal{
			Scope: c.contextScope(), Content: lesson, Section: section, Source: source, RunID: runID, Evidence: evidence,
		})
		if err != nil {
			log.Printf("warning: shared memory candidate write failed: %v", err)
			return
		}
		_ = c.emitEvent("shared_memory_candidate_saved", "coordinator", "", map[string]interface{}{
			"item_id": item.ID, "run_id": runID, "kind": item.Kind, "lifecycle": item.Lifecycle,
		})
		if err := c.rebuildLegacyContextProjections(context.Background()); err != nil {
			log.Printf("warning: shared memory projection rebuild failed: %v", err)
		}
		return
	}
	log.Printf("warning: canonical context unavailable; refusing shared reflexion candidate")
}

// bindCandidateLessonsToManifest remains as a compatibility call site. Shared
// candidate binding and promotion are now one atomic repository transaction in
// CompletionGate, so no JSONL receipt mutation remains.
func (c *Coordinator) bindCandidateLessonsToManifest(_ *EvidenceManifest) error {
	if c == nil || c.contextRepo == nil {
		return fmt.Errorf("canonical context repository is required for candidate promotion")
	}
	return nil
}

func (c *Coordinator) promoteCandidateLessons(manifest *EvidenceManifest) {
	if c == nil || c.contextRepo == nil {
		return
	}
	if err := c.confirmSharedMemoryCandidates(context.Background(), manifest); err != nil {
		log.Printf("warning: canonical shared memory promotion failed: %v", err)
	}
}

func (c *Coordinator) persistReflexionLessonAsync(agentName, taskID, goal, failure, hint string, rescued, verifyPolarityBug bool) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] persistReflexionLessonAsync recovered: %v", r)
			}
		}()
		c.persistPrivateReflexionLesson(agentName, taskID, formatReflexionLesson(agentName, goal, failure, hint, rescued, verifyPolarityBug))
	}()
}
