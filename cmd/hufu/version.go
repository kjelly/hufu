package main

import (
	"fmt"
	"runtime"

	"github.com/kjelly/hufu/internal/buildinfo"
	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print hufu version information",
	Args:  cobra.NoArgs,
	Run: func(_ *cobra.Command, _ []string) {
		info := buildinfo.Current()
		fmt.Printf("hufu %s\n", info.String())
		fmt.Printf("source %s\n", info.Source)
		fmt.Printf("go %s\n", runtime.Version())
	},
}
