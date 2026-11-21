package main

import (
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/llmtimeout"
	hulog "github.com/kjelly/hufu/internal/log"
)

// configureLLMTimeouts applies the hufu.yaml timeouts: block before a command
// makes any model call. Invalid values leave the built-in defaults.
func configureLLMTimeouts() {
	if err := llmtimeout.Configure(config.LoadConfig().Timeouts); err != nil {
		hulog.Printf("warning: %v; using the default model-call timeouts\n", err)
	}
}
