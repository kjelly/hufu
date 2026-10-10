package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/llmtimeout"
	"github.com/kjelly/hufu/internal/team"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that hufu is ready to call agents",
	Long: `Run preflight checks before invoking an agent team:

  - the LLM provider is reachable and which models it exposes
  - the resolved role models and the flag or hufu.yaml that sets each
  - the workspace directory is writable
  - how many agent teams are discoverable
  - team requirements and delegation/tool policies do not conflict
  - static task and acceptance verifier contracts are asserting and resolvable
  - existing event history and unresolved recovery tasks are safe to inspect

Most "the agent did nothing" failures are a provider/model misconfiguration.
Run this first to find them in seconds instead of waiting for a timeout.`,
	RunE: runDoctorReport,
}

func init() {
	doctorCmd.Flags().Bool("json", false, "Print one machine-readable preflight report to stdout")
}

// modelsResponse matches the OpenAI-compatible GET /models payload that both
// OpenAI and Ollama (under /v1) return.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// validateDoctorExecutionTargets performs the same read-only target setup
// gate used by a real run. It deliberately stops before coordinator creation,
// provider contact, MCP loading, or workspace lifecycle I/O.
func validateDoctorExecutionTargets(session *team.TeamSession, cfg *config.Config, lookup targetExecutableLookup) error {
	if err := applyConfiguredBackends(session, cfg); err != nil {
		return err
	}
	roleModels, err := resolveExecutionRoleModels(session, cfg)
	if err != nil {
		return err
	}
	return preflightExecutionTargets(session, cfg, roleModels, lookup)
}

type doctorContractFinding struct {
	Location string
	Finding  team.ContractFinding
}

// collectDoctorContractFindings combines the pure WP-01 verifier lint and
// WP-04 executable resolver for a loaded team without executing contracts.
// projectDir is the same process CWD that NewCoordinator uses at runtime.
func collectDoctorContractFindings(session *team.TeamSession, projectDir string) []doctorContractFinding {
	findings := append(team.LintTeamContracts(session), team.ResolveTeamContractExecutables(session, projectDir)...)
	allowedPaths := append([]string{projectDir, session.Dir, session.Workspace}, session.Config.AllowedPaths...)
	unattended := session.Config.Unattended
	if profile, err := team.ResolveExecutionProfile("", session.Config.ExecutionProfile); err == nil {
		unattended = unattended || profile.IsUnattended()
	}
	findings = append(findings, team.LintEffectiveTeamContracts(session, team.EffectiveTeamContractContext{
		Unattended:        unattended,
		ForceMCP:          session.Config.ForceMCP,
		NoNet:             session.Config.NoNet,
		AllowedPaths:      allowedPaths,
		EnvironmentLookup: os.LookupEnv,
	})...)
	out := make([]doctorContractFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, doctorContractFinding{Location: finding.Field, Finding: finding})
	}
	return out
}

// doctorProviderError exposes only runtime-owned diagnostics. The original
// cause remains available for error matching, but URLs and response bodies
// must never enter the doctor report.
type doctorProviderError struct {
	Code       string
	Message    string
	HTTPStatus int
	Cause      error
}

func (err *doctorProviderError) Error() string { return err.Message }
func (err *doctorProviderError) Unwrap() error { return err.Cause }

func doctorProviderTransportError(err error) *doctorProviderError {
	failure := &doctorProviderError{Code: "transport_failed", Message: "provider connection failed", Cause: err}
	networkErr, isNetwork := errors.AsType[net.Error](err)
	_, isDNS := errors.AsType[*net.DNSError](err)
	_, isTLS := errors.AsType[*tls.CertificateVerificationError](err)
	switch {
	case errors.Is(err, context.Canceled):
		failure.Code, failure.Message = "request_cancelled", "provider request was cancelled"
	case errors.Is(err, context.DeadlineExceeded) || (isNetwork && networkErr.Timeout()):
		failure.Code, failure.Message = "timeout", "provider request timed out"
	case isDNS:
		failure.Code, failure.Message = "dns_failed", "provider hostname could not be resolved"
	case errors.Is(err, syscall.ECONNREFUSED):
		failure.Code, failure.Message = "connection_refused", "provider connection was refused"
	case isTLS:
		failure.Code, failure.Message = "tls_failed", "provider TLS certificate could not be verified"
	}
	return failure
}

func fetchModelsContext(parent context.Context, providerURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(providerURL, "/") + "/models"
	ctx, cancel := context.WithTimeout(parent, llmtimeout.Provider(5*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &doctorProviderError{Code: "invalid_request", Message: "provider request configuration is invalid", Cause: err}
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, doctorProviderTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &doctorProviderError{Code: "http_error", HTTPStatus: resp.StatusCode, Message: fmt.Sprintf("provider returned HTTP %d", resp.StatusCode)}
	}
	var response modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, &doctorProviderError{Code: "invalid_model_list", Message: "provider returned an invalid model list", Cause: err}
	}
	out := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		out = append(out, model.ID)
	}
	return out, nil
}

func modelAvailable(bare string, available []string) bool {
	for _, model := range available {
		if model == bare || providerModelName(model) == bare {
			return true
		}
	}
	return false
}

func providerModelName(model string) string {
	if idx := strings.IndexByte(model, '/'); idx >= 0 {
		return model[idx+1:]
	}
	return model
}

func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".hufu-doctor-probe-*")
	if err != nil {
		return err
	}
	path := probe.Name()
	_, writeErr := probe.WriteString("ok")
	created, statErr := probe.Stat()
	closeErr := probe.Close()
	if statErr != nil {
		return statErr
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(created, current) {
		return fmt.Errorf("workspace probe changed before cleanup")
	}
	removeErr := os.Remove(path)
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}

func firstNonEmpty(vals ...string) string {
	for _, value := range vals {
		if value != "" {
			return value
		}
	}
	return ""
}

// resolveSearchPaths returns the team search paths from the
// --agent-team-search-path flag or the built-in defaults.
func resolveSearchPaths() []string {
	if opts.agentTeamSearchPath != "" {
		return strings.Split(opts.agentTeamSearchPath, ",")
	}
	return team.DefaultSearchPaths()
}
