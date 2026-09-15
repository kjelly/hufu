package team

import "github.com/kjelly/hufu/internal/tools"

// These worker-visible helpers only manipulate coordinator protocol state.
// Their handlers do not inspect or mutate the task's filesystem workspace.
func (*requestAgentTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}

func (*todoTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}

func (*teamInfoTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}

func (*canonicalMemoryQueryTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}
