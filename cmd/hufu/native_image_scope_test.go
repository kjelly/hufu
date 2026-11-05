package main

import (
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestNativeImageRunFlagsRequireExplicitExecutionScope(t *testing.T) {
	for _, test := range []struct {
		name    string
		options runOptions
		allowed bool
	}{
		{"text auto selection", runOptions{autoTeam: true}, true},
		{"explicit team", runOptions{agentTeamName: "fixture", imagePaths: []string{"a.png"}}, true},
		{"default team", runOptions{defaultTeam: true, imagePaths: []string{"a.png", "a.png"}}, true},
		{"missing team", runOptions{imagePaths: []string{"a.png"}}, false},
		{"automatic team", runOptions{autoTeam: true, agentTeamName: "fixture", imagePaths: []string{"a.png"}}, false},
		{"fast route", runOptions{agentTeamName: "fixture", routeMode: "fast", imagePaths: []string{"a.png"}}, false},
		{"decision", runOptions{agentTeamName: "fixture", intent: "decision", imagePaths: []string{"a.png"}}, false},
		{"decision resume", runOptions{agentTeamName: "fixture", resumeDecision: "old", imagePaths: []string{"a.png"}}, false},
		{"memory archive", runOptions{agentTeamName: "fixture", archiveMemory: true, imagePaths: []string{"a.png"}}, false},
		{"empty image path", runOptions{agentTeamName: "fixture", imagePaths: []string{" "}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateNativeImageRunFlags(test.options); (err == nil) != test.allowed {
				t.Fatalf("scope allowed=%t err=%v", test.allowed, err)
			}
		})
	}
}

func TestNativeImageSegmentsRequireOneDirectWorker(t *testing.T) {
	selection := team.PromptSegment{Type: team.SegmentSwitchTeam, Name: "fixture"}
	worker := team.PromptSegment{Type: team.SegmentInvokeAgent, Name: "worker", Content: "Compare images."}
	for _, test := range []struct {
		name     string
		segments []team.PromptSegment
		allowed  bool
	}{
		{"direct worker", []team.PromptSegment{selection, worker}, true},
		{"empty selection is not execution", []team.PromptSegment{selection, selection, worker}, true},
		{"no task", []team.PromptSegment{selection}, false},
		{"empty worker task", []team.PromptSegment{selection, {Type: worker.Type, Name: worker.Name, Content: " "}}, false},
		{"coordinator task", []team.PromptSegment{{Type: selection.Type, Name: selection.Name, Content: worker.Content}}, false},
		{"text task", []team.PromptSegment{selection, {Type: team.SegmentText, Content: worker.Content}}, false},
		{"multiple workers", []team.PromptSegment{selection, worker, worker}, false},
		{"multiple teams", []team.PromptSegment{selection, {Type: selection.Type, Name: "other"}, worker}, false},
		{"pipe", []team.PromptSegment{selection, {Type: worker.Type, Name: worker.Name, Content: worker.Content, IsPiped: true}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateNativeImageSegments(test.segments); (err == nil) != test.allowed {
				t.Fatalf("segments allowed=%t err=%v", test.allowed, err)
			}
		})
	}
}

func TestCanonicalImageFlagsRetainOrderWithoutCrossCommandState(t *testing.T) {
	previous := opts
	opts.imagePaths = []string{"legacy.png"}
	t.Cleanup(func() { opts = previous })
	command, options := newRunCommandWithOptions()
	if err := command.ParseFlags([]string{"--team", "fixture", "--image", "a,b.png", "--image", "second.png", "--image", "a,b.png"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(options.imagePaths, []string{"a,b.png", "second.png", "a,b.png"}) || !slices.Equal(opts.imagePaths, []string{"legacy.png"}) {
		t.Fatal("image flag split commas, lost order/duplicates or mutated legacy command state")
	}
	_, next := newRunCommandWithOptions()
	if len(next.imagePaths) != 0 {
		t.Fatal("another canonical command inherited prior images")
	}
	if command := newDecideCommand(); command.Flags().Lookup("image") != nil {
		t.Fatal("decision command advertised unsupported image input")
	}
}
