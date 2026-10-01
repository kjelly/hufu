package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/utils"
)

const controlDecisionsBlock = `control-decisions:
  model: nimble
  timeout: 4s
  mode: shadow
  points:
    path-reviewer: {mode: active, min-confidence: 0.95}
`

func TestControlDecisionsParseInBothManifestForms(t *testing.T) {
	indented := "  " + strings.ReplaceAll(strings.TrimSuffix(controlDecisionsBlock, "\n"), "\n", "\n  ") + "\n"
	manifests := map[string]string{
		"flat":     "name: control-team\n" + controlDecisionsBlock,
		"v1alpha1": "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: control-team\nspec:\n" + indented,
	}
	for name, manifest := range manifests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTeamManifest(t, dir, manifest)
			cfg, err := parseTeamYML(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.ControlDecisions
			if got.Model != "nimble" || got.Timeout != 4*time.Second || got.Mode != control.ModeShadow ||
				got.Points[control.PathReviewer].Mode != control.ModeActive || *got.Points[control.PathReviewer].MinConfidence != 0.95 {
				t.Fatalf("control-decisions = %#v", got)
			}
		})
	}
}

func TestControlDecisionsRejectInvalidTeamBlock(t *testing.T) {
	tests := map[string]string{
		"unknown mode":  "control-decisions:\n  mode: always\n",
		"unknown point": "control-decisions:\n  points:\n    similar-task: {mode: shadow}\n",
		"unknown field": "control-decisions:\n  backend: sidecar\n",
		"bad threshold": "control-decisions:\n  min-confidence: 2\n",
	}
	for name, block := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTeamManifest(t, dir, "name: control-team\n"+block)
			if _, err := parseTeamYML(dir, nil); err == nil {
				t.Fatal("parseTeamYML accepted an invalid control-decisions block")
			}
		})
	}
}

func TestNoControlDecisionsBlockParsesToZero(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, "name: plain-team\n")
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlDecisions.Mode != "" || cfg.ControlDecisions.Points != nil || cfg.ControlDecisions.Model != "" {
		t.Fatalf("control-decisions = %#v", cfg.ControlDecisions)
	}
}

func loadControlDecisionTeam(t *testing.T, teamBlock string, global control.Config) *TeamSession {
	t.Helper()
	workspace := t.TempDir()
	dir := filepath.Join(workspace, "team")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTeamManifest(t, dir, "name: control-team\nmax-retries: 0\n"+teamBlock)
	writeAgentFile(t, dir, "coder.md", "name: coder\ndescription: Codes.\ntools: view", "Code.")
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	session.Workspace = filepath.Join(workspace, "session")
	if err := os.MkdirAll(session.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(workspace, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := session.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	session.GlobalControlDecisions = global
	return session
}

func newControlDecisionCoordinator(t *testing.T, session *TeamSession) (*Coordinator, error) {
	t.Helper()
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if c.eventStore != nil {
			_ = c.eventStore.Close()
		}
		c.CloseContextPreflight()
	})
	return c, nil
}

func TestCoordinatorMergesHufuAndTeamControlDecisions(t *testing.T) {
	global := control.Config{Endpoint: "http://gpu:11434/v1/systemone", Model: "nimble", Mode: control.ModeShadow,
		Points: map[control.Point]control.PointConfig{control.GuardReviewer: {Mode: control.ModeOff}}}
	session := loadControlDecisionTeam(t, "control-decisions:\n  points:\n    path-reviewer: {mode: active}\n", global)
	c, err := newControlDecisionCoordinator(t, session)
	if err != nil {
		t.Fatal(err)
	}
	want := map[control.Point]control.Mode{
		control.AgentMatcher: control.ModeShadow, control.AskUser: control.ModeShadow,
		control.PathReviewer: control.ModeActive, control.GuardReviewer: control.ModeOff,
	}
	for point, mode := range want {
		if got := c.controlDecisions.Mode(point); got != mode {
			t.Errorf("%s mode = %s, want %s", point, got, mode)
		}
	}
	if len(c.controlDecisions.Hash()) != 64 {
		t.Fatalf("active hash = %q", c.controlDecisions.Hash())
	}
	clone := cloneCoordinator(c, cloneSession(c.session, c.session.Workspace))
	if clone.controlDecisions != c.controlDecisions {
		t.Fatal("extra-model clone does not share the control decision service")
	}
}

func TestCoordinatorRejectsIncompleteMergedControlDecisions(t *testing.T) {
	session := loadControlDecisionTeam(t, "control-decisions:\n  mode: shadow\n", control.Config{})
	if _, err := newControlDecisionCoordinator(t, session); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("NewCoordinator error = %v, want a missing model error", err)
	}
	session = loadControlDecisionTeam(t, "", control.Config{})
	c, err := newControlDecisionCoordinator(t, session)
	if err != nil {
		t.Fatal(err)
	}
	if c.controlDecisions.Enabled() || c.controlDecisions.Hash() != "" {
		t.Fatal("a team without control-decisions enabled a point")
	}
}

func TestCloneSessionCopiesControlDecisions(t *testing.T) {
	session := &TeamSession{GlobalControlDecisions: control.Config{Points: map[control.Point]control.PointConfig{control.AskUser: {Mode: control.ModeShadow}}}}
	session.Config.ControlDecisions = control.Config{Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeActive}}}
	clone := cloneSession(session, "elsewhere")
	clone.GlobalControlDecisions.Points[control.AskUser] = control.PointConfig{Mode: control.ModeOff}
	clone.Config.ControlDecisions.Points[control.PathReviewer] = control.PointConfig{Mode: control.ModeOff}
	if session.GlobalControlDecisions.Points[control.AskUser].Mode != control.ModeShadow || session.Config.ControlDecisions.Points[control.PathReviewer].Mode != control.ModeActive {
		t.Fatal("cloneSession aliased control-decisions")
	}
}

func TestPolicySnapshotPinsActiveControlDecisionsOnly(t *testing.T) {
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 2)
	snapshotHash := func(config control.Config) (string, string) {
		t.Helper()
		service, err := control.New(config, utils.RedactSecrets)
		if err != nil {
			t.Fatal(err)
		}
		c.controlDecisions = service
		state, err := newExecutionPolicyState(c)
		if err != nil {
			t.Fatal(err)
		}
		return state.snapshot.ConfigurationHash, state.snapshot.ControlDecisionHash
	}
	baseline, baselineControl := snapshotHash(control.Config{})
	shadow, shadowControl := snapshotHash(control.Config{Model: "nimble", Mode: control.ModeShadow})
	if baselineControl != "" || shadowControl != "" || shadow != baseline {
		t.Fatalf("off or shadow changed the snapshot: %q %q %q %q", baseline, shadow, baselineControl, shadowControl)
	}
	active, activeControl := snapshotHash(control.Config{Model: "nimble", Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeActive}}})
	if activeControl == "" || active == baseline {
		t.Fatal("an active point is not pinned in the policy snapshot")
	}
	drifted, _ := snapshotHash(control.Config{Model: "nimble", Points: map[control.Point]control.PointConfig{control.PathReviewer: {Mode: control.ModeActive, MinConfidence: new(0.99)}}})
	if drifted == active {
		t.Fatal("an active threshold change does not drift the policy snapshot")
	}
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	state.snapshot.ControlDecisionHash = "not-a-digest"
	if err := validateExecutionPolicySnapshot(state.snapshot); err == nil {
		t.Fatal("an invalid control decision hash was accepted")
	}
}
