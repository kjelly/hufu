package team

// Skill pattern detection: after each round, repeating tool-call sequences
// are surfaced as skill-draft candidates and optionally auto-promoted.

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/tools"
)

// checkSkillPatterns checks for repeating tool call patterns and auto-generates skill drafts
func (c *Coordinator) checkSkillPatterns() {
	if c.skillDetector == nil {
		return
	}

	candidates := c.skillDetector.FindCandidates(context.Background())
	if len(candidates) == 0 {
		return
	}

	// Apply per-session draft cap.
	if c.maxDrafts > 0 && len(candidates) > c.maxDrafts {
		candidates = candidates[:c.maxDrafts]
	}

	// Only report new patterns (more than previously detected)
	newPatterns := len(candidates) - c.skillPatternsDetected
	if newPatterns <= 0 {
		return
	}

	c.skillPatternsDetected = len(candidates)

	// Auto-generate skill drafts
	savedSkills := c.checkSkillPatternsAndSave()

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
		for _, path := range savedSkills {
			fmt.Fprintf(&msg, "  - %s\n", path)
		}
		msg.WriteString("\nReview and refine with: hufu skill review <skill-name>\n")
	}

	c.report(c.newEvent("step").withMessage(msg.String()))
}

// checkSkillPatternsAndSave checks for patterns and auto-generates skill drafts (requires user confirmation)
func (c *Coordinator) checkSkillPatternsAndSave() []string {
	if c.skillDetector == nil || c.skillGenerator == nil {
		return nil
	}

	candidates := c.skillDetector.FindCandidates(context.Background())
	if len(candidates) == 0 {
		return nil
	}

	// Auto-promotion: top-tier candidates (quality >= 0.95, count >= 15) are
	// generated and promoted automatically without user confirmation. This
	// eliminates manual intervention for obviously high-quality patterns.
	const autoPromoteQuality = 0.95
	const autoPromoteCount = 15
	var autoPromoted []string
	var needConfirmation []skill.PatternCandidate
	for _, cand := range candidates {
		if cand.QualityScore >= autoPromoteQuality && cand.Sequence.Count >= autoPromoteCount {
			path, err := c.skillGenerator.GenerateSkill(cand)
			if err != nil {
				log.Printf("[WARN] auto-promote: failed to generate skill draft: %v", err)
				continue
			}
			// Try to promote from drafts/ to the team skill dir
			skillsDir := filepath.Dir(filepath.Dir(path)) // drafts/<name>/SKILL.md -> drafts -> skills
			skillName := filepath.Base(filepath.Dir(path))
			promoted, promErr := skill.PromoteDraft(skillsDir, skillName)
			if promErr != nil {
				log.Printf("[INFO] auto-promote: draft saved but promote failed (may already exist): %v", promErr)
				autoPromoted = append(autoPromoted, path)
			} else {
				autoPromoted = append(autoPromoted, promoted)
				c.report(c.newEvent("step").withMessage(fmt.Sprintf(
					"⚡ Auto-promoted skill: %s (quality %.2f, ×%d)",
					skillName, cand.QualityScore, cand.Sequence.Count)))
			}
		} else {
			needConfirmation = append(needConfirmation, cand)
		}
	}

	var savedSkills []string
	savedSkills = append(savedSkills, autoPromoted...)

	// Multi-select confirm remaining candidates: user picks which drafts to keep.
	if len(needConfirmation) > 0 {
		selected := c.displaySkillPreviewMultiSelect(needConfirmation)
		for _, cand := range selected {
			path, err := c.skillGenerator.GenerateSkill(cand)
			if err != nil {
				log.Printf("[WARN] failed to generate skill draft: %v", err)
				continue
			}
			savedSkills = append(savedSkills, path)
		}
	}

	return savedSkills
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
