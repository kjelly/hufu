package team

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var ErrHistoricalResultGrantDrift = errors.New("historical result source, scope, presentation or policy changed")

// PreparedHistoricalResultGrants is an immutable, read-only preflight result.
// Its content stays private to the runtime. Preparation neither persists a
// grant nor authorizes a task, artifact read or model call.
type PreparedHistoricalResultGrants struct {
	scope             HistoricalTaskScope
	policyFingerprint string
	grants            []HistoricalResultGrant
	presentations     []string
}

type historicalResultGrantRequest struct {
	recipient string
	resultID  string
}

// PrepareHistoricalResultGrants resolves explicit operator bindings before
// provider preparation. Every requested source is required; errors return no
// partial preflight. CLI entry eligibility and durable admission are separate
// runtime boundaries and must succeed before any of this content is delivered.
func (c *Coordinator) PrepareHistoricalResultGrants(ctx context.Context, bindings []string) (*PreparedHistoricalResultGrants, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := c.historicalResultGrantScope(ctx)
	if err != nil {
		return nil, err
	}
	requests, err := c.historicalResultGrantRequests(bindings)
	if err != nil {
		return nil, err
	}
	policy := c.ExecutionPolicySnapshot()
	if policy == nil {
		return nil, fmt.Errorf("historical result grants require a frozen execution policy")
	}
	if err := ValidateExecutionPolicySnapshot(policy); err != nil {
		return nil, err
	}
	if matches, err := c.executionPolicySnapshotMatchesCurrent(policy); err != nil || !matches {
		return nil, fmt.Errorf("%w: execution policy", ErrHistoricalResultGrantDrift)
	}
	reader := c.HistoricalTaskReader()
	if reader == nil {
		return nil, ErrHistoricalTaskReaderUnavailable
	}
	rows, err := reader.Load(ctx, scope)
	if err != nil {
		return nil, err
	}
	prepared := &PreparedHistoricalResultGrants{scope: scope, policyFingerprint: policy.ConfigurationHash}
	totalBytes := 0
	for _, request := range requests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source, err := exactHistoricalGrantSource(rows, request.resultID)
		if err != nil {
			return nil, err
		}
		grant, presentation, err := prepareHistoricalGrantSource(scope, request.recipient, source)
		if err != nil {
			return nil, err
		}
		if grant.PresentationBytes > maxHistoricalResultBytes-totalBytes {
			return nil, fmt.Errorf("historical result presentations exceed %d UTF-8 bytes; no partial delivery is permitted", maxHistoricalResultBytes)
		}
		totalBytes += grant.PresentationBytes
		prepared.grants = append(prepared.grants, grant)
		prepared.presentations = append(prepared.presentations, presentation)
	}
	return prepared, nil
}

func (c *Coordinator) historicalResultGrantScope(ctx context.Context) (HistoricalTaskScope, error) {
	if c == nil || c.session == nil || !c.session.Scope.Managed || c.session.Scope.ProjectID == "" {
		return HistoricalTaskScope{}, fmt.Errorf("historical result grants require the same managed control workspace")
	}
	if c.ExecutionProfile().DisableHistoricalTaskReuse {
		return HistoricalTaskScope{}, fmt.Errorf("historical result grants are disabled by the active execution profile")
	}
	if err := ValidateWorkspaceScope(c.session.Scope); err != nil {
		return HistoricalTaskScope{}, err
	}
	root, err := canonicalWorkspacePath(c.session.Workspace)
	if err != nil || root != c.session.Scope.ControlRoot {
		return HistoricalTaskScope{}, fmt.Errorf("historical result grant workspace does not match the control root")
	}
	tree, err := LoadSessionTreeContext(ctx, root)
	if err != nil {
		return HistoricalTaskScope{}, err
	}
	branch := c.recoveryBranch
	if store := c.EventStore(); branch == "" && store != nil {
		branch = store.BranchID()
	}
	branch = cmp.Or(branch, tree.ActiveBranch)
	name := strings.ToLower(strings.TrimSpace(c.session.Config.Name))
	if tree.Branches[branch] == nil || !historicalGrantCanonicalName(name) {
		return HistoricalTaskScope{}, fmt.Errorf("historical result grant team or branch scope is unavailable")
	}
	return HistoricalTaskScope{Workspace: root, Team: name, Branch: branch}, nil
}

func (c *Coordinator) historicalResultGrantRequests(bindings []string) ([]historicalResultGrantRequest, error) {
	unique := make(map[historicalResultGrantRequest]struct{})
	for _, binding := range bindings {
		name, resultID, ok := strings.Cut(binding, "=")
		name, resultID = strings.TrimSpace(name), strings.TrimSpace(resultID)
		if !ok || !historicalGrantIdentity(name) || strings.ContainsAny(name, "*?") ||
			!strings.HasPrefix(resultID, "res-") || !historicalGrantSHA256(strings.TrimPrefix(resultID, "res-")) {
			return nil, fmt.Errorf("historical result grants require worker=<full-result-id> without wildcards")
		}
		def, _, err := c.resolveAgentName(name)
		if err != nil {
			return nil, fmt.Errorf("resolve historical result recipient: %w", err)
		}
		role := strings.ToLower(strings.TrimSpace(def.Role))
		if role == "coordinator" || role == "orchestrator" {
			return nil, fmt.Errorf("historical results can only be granted to workers")
		}
		recipient := strings.ToLower(strings.TrimSpace(def.Name))
		if !historicalGrantCanonicalName(recipient) {
			return nil, fmt.Errorf("historical result recipient has no canonical identity")
		}
		unique[historicalResultGrantRequest{recipient, resultID}] = struct{}{}
		if len(unique) > maxHistoricalResultGrants {
			return nil, fmt.Errorf("historical result grants exceed %d unique recipient/result pairs", maxHistoricalResultGrants)
		}
	}
	if len(unique) == 0 {
		return nil, fmt.Errorf("historical result grants require at least one binding")
	}
	requests := make([]historicalResultGrantRequest, 0, len(unique))
	for request := range unique {
		requests = append(requests, request)
	}
	slices.SortFunc(requests, func(a, b historicalResultGrantRequest) int {
		if n := strings.Compare(a.recipient, b.recipient); n != 0 {
			return n
		}
		return strings.Compare(a.resultID, b.resultID)
	})
	return requests, nil
}

func exactHistoricalGrantSource(rows []HistoricalTaskResult, resultID string) (HistoricalTaskResult, error) {
	var source *HistoricalTaskResult
	for i := range rows {
		if rows[i].ResultID != resultID {
			continue
		}
		if source != nil {
			return HistoricalTaskResult{}, fmt.Errorf("historical result has ambiguous canonical identity")
		}
		source = &rows[i]
	}
	if source == nil {
		return HistoricalTaskResult{}, fmt.Errorf("historical result is not visible in this team's selected branch lineage")
	}
	return *source, nil
}

func (p *PreparedHistoricalResultGrants) envelope(sessionID, cycleID string) (HistoricalResultGrantsPayload, error) {
	if p == nil {
		return HistoricalResultGrantsPayload{}, fmt.Errorf("historical result preflight is unavailable")
	}
	payload := HistoricalResultGrantsPayload{SchemaVersion: historicalResultGrantsVersion,
		Workspace: p.scope.Workspace, Team: p.scope.Team, BranchID: p.scope.Branch, SessionID: sessionID,
		CycleID: cycleID, PolicyFingerprint: p.policyFingerprint, Grants: slices.Clone(p.grants)}
	data, err := json.Marshal(payload)
	if err != nil {
		return HistoricalResultGrantsPayload{}, err
	}
	return decodeHistoricalResultGrants(RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventHistoricalResultsGranted),
		RunID: cycleID, SessionID: sessionID, BranchID: p.scope.Branch, Actor: "operator", IdempotencyKey: "historical-grants:" + cycleID, Payload: data})
}

// Rebuild from the frozen envelope, never by replacing hashes with newer
// source data. The caller must first authenticate its canonical grant event.
func (c *Coordinator) rebuildHistoricalResultGrants(ctx context.Context, payload HistoricalResultGrantsPayload) (*PreparedHistoricalResultGrants, error) {
	bindings := make([]string, len(payload.Grants))
	for i, grant := range payload.Grants {
		bindings[i] = grant.Recipient + "=" + grant.ResultID
	}
	prepared, err := c.PrepareHistoricalResultGrants(ctx, bindings)
	if err != nil {
		return nil, err
	}
	expected, err := prepared.envelope(payload.SessionID, payload.CycleID)
	if err != nil {
		return nil, err
	}
	if expected.Workspace != payload.Workspace || expected.Team != payload.Team || expected.BranchID != payload.BranchID ||
		expected.SchemaVersion != payload.SchemaVersion || expected.PolicyFingerprint != payload.PolicyFingerprint || !slices.Equal(expected.Grants, payload.Grants) {
		return nil, ErrHistoricalResultGrantDrift
	}
	return prepared, nil
}
