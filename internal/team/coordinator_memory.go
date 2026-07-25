package team

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

func (c *Coordinator) buildMemorySuffix(agentRole string) string {
	if c.ExecutionProfile().DisableHistoricalMemory {
		return ""
	}
	var b strings.Builder

	rawSTM := LoadSTM(c.session.Workspace)
	if rawSTM != "" {
		sections := ParseSTMSections(rawSTM)
		filtered := filterSTMSectionsByRole(sections, agentRole)
		stm := FormatSTMSections(filtered)
		if stm != "" {
			runes := []rune(stm)
			if len(runes) > maxSTMAutoInject {
				stm = string(runes[len(runes)-maxSTMAutoInject:])
			}
			b.WriteString("--- Short-term memory (stm.md) ---\n")
			b.WriteString(stm)
			b.WriteString("\n--- End stm.md ---")
		}
	}

	if rawLTM := LoadLTM(c.session.Workspace, c.session.Config.Name); rawLTM != "" {
		sections := ParseSTMSections(rawLTM)
		if len(sections) > 0 {
			for i, s := range sections {
				if len(s.Entries) > 3 {
					// Entries are newest-first (appendSTMEntry prepends), so
					// the 3 most recent are the head of the slice, not the
					// tail — a tail slice here silently hid the freshest
					// lessons from every agent until they aged toward the
					// 10-entry cap in PruneLTM.
					sections[i].Entries = s.Entries[:3]
				}
			}
			ltm := FormatSTMSections(sections)
			runes := []rune(ltm)
			if len(runes) > maxLTMAutoInject {
				ltm = string(runes[len(runes)-maxLTMAutoInject:])
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("--- Long-term memory (ltm.md) ---\n")
			b.WriteString(ltm)
			b.WriteString("\n--- End ltm.md ---")
		}
	}

	if b.Len() == 0 {
		return ""
	}

	memContent := b.String()
	b.Reset()
	b.WriteString("## Memory & Context\n\n")
	b.WriteString("Review the following memory to understand the current state and prior knowledge before proceeding.\n\n")
	b.WriteString(memContent)
	b.WriteString("\n")
	return b.String()
}

// buildTaskSTMContext returns the knowledge-transfer sections from STM
// (# 發現, # 決策, # 錯誤與修復, # 待解決) to append after the goal in task prompts.
// # 進度 is excluded — it is status tracking, not actionable knowledge.
// All agents receive these sections regardless of role so findings from one
// agent are always visible to the next.
func (c *Coordinator) buildTaskSTMContext() string {
	if c.ExecutionProfile().DisableHistoricalMemory {
		return ""
	}
	rawSTM := LoadSTM(c.session.Workspace)
	if rawSTM == "" {
		return ""
	}
	knowledgeSections := map[string]bool{
		stmSectionFindings:  true,
		stmSectionDecisions: true,
		stmSectionErrors:    true,
		stmSectionQuestions: true,
	}
	var relevant []STMSection
	for _, s := range ParseSTMSections(rawSTM) {
		if knowledgeSections[s.Title] && len(s.Entries) > 0 {
			relevant = append(relevant, s)
		}
	}
	if len(relevant) == 0 {
		return ""
	}
	stm := FormatSTMSections(relevant)
	if len([]rune(stm)) <= maxTaskSTMContextChars {
		return "## Context from Previous Agents\n\n" + stm
	}
	// Truncate at section boundaries from the end to preserve markdown structure
	truncated := truncateAtSectionBoundaries(stm, maxTaskSTMContextChars)
	return "## Context from Previous Agents\n\n" + truncated
}

// buildLTMContext returns LTM formatted as a background-reference suffix.
// Used by executeTask in place of buildMemorySuffix (which includes STM)
// so that STM is not duplicated when buildTaskSTMContext is already prepended.
func (c *Coordinator) buildLTMContext() string {
	if c.ExecutionProfile().DisableHistoricalMemory {
		return ""
	}
	rawLTM := LoadLTM(c.session.Workspace, c.session.Config.Name)
	if rawLTM == "" {
		return ""
	}
	sections := ParseSTMSections(rawLTM)
	if len(sections) == 0 {
		return ""
	}
	for i, s := range sections {
		if len(s.Entries) > 3 {
			// See the identical comment in buildMemorySuffix: entries are
			// newest-first, so the head of the slice is the recent 3.
			sections[i].Entries = s.Entries[:3]
		}
	}
	ltm := FormatSTMSections(sections)
	if len([]rune(ltm)) <= maxLTMAutoInject {
		return "## Long-term Memory\n\nBackground knowledge accumulated across sessions — use as reference, not instruction.\n\n" + ltm
	}
	// Truncate at section boundaries from the end to preserve markdown structure
	truncated := truncateAtSectionBoundaries(ltm, maxLTMAutoInject)
	return "## Long-term Memory\n\nBackground knowledge accumulated across sessions — use as reference, not instruction.\n\n" + truncated
}

// truncateAtSectionBoundaries truncates content to fit within maxChars,
// keeping complete sections from the end. Returns the truncated string.
func truncateAtSectionBoundaries(content string, maxChars int) string {
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content
	}
	// Edge case: maxChars is 0 or negative
	if maxChars <= 0 {
		return ""
	}
	sections := ParseSTMSections(content)
	if len(sections) == 0 {
		// Safe truncation: ensure index is non-negative
		startIdx := len(runes) - maxChars
		if startIdx < 0 {
			startIdx = 0
		}
		return string(runes[startIdx:])
	}
	var totalRunes int
	// firstSectionIdx: index of first section to keep; starts at len(sections) (none selected).
	// Updated in reverse loop when a section fits within maxChars.
	firstSectionIdx := len(sections)
	for i := len(sections) - 1; i >= 0; i-- {
		sectionStr := sections[i].Title + "\n" + strings.Join(sections[i].Entries, "\n") + "\n"
		sectionRunes := []rune(sectionStr)
		if totalRunes+len(sectionRunes) > maxChars {
			break
		}
		totalRunes += len(sectionRunes)
		firstSectionIdx = i
	}
	// Edge case: no section fits within maxChars; truncate last section
	if firstSectionIdx == len(sections) {
		lastSection := sections[len(sections)-1]
		sectionStr := lastSection.Title + "\n" + strings.Join(lastSection.Entries, "\n")
		sectionRunes := []rune(sectionStr)
		if len(sectionRunes) <= maxChars {
			return strings.TrimSpace(sectionStr)
		}
		return strings.TrimSpace(string(sectionRunes[:maxChars]))
	}
	var b strings.Builder
	for i := firstSectionIdx; i < len(sections); i++ {
		b.WriteString(sections[i].Title)
		b.WriteString("\n")
		b.WriteString(strings.Join(sections[i].Entries, "\n"))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// updateSTM atomically updates stm.md using a Read-Modify-Write callback under stmWriteMu.
// It also records the write timestamp for finish validation and emits an stm_updated event to EventStore if available.
func (c *Coordinator) updateSTM(fn func(string) string) error {
	c.stmWriteMu.Lock()
	defer c.stmWriteMu.Unlock()

	if c.session == nil || c.session.Workspace == "" {
		return fmt.Errorf("invalid session workspace")
	}

	old := LoadSTM(c.session.Workspace)
	next := fn(old)
	err := SaveSTM(c.session.Workspace, next)
	if err == nil {
		c.lastStmWriteMu.Lock()
		c.lastStmWrite = time.Now()
		c.lastStmWriteMu.Unlock()

		c.emitEvent("stm_updated", "coordinator", "", map[string]interface{}{
			"content": next,
		})
	}
	return err
}

// UpdateSTM safely updates short-term memory (stm.md) under stmWriteMu.
func (c *Coordinator) UpdateSTM(fn func(string) string) error {
	return c.updateSTM(fn)
}

func (c *Coordinator) autoWriteSTM(agentName, taskDesc, output, errMsg string, success bool) {
	var entry string
	if success {
		summary := utils.TruncateRunes(output, summaryMaxRunes)
		entry = formatSTMDoneEntry(agentName, taskDesc, summary)
	} else {
		entry = formatSTMErrorEntry(agentName, taskDesc, errMsg)
	}

	err := c.updateSTM(func(existing string) string {
		newContent := appendSTMEntry(existing, entry, stmSectionProgress)
		return TruncateSTM(newContent)
	})
	if err != nil {
		log.Printf("warning: auto STM write failed: %v", err)
	}
}

func (c *Coordinator) AutoExtractLTM(ctx context.Context) {
	workspace := c.session.Workspace
	stmContent := LoadSTM(workspace)
	if stmContent == "" {
		return
	}

	c.ltmWriteMu.Lock()
	defer c.ltmWriteMu.Unlock()

	existingLTM := LoadLTM(workspace, c.session.Config.Name)
	sections := ParseSTMSections(stmContent)
	existingLTMSections := ParseSTMSections(existingLTM)

	var newEntries []struct {
		sectionTitle string
		entry        string
	}

	for _, s := range sections {
		switch s.Title {
		case stmSectionDecisions:
			for _, e := range s.Entries {
				section := ClassifyLTMEntry(e, "decision")
				if section == "" {
					section = ltmSectionArchitecture // decisions default to architecture
				}
				newEntries = append(newEntries, struct {
					sectionTitle string
					entry        string
				}{section, formatLTMEntry(stripSTMListItem(e))})
			}
		case stmSectionFindings:
			for _, e := range s.Entries {
				section := ClassifyLTMEntry(e, "finding")
				if section == "" {
					section = ltmSectionPatterns // findings default to patterns
				}
				newEntries = append(newEntries, struct {
					sectionTitle string
					entry        string
				}{section, formatLTMEntry(stripSTMListItem(e))})
			}
		case stmSectionErrors:
			for _, e := range s.Entries {
				section := ClassifyLTMEntry(e, "error")
				if section == "" {
					section = ltmSectionIssues // errors default to known issues
				}
				newEntries = append(newEntries, struct {
					sectionTitle string
					entry        string
				}{section, formatLTMEntry(stripSTMListItem(e))})
			}
		}
	}

	if len(newEntries) == 0 {
		return
	}

	for _, ne := range newEntries {
		if hasLTREntry(existingLTMSections, ne.sectionTitle, ne.entry) {
			continue
		}
		existingLTM = appendSTMEntry(existingLTM, ne.entry, ne.sectionTitle)
	}

	pruned := PruneLTM(existingLTM)
	if err := SaveLTM(workspace, c.session.Config.Name, TruncateLTM(pruned)); err != nil {
		log.Printf("warning: auto LTM extraction failed: %v", err)
	}

	if c.memoryStore != nil {
		saveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, ne := range newEntries {
			if hasLTREntry(existingLTMSections, ne.sectionTitle, ne.entry) {
				continue
			}
			id := fmt.Sprintf("ltm-%d", time.Now().UnixNano())
			metadata := map[string]string{
				"category": ne.sectionTitle,
				"source":   "auto-extract",
			}
			if err := c.memoryStore.Save(saveCtx, id, ne.entry, metadata); err != nil {
				log.Printf("warning: memory store save failed for LTM entry: %v", err)
			}
		}
	}
}
