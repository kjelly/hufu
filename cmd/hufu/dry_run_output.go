package main

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

func dryRunOutputWriter(command *cobra.Command) io.Writer {
	if opts.outputFormat == "json" || opts.eventFormat == "jsonl" || opts.intent == "decision" {
		return command.OutOrStdout()
	}
	return command.ErrOrStderr()
}

func writeDryRunJSON(writer io.Writer, result *team.DryRunResult, sources []roleModelSource) error {
	agents := make([]map[string]any, 0, len(result.Agents))
	for _, agent := range result.Agents {
		agents = append(agents, map[string]any{
			"name": agent.Name, "role": agent.Role, "model": agent.Model,
			"execution_target": agent.ExecutionTarget, "tools": agent.Tools, "skills": agent.Skills,
		})
	}
	skills := make([]map[string]string, 0, len(result.AllSkills))
	for _, skill := range result.AllSkills {
		skills = append(skills, map[string]string{"name": skill.Name, "description": skill.Description})
	}
	findings := make([]map[string]string, 0, len(result.ContractFindings))
	for _, finding := range result.ContractFindings {
		findings = append(findings, map[string]string{
			"severity": finding.Severity, "code": finding.Code, "field": finding.Field,
			"message": finding.Message, "hint": finding.Hint,
		})
	}
	roleTargets := map[string]any{}
	for _, role := range dryRunRoleTargets(result) {
		if role.key == "worker" && result.WorkerRoute != "" {
			roleTargets[role.key] = map[string]any{
				"route": result.WorkerRoute, "candidates": result.WorkerRouteCandidates,
				"source": roleSourceFor(sources, role.name),
			}
		} else if role.target != "" {
			roleTargets[role.key] = map[string]string{"target": role.target, "source": roleSourceFor(sources, role.name)}
		}
	}
	preview := map[string]any{
		"schema_version": 1, "dry_run": true, "team": result.TeamName, "prompt": result.UserPrompt,
		"model": result.Model, "role_targets": roleTargets, "execution_profile": result.ResolvedProfile,
		"subject_root": result.SubjectRoot, "workspace_would_create": result.WorkspaceWouldCreate,
		"request_acceptance_pending": result.RequestAcceptancePending,
		"agents":                     agents, "skills": skills, "matched_skill_names": result.MatchedSkillNames,
		"first_round_tasks": result.FirstRoundTasks, "contract_findings": findings, "warning": result.Error,
	}
	if result.ResolvedRunInputs != nil {
		preview["resolved_run_inputs"] = result.ResolvedRunInputs.Inputs
	}
	return writePreviewJSON(writer, preview)
}

type dryRunRoleTarget struct{ key, name, target string }

func dryRunRoleTargets(result *team.DryRunResult) []dryRunRoleTarget {
	return []dryRunRoleTarget{
		{"worker", "Worker", result.WorkerTarget},
		{"coordinator", "Coordinator", result.CoordinatorTarget},
		{"sidecar", "Sidecar", result.SidecarTarget},
		{"guard", "Guard", result.GuardTarget},
		{"judge", "Judge", result.JudgeTarget},
		{"plan_reviewer", "Plan reviewer", result.PlanReviewerTarget},
	}
}

func writeNativeImageDryRun(writer io.Writer, teamName string, preview team.NativeImagePreview) error {
	if opts.outputFormat == "json" {
		return writePreviewJSON(writer, map[string]any{
			"schema_version": 1, "dry_run": true, "team": teamName,
			"image_preview": map[string]any{
				"target": preview.Target.String(), "image_count": preview.ImageCount, "total_bytes": preview.TotalBytes,
				"model_image_capability": preview.ModelImageCapability, "metadata_only": preview.MetadataOnly,
			},
		})
	}
	if preview.MetadataOnly {
		_, err := fmt.Fprintf(writer, "Image preview: %d image(s), %d bytes, coordinator %s receives metadata only. Worker selection and vision capability remain unverified; no input has been admitted.\n", preview.ImageCount, preview.TotalBytes, preview.Target.String())
		return err
	}
	_, err := fmt.Fprintf(writer, "Image preview: %d image(s), %d bytes, target %s. Local validation only; model vision capability is unverified and no input has been admitted.\n", preview.ImageCount, preview.TotalBytes, preview.Target.String())
	return err
}

func writePreviewJSON(writer io.Writer, preview map[string]any) error {
	return json.MarshalEncode(jsontext.NewEncoder(writer), preview, json.Deterministic(true))
}
