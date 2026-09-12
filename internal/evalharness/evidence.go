package evalharness

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/team"
)

func assertEvidence(ctx context.Context, workspace string, expect *EvidenceExpect, result *team.RunResult, tasks []*team.TodoItem) []EvalFinding {
	if expect == nil {
		return nil
	}
	var manifest *team.EvidenceManifest
	if result != nil {
		manifest = result.EvidenceManifest
	}
	exists := manifest != nil
	var findings []EvalFinding
	if expect.ManifestExists != nil && exists != *expect.ManifestExists {
		findings = append(findings, EvalFinding{Dimension: "evidence.manifest-exists", Expected: fmt.Sprintf("%t", *expect.ManifestExists), Actual: fmt.Sprintf("%t", exists)})
	}
	if manifest == nil {
		if evidenceNeedsManifest(expect) && (expect.ManifestExists == nil || *expect.ManifestExists) {
			findings = append(findings, EvalFinding{Dimension: "evidence.manifest", Expected: "present for configured evidence assertions", Actual: "missing"})
		}
		return findings
	}

	if expect.Status != "" && manifest.Status != expect.Status {
		findings = append(findings, EvalFinding{Dimension: "evidence.status", Expected: expect.Status, Actual: manifest.Status})
	}
	if expect.HashValid != nil {
		valid, reason := verifyManifest(ctx, workspace, manifest)
		if valid != *expect.HashValid {
			findings = append(findings, EvalFinding{Dimension: "evidence.hash-valid", Expected: fmt.Sprintf("%t", *expect.HashValid), Actual: fmt.Sprintf("%t (%s)", valid, reason)})
		}
	}
	if expect.MinArtifactRefs != nil && len(manifest.ArtifactRefs) < *expect.MinArtifactRefs {
		findings = append(findings, EvalFinding{Dimension: "evidence.artifact-refs", Expected: fmt.Sprintf(">= %d", *expect.MinArtifactRefs), Actual: fmt.Sprintf("%d", len(manifest.ArtifactRefs))})
	}
	findings = append(findings, assertEvidenceResults(expect.RequiredResults, manifest, tasks)...)
	findings = append(findings, assertArtifactRefs(expect.RequiredArtifactRefs, manifest.ArtifactRefs, tasks)...)
	return findings
}

func evidenceNeedsManifest(expect *EvidenceExpect) bool {
	return expect != nil && (expect.HashValid != nil || expect.Status != "" || expect.MinArtifactRefs != nil || len(expect.RequiredResults) > 0 || len(expect.RequiredArtifactRefs) > 0)
}

func verifyManifest(ctx context.Context, workspace string, manifest *team.EvidenceManifest) (bool, string) {
	store, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		return false, err.Error()
	}
	if err := manifest.Verify(ctx, store); err != nil {
		return false, err.Error()
	}
	return true, "verified"
}

func assertEvidenceResults(expects []EvidenceResultExpect, manifest *team.EvidenceManifest, tasks []*team.TodoItem) []EvalFinding {
	var findings []EvalFinding
	for index, expect := range expects {
		requirementID := expect.RequirementID
		if expect.TaskIndex != nil {
			if *expect.TaskIndex < 0 || *expect.TaskIndex >= len(tasks) || tasks[*expect.TaskIndex] == nil {
				findings = append(findings, EvalFinding{Dimension: fmt.Sprintf("evidence.required-results[%d].task-index", index), Expected: fmt.Sprintf("task at index %d", *expect.TaskIndex), Actual: fmt.Sprintf("%d task(s)", len(tasks))})
				continue
			}
			requirementID = "task:" + tasks[*expect.TaskIndex].ID
		}
		var actual *team.EvidenceResult
		for resultIndex := range manifest.EvidenceResults {
			if manifest.EvidenceResults[resultIndex].RequirementID == requirementID {
				actual = &manifest.EvidenceResults[resultIndex]
				break
			}
		}
		dimension := fmt.Sprintf("evidence.required-results[%d]", index)
		if actual == nil {
			findings = append(findings, EvalFinding{Dimension: dimension, Expected: "requirement " + requirementID, Actual: "missing"})
			continue
		}
		if expect.Status != "" && !strings.EqualFold(actual.Status, expect.Status) {
			findings = append(findings, EvalFinding{Dimension: dimension + ".status", Expected: expect.Status, Actual: actual.Status})
		}
		if expect.MinArtifactRefs != nil && len(actual.ArtifactRefs) < *expect.MinArtifactRefs {
			findings = append(findings, EvalFinding{Dimension: dimension + ".artifact-refs", Expected: fmt.Sprintf(">= %d", *expect.MinArtifactRefs), Actual: fmt.Sprintf("%d", len(actual.ArtifactRefs))})
		}
	}
	return findings
}

func assertArtifactRefs(expects []ArtifactRefExpect, refs []team.ArtifactRef, tasks []*team.TodoItem) []EvalFinding {
	var findings []EvalFinding
	for index, expect := range expects {
		taskID := ""
		if expect.TaskIndex != nil {
			if *expect.TaskIndex < 0 || *expect.TaskIndex >= len(tasks) || tasks[*expect.TaskIndex] == nil {
				findings = append(findings, EvalFinding{Dimension: fmt.Sprintf("evidence.required-artifact-refs[%d].task-index", index), Expected: fmt.Sprintf("task at index %d", *expect.TaskIndex), Actual: fmt.Sprintf("%d task(s)", len(tasks))})
				continue
			}
			taskID = tasks[*expect.TaskIndex].ID
		}
		count := 0
		for _, ref := range refs {
			if expect.Kind != "" && ref.Kind != expect.Kind {
				continue
			}
			if taskID != "" && ref.TaskID != taskID {
				continue
			}
			count++
		}
		if count < expect.MinCount {
			findings = append(findings, EvalFinding{Dimension: fmt.Sprintf("evidence.required-artifact-refs[%d]", index), Expected: fmt.Sprintf(">= %d matching ref(s)", expect.MinCount), Actual: fmt.Sprintf("%d", count)})
		}
	}
	return findings
}

func assertAudit(ctx context.Context, workspace, expectedVerdict, runID string) []EvalFinding {
	if expectedVerdict == "" {
		return nil
	}
	result, err := auditverify.VerifyWorkspaceRun(ctx, workspace, runID, auditverify.VerifyOptions{})
	if err != nil {
		return []EvalFinding{{Dimension: "audit-verdict", Expected: expectedVerdict, Actual: "error: " + err.Error()}}
	}
	actual := string(result.Verdict)
	if actual == expectedVerdict {
		return nil
	}
	return []EvalFinding{{Dimension: "audit-verdict", Expected: expectedVerdict, Actual: actual}}
}
