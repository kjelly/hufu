package team

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

// saveSkillTool lets an agent propose a reusable workflow. Agents never
// publish operational skills themselves: every call writes a reviewable draft
// that an explicit human maintenance action must approve and apply.
type saveSkillTool struct {
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
}

func (t *saveSkillTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "save_skill",
		Description: "Propose a reusable skill as a reviewable SKILL.md draft. It is not active or loadable until an explicit human approval applies it.",
		Parameters: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Unique skill name in kebab-case (e.g. 'git-rebase', 'deploy-docker'). Used as directory name and lookup key.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "One-paragraph description of what the skill does and when to use it.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full Markdown body of the skill: step-by-step workflow, examples, rules, and any gotchas learned.",
			},
		},
		Required: []string{"name", "description", "content"},
	}
}

func (t *saveSkillTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *saveSkillTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *saveSkillTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Name == "" {
		return fantasy.NewTextErrorResponse("name is required"), nil
	}
	if args.Description == "" {
		return fantasy.NewTextErrorResponse("description is required"), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	path, err := t.coordinator.saveAndReloadSkill(args.Name, args.Description, args.Content, true)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to save skill: %v", err)), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf(
		"Skill proposal %q saved to %s; it remains a draft pending explicit human approval.", args.Name, path,
	)), nil
}
