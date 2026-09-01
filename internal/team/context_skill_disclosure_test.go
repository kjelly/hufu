package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

func TestMandatorySkillLoadIsEnforcedBeforeTaskWork(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "task"}})[0]
	c.taskTracker.TodoList().SetInjectedSkills(item.ID, []string{"dependency", "root"})
	ctx := context.WithValue(context.Background(), todoIDKey{}, item.ID)
	if denial := c.mandatorySkillLoadDenial(ctx, "bash", `{}`); !strings.Contains(denial, "dependency") {
		t.Fatalf("task work denial = %q", denial)
	}
	if denial := c.mandatorySkillLoadDenial(ctx, "load_skill", `{"name":"root"}`); !strings.Contains(denial, "dependency") {
		t.Fatalf("out-of-order load denial = %q", denial)
	}
	if denial := c.mandatorySkillLoadDenial(ctx, "load_skill", `{"name":"dependency"}`); denial != "" {
		t.Fatalf("first load denied: %q", denial)
	}
	c.taskTracker.TodoList().AddLoadedSkill(item.ID, "dependency")
	if denial := c.mandatorySkillLoadDenial(ctx, "load_skill", `{"name":"root"}`); denial != "" {
		t.Fatalf("second load denied: %q", denial)
	}
	c.taskTracker.TodoList().AddLoadedSkill(item.ID, "root")
	if denial := c.mandatorySkillLoadDenial(ctx, "bash", `{}`); denial != "" {
		t.Fatalf("task work remained denied: %q", denial)
	}
}

func TestSkillContextUsesProgressiveDisclosureWhenLoadSkillIsGranted(t *testing.T) {
	c := &Coordinator{skills: []*skill.SkillDef{{Name: "large", Description: "large workflow", Path: "skills/large/SKILL.md", Content: "FULL SECRET INSTRUCTIONS"}}, skillUsage: make(map[string]*skillUsageState), taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "goal"}})[0]
	items, err := c.buildSkillContextItems(&agent.AgentDef{Name: "worker", Skills: "large"}, "worker", "goal", item.ID, map[string]bool{"load_skill": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != "skill_summary" || !items[0].Required || strings.Contains(items[0].Content, "FULL SECRET INSTRUCTIONS") || !strings.Contains(items[0].Content, "call `load_skill`") {
		t.Fatalf("skill summary = %#v", items)
	}
	if usage := c.SkillUsage(); len(usage) != 0 {
		t.Fatalf("summary disclosure was incorrectly recorded as a full skill use: %#v", usage)
	}
	if len(item.InjectedSkills) != 1 || item.InjectedSkills[0] != "large" {
		t.Fatalf("explicit grant pending skills = %v, want [large]", item.InjectedSkills)
	}
}

func TestSkillContextFallsBackToFullInstructionsWithoutLoadSkill(t *testing.T) {
	c := &Coordinator{skills: []*skill.SkillDef{{Name: "large", Description: "large workflow", Path: "skills/large/SKILL.md", Content: "FULL REQUIRED INSTRUCTIONS"}}, skillUsage: make(map[string]*skillUsageState), taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "goal"}})[0]
	item.InjectedSkills = []string{"stale-pending-skill"}
	items, err := c.buildSkillContextItems(&agent.AgentDef{Name: "worker", Skills: "large"}, "worker", "goal", item.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != "skill_full" || !items[0].Required || !strings.Contains(items[0].Content, "FULL REQUIRED INSTRUCTIONS") {
		t.Fatalf("full fallback = %#v", items)
	}
	if len(item.InjectedSkills) != 0 {
		t.Fatalf("full disclosure left mandatory-load state: %v", item.InjectedSkills)
	}
	ctx := context.WithValue(t.Context(), todoIDKey{}, item.ID)
	if denial := c.mandatorySkillLoadDenial(ctx, "bash", `{}`); denial != "" {
		t.Fatalf("full disclosure denied task work: %q", denial)
	}
}

func TestLegacySkillPromptUsesResolvedLoadSkillGrant(t *testing.T) {
	c := &Coordinator{
		skills:       []*skill.SkillDef{{Name: "large", Description: "large workflow", Path: "skills/large/SKILL.md", Content: "FULL REQUIRED INSTRUCTIONS"}},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	def := &agent.AgentDef{Name: "worker", Skills: "large"}

	withoutGrant := c.appendSkillContext("goal", def, "worker", "goal", "", nil)
	if !strings.Contains(withoutGrant, "FULL REQUIRED INSTRUCTIONS") || !strings.Contains(withoutGrant, "Full skill instructions are already supplied") {
		t.Fatalf("legacy prompt without load_skill grant = %q", withoutGrant)
	}
	if strings.Contains(withoutGrant, "load_skill") {
		t.Fatalf("legacy prompt without load_skill grant asks for unavailable tool: %q", withoutGrant)
	}

	withGrant := c.appendSkillContext("goal", def, "worker", "goal", "", map[string]bool{"load_skill": true})
	if !strings.Contains(withGrant, "large workflow") || !strings.Contains(withGrant, "Call `load_skill`") {
		t.Fatalf("legacy prompt with load_skill grant = %q", withGrant)
	}
}

func TestSkillContextFailsWhenAssignedSkillDependencyIsUnavailable(t *testing.T) {
	c := &Coordinator{skills: []*skill.SkillDef{{Name: "root", Path: "skills/root/SKILL.md", Content: "Read skills/missing/SKILL.md first."}}, skillUsage: make(map[string]*skillUsageState), reportStatus: func(StatusEvent) {}}
	_, err := c.buildSkillContextItems(&agent.AgentDef{Name: "worker", Skills: "root"}, "worker", "goal", "", map[string]bool{"load_skill": true})
	if err == nil || !strings.Contains(err.Error(), "unavailable dependencies") {
		t.Fatalf("dependency preflight error = %v", err)
	}
}

func TestSkillContextFailsWhenAssignedSkillWasFilteredOut(t *testing.T) {
	c := &Coordinator{skillUsage: make(map[string]*skillUsageState), reportStatus: func(StatusEvent) {}}
	_, err := c.buildSkillContextItems(&agent.AgentDef{Name: "worker", Skills: "missing"}, "worker", "goal", "", nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable after team filtering") {
		t.Fatalf("assigned skill preflight error = %v", err)
	}
}
