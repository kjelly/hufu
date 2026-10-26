package inspect

import "github.com/kjelly/hufu/internal/team"

type ResultCandidateData struct {
	Prepared   bool   `json:"prepared"`
	Version    int    `json:"version"`
	ArtifactID string `json:"artifact_id"`
	SHA256     string `json:"sha256"`
	IssueCount int    `json:"issue_count"`
}

func projectResultCandidate(candidate *team.ResultCandidate, attempt int) *ResultCandidateData {
	if candidate == nil || attempt != 0 && attempt != candidate.Attempt {
		return nil
	}
	return &ResultCandidateData{Prepared: candidate.Prepared, Version: candidate.Version, ArtifactID: safeOpaqueRef(candidate.ArtifactID), SHA256: safeOpaqueRef(candidate.SHA256), IssueCount: len(candidate.Issues)}
}
