package main

import "testing"

func TestScaffoldTemplatesHaveExpectedAgents(t *testing.T) {
	tests := []struct {
		name  string
		files []string
	}{
		{name: "default", files: []string{"worker.md"}},
		{name: "dev", files: []string{"developer.md", "reviewer.md", "tester.md"}},
		{name: "research", files: []string{"researcher.md", "writer.md"}},
		{name: "ops", files: []string{"operator.md", "monitor.md"}},
		{name: "minimal", files: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := scaffoldTemplates[tt.name]
			if len(template.agents) != len(tt.files) {
				t.Fatalf("agent count = %d, want %d", len(template.agents), len(tt.files))
			}
			for _, filename := range tt.files {
				if _, ok := template.agents[filename]; !ok {
					t.Errorf("missing %s", filename)
				}
			}
		})
	}
}
