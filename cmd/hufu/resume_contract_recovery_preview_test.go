package main

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestContractRecoveryPreviewFormats(t *testing.T) {
	plan := &team.ContractRecoveryPlan{SourceBranch: "source-branch", SourceHead: "source-event", MetadataOnly: true, InputPolicyUpgrade: &team.RunInputPolicyRecovery{SnapshotID: "frozen-snapshot", SnapshotSHA256: "snapshot-sha", OriginalSHA256: "old-policy", AcceptedSHA256: "new-policy"}, Retained: 1, Retried: 2, SHA256: strings.Repeat("a", 64), ResumePhase: team.PhaseVerify, Tasks: []team.ContractRecoveryTask{
		{ID: "1", Action: "retain", Reason: "result_revalidated", SourceStatus: team.TaskDone},
		{ID: "2", Action: "retry", Reason: "original_failure", SourceStatus: team.TaskBlocked},
		{ID: "3", Action: "retry", Reason: "upstream_changed", AffectedBy: []string{"2"}, SourceStatus: team.TaskDone},
	}, ContractChanges: []team.ContractRecoveryChange{{Owner: "contract:review", Original: team.ResultContractRef{ID: "old.json"}, Accepted: team.ResultContractRef{ID: "new.json"}}}, ReopenedAcceptance: []string{"criterion:report"}, ReopenedVerification: []string{"3"}}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeContractRecoveryPreview(&out, plan, format); err != nil {
				t.Fatal(err)
			}
			if format == "json" {
				var decoded team.ContractRecoveryPlan
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(*plan, decoded) {
					t.Fatal("JSON lost plan details")
				}
			} else {
				for _, want := range []string{"source-branch", "source-event", "project files remain shared", "old-policy -> new-policy", "frozen-snapshot", "retain 1; retry 2", "original failures: 1", "affected downstream: 1", "original failed or blocked task", "affected by: 2", "old.json", "new.json", "criterion:report", `Task "3"`, plan.SHA256, "no tasks executed"} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("missing %q: %s", want, out.String())
					}
				}
			}
		})
	}
}

func TestResumeContractRecoveryWorkRetryPreview(t *testing.T) {
	var plan team.ContractRecoveryPlan
	if err := json.Unmarshal([]byte(`{"retained":31,"retried":1,"work_retry":{"version":1,"task_id":"32","missing_decisions":[{}]}}`), &plan); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeContractRecoveryPreview(&out, &plan, format); err != nil {
				t.Fatal(err)
			}
			if format == "json" {
				var replay team.ContractRecoveryPlan
				if err := json.Unmarshal(out.Bytes(), &replay); err != nil {
					t.Fatal("JSON lost work retry proof", err)
				}
				before, _ := json.Marshal(plan, json.Deterministic(true))
				after, _ := json.Marshal(replay, json.Deterministic(true))
				if string(before) != string(after) {
					t.Fatal("JSON changed work retry proof")
				}
				return
			}
			for _, want := range []string{"Read-only work recovery preview", "retain 31; retry 1", `task "32"`, "missing required decisions: 1", "old work and decisions remain", "no tasks executed"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q: %s", want, out.String())
				}
			}
		})
	}
}
