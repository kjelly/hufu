package webresearch_test

import (
	"encoding/json/v2"
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
