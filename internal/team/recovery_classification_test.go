package team

import "testing"

func TestInferSideEffectClass(t *testing.T) {
	tests := []struct {
		tools string
		want  SideEffectClass
	}{
		{"", SideEffectNone},
		{"all", SideEffectInfraMutation}, // "all" grants sudo → conservative
		{"view,grep,glob,ls", SideEffectNone},
		{"view,write,edit", SideEffectWorkspaceWrite},
		{"bash", SideEffectWorkspaceWrite},
		{"memory_save", SideEffectWorkspaceWrite},
		{"stm_write", SideEffectWorkspaceWrite},
		{"ltm_update", SideEffectWorkspaceWrite},
		{"golang,lua", SideEffectWorkspaceWrite},
		{"view,download,fetch", SideEffectWorkspaceWrite},
		{"view,ssh", SideEffectExternalWrite},
		{"bash,ssh", SideEffectExternalWrite},      // ssh present, no sudo
		{"view,sudo", SideEffectInfraMutation},     // sudo highest
		{"bash,ssh,sudo", SideEffectInfraMutation}, // sudo wins
		{"edit,ssh,write", SideEffectExternalWrite},
	}
	for _, tt := range tests {
		got := InferSideEffectClass(tt.tools)
		if got != tt.want {
			t.Errorf("InferSideEffectClass(%q) = %q, want %q", tt.tools, got, tt.want)
		}
	}
}
