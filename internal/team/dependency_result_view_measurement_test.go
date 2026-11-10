package team

import (
	"encoding/json/v2"
	"os"
	"testing"
)

// The counts were measured offline with the named tokenizer, not the runtime's
// estimator. Pin the exact fixture bytes so stale measurements cannot pass CI.
func TestDependencyResultViewMeasuredFixture(t *testing.T) {
	var measurement struct {
		Tokenizer string                 `json:"tokenizer"`
		Version   string                 `json:"tokenizer_version"`
		Encoding  string                 `json:"encoding"`
		Fields    []string               `json:"fields"`
		Full      measuredDependencyView `json:"full"`
		Selected  measuredDependencyView `json:"selected"`
	}
	data, err := os.ReadFile("testdata/dependency-result-view/token-measurement.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &measurement); err != nil {
		t.Fatal(err)
	}
	if measurement.Tokenizer != "tiktoken" || measurement.Version == "" || measurement.Encoding != "cl100k_base" {
		t.Fatal("measurement tokenizer provenance is missing")
	}
	result, source := dependencyViewRenderFixture()
	view, err := buildDependencyResultView(&DependencyResultViewSpec{Mode: "selected", Fields: measurement.Fields}, []TaskResult{result}, []DependencyResultViewSource{source})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, text string
		measured   measuredDependencyView
	}{
		{"full", FormatDependencyResults([]TaskResult{result}), measurement.Full},
		{"selected", view.content, measurement.Selected},
	} {
		if len(test.text) != test.measured.Bytes || dependencyViewDigest([]byte(test.text)) != test.measured.SHA256 {
			t.Fatalf("%s fixture changed; repeat the offline tokenizer measurement", test.name)
		}
	}
	if measurement.Selected.Bytes >= measurement.Full.Bytes || measurement.Selected.Tokens <= 0 || measurement.Selected.Tokens >= measurement.Full.Tokens {
		t.Fatal("measured selected view does not reduce both bytes and tokens")
	}
}

type measuredDependencyView struct {
	Bytes  int    `json:"bytes"`
	Tokens int    `json:"tokens"`
	SHA256 string `json:"sha256"`
}
