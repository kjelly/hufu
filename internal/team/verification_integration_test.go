package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
)

// TestVerifySpecPropagation_ClonePtr verifies that cloneVerificationSpecPtr works correctly.
func TestVerifySpecPropagation_ClonePtr(t *testing.T) {
	vs := &agent.VerificationSpec{
		Type: agent.VerifyFileExists,
		Path: "workspace/output.md",
		Mode: "success",
	}
	spec := cloneVerificationSpecPtr(vs)
	if spec == nil {
		t.Fatal("cloneVerificationSpecPtr returned nil")
	}
	if spec == vs {
		t.Fatal("cloneVerificationSpecPtr must return a new pointer")
	}
	if spec.Path != vs.Path || spec.Type != vs.Type || spec.Mode != vs.Mode {
		t.Fatalf("cloned spec mismatch: %+v vs %+v", spec, vs)
	}
	if got := cloneVerificationSpecPtr(nil); got != nil {
		t.Fatalf("cloneVerificationSpecPtr(nil) = %v, want nil", got)
	}
}

func TestVerifySpecLegacyModePopulatesMixedTypedTask(t *testing.T) {
	c := &Coordinator{projectDir: t.TempDir()}
	verification, err := c.verifyTaskDeliverableWithSpec(context.Background(), nil, TaskDef{
		Verify:     "exit 1",
		VerifyMode: "observation",
		VerifySpec: &VerificationSpec{Type: VerifyCommandExit},
	})
	if err != nil {
		t.Fatalf("legacy verify_mode must populate a typed spec with no mode: %v", err)
	}
	if verification == nil {
		t.Fatal("expected verification evidence")
	}
	if verification.Spec == nil || verification.Spec.Mode != "observation" {
		t.Fatalf("typed verification mode = %#v, want observation", verification.Spec)
	}
	if verification.Command != "exit 1" {
		t.Fatalf("typed verification command = %q, want legacy command", verification.Command)
	}
}

// TestVerifySpecCacheKey_TypedAndLegacyEquivalent verifies that compatibility
// translation gives equivalent command-exit contracts the same cache identity.
func TestVerifySpecCacheKey_TypedAndLegacyEquivalent(t *testing.T) {
	desc := "compile and test the project"
	legacyKey := taskCacheIdentityWithSpec(desc, nil, "go build ./...", "success")
	typedKey := taskCacheIdentityWithSpec(desc, &VerificationSpec{
		Type:    VerifyCommandExit,
		Command: "go build ./...",
		Mode:    "success",
	}, "", "")
	if legacyKey != typedKey {
		t.Errorf("typed and legacy command_exit specs must share a cache key: legacy=%q typed=%q", legacyKey, typedKey)
	}
}

// TestVerifySpecCacheKey_AssertionOrderDeterminism verifies that assertion ordering
// is canonicalized so identical specs with different ordering produce the same key.
func TestVerifySpecCacheKey_AssertionOrderDeterminism(t *testing.T) {
	desc := "generate report"
	spec1 := &VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "report.json",
		Mode: "success",
		Assertions: []JSONAssertion{
			{Path: "status", Equals: "ok"},
			{Path: "status", Equals: "ready"},
		},
	}
	spec2 := &VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "report.json",
		Mode: "success",
		Assertions: []JSONAssertion{
			{Path: "status", Equals: "ready"},
			{Path: "status", Equals: "ok"},
		},
	}
	key1 := taskCacheIdentityWithSpec(desc, spec1, "", "")
	key2 := taskCacheIdentityWithSpec(desc, spec2, "", "")
	if key1 != key2 {
		t.Errorf("assertion order must be canonicalized: key1=%q key2=%q", key1, key2)
	}
}

func TestRepeatedJSONAssertionPathsCanonicalizeInvalidationJournalAndFingerprint(t *testing.T) {
	workspace := t.TempDir()
	first := &VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "report.json",
		Assertions: []JSONAssertion{
			{Path: "status", Equals: "ok"},
			{Path: "status", Equals: "ready"},
		},
	}
	reversed := &VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "report.json",
		Assertions: []JSONAssertion{
			{Path: "status", Equals: "ready"},
			{Path: "status", Equals: "ok"},
		},
	}
	const agentKey = "builder"
	const task = "verify generated report"

	if firstKey, reversedKey := taskCacheIdentityWithSpec(task, first, "", ""), taskCacheIdentityWithSpec(task, reversed, "", ""); firstKey != reversedKey {
		t.Fatalf("reordered same-path assertions must share a cache identity: first=%q reversed=%q", firstKey, reversedKey)
	}
	if firstFingerprint, reversedFingerprint := ComputeVerificationFingerprintFull(*first, nil, workspace, "", ""), ComputeVerificationFingerprintFull(*reversed, nil, workspace, "", ""); firstFingerprint != reversedFingerprint {
		t.Fatalf("reordered same-path assertions must share a fingerprint: first=%q reversed=%q", firstFingerprint, reversedFingerprint)
	}

	c := &Coordinator{taskResultCache: make(map[string][]cachedTaskEntry)}
	c.SetPolicyEngine(&defaultPolicyEngine{c: c})
	c.storeTaskCacheWithTypedVerification(agentKey, task, first, "", "", "cached")
	c.invalidateTaskCacheWithTypedVerification(agentKey, task, reversed, "", "")
	if entries := c.taskResultCache[agentKey]; len(entries) != 0 {
		t.Fatalf("invalidation with equivalent reordered contract retained cache entries: %#v", entries)
	}

	logs := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	records := []journalRecord{
		{Op: "put", Agent: agentKey, Desc: task, VerifySpec: first, Output: "cached", TS: now},
		{Op: "del", Agent: agentKey, Desc: task, VerifySpec: reversed, TS: now},
	}
	var data []byte
	for _, rec := range records {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, append(line, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(logs, taskJournalFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	replayed, err := loadTaskJournal(workspace, time.Now(), taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	if entries := replayed[agentKey]; len(entries) != 0 {
		t.Fatalf("journal tombstone with equivalent reordered contract retained entries: %#v", entries)
	}
}

// TestVerifySpecCache_ObservationModeNotCacheable verifies that observation-mode results
// are not stored in the task cache.
func TestVerifySpecCache_ObservationModeNotCacheable(t *testing.T) {
	c := &Coordinator{
		taskResultCache: make(map[string][]cachedTaskEntry),
		session:         &TeamSession{Config: agent.TeamConfig{Name: "test"}},
	}
	c.SetPolicyEngine(&defaultPolicyEngine{c: c})

	spec := &VerificationSpec{
		Type:    VerifyCommandExit,
		Command: "echo hello",
		Mode:    "observation",
	}
	c.storeTaskCacheWithTypedVerification("builder", "build the thing", spec, "", "observation", "done")

	c.taskResultCacheMu.RLock()
	entries := c.taskResultCache["builder"]
	c.taskResultCacheMu.RUnlock()

	for _, e := range entries {
		if e.taskDesc == "build the thing" {
			t.Fatalf("observation-mode result must not be stored in cache: %+v", e)
		}
	}
}

func TestVerifySpecCache_MixedLegacyObservationModeNotCacheable(t *testing.T) {
	c := &Coordinator{
		taskResultCache: make(map[string][]cachedTaskEntry),
		session:         &TeamSession{Config: agent.TeamConfig{Name: "test"}},
	}
	c.SetPolicyEngine(&defaultPolicyEngine{c: c})

	// Legacy mode remains authoritative when a migration-era typed spec omits it.
	c.storeTaskCacheWithTypedVerification("builder", "inspect state", &VerificationSpec{
		Type:    VerifyCommandExit,
		Command: "echo state",
	}, "echo state", "observation", "done")
	if _, ok := c.lookupTaskCacheWithTypedVerification(context.Background(), "builder", "inspect state", &VerificationSpec{
		Type:    VerifyCommandExit,
		Command: "echo state",
	}, "echo state", "observation"); ok {
		t.Fatal("mixed typed/legacy observation verification must not be cacheable")
	}
}

func TestTypedVerificationInvalidationAndJournalTombstoneAreContractScoped(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{taskResultCache: make(map[string][]cachedTaskEntry)}
	c.SetPolicyEngine(&defaultPolicyEngine{c: c})

	first := &VerificationSpec{Type: VerifyCommandExit, Command: "test -f report-a.json"}
	second := &VerificationSpec{Type: VerifyCommandExit, Command: "test -f report-b.json"}
	const agentKey = "builder"
	const task = "verify generated report"
	c.storeTaskCacheWithTypedVerification(agentKey, task, first, "", "", "first")
	c.storeTaskCacheWithTypedVerification(agentKey, task, second, "", "", "second")

	// Exercise the DAG reset call site, not just the cache helper.
	s := &dagScheduler{
		coord:  c,
		tasks:  []TaskDef{{Agent: agentKey, Goal: task, VerifySpec: first}},
		states: []TaskStatus{TaskPending},
	}
	s.resetTask(0, "retry")
	entries := c.taskResultCache[agentKey]
	if len(entries) != 1 || !entries[0].matchesVerificationContract(second, "", "") {
		t.Fatalf("DAG invalidation removed unrelated typed cache entry: %#v", entries)
	}

	// A typed tombstone must have the same contract-scoped behavior after a
	// restart/replay of the append-only journal.
	logs := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339)
	records := []journalRecord{
		{Op: "put", Agent: agentKey, Desc: task, VerifySpec: first, Output: "first", TS: now},
		{Op: "put", Agent: agentKey, Desc: task, VerifySpec: second, Output: "second", TS: now},
		{Op: "del", Agent: agentKey, Desc: task, VerifySpec: first, TS: now},
	}
	var data []byte
	for _, rec := range records {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, append(line, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(logs, taskJournalFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	replayed, err := loadTaskJournal(workspace, time.Now(), taskJournalMaxAge, maxTaskCacheEntries)
	if err != nil {
		t.Fatal(err)
	}
	entries = replayed[agentKey]
	if len(entries) != 1 || !entries[0].matchesVerificationContract(second, "", "") {
		t.Fatalf("journal tombstone removed unrelated typed cache entry: %#v", entries)
	}
}

func TestVerifySpecCache_MalformedSpecIsCacheMiss(t *testing.T) {
	c := &Coordinator{
		taskResultCache: map[string][]cachedTaskEntry{
			"builder": {{
				taskDesc:   "write result",
				verifySpec: &VerificationSpec{Type: VerifyFileExists},
				output:     "stale success",
			}},
		},
		session: &TeamSession{Config: agent.TeamConfig{Name: "test"}},
	}
	c.SetPolicyEngine(&defaultPolicyEngine{c: c})
	if _, ok := c.lookupTaskCacheWithTypedVerification(context.Background(), "builder", "write result", &VerificationSpec{Type: VerifyFileExists}, "", ""); ok {
		t.Fatal("malformed typed verification must not reuse a cached success")
	}
}

func TestVerifySpecCache_FileAndJSONEvidenceMustRemainFresh(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		taskResultCache: make(map[string][]cachedTaskEntry),
		session:         &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		projectDir:      dir,
	}
	c.SetPolicyEngine(&defaultPolicyEngine{c: c})

	fileSpec := &VerificationSpec{Type: VerifyFileExists, Path: "artifact.txt"}
	if err := os.WriteFile(filepath.Join(dir, fileSpec.Path), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileEvidence, err := ExecuteVerificationSpec(context.Background(), "sh", dir, *fileSpec)
	if err != nil {
		t.Fatal(err)
	}
	c.storeTaskCacheWithTypedVerificationEvidence("builder", "create artifact", fileSpec, "", "", "cached", fileEvidence)
	if _, ok := c.lookupTaskCacheWithTypedVerification(context.Background(), "builder", "create artifact", fileSpec, "", ""); !ok {
		t.Fatal("fresh file evidence should permit a cache hit")
	}
	if err := os.WriteFile(filepath.Join(dir, fileSpec.Path), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.lookupTaskCacheWithTypedVerification(context.Background(), "builder", "create artifact", fileSpec, "", ""); ok {
		t.Fatal("changed file evidence must invalidate the cache hit")
	}

	jsonSpec := &VerificationSpec{Type: VerifyJSONAssert, Path: "result.json", Assertions: []JSONAssertion{{Path: "status", Equals: "ok"}}}
	if err := os.WriteFile(filepath.Join(dir, jsonSpec.Path), []byte(`{"status":"ok"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonEvidence, err := ExecuteVerificationSpec(context.Background(), "sh", dir, *jsonSpec)
	if err != nil {
		t.Fatal(err)
	}
	c.storeTaskCacheWithTypedVerificationEvidence("builder", "create result", jsonSpec, "", "", "cached", jsonEvidence)
	if _, ok := c.lookupTaskCacheWithTypedVerification(context.Background(), "builder", "create result", jsonSpec, "", ""); !ok {
		t.Fatal("fresh JSON evidence should permit a cache hit")
	}
	if err := os.WriteFile(filepath.Join(dir, jsonSpec.Path), []byte(`{"status":"failed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.lookupTaskCacheWithTypedVerification(context.Background(), "builder", "create result", jsonSpec, "", ""); ok {
		t.Fatal("changed JSON assertion target must invalidate the cache hit")
	}
}

// TestVerifySpecCache_ObservationModeLookupNotReturned verifies that observation cache
// entries are excluded from cache hits.
func TestVerifySpecCache_ObservationModeLookupNotReturned(t *testing.T) {
	entry := cachedTaskEntry{
		taskDesc:   "build the thing",
		verifySpec: &VerificationSpec{Type: VerifyCommandExit, Command: "echo ok", Mode: "observation"},
		output:     "done",
	}
	mandatorySpec := &VerificationSpec{Type: VerifyCommandExit, Command: "echo ok", Mode: "success"}
	if entry.matchesWithSpec("build the thing", mandatorySpec, "", "") {
		t.Fatal("observation cache entry must not match a mandatory success lookup")
	}
	if entry.matchesWithSpec("build the thing", &VerificationSpec{Type: VerifyCommandExit, Command: "echo ok", Mode: "observation"}, "", "") {
		t.Fatal("observation cache entry must not match even an observation lookup")
	}
}

// TestVerifySpecCache_SemanticCandidatesRequireEquivalentTypedContract guards
// the sidecar semantic-cache branch. Typed command_exit specs commonly leave
// the legacy verify fields empty, so only comparing those fields would let a
// semantically similar task reuse a result verified by a different command.
func TestVerifySpecCache_SemanticCandidatesRequireEquivalentTypedContract(t *testing.T) {
	entry := cachedTaskEntry{
		taskDesc: "confirm the generated report exists",
		verifySpec: &VerificationSpec{
			Type:    VerifyCommandExit,
			Command: "test -f report-a.md",
			Mode:    "success",
		},
	}

	requested := &VerificationSpec{
		Type:    VerifyCommandExit,
		Command: "test -f report-b.md",
		Mode:    "success",
	}
	if entry.matchesVerificationContract(requested, "", "") {
		t.Fatal("semantic cache candidates must not share results across distinct typed command_exit verifiers")
	}
	if !entry.matchesVerificationContract(&VerificationSpec{
		Type:    VerifyCommandExit,
		Command: "test -f report-a.md",
		Mode:    "success",
	}, "", "") {
		t.Fatal("equivalent typed command_exit verifier should remain eligible for semantic cache matching")
	}
}

// TestFingerprintDeterminism verifies ComputeVerificationFingerprintFull produces
// identical fingerprints for equivalent (reordered) assertion specs.
func TestFingerprintDeterminism(t *testing.T) {
	dir := t.TempDir()
	spec1 := VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "out.json",
		Mode: "success",
		Assertions: []JSONAssertion{
			{Path: "z_field", Equals: "last"},
			{Path: "a_field", Equals: 1},
		},
	}
	spec2 := VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "out.json",
		Mode: "success",
		Assertions: []JSONAssertion{
			{Path: "a_field", Equals: 1},
			{Path: "z_field", Equals: "last"},
		},
	}

	fp1 := ComputeVerificationFingerprintFull(spec1, nil, dir, "", "")
	fp2 := ComputeVerificationFingerprintFull(spec2, nil, dir, "", "")
	if fp1 != fp2 {
		t.Errorf("fingerprints must be identical for reordered assertions: fp1=%s fp2=%s", fp1, fp2)
	}
	fp3 := ComputeVerificationFingerprintFull(spec1, nil, dir, "rev-42", "")
	if fp1 == fp3 {
		t.Errorf("fingerprint must differ when acceptance revision changes")
	}
	fp4 := ComputeVerificationFingerprintFull(spec1, nil, dir, "", "rbash")
	if fp1 == fp4 {
		t.Errorf("fingerprint must differ when security mode changes")
	}
}

// TestAcceptanceVerificationEvidence verifies that runAcceptance populates VerificationEvidence.
func TestAcceptanceVerificationEvidence(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "artifact.txt")
	if err := os.WriteFile(sentinel, []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session: &TeamSession{
			Config: agent.TeamConfig{Name: "test"},
		},
		acceptanceSpec: &agent.AcceptanceSpec{
			Verifications: []agent.VerificationSpec{
				{Type: agent.VerifyFileExists, Path: sentinel, Mode: "success"},
			},
		},
		projectDir:  dir,
		taskTracker: NewTaskTracker(),
	}

	res, err := c.runAcceptance(context.Background())
	if err != nil {
		t.Fatalf("runAcceptance failed: %v", err)
	}
	if !res.Passed {
		t.Fatalf("expected acceptance to pass, got errors: %v", res.Errors)
	}
	if len(res.VerificationEvidence) == 0 {
		t.Fatal("expected VerificationEvidence to be populated after acceptance run")
	}
	ev := res.VerificationEvidence[0]
	if ev.ExitCode != 0 {
		t.Fatalf("expected evidence exit code 0, got %d", ev.ExitCode)
	}
	if !strings.HasPrefix(ev.Fingerprint, "vfp_") {
		t.Fatalf("expected evidence fingerprint with vfp_ prefix, got %q", ev.Fingerprint)
	}

	firstFingerprint := ev.Fingerprint
	c.acceptanceContractRevision = 2
	res2, err := c.runAcceptance(context.Background())
	if err != nil {
		t.Fatalf("runAcceptance after contract revision failed: %v", err)
	}
	if len(res2.VerificationEvidence) != 1 {
		t.Fatalf("expected one verification evidence result after revision, got %d", len(res2.VerificationEvidence))
	}
	if res2.VerificationEvidence[0].Fingerprint == firstFingerprint {
		t.Fatal("acceptance contract revision must invalidate the evidence fingerprint")
	}
}

// TestVerifySpecEventStoreRoundTrip verifies that VerifySpec round-trips through JSON.
func TestVerifySpecEventStoreRoundTrip(t *testing.T) {
	vs := &VerificationSpec{
		Type: VerifyFileExists,
		Path: "workspace/output.md",
		Mode: "success",
	}

	payload := map[string]interface{}{
		"id":          "1",
		"verify_spec": vs,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	var roundTripped map[string]json.RawMessage
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if _, ok := roundTripped["verify_spec"]; !ok {
		t.Fatal("verify_spec must be present in JSON payload")
	}

	var recovered VerificationSpec
	if err := json.Unmarshal(roundTripped["verify_spec"], &recovered); err != nil {
		t.Fatalf("verify_spec must unmarshal cleanly: %v", err)
	}
	if recovered.Type != vs.Type || recovered.Path != vs.Path {
		t.Fatalf("verify_spec round-trip failed: got %+v want %+v", recovered, vs)
	}
}

func TestVerifySpecTaskJournalRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	logsDir := filepath.Join(workspace, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := &VerificationSpec{
		Type: VerifyJSONAssert,
		Path: "result.json",
		Mode: "success",
		Assertions: []JSONAssertion{{
			Path:   "status",
			Equals: "ok",
		}},
	}
	record := journalRecord{
		Op:         "put",
		Agent:      "builder",
		Desc:       "produce verified result",
		VerifySpec: spec,
		Output:     "done",
		TS:         time.Now().Format(time.RFC3339),
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskJournalPath(workspace), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := loadTaskJournal(workspace, time.Now(), 0, 10)
	if err != nil {
		t.Fatalf("load typed journal record: %v", err)
	}
	loaded := entries["builder"]
	if len(loaded) != 1 || loaded[0].verifySpec == nil {
		t.Fatalf("typed verify spec was not restored: %#v", loaded)
	}
	if got := loaded[0].verifySpec; got.Type != VerifyJSONAssert || got.Path != "result.json" || len(got.Assertions) != 1 || got.Assertions[0].Equals != "ok" {
		t.Fatalf("restored typed verify spec = %#v", got)
	}
	if loaded[0].verifySpec == spec {
		t.Fatal("journal restoration must detach the caller-owned verification spec")
	}
}

func TestAcceptanceConfigParsesTypedVerificationAndLegacyString(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: typed-acceptance
acceptance:
  verifications:
    - type: json_assert
      path: result.json
      mode: success
      assertions:
        - path: status
          equals: ok
`
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parse typed acceptance: %v", err)
	}
	if cfg.AcceptanceSpec == nil || len(cfg.AcceptanceSpec.Verifications) != 1 {
		t.Fatalf("typed acceptance was not retained: %#v", cfg.AcceptanceSpec)
	}
	got := cfg.AcceptanceSpec.Verifications[0]
	if got.Type != VerifyJSONAssert || got.Path != "result.json" || len(got.Assertions) != 1 || got.Assertions[0].Equals != "ok" {
		t.Fatalf("typed verification mismatch: %#v", got)
	}

	legacyDir := t.TempDir()
	legacyYAML := "name: legacy-acceptance\nacceptance: test -f result.json\n"
	if err := os.WriteFile(filepath.Join(legacyDir, "team.yml"), []byte(legacyYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyCfg, err := parseTeamYML(legacyDir, nil)
	if err != nil {
		t.Fatalf("parse legacy acceptance: %v", err)
	}
	if legacyCfg.AcceptanceSpec == nil || len(legacyCfg.AcceptanceSpec.Commands) != 1 || legacyCfg.AcceptanceSpec.Commands[0] != "test -f result.json" {
		t.Fatalf("legacy acceptance was not translated: %#v", legacyCfg.AcceptanceSpec)
	}
}
