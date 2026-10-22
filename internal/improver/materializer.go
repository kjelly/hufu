package improver

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/team"
)

type ResolvedSurface struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Agent      string `json:"agent,omitempty"`
	Path       string `json:"path"`
	Field      string `json:"field,omitempty"`
	BeforeHash string `json:"before_hash"`
}

type FrozenSkill struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Variant struct {
	Inventory     []File                    `json:"inventory"`
	Agents        map[string]agent.AgentDef `json:"agents"`
	Config        agent.TeamConfig          `json:"config"`
	Skills        []FrozenSkill             `json:"skills"`
	Vars          map[string]string         `json:"vars"`
	BuildDigest   string                    `json:"build_digest"`
	VariantDigest string                    `json:"variant_digest"`
	FixedDigest   string                    `json:"fixed_digest"`
	Tree          Tree                      `json:"-"`
	fixedAuthored map[string]any
	sources       *team.TeamSourceIndex
}

// Materialize captures and loads an independent team copy. The temporary copy
// is removed before return; no runtime workspace or provider is initialized.
func Materialize(dir string, vars map[string]string, worker, buildDigest string, allowed []paired.Surface) (Variant, []ResolvedSurface, error) {
	tree, err := CaptureTree(dir)
	if err != nil {
		return Variant{}, nil, err
	}
	return materializeTree(tree, vars, worker, buildDigest, allowed)
}

func materializeTree(tree Tree, vars map[string]string, worker, buildDigest string, allowed []paired.Surface) (variant Variant, resolved []ResolvedSurface, retErr error) {
	if err := checkClassifiedFields(reflect.TypeFor[agent.AgentDef](), agentFields); err != nil {
		return Variant{}, nil, err
	}
	if err := validateAuthoredTree(tree); err != nil {
		return Variant{}, nil, err
	}
	if err := validateRenderedTree(tree, vars); err != nil {
		return Variant{}, nil, err
	}
	parent, err := os.MkdirTemp("", "hufu-paired-")
	if err != nil {
		return Variant{}, nil, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(parent)) }()
	dir := filepath.Join(parent, "team")
	if err := tree.WriteNew(dir); err != nil {
		return Variant{}, nil, err
	}
	inspection, err := team.InspectTeamWithOptions(dir, vars, nil, team.DefaultProviderRegistry, team.TeamCompileLint,
		team.TeamLoadOptions{SkillSearchPaths: []string{filepath.Join(dir, "skills")}})
	if err != nil {
		return Variant{}, nil, err
	}
	if !inspection.Complete || inspection.Session == nil {
		return Variant{}, nil, errors.New("team: incomplete normalized definition")
	}
	if err := validateEffectiveProfile(inspection.Session, worker); err != nil {
		return Variant{}, nil, err
	}
	result := Variant{
		Inventory: tree.Inventory(), Agents: map[string]agent.AgentDef{}, Vars: maps.Clone(vars),
		BuildDigest: buildDigest, Tree: tree, sources: inspection.Sources, Config: inspection.Session.Config,
	}
	for _, def := range inspection.Session.Agents {
		result.Agents[def.Name] = *def
	}
	result.Skills, err = captureSelectedSkills(inspection.Session, worker)
	if err != nil {
		return Variant{}, nil, err
	}
	surfaces, err := resolveSurfaces(result, inspection.Session, allowed)
	if err != nil {
		return Variant{}, nil, err
	}
	result.fixedAuthored, err = authoredProjection(tree, surfaces)
	if err != nil {
		return Variant{}, nil, err
	}
	fixedAgents := maps.Clone(result.Agents)
	fixedSkills := slices.Clone(result.Skills)
	for _, surface := range surfaces {
		if surface.Kind == "skill_body" {
			for i := range fixedSkills {
				if fixedSkills[i].Path == surface.Path {
					fixedSkills[i].Content = ""
				}
			}
		} else {
			def := fixedAgents[surface.Agent]
			if def.Capabilities != team.ExtractCapabilitiesFromSystem(def.System) {
				return Variant{}, nil, errors.New("unsupported_profile: unclassified capability derivation")
			}
			def.Capabilities = ""
			def.System = ""
			fixedAgents[surface.Agent] = def
		}
	}
	result.FixedDigest, err = projectionDigest("fixed_team", map[string]any{
		"authored": result.fixedAuthored, "agents": fixedAgents, "skills": fixedSkills,
		"vars": vars, "build_digest": buildDigest, "config": result.Config,
	})
	if err != nil {
		return Variant{}, nil, err
	}
	result.VariantDigest, err = projectionDigest("variant", map[string]any{
		"inventory": result.Inventory, "agents": result.Agents, "skills": result.Skills,
		"vars": result.Vars, "build_digest": buildDigest, "config": result.Config,
	})
	return result, surfaces, err
}

func projectionDigest(kind string, value any) (string, error) {
	raw, err := paired.ProjectionJSON(value)
	if err != nil {
		return "", err
	}
	return paired.Digest(kind, jsontext.Value(raw))
}

func validateEffectiveProfile(session *team.TeamSession, worker string) error {
	def := exactWorker(session, worker)
	if def == nil || def.Name != worker || worker == "Helper" || def.Role == "coordinator" || def.Role == "orchestrator" {
		return errors.New("worker: exact existing worker identity required")
	}
	cfg := session.Config
	if (cfg.DefaultLLMBackend != "" && cfg.DefaultLLMBackend != "hufu-local") ||
		(cfg.WorkerMemory.Mode != "" && cfg.WorkerMemory.Mode != agent.WorkerMemoryOff) ||
		(cfg.MemoryLearning.Mode != "" && cfg.MemoryLearning.Mode != agent.MemoryLearningOff) ||
		(cfg.SemanticRetrieval.Mode != "" && cfg.SemanticRetrieval.Mode != "off") || cfg.ProjectContext {
		return errors.New("unsupported_profile: backend, memory, semantic retrieval or project context")
	}
	if (def.Memory.Mode != "" && def.Memory.Mode != agent.WorkerMemoryOff) || (def.SideEffect != "" && def.SideEffect != "none") ||
		def.ProviderURL != "" || len(def.Guard) > 0 || len(def.ExtraModels) > 0 || def.ExecutionRoute != "" ||
		def.SubagentProvider != "" && def.SubagentProvider != "hufu-local" {
		return errors.New("unsupported_profile: worker requires an unsupported capability")
	}
	for tool := range strings.SplitSeq(def.Tools, ",") {
		tool = strings.TrimSpace(tool)
		if tool != "submit_result" && !slices.Contains(cfg.ToolsDenied, tool) {
			return fmt.Errorf("unsupported_profile: worker tool %q", tool)
		}
	}
	if slices.Contains(cfg.ToolsDenied, "submit_result") {
		return errors.New("unsupported_profile: submit_result is denied")
	}
	return nil
}

func captureSelectedSkills(session *team.TeamSession, worker string) ([]FrozenSkill, error) {
	names := skill.ParseSkillList(exactWorker(session, worker).Skills)
	selected := make([]*skill.SkillDef, 0, len(names))
	for _, name := range names {
		index := slices.IndexFunc(session.Skills, func(s *skill.SkillDef) bool { return s.Name == name })
		if index < 0 {
			return nil, fmt.Errorf("skill %q: not found in frozen local catalog", name)
		}
		selected = append(selected, session.Skills[index])
	}
	result := make([]FrozenSkill, 0, len(selected))
	for _, sk := range selected {
		if sk.AllowedTools != "" || len(skill.UnresolvedSkillDependencies(sk, selected)) != 0 ||
			len(skill.ExpandSkillDependencies(sk, selected)) != 1 {
			return nil, fmt.Errorf("skill %q: only independent text skills are supported", sk.Name)
		}
		if len(sk.Content) > paired.MaxTextBytes {
			return nil, fmt.Errorf("skill %q: text limit exceeded", sk.Name)
		}
		relative, err := filepath.Rel(session.Dir, sk.Path)
		if err != nil || !paired.SafeRelativePath(filepath.ToSlash(relative)) {
			return nil, errors.New("skill escapes frozen team")
		}
		result = append(result, FrozenSkill{sk.Name, filepath.ToSlash(relative), sk.Content})
	}
	slices.SortFunc(result, func(a, b FrozenSkill) int { return strings.Compare(a.Path, b.Path) })
	return result, nil
}

func exactWorker(session *team.TeamSession, name string) *agent.AgentDef {
	for _, def := range session.Agents {
		if def.Name == name {
			return def
		}
	}
	return nil
}

// CompareCandidate checks authored surfaces and every normalized agent field.
// It never masks a field by its name looking like a path or hash.
func CompareCandidate(baseline, candidate Variant, surfaces []ResolvedSurface) error {
	if baseline.FixedDigest != candidate.FixedDigest || !reflect.DeepEqual(baseline.fixedAuthored, candidate.fixedAuthored) {
		return errors.New("protected_surface: candidate changed fixed authored or effective settings")
	}
	for _, surface := range surfaces {
		before, err := surfaceText(baseline.Tree, surface)
		if err != nil {
			return err
		}
		after, err := surfaceText(candidate.Tree, surface)
		if err != nil {
			return err
		}
		if before != after {
			if err := validateReplacement(after); err != nil {
				return fmt.Errorf("%s: %w", surface.ID, err)
			}
		}
	}
	return nil
}
