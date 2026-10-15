package team

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/belief"
)

// ExecutionBeliefSnapshot pins the model and adapter in the existing policy
// snapshot. Absence preserves the configuration hash of teams without belief.
type ExecutionBeliefSnapshot struct {
	ModelDigest    string `json:"model_digest"`
	ReducerVersion string `json:"reducer_version"`
	ExperimentID   string `json:"experiment_id"`
}

// CompiledBelief is process-local, immutable after team loading. Replay reads
// the bound artifact instead; it never uses this as a replacement for history.
type CompiledBelief struct {
	model    belief.Model
	snapshot ExecutionBeliefSnapshot
}

func executionPolicyBelief(session *TeamSession) *ExecutionBeliefSnapshot {
	if session == nil || session.Belief == nil {
		return nil
	}
	return new(session.Belief.snapshot)
}

func compileTeamBelief(session *TeamSession) error {
	if session.Config.Belief == nil {
		session.Belief = nil
		return nil
	}
	config := session.Config.Belief
	if !filepath.IsLocal(config.ModelFile) || strings.TrimSpace(config.Experiment) == "" {
		return fmt.Errorf("belief requires a team-relative model-file and an experiment")
	}
	root, err := os.OpenRoot(session.Dir)
	if err != nil {
		return fmt.Errorf("belief model directory unavailable: %w", err)
	}
	defer func() { _ = root.Close() }()
	// Root confines symlinks and directory traversal even if the filesystem
	// changes after validation. Only regular, bounded resources are admitted.
	file, err := root.Open(config.ModelFile)
	if err != nil {
		return fmt.Errorf("belief model unavailable: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > belief.MaxModelBytes {
		return fmt.Errorf("belief model must be a regular file within the model size limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, belief.MaxModelBytes+1))
	if err != nil {
		return fmt.Errorf("belief model read failed: %w", err)
	}
	model, err := belief.DecodeModel(data)
	if err != nil {
		return fmt.Errorf("belief model: %w", err)
	}
	if err := validateBeliefVerificationExperiment(model, config.Experiment); err != nil {
		return err
	}
	digest, err := belief.ModelDigest(model)
	if err != nil {
		return err
	}
	session.Belief = &CompiledBelief{model: model, snapshot: ExecutionBeliefSnapshot{ModelDigest: digest, ReducerVersion: belief.ReducerVersion, ExperimentID: config.Experiment}}
	return nil
}

func validateBeliefVerificationExperiment(model belief.Model, experimentID string) error {
	index := slices.IndexFunc(model.Experiments, func(experiment belief.Experiment) bool { return experiment.ID == experimentID })
	if index < 0 {
		return fmt.Errorf("belief verification experiment is not declared")
	}
	experiment := model.Experiments[index]
	if experiment.Source != "verification" || len(experiment.Outcomes) != 2 || !slices.ContainsFunc(experiment.Outcomes, func(outcome belief.Outcome) bool { return outcome.ID == "pass" }) || !slices.ContainsFunc(experiment.Outcomes, func(outcome belief.Outcome) bool { return outcome.ID == "fail" }) {
		return fmt.Errorf("belief verification experiment must declare exactly pass and fail outcomes")
	}
	return nil
}

func validateExecutionBeliefSnapshot(snapshot *ExecutionBeliefSnapshot) error {
	if snapshot == nil {
		return nil
	}
	if !decisionDigestPattern.MatchString(snapshot.ModelDigest) || snapshot.ReducerVersion != belief.ReducerVersion || strings.TrimSpace(snapshot.ExperimentID) == "" {
		return fmt.Errorf("invalid belief execution policy binding")
	}
	return nil
}
