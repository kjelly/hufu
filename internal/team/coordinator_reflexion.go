package team

// Reflexion-to-LTM: when a failed task is rescued by a reflection hint (or
// exhausts its retries), the lesson is persisted to long-term memory so
// future runs benefit from it instead of rediscovering the same failure.

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
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

type reflexionLessonRecord struct {
	ID                   string    `json:"id"`
	Lesson               string    `json:"lesson"`
	Section              string    `json:"section,omitempty"`
	Source               string    `json:"source,omitempty"`
	RunID                string    `json:"run_id"`
	EvidenceManifestHash string    `json:"evidence_manifest_hash,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	Status               string    `json:"status"`
}

const (
	reflexionCandidatesFile = "reflexion_candidates.jsonl"
	reflexionConfirmedFile  = "reflexion_confirmed.jsonl"
)

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

func (c *Coordinator) persistKnowledgeCandidate(lesson, section, source string) {
	lesson = strings.TrimSpace(lesson)
	if lesson == "" || c == nil || c.session == nil {
		return
	}
	if strings.TrimSpace(section) == "" {
		section = ltmSectionIssues
	}
	c.ltmWriteMu.Lock()
	defer c.ltmWriteMu.Unlock()
	workspace := c.session.Workspace
	if err := os.MkdirAll(filepath.Join(workspace, logsDir), 0o700); err != nil {
		log.Printf("warning: reflexion candidate directory creation failed: %v", err)
		return
	}
	hash := sha256.Sum256([]byte(lesson))
	runID := c.executionRunID
	if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		runID = c.taskTracker.TodoList().RunID()
	}
	record := reflexionLessonRecord{ID: fmt.Sprintf("%x", hash[:]), Lesson: utils.RedactSecrets(lesson), Section: section, Source: source, RunID: runID, CreatedAt: time.Now().UTC(), Status: "candidate"}
	path := filepath.Join(workspace, logsDir, reflexionCandidatesFile)
	if existing, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(existing)
		for scanner.Scan() {
			var prior reflexionLessonRecord
			if json.Unmarshal(scanner.Bytes(), &prior) == nil && prior.ID == record.ID {
				_ = existing.Close()
				return
			}
		}
		_ = existing.Close()
	}
	b, err := json.Marshal(record)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("warning: reflexion candidate write failed: %v", err)
		return
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		log.Printf("warning: reflexion candidate write failed: %v", err)
	}
	_ = f.Close()
}

// bindCandidateLessonsToManifest records the exact sealed manifest digest on
// candidates from the same run before any candidate can be promoted. Binding
// is a separate durable step so promotion never fills in an absent digest.
func (c *Coordinator) bindCandidateLessonsToManifest(manifest *EvidenceManifest) error {
	if c == nil || c.session == nil || manifest == nil || manifest.Status != "accepted" || strings.TrimSpace(manifest.RunID) == "" || strings.TrimSpace(manifest.ManifestHash) == "" {
		return fmt.Errorf("accepted manifest identity is incomplete")
	}
	c.ltmWriteMu.Lock()
	defer c.ltmWriteMu.Unlock()
	path := filepath.Join(c.session.Workspace, logsDir, reflexionCandidatesFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(c.session.Workspace, logsDir), "reflexion-candidates-*.tmp")
	if err != nil {
		_ = f.Close()
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmp.Name())
		}
	}()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		var record reflexionLessonRecord
		if json.Unmarshal(line, &record) == nil && record.Status == "candidate" && record.RunID == manifest.RunID && record.EvidenceManifestHash == "" {
			record.EvidenceManifestHash = manifest.ManifestHash
			line, err = json.Marshal(record)
			if err != nil {
				return err
			}
		}
		if _, err := tmp.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	return nil
}

// promoteCandidateLessons is called only after CompletionGate accepts the
// run. It converts candidate records into confirmed LTM entries and records a
// durable confirmation receipt.
func (c *Coordinator) promoteCandidateLessons(manifest *EvidenceManifest) {
	if c == nil || c.session == nil || manifest == nil || manifest.Status != "accepted" || strings.TrimSpace(manifest.ManifestHash) == "" {
		return
	}
	c.ltmWriteMu.Lock()
	defer c.ltmWriteMu.Unlock()
	workspace := c.session.Workspace
	path := filepath.Join(workspace, logsDir, reflexionCandidatesFile)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	confirmedPath := filepath.Join(workspace, logsDir, reflexionConfirmedFile)
	confirmed, err := os.OpenFile(confirmedPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = confirmed.Close() }()
	confirmedIDs := make(map[string]bool)
	if prior, openErr := os.Open(confirmedPath); openErr == nil {
		s := bufio.NewScanner(prior)
		for s.Scan() {
			var record reflexionLessonRecord
			if json.Unmarshal(s.Bytes(), &record) == nil && record.ID != "" {
				confirmedIDs[record.ID] = true
			}
		}
		_ = prior.Close()
	}
	existingLTM := LoadLTM(workspace, c.session.Config.Name)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record reflexionLessonRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.ID == "" || strings.TrimSpace(record.Lesson) == "" {
			continue
		}
		if record.RunID == "" || record.RunID != manifest.RunID {
			continue
		}
		if record.EvidenceManifestHash == "" || record.EvidenceManifestHash != manifest.ManifestHash {
			continue
		}
		if confirmedIDs[record.ID] {
			continue
		}
		section := record.Section
		if section == "" {
			section = ClassifyLTMEntry(record.Lesson, "error")
			if section == "" {
				section = ltmSectionIssues
			}
		}
		entry := formatLTMEntry(record.Lesson)
		if !hasLTREntry(ParseSTMSections(existingLTM), section, entry) {
			existingLTM = appendSTMEntry(existingLTM, entry, section)
		}
		record.Status = "confirmed"
		record.EvidenceManifestHash = manifest.ManifestHash
		confirmedIDs[record.ID] = true
		if b, marshalErr := json.Marshal(record); marshalErr == nil {
			_, _ = confirmed.Write(append(b, '\n'))
		}
	}
	if err := SaveLTM(workspace, c.session.Config.Name, TruncateLTM(PruneLTM(existingLTM))); err != nil {
		log.Printf("warning: confirmed reflexion LTM write failed: %v", err)
	}
}

func (c *Coordinator) persistReflexionLessonAsync(agentName, goal, failure, hint string, rescued, verifyPolarityBug bool) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] persistReflexionLessonAsync recovered: %v", r)
			}
		}()
		c.persistReflexionLesson(formatReflexionLesson(agentName, goal, failure, hint, rescued, verifyPolarityBug))
	}()
}
