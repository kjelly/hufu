package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/belief"
)

// ExecutionDiagnosticSnapshot pins prepared contracts, not executable episodes.
// Its absence preserves legacy policy hashes. No stage-1 caller can dispatch it.
type ExecutionDiagnosticSnapshot struct {
	SchemaVersion int    `json:"schema_version"`
	Mode          string `json:"mode"`
	Actor         string `json:"actor"`
	ModelDigest   string `json:"model_digest"`
	PolicyDigest  string `json:"policy_digest"`
	AdapterDigest string `json:"adapter_digest"`
	CatalogDigest string `json:"catalog_digest"`
	BindingDigest string `json:"binding_digest"`
}

type diagnosticAdapter struct {
	SchemaVersion int      `json:"schema_version"`
	Version       string   `json:"version"`
	Source        string   `json:"source"`
	Experiments   []string `json:"experiments"`
	Outcomes      []string `json:"outcomes"`
}

// CompiledDiagnosticLoop is an immutable preparation record, deliberately
// separate from CompiledBelief and its receipt-per-epoch adapter.
type CompiledDiagnosticLoop struct {
	model    belief.Model
	policy   belief.DiagnosticPolicy
	adapter  diagnosticAdapter
	snapshot ExecutionDiagnosticSnapshot
}

func compileTeamDiagnosticLoop(session *TeamSession) error {
	if session.Config.DiagnosticLoop == nil {
		session.DiagnosticLoop = nil
		return nil
	}
	config := session.Config.DiagnosticLoop
	if config.Mode != "off" {
		return fmt.Errorf("diagnostic-loop currently supports preparation with mode off only; shadow and bounded_read_only are unavailable")
	}
	if config.Actor == "" || len(config.Actor) > 128 || config.Actor != normalizedName(config.Actor) || session.Agents[config.Actor] == nil {
		return fmt.Errorf("diagnostic-loop requires an explicit existing actor")
	}
	data, err := readDiagnosticFile(session.Dir, config.ModelFile, belief.MaxModelBytes)
	if err != nil {
		return err
	}
	model, err := belief.DecodeModel(data)
	if err != nil {
		return fmt.Errorf("diagnostic-loop model: %w", err)
	}
	data, err = readDiagnosticFile(session.Dir, config.PolicyFile, belief.MaxPolicyBytes)
	if err != nil {
		return err
	}
	policy, err := belief.DecodeDiagnosticPolicy(data)
	if err != nil {
		return fmt.Errorf("diagnostic-loop policy: %w", err)
	}
	if err := belief.ValidateDiagnosticPolicy(model, policy); err != nil {
		return err
	}
	data, err = readDiagnosticFile(session.Dir, config.AdapterFile, belief.MaxPolicyBytes)
	if err != nil {
		return err
	}
	adapter, err := decodeDiagnosticAdapter(data, model, policy)
	if err != nil {
		return err
	}
	if err := validateDiagnosticCatalog(session, policy); err != nil {
		return err
	}
	modelDigest, err := belief.ModelDigest(model)
	if err != nil {
		return err
	}
	policyDigest, err := belief.DiagnosticPolicyDigest(policy)
	if err != nil {
		return err
	}
	adapterDigest, err := diagnosticContractDigest("hufu-diagnostic-adapter-v1", adapter)
	if err != nil {
		return err
	}
	snapshot := ExecutionDiagnosticSnapshot{SchemaVersion: 1, Mode: "off", Actor: config.Actor, ModelDigest: modelDigest, PolicyDigest: policyDigest, AdapterDigest: adapterDigest, CatalogDigest: session.ActionCatalog.Hash}
	snapshot.BindingDigest, err = diagnosticContractDigest("hufu-diagnostic-binding-v1", snapshot)
	if err != nil {
		return err
	}
	session.DiagnosticLoop = &CompiledDiagnosticLoop{model: model, policy: policy, adapter: adapter, snapshot: snapshot}
	return nil
}

func validateDiagnosticCatalog(session *TeamSession, policy belief.DiagnosticPolicy) error {
	if session.ActionCatalog == nil || !runInputHashPattern.MatchString(session.ActionCatalog.Hash) {
		return fmt.Errorf("diagnostic-loop requires a frozen action catalog")
	}
	for _, candidate := range policy.Candidates {
		entry, ok := session.ActionCatalog.Lookup(candidate.ID)
		if !ok || entry.SideEffect != SideEffectNone || !slices.Contains(entry.Discover, session.Config.DiagnosticLoop.Actor) || !slices.Contains(entry.Propose, session.Config.DiagnosticLoop.Actor) || !runInputHashPattern.MatchString(entry.Hash) {
			return fmt.Errorf("diagnostic-loop candidate must bind an existing read-only catalog action with discover and propose grants")
		}
	}
	return nil
}

func decodeDiagnosticAdapter(data []byte, model belief.Model, policy belief.DiagnosticPolicy) (diagnosticAdapter, error) {
	var adapter diagnosticAdapter
	if err := json.Unmarshal(data, &adapter, json.RejectUnknownMembers(true)); err != nil {
		return adapter, fmt.Errorf("invalid diagnostic-loop adapter JSON")
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal(data, &object); err != nil {
		return adapter, fmt.Errorf("invalid diagnostic-loop adapter object")
	}
	for _, key := range []string{"schema_version", "version", "source", "experiments", "outcomes"} {
		if raw, ok := object[key]; !ok || string(raw) == "null" {
			return adapter, fmt.Errorf("diagnostic-loop adapter requires %s", key)
		}
	}
	if adapter.SchemaVersion != 1 || adapter.Version != belief.DiagnosticAdapterVersion || adapter.Source != "verification" || !slices.Equal(adapter.Outcomes, []string{"pass", "fail"}) || len(adapter.Experiments) != len(policy.Candidates) {
		return adapter, fmt.Errorf("unsupported diagnostic-loop adapter contract")
	}
	seen := map[string]bool{}
	for _, id := range adapter.Experiments {
		if seen[id] || !slices.ContainsFunc(policy.Candidates, func(c belief.DiagnosticCandidate) bool { return c.ExperimentID == id }) {
			return adapter, fmt.Errorf("diagnostic-loop adapter experiment binding is invalid")
		}
		if err := validateBeliefVerificationExperiment(model, id); err != nil {
			return adapter, err
		}
		seen[id] = true
	}
	return adapter, nil
}

func readDiagnosticFile(dir, relative string, limit int64) ([]byte, error) {
	if !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("diagnostic-loop resources must be team-relative")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("diagnostic-loop directory unavailable: %w", err)
	}
	defer func() { _ = root.Close() }()
	// Reject symlinks even when they remain inside the team root.
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("diagnostic-loop resource is missing or contains a symlink")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("diagnostic-loop resource must be a regular file")
		}
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, fmt.Errorf("diagnostic-loop resource unavailable: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("diagnostic-loop resource must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("diagnostic-loop resource read failed or exceeded limit")
	}
	return data, nil
}

func diagnosticContractDigest(domain string, value any) (string, error) {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(domain+"\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}

func executionPolicyDiagnostic(session *TeamSession) *ExecutionDiagnosticSnapshot {
	if session == nil || session.DiagnosticLoop == nil {
		return nil
	}
	return new(session.DiagnosticLoop.snapshot)
}

func validateExecutionDiagnosticSnapshot(snapshot *ExecutionDiagnosticSnapshot) error {
	if snapshot == nil {
		return nil
	}
	if snapshot.SchemaVersion != 1 || snapshot.Mode != "off" || snapshot.Actor == "" || len(snapshot.Actor) > 128 || snapshot.Actor != normalizedName(snapshot.Actor) {
		return fmt.Errorf("invalid prepared diagnostic-loop snapshot")
	}
	for _, digest := range []string{snapshot.ModelDigest, snapshot.PolicyDigest, snapshot.AdapterDigest, snapshot.BindingDigest} {
		if !belief.ValidDiagnosticDigest(digest) {
			return fmt.Errorf("invalid diagnostic-loop snapshot digest")
		}
	}
	if !runInputHashPattern.MatchString(snapshot.CatalogDigest) {
		return fmt.Errorf("invalid diagnostic-loop catalog digest")
	}
	copySnapshot := *snapshot
	copySnapshot.BindingDigest = ""
	expected, err := diagnosticContractDigest("hufu-diagnostic-binding-v1", copySnapshot)
	if err != nil || expected != snapshot.BindingDigest {
		return fmt.Errorf("invalid diagnostic-loop snapshot binding")
	}
	return nil
}
