package team

import (
	"reflect"
	"testing"
)

func TestNearestModelNames(t *testing.T) {
	available := map[string]bool{
		"glm-5.2:cloud":      true,
		"glm-5.2:local":      true,
		"qwen3:8b":           true,
		"minimax-m2.7:cloud": true,
	}
	cases := []struct {
		name    string
		missing string
		want    []string
	}{
		{"missing tag colon", "glm-5.2cloud", []string{"glm-5.2:cloud", "glm-5.2:local"}},
		{"wrong tag", "qwen3:70b", []string{"qwen3:8b"}},
		{"no match", "llama9:1b", nil},
		{"too short", "g", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nearestModelNames(tc.missing, available, 3)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("suggestions = %v, want %v", got, tc.want)
			}
		})
	}
}
