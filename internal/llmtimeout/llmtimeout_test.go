package llmtimeout

import (
	"testing"
	"time"
)

func TestCategoriesFallBackUntilConfigured(t *testing.T) {
	t.Cleanup(func() { current.Store(nil) })
	if got := Review(60 * time.Second); got != 60*time.Second {
		t.Fatalf("unconfigured review = %s, want the call's default", got)
	}
	if err := Configure(Settings{Review: 3 * time.Minute, Provider: 20 * time.Second}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"review set", Review(60 * time.Second), 3 * time.Minute},
		{"provider set", Provider(5 * time.Second), 20 * time.Second},
		{"sidecar unset", Sidecar(5 * time.Second), 5 * time.Second},
		{"decision unset", DecisionMax(30 * time.Second), 30 * time.Second},
	} {
		if test.got != test.want {
			t.Errorf("%s = %s, want %s", test.name, test.got, test.want)
		}
	}
}

func TestConfigureRejectsOutOfRangeValues(t *testing.T) {
	t.Cleanup(func() { current.Store(nil) })
	for _, settings := range []Settings{{Decision: -time.Second}, {Sidecar: 2 * time.Hour}} {
		if err := Configure(settings); err == nil {
			t.Errorf("Configure(%+v) accepted an out-of-range value", settings)
		}
	}
	if got := DecisionMax(30 * time.Second); got != 30*time.Second {
		t.Fatalf("rejected settings changed the decision limit to %s", got)
	}
}

func TestMergeOverridesOnlySetFields(t *testing.T) {
	base := Settings{Decision: 45 * time.Second, Review: 2 * time.Minute}
	got := base.Merge(Settings{Review: 5 * time.Minute, Sidecar: 15 * time.Second})
	want := Settings{Decision: 45 * time.Second, Review: 5 * time.Minute, Sidecar: 15 * time.Second}
	if got != want {
		t.Fatalf("merged = %+v, want %+v", got, want)
	}
}
