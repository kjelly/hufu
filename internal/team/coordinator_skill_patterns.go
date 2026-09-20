package team

// Skill pattern detection: after each round, repeating tool-call sequences
// are surfaced as skill-draft candidates. Publication is always explicitly
// approved by a human through the skill lifecycle command.

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

// checkSkillPatterns checks for repeating tool call patterns and auto-generates skill drafts.
// A canceled invocation does not replace the last completed projection.
func (c *Coordinator) checkSkillPatterns(ctx context.Context) {
	if !c.autoSkillsEnabled || c.skillDetector == nil {
		return
	}
	if ctx == nil || ctx.Err() != nil {
		return
	}

	candidates := c.skillDetector.FindCandidates(ctx)
	if ctx.Err() != nil {
		return
	}
	if len(candidates) == 0 {
		if err := c.persistSkillPatternSnapshot([]skill.PatternCandidate{}, nil); err != nil {
			log.Printf("[WARN] failed to persist skill pattern snapshot: %s", utils.RedactSecrets(err.Error()))
		}
		return
	}

	// Apply per-session draft cap.
	if c.maxDrafts > 0 && len(candidates) > c.maxDrafts {
		candidates = candidates[:c.maxDrafts]
	}

	// Only report new patterns (more than previously detected)
	newPatterns := len(candidates) - c.skillPatternsDetected
	var savedSkills []skill.SavedPatternDraft
	if newPatterns > 0 {
		c.skillPatternsDetected = len(candidates)
		savedSkills = c.checkSkillPatternsAndSave(candidates)
	}

	if err := c.persistSkillPatternSnapshot(candidates, savedSkills); err != nil {
		log.Printf("[WARN] failed to persist skill pattern snapshot: %s", utils.RedactSecrets(err.Error()))
	}
	if newPatterns <= 0 {
		return
	}

	// Report skill pattern suggestions
	var msg strings.Builder
	msg.WriteString("─── SKILL SUGGESTIONS ───\n")
	fmt.Fprintf(&msg, "Detected %d new repeating pattern(s):\n", newPatterns)
	for i := 0; i < newPatterns && i < 3; i++ {
		cand := candidates[i]
		fmt.Fprintf(&msg, "  %d. [%s] ×%d - %s\n",
			i+1,
			strings.Join(cand.Sequence.Tools, " → "),
			cand.Sequence.Count,
			cand.SuggestedDesc)
	}
	if len(candidates) > 3 {
		fmt.Fprintf(&msg, "  ... and %d more\n", len(candidates)-3)
	}
	if len(savedSkills) > 0 {
		msg.WriteString("\nDraft skills saved to:\n")
		for _, draft := range savedSkills {
			fmt.Fprintf(&msg, "  - %s\n", draft.Path)
		}
		msg.WriteString("\nReview and refine with:\n")
		for _, draft := range savedSkills {
			fmt.Fprintf(&msg, "  %s\n", c.skillDraftReviewCommand(draft.Name))
		}
	}

	c.report(c.newEvent("step").withMessage(msg.String()))
}

func (c *Coordinator) recordSkillPatternToolCall(agentName, toolName, input, taskDesc string) {
	if c == nil || !c.autoSkillsEnabled || c.skillDetector == nil {
		return
	}
	c.skillDetector.RecordToolCall(agentName, toolName, input, taskDesc)
}

func (c *Coordinator) skillDraftReviewCommand(draftName string) string {
	if c != nil && c.session != nil {
		teamDir := filepath.Clean(c.session.Dir)
		workspace := filepath.Clean(c.session.Workspace)
		if teamDir != "." && teamDir != workspace {
			teamName := filepath.Base(teamDir)
			if teamName != "." && teamName != string(filepath.Separator) {
				return fmt.Sprintf(
					"hufu skill review %s --team %s --agent-team-search-path %s",
					strconv.Quote(draftName),
					strconv.Quote(teamName),
					strconv.Quote(filepath.Dir(teamDir)),
				)
			}
		}
		if teamDir != "." {
			return fmt.Sprintf("hufu skill review %s --workspace %s", strconv.Quote(draftName), strconv.Quote(teamDir))
		}
	}
	return fmt.Sprintf("hufu skill review %s", strconv.Quote(draftName))
}

// checkSkillPatternsAndSave checks for patterns and auto-generates skill drafts (requires user confirmation)
func (c *Coordinator) checkSkillPatternsAndSave(candidates []skill.PatternCandidate) []skill.SavedPatternDraft {
	if c.skillDetector == nil || c.skillGenerator == nil {
		return nil
	}

	// Top-tier candidates are safe to draft automatically, but never safe to
	// publish automatically: a generated procedure changes future agent
	// behavior and therefore requires an explicit human promotion step.
	const autoPromoteQuality = 0.95
	const autoPromoteCount = 15
	var autoDrafts []skill.SavedPatternDraft
	var needConfirmation []skill.PatternCandidate
	for _, cand := range candidates {
		if cand.QualityScore >= autoPromoteQuality && cand.Sequence.Count >= autoPromoteCount {
			path, err := c.skillGenerator.GenerateSkill(cand)
			if err != nil {
				log.Printf("[WARN] automatic skill draft generation failed: %v", err)
				continue
			}
			draft, err := savedPatternDraft(cand, path)
			if err != nil {
				log.Printf("[WARN] automatic skill draft identity failed: %s", utils.RedactSecrets(err.Error()))
				continue
			}
			autoDrafts = append(autoDrafts, draft)
			c.report(c.newEvent("step").withMessage(fmt.Sprintf(
				"Generated high-confidence skill draft awaiting approval: %s (quality %.2f, ×%d)",
				cand.SuggestedName, cand.QualityScore, cand.Sequence.Count)))
		} else {
			needConfirmation = append(needConfirmation, cand)
		}
	}

	var savedSkills []skill.SavedPatternDraft
	savedSkills = append(savedSkills, autoDrafts...)

	// Multi-select confirm remaining candidates: user picks which drafts to keep.
	if len(needConfirmation) > 0 {
		selected := c.displaySkillPreviewMultiSelect(needConfirmation)
		for _, cand := range selected {
			path, err := c.skillGenerator.GenerateSkill(cand)
			if err != nil {
				log.Printf("[WARN] failed to generate skill draft: %v", err)
				continue
			}
			draft, err := savedPatternDraft(cand, path)
			if err != nil {
				log.Printf("[WARN] skill draft identity failed: %s", utils.RedactSecrets(err.Error()))
				continue
			}
			savedSkills = append(savedSkills, draft)
		}
	}

	return savedSkills
}

func savedPatternDraft(candidate skill.PatternCandidate, path string) (skill.SavedPatternDraft, error) {
	patternID, err := skill.PatternCandidateID(candidate)
	if err != nil {
		return skill.SavedPatternDraft{}, err
	}
	name := filepath.Base(filepath.Dir(path))
	if strings.TrimSpace(path) == "" || name == "." || name == string(filepath.Separator) {
		return skill.SavedPatternDraft{}, fmt.Errorf("generated skill draft path %q has no name", path)
	}
	return skill.SavedPatternDraft{
		PatternID: patternID,
		Name:      name,
		Path:      path,
	}, nil
}

func (c *Coordinator) persistSkillPatternSnapshot(
	candidates []skill.PatternCandidate,
	savedDrafts []skill.SavedPatternDraft,
) error {
	if c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return fmt.Errorf("skill pattern snapshot workspace is unavailable")
	}

	path := skill.SkillPatternSnapshotPath(c.session.Workspace)
	var previous *skill.SkillPatternSnapshot
	if loaded, available, err := skill.LoadSkillPatternSnapshot(path); err != nil {
		log.Printf("[WARN] replacing unreadable skill pattern snapshot: %s", utils.RedactSecrets(err.Error()))
	} else if available {
		previous = &loaded
	}

	snapshot, err := skill.NewSkillPatternSnapshot(
		c.contextRunID(),
		c.session.Config.Name,
		time.Now().UTC(),
		candidates,
		savedDrafts,
		previous,
	)
	if err != nil {
		return err
	}
	data, err := skill.EncodeSkillPatternSnapshot(snapshot)
	if err != nil {
		return err
	}
	if err := AtomicWriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write skill pattern snapshot: %w", err)
	}
	return nil
}

// displaySkillPreviewMultiSelect shows the candidate list and asks the user
// to pick which drafts to generate. Returns the filtered list.
// Returns nil if the user declines all (empty input, "n", or invalid).
// Uses the TUI ask_user infrastructure when available (so it works in
// Bubble Tea mode); falls back to a stderr prompt + stdin read otherwise.
func (c *Coordinator) displaySkillPreviewMultiSelect(candidates []skill.PatternCandidate) []skill.PatternCandidate {
	var msg strings.Builder
	msg.WriteString("\n─── SKILL GENERATION PREVIEW ───\n")
	fmt.Fprintf(&msg, "Detected %d high-quality patterns:\n\n", len(candidates))

	for i, cand := range candidates {
		fmt.Fprintf(&msg, "%d. **%s** (quality %.2f, ×%d)\n",
			i+1, cand.SuggestedName, cand.QualityScore, cand.Sequence.Count)
		fmt.Fprintf(&msg, "   Tools: %s\n", strings.Join(cand.Sequence.Tools, " → "))
		if cand.GeneralizationReason != "" {
			fmt.Fprintf(&msg, "   %s\n", cand.GeneralizationReason)
		}
		msg.WriteString("\n")
	}

	msg.WriteString("Keep which drafts? Type numbers (e.g. \"1,3\"), \"a\" for all, \"n\" for none.\n")
	msg.WriteString("Default: n. ")

	// Try the TUI path first. It is a no-op when no TUI callback is registered
	// (i.e. plain CLI mode) and returns ok=false; we then fall back to stdin.
	response, handled := tools.TryAskUserTUI(context.Background(), msg.String(), "free_text", nil, true)
	if !handled {
		fmt.Fprint(os.Stderr, msg.String())
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(line))
	} else {
		response = strings.TrimSpace(strings.ToLower(response))
	}

	if response == "" || response == "n" {
		return nil
	}
	if response == "a" {
		return candidates
	}

	parts := strings.Split(response, ",")
	seen := map[int]bool{}
	var selected []skill.PatternCandidate
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > len(candidates) {
			log.Printf("[WARN] invalid selection %q, ignoring", p)
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		selected = append(selected, candidates[n-1])
	}
	return selected
}
