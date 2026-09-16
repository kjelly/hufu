package team

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDecisionRuntimeDoesNotBranchOnProfileDisplayNames(t *testing.T) {
	files, err := filepath.Glob("decision*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"light", "standard", "high-stakes", "strategic", "debiasing"} {
			if strings.Contains(string(content), strconv.Quote(name)) {
				t.Errorf("production decision runtime %s contains profile-name literal %q", path, name)
			}
		}
	}
}
