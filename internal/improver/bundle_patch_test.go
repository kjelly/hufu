package improver

import (
	"bytes"
	"testing"
)

func TestBundlePatchRoundTripPreservesExactText(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
	}{
		{"worker.md", "old\n", "new\n"},
		{"no-newline.md", "old", "new"},
		{"changed-newline.md", "old\n", "new"},
		{"added-newline.md", "old", "new\n"},
		{"empty.md", "", "new\n"},
		{"space and \"quote\".md", "old\r\n", "new\r\n"},
		{"中文.md", "-- a/trap\n++ b/trap\n", "updated\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline := Tree{files: map[string][]byte{tc.name: []byte(tc.before)}}
			candidate := Tree{files: map[string][]byte{tc.name: []byte(tc.after)}}
			patch, err := exactPatch(t.Context(), baseline, candidate)
			if err != nil {
				t.Fatal(err)
			}
			applied, err := applyPatchTree(t.Context(), baseline, patch)
			if err != nil || !bytes.Equal(applied.files[tc.name], []byte(tc.after)) {
				t.Fatalf("round-trip mismatch: %v", err)
			}
		})
	}
}

func TestBundlePatchRejectsDriftAndReproducesYAMLVariant(t *testing.T) {
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := ApplyProposal(prepared, testChangeProposal(prepared, "Return the category exactly.\n"))
	if err != nil {
		t.Fatal(err)
	}
	patch, err := exactPatch(t.Context(), prepared.Variants["baseline"].Tree, candidate.Tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPatchRoundTrip(t.Context(), prepared, candidate, patch); err != nil {
		t.Fatal(err)
	}
	bad := bytes.Replace(patch, []byte("Return the category exactly."), []byte("Different output."), 1)
	if err := verifyPatchRoundTrip(t.Context(), prepared, candidate, bad); err == nil {
		t.Fatal("different candidate patch passed round-trip")
	}
	drifted := Tree{files: map[string][]byte{"team.yaml": []byte("drifted\n")}}
	if _, err := applyPatchTree(t.Context(), drifted, patch); err == nil {
		t.Fatal("patch applied to a different baseline")
	}
}
