package main

import (
	"fmt"
	"os"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/llmtimeout"
)

// configureLLMTimeouts applies the hufu.yaml timeouts: block before a command
// makes any model call. Invalid values leave the built-in defaults.
func configureLLMTimeouts() {
	if err := llmtimeout.Configure(config.LoadConfig().Timeouts); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; using the default model-call timeouts\n", err)
	}
}
