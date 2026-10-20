package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/mcpserver"
)

func newMCPCommand() *cobra.Command {
	command := &cobra.Command{Use: "mcp", Short: "Expose read-only run inspection to local MCP clients"}
	var query inspect.InspectQuery
	serve := &cobra.Command{
		Use: "serve", Short: "Serve one fixed workspace/session/branch/run over stdio",
		Args: cobra.NoArgs,
		// Stdio has no CLI display configuration or interactive pre-run work.
		PersistentPreRun: func(*cobra.Command, []string) {},
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			service, err := mcpserver.New(ctx, query)
			if err != nil {
				return fmt.Errorf("MCP startup: %s", mcpserver.ErrorCode(err))
			}
			if err := service.Serve(ctx, os.Stdin, os.Stdout); err != nil {
				if ctx.Err() != nil && context.Cause(ctx) != nil {
					return nil
				}
				return fmt.Errorf("MCP transport: %s", mcpserver.ErrorCode(err))
			}
			return nil
		},
	}
	serve.Flags().StringVar(&query.Workspace, "workspace", "", "Absolute path of the existing workspace")
	serve.Flags().StringVar(&query.SessionID, "session", "", "Fixed session ID")
	serve.Flags().StringVar(&query.BranchID, "branch", "", "Fixed canonical branch ID (not a name or label)")
	serve.Flags().StringVar(&query.RunID, "run", "", "Fixed run ID")
	for _, flag := range []string{"workspace", "session", "branch", "run"} {
		_ = serve.MarkFlagRequired(flag)
	}
	command.AddCommand(serve)
	return command
}
