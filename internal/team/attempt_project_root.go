package team

import (
	"context"
	"strings"

	"github.com/kjelly/hufu/internal/tools"
)

// attemptProjectRoot is the project root the current attempt's tools see:
// its isolated execution root when one is installed on ctx, otherwise the
// canonical project directory. Attempt-bound work (the worker's prompt,
// verification commands) resolves paths against it; canonical identities
// (policy snapshots, resource claims, verification fingerprints) keep using
// c.projectDir.
func (c *Coordinator) attemptProjectRoot(ctx context.Context) string {
	if ctx != nil {
		if root, ok := ctx.Value(tools.AgentExecutionRootKey).(tools.AgentExecutionRoot); ok && strings.TrimSpace(root.Root) != "" {
			return root.Root
		}
	}
	if c == nil {
		return ""
	}
	return c.projectDir
}

// verificationWorkDirFor is where an attempt's verification commands run.
func (c *Coordinator) verificationWorkDirFor(ctx context.Context) string {
	if root := c.attemptProjectRoot(ctx); root != "" {
		return root
	}
	return c.verificationWorkDir()
}

// canonicalVerificationFingerprint keeps a verification fingerprint on the
// canonical project root when the command ran in an isolated attempt root.
// Every attempt has its own root, so a root-derived fingerprint would never
// repeat and repeated-failure detection would stop working.
func (c *Coordinator) canonicalVerificationFingerprint(spec VerificationSpec, result *VerificationResult, ranIn string) {
	if result == nil || ranIn == c.verificationWorkDir() {
		return
	}
	result.Fingerprint = ComputeVerificationFingerprint(spec, result, c.verificationWorkDir())
}
