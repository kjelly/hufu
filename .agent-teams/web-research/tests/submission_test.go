package webresearch_test

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
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

// Pin the original rendered v1 definition, not the template's source bytes.
// Old sessions must retain exactly the same prompt and contract reference.
func TestReportContractSelectionPreservesDefault(t *testing.T) {
	raw, err := os.ReadFile("../verifier.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", "v1", "v2", "unknown"} {
		t.Run("version_"+version, func(t *testing.T) {
			vars := map[string]string{"TEAM_NAME": "web-research"}
			if version != "" {
				vars["report_contract"] = version
			}
			rendered, err := team.RenderTemplateText(string(raw), "verifier.md", vars)
			if err != nil {
				t.Fatal(err)
			}
			if version == "" || version == "v1" {
				if got := fmt.Sprintf("%x", sha256.Sum256([]byte(rendered))); got != "9f12dc9cb71cf734e40e4094810cb2c0b5e2573cb3b4720b2cf727904661742c" {
					t.Fatalf("legacy definition changed: %s", got)
				}
			}
			if version == "v2" {
				for _, forbidden := range []string{`"input_review":`, "fill `input_review`", "Record each block's task ID", "submit the complete result directly", "report-v1.json"} {
					if strings.Contains(rendered, forbidden) {
						t.Fatalf("v2 still asks for old submission: %s", forbidden)
					}
				}
				if !strings.Contains(rendered, "schemas/report-v2.json") || !strings.Contains(rendered, "submit only local edits") {
					t.Fatal("v2 not selected")
				}
			}
			// Use the real loader: unsupported versions must not silently choose v1.
			session, loadErr := team.LoadTeam("..", vars, nil, nil)
			if version == "unknown" {
				if loadErr == nil {
					t.Fatal("unknown contract version accepted")
				}
				return
			}
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if session.Agents["verifier"] == nil {
				t.Fatal("verifier missing")
			}
		})
	}
}
