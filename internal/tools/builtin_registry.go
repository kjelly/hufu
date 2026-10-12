//go:build linux || darwin
// +build linux darwin

package tools

import (
	"charm.land/fantasy"
)

type builtinToolFactory struct {
	name  string
	build func(...ToolOption) fantasy.AgentTool
}

var builtinToolFactories = []builtinToolFactory{
	{"bash", func(opts ...ToolOption) fantasy.AgentTool { return NewBashTool(opts...) }},
	{"sudo", func(opts ...ToolOption) fantasy.AgentTool { return NewSudoTool(opts...) }},
	{"wait_for", func(opts ...ToolOption) fantasy.AgentTool { return NewWaitForTool(opts...) }},
	{"ssh", func(opts ...ToolOption) fantasy.AgentTool { return NewSshTool(opts...) }},
	{"scp", func(opts ...ToolOption) fantasy.AgentTool { return NewScpTool(opts...) }},
	{"ssh_disconnect", func(opts ...ToolOption) fantasy.AgentTool { return NewSSHDisconnectTool(opts...) }},
	{"view", func(opts ...ToolOption) fantasy.AgentTool { return NewViewTool(opts...) }},
	{"write", func(opts ...ToolOption) fantasy.AgentTool { return NewWriteTool(opts...) }},
	{"edit", func(opts ...ToolOption) fantasy.AgentTool { return NewEditTool(opts...) }},
	{"multiedit", func(opts ...ToolOption) fantasy.AgentTool { return NewMultiEditTool(opts...) }},
	{"grep", func(opts ...ToolOption) fantasy.AgentTool { return NewGrepTool(opts...) }},
	{"glob", func(opts ...ToolOption) fantasy.AgentTool { return NewGlobTool(opts...) }},
	{"ls", func(opts ...ToolOption) fantasy.AgentTool { return NewLsTool(opts...) }},
	{"lua", func(opts ...ToolOption) fantasy.AgentTool { return NewLuaTool(opts...) }},
	{"golang", func(opts ...ToolOption) fantasy.AgentTool { return NewGolangTool(opts...) }},
	{"javascript", func(opts ...ToolOption) fantasy.AgentTool { return NewJavascriptTool(opts...) }},
	{"ask_user", func(opts ...ToolOption) fantasy.AgentTool { return NewAskUserTool(opts...) }},
	{"download", func(opts ...ToolOption) fantasy.AgentTool { return NewDownloadTool(opts...) }},
	{"fetch", func(opts ...ToolOption) fantasy.AgentTool { return NewFetchTool(opts...) }},
	{"agentic_fetch", func(opts ...ToolOption) fantasy.AgentTool { return NewAgenticFetchTool(opts...) }},
	{"web_search", func(opts ...ToolOption) fantasy.AgentTool { return NewWebSearchTool(opts...) }},
	{"web_fetch", func(opts ...ToolOption) fantasy.AgentTool { return NewWebFetchTool(opts...) }},
	{"random", func(opts ...ToolOption) fantasy.AgentTool { return NewRandomTool(opts...) }},
	{"math", func(opts ...ToolOption) fantasy.AgentTool { return NewMathTool(opts...) }},
	{"create_skill", func(opts ...ToolOption) fantasy.AgentTool { return NewCreateSkillTool(opts...) }},
}

// BuiltinToolNames reports the model-visible built-in registry without
// constructing handlers. It is safe for offline validation and mirrors the
// policy filtering performed by AllTools.
func BuiltinToolNames(noNet, forceMCP bool) []string {
	names := make([]string, 0, len(builtinToolFactories))
	for _, factory := range builtinToolFactories {
		if noNet && (factory.name == "fetch" || factory.name == "download" || factory.name == "agentic_fetch" || factory.name == "web_search" || factory.name == "web_fetch") {
			continue
		}
		if forceMCP && ForceMCPBlockedTools[factory.name] {
			continue
		}
		names = append(names, factory.name)
	}
	return names
}

func AllTools(opts ...ToolOption) []fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	tools := make([]fantasy.AgentTool, 0, len(builtinToolFactories))
	for _, factory := range builtinToolFactories {
		tools = append(tools, factory.build(opts...))
	}
	if cfg.NetworkBlock {
		netTools := map[string]bool{"fetch": true, "download": true, "agentic_fetch": true, "web_search": true, "web_fetch": true}
		filtered := make([]fantasy.AgentTool, 0, len(tools))
		for _, t := range tools {
			if !netTools[t.Info().Name] {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}
	if cfg.ForceMCP {
		filtered := make([]fantasy.AgentTool, 0, len(tools))
		for _, t := range tools {
			if !ForceMCPBlockedTools[t.Info().Name] {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}
	if cfg.Hooks != nil {
		for _, t := range tools {
			if ct, ok := t.(*coreTool); ok {
				ct.hooks = cfg.Hooks
			}
		}
	}
	return tools
}

func FilterTools(all []fantasy.AgentTool, allowed map[string]bool) []fantasy.AgentTool {
	if len(allowed) == 0 {
		return all
	}
	var filtered []fantasy.AgentTool
	for _, t := range all {
		if allowed[t.Info().Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
