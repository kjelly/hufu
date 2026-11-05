package main

import (
	"bytes"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func TestApplyImageProfileKeepsCommandScopeAndPathCommas(t *testing.T) {
	for _, scenario := range []string{"root", "declared child", "unsupported child", "explicit child"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			writeHufuYAML(t, dir, "profiles:\n  p:\n    image: a,b.png\n")
			defer chdir(t, dir)()
			previous := opts
			t.Cleanup(func() { opts = previous })
			opts = runOptions{}
			root := &cobra.Command{Use: "hufu"}
			root.Flags().StringArrayVar(&opts.imagePaths, "image", nil, "")
			root.Flags().Lookup("image").Annotations = map[string][]string{profileCommandLocalFlag: {"true"}}
			command := root
			var childImages []string
			if scenario != "root" {
				command = &cobra.Command{Use: "child"}
				root.AddCommand(command)
				if scenario != "unsupported child" {
					command.Flags().StringArrayVar(&childImages, "image", nil, "")
				}
			}
			command.SetErr(new(bytes.Buffer))
			if scenario == "explicit child" {
				if err := command.ParseFlags([]string{"--image", "explicit.png", "--image", "explicit.png"}); err != nil {
					t.Fatal(err)
				}
			}
			err := applyNamedProfile(command, "p")
			if scenario == "unsupported child" {
				if err == nil || len(opts.imagePaths) != 0 {
					t.Fatal("unsupported command accepted an image profile through root state", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"a,b.png"}
			got := childImages
			if scenario == "root" {
				got = opts.imagePaths
			} else if len(opts.imagePaths) != 0 {
				t.Fatal("child image profile mutated root command input")
			}
			if scenario == "explicit child" {
				want = []string{"explicit.png", "explicit.png"}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("profile image values=%q want=%q", got, want)
			}
		})
	}
}
