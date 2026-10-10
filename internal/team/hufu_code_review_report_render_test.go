package team

import (
	"bytes"
	"strings"
	"testing"
)

func TestHufuCodeReviewReportRendersOptionalInvariantEvidence(t *testing.T) {
	contract, err := compileResultContractSchema(hufuCodeReviewTeamDir(t), "schemas/report-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	// This is a schema-valid report boundary fixture. Runtime evidence binding
	// independently verifies the actual source task IDs and payload hashes.
	const fixture = `{
		"audit_complete":true,"coverage":"complete","gap_resolutions":[],
		"scope":{"requested":{"kind":"last_n","history":"first_parent","head":"HEAD","count":10,"commit_type":"feat"},
			"requested_input_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"resolved":{"base":"base","head":"head","selected_commit_count":10,"available_commit_count":10,"history_exhausted":false,"repository_shallow":false},
			"observed_budget":{"changed_paths":1,"total_diff_bytes":1,"total_diff_lines":1,"workset_items":1},"satisfied":true},
		"test_verification":{"passed":true,"reviewed_revision":"head","base_revision":"head","command":["go","test","./internal/team"],"packages":["./internal/team"],
			"artifact_id":"sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","artifact_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"requested_input_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","exit_code":0,"timed_out":false,"output_truncated":false},
		"input_review":[{"task_id":"3","payload_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"record":{"kind":"review","review_revision":"head","workset_key":"unit-0000","lens":"general","findings":[],"gaps":[],"invariant_assessments":[]}}]
	}`
	for _, status := range []string{"preserved", "violated", "unknown"} {
		for _, evidence := range []string{"omitted", "empty", "present", "null"} {
			t.Run(status+"/"+evidence, func(t *testing.T) {
				value, err := decodeResultContractJSON([]byte(fixture))
				if err != nil {
					t.Fatal(err)
				}
				assessment := map[string]any{"invariant_id": "retained-invariant", "status": status, "summary": "retained assessment"}
				switch evidence {
				case "empty":
					assessment["missing_evidence"] = []any{}
				case "present":
					assessment["missing_evidence"] = []any{"missing reproduction"}
				case "null":
					assessment["missing_evidence"] = nil
				}
				source := value.(map[string]any)["input_review"].([]any)[0].(map[string]any)["record"].(map[string]any)
				source["invariant_assessments"] = []any{assessment}
				if err := contract.schema.Validate(value); err != nil {
					if evidence == "null" {
						return
					}
					t.Fatal(err)
				}
				if evidence == "null" {
					t.Fatal("null optional array unexpectedly passed the schema")
				}
				var report bytes.Buffer
				if err := contract.finalReport.Execute(&report, value); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(report.String(), "invariant retained-invariant："+status+"；retained assessment") {
					t.Fatalf("report lost the assessment: %s", report.String())
				}
				if strings.Contains(report.String(), "missing reproduction") != (evidence == "present") {
					t.Fatalf("report lost or invented missing evidence: %s", report.String())
				}
				delete(assessment, "summary")
				if err := contract.schema.Validate(value); err == nil {
					t.Fatal("missing required assessment summary passed the schema")
				}
				report.Reset()
				if err := contract.finalReport.Execute(&report, value); err == nil {
					t.Fatal("renderer silently accepted a missing required field")
				}
			})
		}
	}
}
