package main

import (
	"fmt"
	"io"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
)

func writeDerivedForkStatus(out io.Writer, view *operatorpkg.DerivedForkView) {
	if view == nil {
		return
	}
	if view.AwaitingNewRequest {
		_, _ = fmt.Fprintln(out, "Request:    awaiting_new_request (provide a new request for this team)")
	}
	_, _ = fmt.Fprintf(out, "Derived:    branch %s, run %s (%s)\n", safeOverviewValue(view.SourceBranchID),
		safeOverviewValue(valueOrUnavailable(view.SourceRunID)), safeOverviewValue(view.SourceRunMembership))
	_, _ = fmt.Fprintf(out, "Snapshot:   %s; capture origin %s\n", safeOverviewValue(view.SourceSnapshotID), safeOverviewValue(view.CaptureOrigin))
}
