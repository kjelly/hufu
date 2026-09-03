package preset

import "testing"

func TestLookupIsDeterministic(t *testing.T) {
	for _, name := range []string{"readonly", "coding", "review", "research", "writer", "ops"} {
		first, ok := Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) not found", name)
		}
		for range 5 {
			again, ok := Lookup(name)
			if !ok || again.Name != first.Name || again.SideEffect != first.SideEffect || len(again.Tools) != len(first.Tools) {
				t.Fatalf("Lookup(%q) is not deterministic across repeated calls", name)
			}
		}
	}
}

func TestLookupIsCaseInsensitiveAndTrimmed(t *testing.T) {
	for _, input := range []string{"CODING", " Coding ", "coding"} {
		if _, ok := Lookup(input); !ok {
			t.Errorf("Lookup(%q) = not found, want the coding preset", input)
		}
	}
}

func TestLookupUnknownPresetNotFound(t *testing.T) {
	if _, ok := Lookup("does-not-exist"); ok {
		t.Fatal("Lookup(\"does-not-exist\") = found, want not found")
	}
}

// Security requirement (spec.md Specification 01 §6, §11 Rule 3): no
// built-in preset may grant sudo by default.
func TestNoPresetGrantsSudoByDefault(t *testing.T) {
	for _, name := range Names() {
		p, _ := Lookup(name)
		for _, tool := range p.Tools {
			if tool == "sudo" {
				t.Errorf("preset %q grants sudo by default; this is forbidden", name)
			}
		}
	}
}

func TestNamesIsSortedAndComplete(t *testing.T) {
	got := Names()
	want := []string{"coding", "ops", "readonly", "research", "review", "writer"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Fatalf("Names()[%d] = %q, want %q (Names() = %v)", i, got[i], name, got)
		}
	}
}
