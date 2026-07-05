package team

import (
	"reflect"
	"testing"
)

func TestExpandPipelineDeps(t *testing.T) {
	cases := []struct {
		name  string
		tasks []TaskDef
		want  [][]int // expected DependsOn per task
	}{
		{
			name:  "no pipeline flags unchanged",
			tasks: []TaskDef{{Agent: "a"}, {Agent: "b", DependsOn: []int{0}}},
			want:  [][]int{nil, {0}},
		},
		{
			name:  "linear chain",
			tasks: []TaskDef{{Agent: "a"}, {Agent: "b", Pipeline: true}, {Agent: "c", Pipeline: true}},
			want:  [][]int{nil, {0}, {1}},
		},
		{
			name:  "pipeline on first task ignored",
			tasks: []TaskDef{{Agent: "a", Pipeline: true}, {Agent: "b", Pipeline: true}},
			want:  [][]int{nil, {0}},
		},
		{
			name:  "union with explicit depends_on",
			tasks: []TaskDef{{Agent: "a"}, {Agent: "b"}, {Agent: "c", Pipeline: true, DependsOn: []int{0}}},
			want:  [][]int{nil, nil, {0, 1}},
		},
		{
			name:  "explicit dep already covers previous task",
			tasks: []TaskDef{{Agent: "a"}, {Agent: "b", Pipeline: true, DependsOn: []int{0}}},
			want:  [][]int{nil, {0}},
		},
		{
			name: "mixed parallel and chain",
			tasks: []TaskDef{
				{Agent: "a"},
				{Agent: "b"},
				{Agent: "c", Pipeline: true},
			},
			want: [][]int{nil, nil, {1}},
		},
		{
			name:  "duplicate explicit deps deduplicated",
			tasks: []TaskDef{{Agent: "a"}, {Agent: "b", Pipeline: true, DependsOn: []int{0, 0}}},
			want:  [][]int{nil, {0}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandPipelineDeps(tc.tasks)
			if len(got) != len(tc.tasks) {
				t.Fatalf("expected %d tasks, got %d", len(tc.tasks), len(got))
			}
			for i := range got {
				want := tc.want[i]
				if len(want) == 0 && len(got[i].DependsOn) == 0 {
					continue
				}
				if !reflect.DeepEqual(got[i].DependsOn, want) {
					t.Errorf("task %d: DependsOn = %v, want %v", i, got[i].DependsOn, want)
				}
			}
			if detectTaskCycle(got) {
				t.Errorf("expanded tasks contain a dependency cycle")
			}
		})
	}
}

func TestExpandPipelineDepsDoesNotMutateInput(t *testing.T) {
	orig := []TaskDef{
		{Agent: "a"},
		{Agent: "b", Pipeline: true, DependsOn: []int{0}},
	}
	origDeps := orig[1].DependsOn

	got := expandPipelineDeps(orig)

	if !reflect.DeepEqual(orig[1].DependsOn, []int{0}) {
		t.Errorf("input DependsOn mutated: %v", orig[1].DependsOn)
	}
	// The output must use a fresh backing array, never alias the input's.
	got[1].DependsOn[0] = 99
	if origDeps[0] != 0 {
		t.Errorf("output aliases input backing array")
	}
}
