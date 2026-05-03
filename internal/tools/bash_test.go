package tools

import "testing"

func TestNormalizeBashCommand(t *testing.T) {
	cases := []struct {
		name          string
		command       string
		workspaceName string
		want          string
	}{
		{
			name:          "path after space",
			command:       "cat /workspace/file.txt",
			workspaceName: "workspace",
			want:          "cat ./workspace/file.txt",
		},
		{
			name:          "cd to workspace",
			command:       "cd /workspace",
			workspaceName: "workspace",
			want:          "cd ./workspace",
		},
		{
			name:          "path at start of string",
			command:       "/workspace/run.sh",
			workspaceName: "workspace",
			want:          "./workspace/run.sh",
		},
		{
			name:          "redirection into workspace",
			command:       "echo hello > /workspace/out.txt",
			workspaceName: "workspace",
			want:          "echo hello > ./workspace/out.txt",
		},
		{
			name:          "system path with workspace component not rewritten",
			command:       "cat /usr/workspace/file.txt",
			workspaceName: "workspace",
			want:          "cat /usr/workspace/file.txt",
		},
		{
			name:          "workspace name not a prefix of another name",
			command:       "cat /workspacefoo/file.txt",
			workspaceName: "workspace",
			want:          "cat /workspacefoo/file.txt",
		},
		{
			name:          "multiple occurrences",
			command:       "cp /workspace/a.txt /workspace/b.txt",
			workspaceName: "workspace",
			want:          "cp ./workspace/a.txt ./workspace/b.txt",
		},
		{
			name:          "custom workspace name",
			command:       "cat /myworkspace/file.txt",
			workspaceName: "myworkspace",
			want:          "cat ./myworkspace/file.txt",
		},
		{
			name:          "empty workspace name is no-op",
			command:       "cat /workspace/file.txt",
			workspaceName: "",
			want:          "cat /workspace/file.txt",
		},
		{
			name:          "already relative path unchanged",
			command:       "cat ./workspace/file.txt",
			workspaceName: "workspace",
			want:          "cat ./workspace/file.txt",
		},
		{
			name:          "pipe-separated commands",
			command:       "cat /workspace/data.txt | grep foo",
			workspaceName: "workspace",
			want:          "cat ./workspace/data.txt | grep foo",
		},
		{
			name:          "semicolon-separated commands",
			command:       "cd /tmp; cat /workspace/file.txt",
			workspaceName: "workspace",
			want:          "cd /tmp; cat ./workspace/file.txt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeBashCommand(tc.command, tc.workspaceName)
			if got != tc.want {
				t.Errorf("normalizeBashCommand(%q, %q)\n  got  %q\n  want %q", tc.command, tc.workspaceName, got, tc.want)
			}
		})
	}
}

func TestNormalizeWorkspacePath(t *testing.T) {
	cases := []struct {
		name          string
		path          string
		workspaceName string
		want          string
	}{
		{
			name:          "root-style path rewritten",
			path:          "/workspace/file.txt",
			workspaceName: "workspace",
			want:          "./workspace/file.txt",
		},
		{
			name:          "bare workspace dir rewritten",
			path:          "/workspace",
			workspaceName: "workspace",
			want:          "./workspace",
		},
		{
			name:          "relative path unchanged",
			path:          "./workspace/file.txt",
			workspaceName: "workspace",
			want:          "./workspace/file.txt",
		},
		{
			name:          "unrelated absolute path unchanged",
			path:          "/tmp/file.txt",
			workspaceName: "workspace",
			want:          "/tmp/file.txt",
		},
		{
			name:          "prefix match must be full component",
			path:          "/workspacefoo/file.txt",
			workspaceName: "workspace",
			want:          "/workspacefoo/file.txt",
		},
		{
			name:          "empty workspace name is no-op",
			path:          "/workspace/file.txt",
			workspaceName: "",
			want:          "/workspace/file.txt",
		},
		{
			name:          "custom workspace name",
			path:          "/myws/file.txt",
			workspaceName: "myws",
			want:          "./myws/file.txt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeWorkspacePath(tc.path, tc.workspaceName)
			if got != tc.want {
				t.Errorf("normalizeWorkspacePath(%q, %q)\n  got  %q\n  want %q", tc.path, tc.workspaceName, got, tc.want)
			}
		})
	}
}
