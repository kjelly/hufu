package inspect

import (
	"context"
	"fmt"
	"strings"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

// SemanticRetrievalData summarizes the workspace's content-free semantic
// retrieval traces (docs/reference/semantic-retrieval.md) and the runtime
// status recorded when the selected run, or the latest run, finished.
type SemanticRetrievalData struct {
	Runtime      *team.SemanticRuntimeRecord   `json:"runtime,omitempty"`
	Summary      team.SemanticRetrievalSummary `json:"summary"`
	SkippedLines int                           `json:"skipped_lines,omitempty"`
}

// InspectSemanticRetrieval reads the workspace trace and runtime status
// files, optionally narrowed to one run and one task. The files are not part
// of the event chain, so the envelope reports the chain as not applicable.
func InspectSemanticRetrieval(_ context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindSemanticRetrieval); err != nil {
		return nil, err
	}
	workspace, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: query.Workspace, Mode: "exact"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	resolved := query
	resolved.Workspace = workspace.WorkspaceExact
	traces, skippedTraces, err := team.ReadSemanticRetrievalTraces(resolved.Workspace, resolved.RunID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIntegrity, err)
	}
	if taskID := strings.TrimSpace(resolved.TaskID); taskID != "" {
		filtered := traces[:0]
		for _, trace := range traces {
			if trace.TaskID == taskID {
				filtered = append(filtered, trace)
			}
		}
		traces = filtered
	}
	records, skippedRecords, err := team.ReadSemanticRuntimeRecords(resolved.Workspace, resolved.RunID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIntegrity, err)
	}
	if resolved.RunID != "" && len(traces) == 0 && len(records) == 0 {
		return nil, fmt.Errorf("%w: no semantic retrieval traces for run %q", ErrNotFound, resolved.RunID)
	}
	data := SemanticRetrievalData{Summary: team.SummarizeSemanticRetrievalTraces(traces), SkippedLines: skippedTraces + skippedRecords}
	if len(records) > 0 {
		latest := records[len(records)-1]
		data.Runtime = &latest
	}
	result := envelope(KindSemanticRetrieval, resolved, "", data)
	result.Integrity = Integrity{EventChain: "not_applicable", Projection: "workspace_trace_files"}
	return result, nil
}
