package webresearch_test

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"os"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestReportSubmissionVersionsPreserveLegacyContract(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		raw, err := os.ReadFile("../schemas/report-" + version + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := team.CompileResultContractDocument("schemas/report-"+version+".json", raw); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		owned, _ := doc["x-hufu-evidence-inputs"].(map[string]any)["runtime_owned"].(bool)
		if owned != (version == "v2") {
			t.Fatalf("%s evidence ownership changed", version)
		}
		fields, _ := doc["x-hufu-submission-fields"].([]any)
		if version == "v2" && len(fields) != 4 || version == "v1" && fields != nil {
			t.Fatalf("%s submission fields: %v", version, fields)
		}
	}
}

// The new pipeline keeps the historical documents immutable; it does not
// reinterpret their stored payloads as the new audit/synthesis contracts.
func TestLegacyReportSchemasRemainUnchanged(t *testing.T) {
	hashes := map[string]string{
		"v1": "e27b074a1045b91e9062f5770c3128f07aeafdd12312da2af25728ee7617963a",
		"v2": "12968af5fb09afbbd791159791e19168cbd31c7b29341a1c82cb4d89b4120833",
	}
	for version, want := range hashes {
		raw, err := os.ReadFile("../schemas/report-" + version + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
			t.Fatalf("historical %s schema changed: %s", version, got)
		}
	}
}
