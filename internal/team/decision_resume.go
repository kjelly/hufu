package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

type DecisionResumeInfo struct {
	LogicalRunID         string
	BranchID             string
	TeamName             string
	Question             string
	ProfileRef           string
	Generation           uint32
	ExistingBinding      *PrimaryBindingV1
	ExistingBindingEvent *string
}

// InspectDecisionResume resolves immutable resume identity without creating
// workspace files or consulting current team configuration.
func InspectDecisionResume(ctx context.Context, workspace, logicalRunID string) (DecisionResumeInfo, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || !decisionLogicalIDPattern.MatchString(logicalRunID) {
		return DecisionResumeInfo{}, fmt.Errorf("decision resume requires an exact workspace and logical run id")
	}
	eventPath := filepath.Join(workspace, logsDir, eventStoreFile)
	if _, err := os.Stat(eventPath); err != nil {
		return DecisionResumeInfo{}, fmt.Errorf("decision resume event store: %w", err)
	}
	store, err := OpenEventStore(workspace)
	if err != nil {
		return DecisionResumeInfo{}, err
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return DecisionResumeInfo{}, err
	}
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return DecisionResumeInfo{}, err
	}
	branchID := tree.ActiveBranch
	projection, err := ReplayLogicalDecisionRun(events, logicalRunID, branchID)
	if err != nil {
		return DecisionResumeInfo{}, err
	}
	var opened DecisionRunOpenedPayload
	for _, event := range events {
		if event.BranchID != branchID || EventType(event.Type) != EventDecisionRunOpened {
			continue
		}
		var payload DecisionRunOpenedPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.LogicalRunID == logicalRunID {
			opened = payload
			break
		}
	}
	if opened.LogicalRunID == "" {
		return DecisionResumeInfo{}, fmt.Errorf("decision resume opening event is unavailable")
	}
	artifacts, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		return DecisionResumeInfo{}, err
	}
	readJSON := func(ref DecisionArtifactRef, target any) error {
		artifact, resolveErr := resolveDecisionArtifactRef(ctx, artifacts, ref, "decision resume artifact")
		if resolveErr != nil {
			return resolveErr
		}
		reader, openErr := artifacts.Open(ctx, artifact.ID)
		if openErr != nil {
			return openErr
		}
		defer func() { _ = reader.Close() }()
		data, readErr := io.ReadAll(reader)
		if readErr != nil {
			return readErr
		}
		return json.Unmarshal(data, target)
	}
	var requirement struct {
		Question string `json:"question"`
	}
	if err := readJSON(opened.RequirementRef, &requirement); err != nil {
		return DecisionResumeInfo{}, err
	}
	var profileValue map[string]any
	if err := readJSON(opened.ProfileBundleRef, &profileValue); err != nil {
		return DecisionResumeInfo{}, err
	}
	profileRef, _ := profileValue["ref"].(string)
	bundle, err := agent.ResolveBuiltInDecisionProfileBundle(profileRef)
	var builtInValue map[string]any
	if err == nil {
		err = json.Unmarshal(bundle.RawJSON, &builtInValue)
	}
	actualCanonical, actualErr := CanonicalDecisionJSON(profileValue)
	builtInCanonical, builtInErr := CanonicalDecisionJSON(builtInValue)
	if err != nil || actualErr != nil || builtInErr != nil || !bytes.Equal(actualCanonical, builtInCanonical) {
		return DecisionResumeInfo{}, fmt.Errorf("decision resume profile bundle is not a trusted immutable V2 bundle")
	}
	var authority struct {
		TeamName string `json:"team_name"`
	}
	if err := readJSON(opened.AuthoritySnapshotRef, &authority); err != nil {
		return DecisionResumeInfo{}, err
	}
	if strings.TrimSpace(authority.TeamName) == "" {
		return DecisionResumeInfo{}, fmt.Errorf("decision resume authority snapshot has no team name")
	}
	info := DecisionResumeInfo{
		LogicalRunID: logicalRunID, BranchID: branchID, TeamName: authority.TeamName, Question: requirement.Question,
		ProfileRef: profileRef, Generation: projection.CurrentGeneration, ExistingBinding: clonePrimaryBinding(projection.ActivePrimary),
	}
	if projection.ActivePrimary != nil {
		if eventID := primaryBindingEventID(events, *projection.ActivePrimary); eventID != "" {
			info.ExistingBindingEvent = &eventID
		} else {
			return DecisionResumeInfo{}, fmt.Errorf("decision resume primary binding event is unavailable")
		}
	}
	return info, nil
}
