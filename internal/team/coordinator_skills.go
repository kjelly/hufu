package team

// Skill management: loading, matching, usage tracking, and the auxiliary
// STM/LTM context budget helpers used when injecting skills into prompts.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/skill"
)

// skillUsageState is the internal mutable record; Agents uses a map for O(1) dedup.
type skillUsageState struct {
	Name   string
	Count  int
	Agents map[string]bool
}

// SkillUsageEntry is the read-only snapshot returned by SkillUsage().
type SkillUsageEntry struct {
	Name   string
	Count  int
	Agents []string // sorted list of agent names
}

func (c *Coordinator) recordSkillUsage(name, agentName string) {
	key := strings.ToLower(name)
	func() {
		c.skillUsageMu.Lock()
		defer c.skillUsageMu.Unlock()
		entry, ok := c.skillUsage[key]
		if !ok {
			entry = &skillUsageState{
				Name:   name,
				Agents: make(map[string]bool),
			}
			c.skillUsage[key] = entry
		}
		entry.Count++
		entry.Agents[agentName] = true
	}()
	c.report(c.newEvent("skill_used").withSkillName(name).withAgent(agentName))

	if c.session != nil && c.session.Workspace != "" {
		if err := skill.RecordUsage(c.session.Workspace, name, agentName); err != nil {
			log.Printf("[WARN] failed to persist skill usage: %v", err)
		}
	}
}

func (c *Coordinator) SkillUsage() []SkillUsageEntry {
	c.skillUsageMu.Lock()
	defer c.skillUsageMu.Unlock()
	result := make([]SkillUsageEntry, 0, len(c.skillUsage))
	for _, entry := range c.skillUsage {
		agents := make([]string, 0, len(entry.Agents))
		for k := range entry.Agents {
			agents = append(agents, k)
		}
		sort.Strings(agents)
		result = append(result, SkillUsageEntry{
			Name:   entry.Name,
			Count:  entry.Count,
			Agents: agents,
		})
	}
	return result
}

// getSkills returns a snapshot of the current skill list, safe for concurrent use.
func (c *Coordinator) getSkills() []*skill.SkillDef {
	c.skillsMu.RLock()
	defer c.skillsMu.RUnlock()
	return c.skills
}

func (c *Coordinator) skillDirs() []string {
	dirs := []string{filepath.Join(c.session.Dir, "skills")}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		dirs = append(dirs, filepath.Join(cwd, ".agents", "skills"))
	}
	return append(dirs, filepath.Join(os.Getenv("HOME"), ".agents", "skills"))
}

func (c *Coordinator) setAutoLoadedSkills(skills []*skill.SkillDef) {
	c.autoLoadedSkillsMu.Lock()
	defer c.autoLoadedSkillsMu.Unlock()
	c.autoLoadedSkills = skills
}

func (c *Coordinator) getAutoLoadedSkills() []*skill.SkillDef {
	c.autoLoadedSkillsMu.RLock()
	defer c.autoLoadedSkillsMu.RUnlock()
	return c.autoLoadedSkills
}

// saveAndReloadSkill writes a SKILL.md to the team's local skill directory and
// immediately hot-reloads c.skills so the new skill is available in the same session.
// When asDraft is true, the file is written under skills/drafts/ instead of skills/.
func (c *Coordinator) saveAndReloadSkill(name, description, content string, asDraft bool) (string, error) {
	slug := strings.Trim(skillSlugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		return "", fmt.Errorf("invalid skill name %q", name)
	}

	skillDir := filepath.Join(c.session.Dir, "skills", slug)
	if asDraft {
		skillDir = filepath.Join(c.session.Dir, "skills", "drafts", slug)
	}
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create skill directory: %w", err)
	}

	// Build YAML-safe description block.
	descLines := strings.Split(strings.TrimSpace(description), "\n")
	var descYAML string
	if len(descLines) == 1 {
		descYAML = "description: " + descLines[0]
	} else {
		var db strings.Builder
		db.WriteString("description: |\n")
		for _, l := range descLines {
			db.WriteString("  ")
			db.WriteString(l)
			db.WriteString("\n")
		}
		descYAML = strings.TrimRight(db.String(), "\n")
	}

	var fileContent string
	if asDraft {
		now := time.Now().UTC().Format(time.RFC3339)
		fileContent = fmt.Sprintf("---\nname: %s\n%s\ncreated_at: %s\nlast_modified: %s\n---\n\n%s\n",
			name, descYAML, now, now, strings.TrimSpace(content))
	} else {
		fileContent = fmt.Sprintf("---\nname: %s\n%s\n---\n\n%s\n", name, descYAML, strings.TrimSpace(content))
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(fileContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write skill file: %w", err)
	}

	// Hot-reload: rediscover and re-filter skills from all directories.
	allSkills := skill.DiscoverSkills(c.skillDirs(), false)
	includeSkills := skill.ParseSkillList(c.session.Config.Skills)
	excludeSkills := skill.ParseSkillList(c.session.Config.SkillsExclude)
	newSkills := skill.FilterSkills(allSkills, includeSkills, excludeSkills)
	newSkills = skill.ExpandSkillDependenciesForSet(newSkills, allSkills, excludeSkills)

	func() {
		c.skillsMu.Lock()
		defer c.skillsMu.Unlock()
		c.skills = newSkills
	}()

	return skillPath, nil
}

// appendSkillContext appends skill prefix and auto-matched skill suggestions to
// prompt. todoID may be empty; SetInjectedSkills is skipped when it is.
func (c *Coordinator) appendSkillContext(prompt string, agentDef *agent.AgentDef, agentName, goal, todoID string) string {
	if prefix := c.buildSkillPromptPrefix(agentDef); prefix != "" {
		prompt += "\n\n" + prefix
	}
	if suggestion, names := c.buildSuggestedSkillsText(agentDef, agentName, goal); suggestion != "" {
		if todoID != "" {
			c.taskTracker.TodoList().SetInjectedSkills(todoID, names)
		}
		prompt += "\n\n" + suggestion
	}
	return prompt
}

func (c *Coordinator) buildSkillPromptPrefix(agentDef *agent.AgentDef) string {
	agentSkillNames := skill.ParseSkillList(agentDef.Skills)
	if len(agentSkillNames) == 0 {
		return ""
	}
	cachedSkills := c.getSkills()
	foundMap := map[string]bool{}
	var b strings.Builder
	b.WriteString("## Relevant Skills\n\n")
	for _, s := range skill.SkillsByName(cachedSkills, agentSkillNames) {
		for _, expanded := range skill.ExpandSkillDependencies(s, cachedSkills) {
			fmt.Fprintf(&b, "### %s\n*File: %s*\n\n%s\n\n", expanded.Name, expanded.Path, expanded.Content)
			foundMap[strings.ToLower(expanded.Name)] = true
		}
	}
	for _, name := range agentSkillNames {
		if !foundMap[strings.ToLower(strings.TrimSpace(name))] {
			fmt.Fprintf(os.Stderr, "warning: agent %q requests skill %q which is not available (check team skills-exclude config)\n", agentDef.Name, name)
		}
	}
	b.WriteString("---\n\n")
	return b.String()
}

func (c *Coordinator) buildSuggestedSkillsText(agentDef *agent.AgentDef, agentName string, taskDesc string) (string, []string) {
	relevant := c.computeRelevantSkills(agentDef, taskDesc)
	if len(relevant) == 0 {
		return "", nil
	}

	names := make([]string, len(relevant))
	for i, s := range relevant {
		names[i] = s.Name
		c.report(c.newEvent("skill_auto_loaded").withAgent(agentName).withSkillName(s.Name))
		c.recordSkillUsage(s.Name, agentName)
	}

	var b strings.Builder
	b.WriteString("## Suggested Skills\n\n")
	b.WriteString("The following skills are relevant to your task. Call `load_skill` to load ALL of them before starting work:\n\n")
	for _, s := range relevant {
		desc := s.Description
		if utf8.RuneCountInString(desc) > 80 {
			runes := []rune(desc)
			desc = string(runes[:80]) + "..."
		}
		fmt.Fprintf(&b, "- **%s**: %s\n", s.Name, desc)
	}
	b.WriteString("\n")
	return b.String(), names
}

func (c *Coordinator) computeRelevantSkills(agentDef *agent.AgentDef, taskDesc string) []*skill.SkillDef {
	autoSkills := c.getAutoLoadedSkills()
	if len(autoSkills) == 0 && len(c.forcedSkillNames) == 0 {
		return nil
	}

	existingSkills := skill.ParseSkillList(agentDef.Skills)
	existingSet := map[string]bool{}
	for _, name := range existingSkills {
		existingSet[strings.ToLower(strings.TrimSpace(name))] = true
	}

	agentText := strings.ToLower(agentDef.Name + " " + agentDef.Description + " " + agentDef.Role)
	taskText := strings.ToLower(taskDesc)

	var relevant []*skill.SkillDef
	addedSet := map[string]bool{}
	for _, s := range autoSkills {
		if existingSet[strings.ToLower(s.Name)] {
			continue
		}
		keywords := extractSkillKeywords(s)
		if !containsAny(keywords, agentText) {
			continue
		}
		if !containsAny(keywords, taskText) {
			continue
		}
		addedSet[strings.ToLower(s.Name)] = true
		relevant = append(relevant, s)
	}

	if len(c.forcedSkillNames) > 0 {
		allSkills := c.getSkills()
		for _, s := range allSkills {
			if c.forcedSkillNames[strings.ToLower(s.Name)] && !existingSet[strings.ToLower(s.Name)] && !addedSet[strings.ToLower(s.Name)] {
				relevant = append(relevant, s)
				addedSet[strings.ToLower(s.Name)] = true
			}
		}
	}

	return relevant
}

func containsAny(keywords []string, text string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

const maxSTMAutoInject = 2000
const maxLTMAutoInject = 3000
const maxTaskSTMContextChars = 1500

// maxWorkerAuxContextChars caps the combined size of the auxiliary context
// blocks (prior-agent STM, concurrent tasks, LTM background) appended to a
// worker prompt, so the total injected context cannot grow unbounded and
// overflow a small model's window.
const maxWorkerAuxContextChars = 5000

// assembleContextWithinBudget joins context blocks (already in priority order)
// separated by blank lines, including each only while the running total stays
// within budget. Lower-priority trailing blocks are dropped entirely rather
// than truncated mid-way, preserving each block's markdown structure. The
// returned string is prefixed with "\n\n" when non-empty so it can be appended
// directly to a prompt.
func assembleContextWithinBudget(parts []string, budget int) string {
	if budget <= 0 {
		return ""
	}
	var b strings.Builder
	total := 0
	for _, p := range parts {
		if p == "" {
			continue
		}
		n := len([]rune(p))
		if total+n > budget {
			continue
		}
		total += n
		b.WriteString("\n\n")
		b.WriteString(p)
	}
	return b.String()
}

// This function has been moved to ltm.go as ClassifyLTMEntry

func stripSTMListItem(entry string) string {
	s := strings.TrimSpace(entry)
	for _, prefix := range []string{"- [FAILED] ", "- ", "* "} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	return s
}

func hasLTREntry(sections []STMSection, sectionTitle, entry string) bool {
	for _, s := range sections {
		if s.Title == sectionTitle {
			normalized := normalizeLTREntry(entry)
			for _, e := range s.Entries {
				if normalizeLTREntry(e) == normalized {
					return true
				}
			}
		}
	}
	return false
}

func (c *Coordinator) extractSkillFromToolCall(toolName, input string) string {
	if toolName != "load_skill" {
		return ""
	}
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil || args.Name == "" {
		return ""
	}
	nameLower := strings.ToLower(args.Name)
	for _, s := range c.getSkills() {
		if strings.ToLower(s.Name) == nameLower {
			return s.Name
		}
	}
	return args.Name
}

func (c *Coordinator) matchSkillsForPrompt(prompt string) []*skill.SkillDef {
	skills := c.getSkills()
	if len(skills) == 0 {
		return nil
	}
	promptLower := strings.ToLower(prompt)
	var matched []*skill.SkillDef
	for _, s := range skills {
		for _, kw := range extractSkillKeywords(s) {
			if strings.Contains(promptLower, kw) {
				matched = append(matched, s)
				break
			}
		}
	}
	return matched
}

func extractSkillKeywords(s *skill.SkillDef) []string {
	stopWords := map[string]bool{
		"this": true, "that": true, "with": true, "from": true,
		"for": true, "the": true, "and": true, "its": true,
		"use": true, "like": true, "into": true, "over": true,
		"when": true, "also": true, "just": true, "than": true,
		"then": true, "will": true, "your": true, "both": true,
	}
	seen := map[string]bool{}
	var result []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !seen[s] && !stopWords[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	for _, part := range strings.Split(s.Name, "-") {
		if len(part) >= 3 {
			add(part)
		}
	}
	add(s.Name)
	add(strings.ReplaceAll(s.Name, "-", " "))
	for _, word := range strings.Fields(s.Description) {
		word = strings.Trim(word, ".,;:!?\"'()")
		if len(word) >= 4 {
			add(word)
		}
	}
	return result
}

func (c *Coordinator) SkillDetector() *skill.SkillPatternDetector {
	return c.skillDetector
}

func (c *Coordinator) observeSidecarUsage(result *fantasy.AgentResult) {
	if c == nil || result == nil {
		return
	}
	usage := usageFromSteps(result.Steps)
	if result.TotalUsage.TotalTokens > int64(usage.TotalTokens) {
		usage.TotalTokens = int(result.TotalUsage.TotalTokens)
	}
	if usage.TotalTokens > 0 {
		c.recordNoProgressTokens(int64(usage.TotalTokens))
	}
}

func (c *Coordinator) attachSidecarUsageObserver(s *sidecar.Sidecar) *sidecar.Sidecar {
	if s != nil {
		s.SetUsageObserver(c.observeSidecarUsage)
	}
	return s
}

func (c *Coordinator) Sidecar() *sidecar.Sidecar {
	if c.sidecarModel == "" {
		return nil
	}
	c.sidecarInitMu.Lock()
	defer c.sidecarInitMu.Unlock()
	if c.sidecarInit {
		return c.sidecarInst
	}
	ctx := context.Background()
	s, err := sidecar.NewSidecar(ctx, c.providerManager.GetProvider(c.sidecarModel), c.sidecarModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ sidecar model %q unavailable: %v (auto-skills and skill matching disabled — set --sidecar-model to a working model to enable)\n", c.sidecarModel, err)
		return nil
	}
	c.sidecarInst = c.attachSidecarUsageObserver(s)
	c.sidecarInit = true
	return c.sidecarInst
}

func (c *Coordinator) GuardSidecar() *sidecar.Sidecar {
	if c.guardModel == "" {
		return nil
	}
	c.guardInitMu.Lock()
	defer c.guardInitMu.Unlock()
	if c.guardInit {
		return c.guardInst
	}
	ctx := context.Background()
	s, err := sidecar.NewSidecar(ctx, c.providerManager.GetProvider(c.guardModel), c.guardModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ guard model %q unavailable: %v (guard review disabled — tool calls will be denied until a working model is configured)\n", c.guardModel, err)
		return nil
	}
	c.guardInst = c.attachSidecarUsageObserver(s)
	c.guardInit = true
	return c.guardInst
}

// JudgeSidecar returns the lazily-initialized judge model used to pick the
// best of several multi-model candidate results, or nil when none is
// configured (callers then fall back to concatenation merge).
func (c *Coordinator) JudgeSidecar() *sidecar.Sidecar {
	if c.judgeModel == "" {
		return nil
	}
	c.judgeInitMu.Lock()
	defer c.judgeInitMu.Unlock()
	if c.judgeInit {
		return c.judgeInst
	}
	ctx := context.Background()
	s, err := sidecar.NewSidecar(ctx, c.providerManager.GetProvider(c.judgeModel), c.judgeModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ judge model %q unavailable: %v (multi-model results fall back to concatenation merge)\n", c.judgeModel, err)
		return nil
	}
	c.judgeInst = c.attachSidecarUsageObserver(s)
	c.judgeInit = true
	return c.judgeInst
}

func (c *Coordinator) matchSkillsWithSidecar(ctx context.Context, prompt string) []*skill.SkillDef {
	if !c.autoSkillsEnabled {
		return nil
	}
	allSkills := c.getSkills()
	if len(allSkills) == 0 {
		return nil
	}

	var matched []*skill.SkillDef

	s := c.AgentPool().Sidecar()
	if s != nil {
		summaries := make([]sidecar.SkillSummary, len(allSkills))
		for i, sk := range allSkills {
			summaries[i] = sidecar.SkillSummary{
				Name:        sk.Name,
				Description: sk.Description,
			}
		}
		if c.think {
			c.emitThinkSidecar("MatchSkills", fmt.Sprintf("matching %d skills against prompt", len(allSkills)))
		}
		names, err := s.MatchSkills(ctx, prompt, summaries)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: sidecar skill matching failed, using keyword fallback: %v\n", err)
		} else if len(names) > 0 {
			nameSet := map[string]bool{}
			for _, n := range names {
				nameSet[strings.ToLower(strings.TrimSpace(n))] = true
			}
			for _, sk := range allSkills {
				if nameSet[strings.ToLower(sk.Name)] {
					matched = append(matched, sk)
				}
			}
			matchedNames := make([]string, len(matched))
			for i, sk := range matched {
				matchedNames[i] = sk.Name
			}
			if c.think {
				c.emitThinkSidecar("MatchSkills", fmt.Sprintf("matched: %s", strings.Join(matchedNames, ", ")))
			}
			c.report(c.newEvent("sidecar_call").withMessage("match_skills → " + strings.Join(matchedNames, ", ")))
		}
		if len(matched) == 0 {
			if c.think {
				c.emitThinkSidecar("MatchSkills", "no matches")
			}
			c.report(c.newEvent("sidecar_call").withMessage("match_skills → (no matches)"))
		}
	} else {
		fallback := c.matchSkillsForPrompt(prompt)
		if len(fallback) > 0 {
			names := make([]string, len(fallback))
			for i, sk := range fallback {
				names[i] = sk.Name
			}
			if c.think {
				c.emitThinkSidecar("MatchSkills(keyword)", fmt.Sprintf("fallback matched: %s", strings.Join(names, ", ")))
			}
			c.report(c.newEvent("sidecar_call").withMessage("match_skills (keyword) → " + strings.Join(names, ", ")))
		}
		matched = fallback
	}

	return matched
}

// SkillMatchesPrompt returns true when the prompt contains any keyword
// extracted from the skill's name or description (case-insensitive).
// This is the LLM-free fallback used by DryRun().
func SkillMatchesPrompt(s *skill.SkillDef, prompt string) bool {
	p := strings.ToLower(prompt)
	if p == "" || s == nil {
		return false
	}
	for _, kw := range extractSkillKeywords(s) {
		if strings.Contains(p, kw) {
			return true
		}
	}
	return false
}
