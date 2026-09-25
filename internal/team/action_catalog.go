package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Limits of the team action catalog. See docs/reference/action-providers.md.
const (
	actionCatalogSnapshotVersion = 1
	maxActionCatalogEntries      = 128
	maxActionDescriptionBytes    = 2048
	maxActionTypeBytes           = 128
	maxActionSchemaBytes         = 64 << 10
	maxActionInvocationsLimit    = 64
)

// Protocol tool names reserved by a team that declares an action catalog.
const (
	teamActionListToolName    = "team_action_list"
	teamActionGetToolName     = "team_action_get"
	teamActionProposeToolName = "team_action_propose"
)

var teamActionToolNames = []string{teamActionListToolName, teamActionGetToolName, teamActionProposeToolName}

var actionCatalogIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// actionCatalogEntryYAML is one authored action-catalog entry. Recovery stays
// a string so an omitted value can be told apart from an explicit retry, and
// MaxInvocations a pointer so an omitted value can take the default.
type actionCatalogEntryYAML struct {
	Description  string                      `yaml:"description"`
	Capability   string                      `yaml:"capability"`
	Type         string                      `yaml:"type"`
	Agent        string                      `yaml:"agent"`
	SideEffect   string                      `yaml:"side-effect"`
	Recovery     string                      `yaml:"recovery"`
	InputSchema  *runInputSchemaYAML         `yaml:"input-schema"`
	OutputSchema *runInputSchemaYAML         `yaml:"output-schema"`
	Access       actionCatalogAccessYAML     `yaml:"access"`
	Invocation   actionCatalogInvocationYAML `yaml:"invocation"`
}

type actionCatalogAccessYAML struct {
	Discover []string `yaml:"discover"`
	Propose  []string `yaml:"propose"`
}

type actionCatalogInvocationYAML struct {
	RequireProposal bool `yaml:"require-proposal"`
	AllowUnattended bool `yaml:"allow-unattended"`
	MaxInvocations  *int `yaml:"max-invocations"`
}

// ActionCatalogEntry is one normalized, immutable catalog entry. Agent names
// are canonical lower-case AgentDef names.
type ActionCatalogEntry struct {
	ID              string          `json:"id"`
	Description     string          `json:"description"`
	Capability      string          `json:"capability"`
	Type            string          `json:"type"`
	Agent           string          `json:"agent"`
	SideEffect      SideEffectClass `json:"side_effect"`
	Recovery        RecoveryPolicy  `json:"recovery"`
	InputSchema     RunInputSchema  `json:"input_schema"`
	OutputSchema    *RunInputSchema `json:"output_schema,omitempty"`
	Discover        []string        `json:"discover"`
	Propose         []string        `json:"propose"`
	RequireProposal bool            `json:"require_proposal"`
	AllowUnattended bool            `json:"allow_unattended"`
	MaxInvocations  int             `json:"max_invocations"`
	// ProviderIdentityHash pins the capability's provider configuration.
	// It is part of Hash but never shown in worker or coordinator tool output.
	ProviderIdentityHash string `json:"provider_identity_hash"`
	Hash                 string `json:"-"`
}

// ActionCatalogSnapshot is the catalog frozen when the team is loaded.
type ActionCatalogSnapshot struct {
	Version int                  `json:"version"`
	Entries []ActionCatalogEntry `json:"entries"`
	Hash    string               `json:"-"`
}

// Lookup returns the entry with exactly this ID.
func (s *ActionCatalogSnapshot) Lookup(id string) (ActionCatalogEntry, bool) {
	if s == nil {
		return ActionCatalogEntry{}, false
	}
	for _, entry := range s.Entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return ActionCatalogEntry{}, false
}

func (s *ActionCatalogSnapshot) clone() *ActionCatalogSnapshot {
	if s == nil {
		return nil
	}
	clone := &ActionCatalogSnapshot{Version: s.Version, Hash: s.Hash, Entries: make([]ActionCatalogEntry, len(s.Entries))}
	for i, entry := range s.Entries {
		clone.Entries[i] = entry.clone()
	}
	return clone
}

func (e ActionCatalogEntry) clone() ActionCatalogEntry {
	e.InputSchema = cloneRunInputSchema(e.InputSchema)
	if e.OutputSchema != nil {
		output := cloneRunInputSchema(*e.OutputSchema)
		e.OutputSchema = &output
	}
	e.Discover = slices.Clone(e.Discover)
	e.Propose = slices.Clone(e.Propose)
	return e
}

// hashActionCatalogEntry returns the entry's content hash; Hash itself is
// excluded from the encoding.
func hashActionCatalogEntry(entry ActionCatalogEntry) (string, error) {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("encode action catalog entry %q: %w", entry.ID, err)
	}
	return runInputHash(encoded), nil
}

// rehash recomputes the snapshot hash from the entries' IDs and hashes.
func (s *ActionCatalogSnapshot) rehash() error {
	type idHash struct {
		ID   string `json:"id"`
		Hash string `json:"hash"`
	}
	pairs := make([]idHash, 0, len(s.Entries))
	for _, entry := range s.Entries {
		pairs = append(pairs, idHash{ID: entry.ID, Hash: entry.Hash})
	}
	encoded, err := json.Marshal(pairs)
	if err != nil {
		return fmt.Errorf("encode action catalog snapshot: %w", err)
	}
	s.Hash = runInputHash(encoded)
	return nil
}

// loadActionCatalog re-reads the manifest's action-catalog node and decodes
// every entry strictly. A malformed entry becomes a finding and does not stop
// the others; only an unreadable manifest returns an error.
func loadActionCatalog(absDir string, vars map[string]string) (map[string]actionCatalogEntryYAML, []ContractFinding, error) {
	data, filename, found, err := readTeamManifestSource(absDir, vars)
	if err != nil {
		return nil, nil, fmt.Errorf("read team action catalog: %w", err)
	}
	if !found {
		return nil, nil, nil
	}
	yc, _, err := decodeTeamManifestYAML(filename, data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse team action catalog: %w", err)
	}
	if len(yc.ActionCatalog) == 0 {
		return nil, nil, nil
	}
	entries := make(map[string]actionCatalogEntryYAML, len(yc.ActionCatalog))
	var findings []ContractFinding
	for id, node := range yc.ActionCatalog {
		entry, err := decodeActionCatalogEntry(node)
		if err != nil {
			findings = append(findings, errorFinding(actionCatalogField(id, ""), FindingActionCatalogEntryInvalid, fmt.Sprintf("action %q: %v", id, err)))
			continue
		}
		entries[id] = entry
	}
	return entries, findings, nil
}

func decodeActionCatalogEntry(node yaml.Node) (actionCatalogEntryYAML, error) {
	encoded, err := yaml.Marshal(&node)
	if err != nil {
		return actionCatalogEntryYAML{}, fmt.Errorf("encode entry: %w", err)
	}
	var entry actionCatalogEntryYAML
	decoder := yaml.NewDecoder(bytes.NewReader(encoded))
	decoder.KnownFields(true)
	if err := decoder.Decode(&entry); err != nil {
		return actionCatalogEntryYAML{}, fmt.Errorf("decode entry: %w", err)
	}
	return entry, nil
}

// attachActionCatalog loads, normalizes, and freezes the team's action
// catalog. Structural findings are kept on the session for
// validateActionCatalog to report.
func attachActionCatalog(session *TeamSession, absDir string, vars map[string]string) error {
	raw, findings, err := loadActionCatalog(absDir, vars)
	if err != nil {
		return err
	}
	snapshot, normalizeFindings, err := normalizeActionCatalog(session, raw)
	if err != nil {
		return err
	}
	session.ActionCatalog = snapshot
	session.actionCatalogFindings = append(findings, normalizeFindings...)
	return nil
}

// normalizeActionCatalog checks each entry's own fields and limits. Only
// entries without structural findings enter the snapshot, which is nil when
// no entry is valid.
func normalizeActionCatalog(session *TeamSession, raw map[string]actionCatalogEntryYAML) (*ActionCatalogSnapshot, []ContractFinding, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	if len(raw) > maxActionCatalogEntries {
		return nil, []ContractFinding{errorFinding("action-catalog", FindingActionCatalogEntryInvalid, fmt.Sprintf("action catalog declares %d entries (maximum %d)", len(raw), maxActionCatalogEntries))}, nil
	}
	ids := make([]string, 0, len(raw))
	for id := range raw {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snapshot := &ActionCatalogSnapshot{Version: actionCatalogSnapshotVersion}
	var findings []ContractFinding
	for _, id := range ids {
		entry, entryFindings := normalizeActionCatalogEntry(session, id, raw[id])
		findings = append(findings, entryFindings...)
		if len(entryFindings) > 0 {
			continue
		}
		hash, err := hashActionCatalogEntry(entry)
		if err != nil {
			return nil, nil, err
		}
		entry.Hash = hash
		snapshot.Entries = append(snapshot.Entries, entry)
	}
	if len(snapshot.Entries) == 0 {
		return nil, findings, nil
	}
	if err := snapshot.rehash(); err != nil {
		return nil, nil, err
	}
	return snapshot, findings, nil
}

func normalizeActionCatalogEntry(session *TeamSession, id string, raw actionCatalogEntryYAML) (ActionCatalogEntry, []ContractFinding) {
	if !actionCatalogIDPattern.MatchString(id) {
		return ActionCatalogEntry{}, []ContractFinding{errorFinding(actionCatalogField(id, ""), FindingActionCatalogIDInvalid,
			fmt.Sprintf("action ID %q must match %s", id, actionCatalogIDPattern.String()))}
	}
	entry := ActionCatalogEntry{
		ID: id, Description: strings.TrimSpace(raw.Description), Capability: normalizeCapability(raw.Capability),
		Type: strings.TrimSpace(raw.Type), Agent: canonicalCatalogAgentName(session, raw.Agent),
		RequireProposal: raw.Invocation.RequireProposal, AllowUnattended: raw.Invocation.AllowUnattended,
	}
	var findings []ContractFinding
	findings = append(findings, validateActionCatalogScalars(id, entry)...)
	sideEffect, recovery, policyFindings := normalizeActionCatalogPolicy(id, raw)
	entry.SideEffect, entry.Recovery = sideEffect, recovery
	findings = append(findings, policyFindings...)
	input, output, schemaFindings := normalizeActionCatalogSchemas(id, raw)
	entry.InputSchema, entry.OutputSchema = input, output
	findings = append(findings, schemaFindings...)
	discover, propose, accessFindings := normalizeActionCatalogAccess(session, id, raw.Access)
	entry.Discover, entry.Propose = discover, propose
	findings = append(findings, accessFindings...)
	entry.MaxInvocations = 1
	if raw.Invocation.MaxInvocations != nil {
		entry.MaxInvocations = *raw.Invocation.MaxInvocations
		if entry.MaxInvocations < 1 || entry.MaxInvocations > maxActionInvocationsLimit {
			findings = append(findings, errorFinding(actionCatalogField(id, "invocation.max-invocations"), FindingActionCatalogInvocationInvalid,
				fmt.Sprintf("action %q max-invocations %d must be between 1 and %d", id, entry.MaxInvocations, maxActionInvocationsLimit)))
		}
	}
	entry.ProviderIdentityHash = actionProviderIdentityHash(session, entry.Capability)
	return entry, findings
}

func validateActionCatalogScalars(id string, entry ActionCatalogEntry) []ContractFinding {
	var findings []ContractFinding
	invalid := func(field, message string) {
		findings = append(findings, errorFinding(actionCatalogField(id, field), FindingActionCatalogEntryInvalid, fmt.Sprintf("action %q %s", id, message)))
	}
	switch {
	case entry.Description == "":
		invalid("description", "requires a description")
	case len(entry.Description) > maxActionDescriptionBytes:
		invalid("description", fmt.Sprintf("description exceeds %d bytes", maxActionDescriptionBytes))
	}
	if entry.Capability == "" {
		invalid("capability", "requires a capability")
	}
	switch {
	case entry.Type == "":
		invalid("type", "requires a type")
	case len(entry.Type) > maxActionTypeBytes:
		invalid("type", fmt.Sprintf("type exceeds %d bytes", maxActionTypeBytes))
	}
	if entry.Agent == "" {
		invalid("agent", "requires an agent")
	}
	return findings
}

// normalizeActionCatalogPolicy resolves side-effect and recovery. A
// workspace_write entry must choose its recovery explicitly: hufu cannot
// verify that a provider is safe to re-run.
func normalizeActionCatalogPolicy(id string, raw actionCatalogEntryYAML) (SideEffectClass, RecoveryPolicy, []ContractFinding) {
	var findings []ContractFinding
	sideEffect := SideEffectClass(strings.TrimSpace(raw.SideEffect))
	switch sideEffect {
	case SideEffectNone, SideEffectWorkspaceWrite:
	default:
		findings = append(findings, errorFinding(actionCatalogField(id, "side-effect"), FindingActionCatalogSideEffectUnsupported,
			fmt.Sprintf("action %q side-effect %q is not supported; use none or workspace_write", id, raw.SideEffect)))
	}
	recovery := RecoveryPolicy(strings.TrimSpace(raw.Recovery))
	switch recovery {
	case "":
		recovery = RecoveryRetry
		if sideEffect == SideEffectWorkspaceWrite {
			findings = append(findings, errorFinding(actionCatalogField(id, "recovery"), FindingActionCatalogRecoveryRequired,
				fmt.Sprintf("action %q is workspace_write and must set recovery (retry only when the provider is idempotent by HUFU_CATALOG_INVOCATION_ID, otherwise manual or never)", id)))
		}
	case RecoveryRetry, RecoveryManual, RecoveryNever:
	default:
		findings = append(findings, errorFinding(actionCatalogField(id, "recovery"), FindingActionCatalogRecoveryInvalid,
			fmt.Sprintf("action %q recovery %q must be retry, manual, or never", id, raw.Recovery)))
	}
	return sideEffect, recovery, findings
}

// normalizeActionCatalogAccess resolves discover/propose names to canonical
// agent names. Unknown names are kept for validateActionCatalog to report;
// two spellings of one agent are a duplicate.
func normalizeActionCatalogAccess(session *TeamSession, id string, raw actionCatalogAccessYAML) ([]string, []string, []ContractFinding) {
	var findings []ContractFinding
	resolve := func(field string, names []string) []string {
		result := make([]string, 0, len(names))
		seen := make(map[string]bool, len(names))
		for _, name := range names {
			canonical := canonicalCatalogAgentName(session, name)
			if canonical == "" {
				findings = append(findings, errorFinding(actionCatalogField(id, field), FindingActionCatalogAccessInvalid, fmt.Sprintf("action %q %s lists an empty agent name", id, field)))
				continue
			}
			if seen[canonical] {
				findings = append(findings, errorFinding(actionCatalogField(id, field), FindingActionCatalogAccessInvalid, fmt.Sprintf("action %q %s lists agent %q more than once", id, field, canonical)))
				continue
			}
			seen[canonical] = true
			result = append(result, canonical)
		}
		sort.Strings(result)
		return result
	}
	discover := resolve("access.discover", raw.Discover)
	propose := resolve("access.propose", raw.Propose)
	for _, name := range propose {
		if !slices.Contains(discover, name) {
			findings = append(findings, errorFinding(actionCatalogField(id, "access.propose"), FindingActionCatalogAccessInvalid,
				fmt.Sprintf("action %q lets %q propose but not discover; propose must be a subset of discover", id, name)))
		}
	}
	return discover, propose, findings
}

// canonicalCatalogAgentName returns the lower-case AgentDef name for an
// exact alias or name match, or the normalized input when none resolves.
func canonicalCatalogAgentName(session *TeamSession, name string) string {
	if def := sessionAgentDef(session, name); def != nil {
		return normalizedName(def.Name)
	}
	return normalizedName(name)
}

// actionCatalogField is the lint field path of an entry or one of its keys.
func actionCatalogField(id, key string) string {
	if key == "" {
		return "action-catalog." + id
	}
	return "action-catalog." + id + "." + key
}

// actionCatalogFieldID returns the entry ID of an action-catalog field path.
func actionCatalogFieldID(field string) (string, bool) {
	rest, ok := strings.CutPrefix(field, "action-catalog.")
	if !ok {
		return "", false
	}
	id, _, _ := strings.Cut(rest, ".")
	return id, id != ""
}
