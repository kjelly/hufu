package improver

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

func resolveSurfaces(v Variant, session *team.TeamSession, allowed []paired.Surface) ([]ResolvedSurface, error) {
	surfaces := make([]ResolvedSurface, 0, len(allowed))
	seen := map[string]bool{}
	for _, declaration := range allowed {
		surface := ResolvedSurface{Kind: declaration.Kind}
		if declaration.Kind == "skill_body" {
			if !slices.ContainsFunc(v.Skills, func(s FrozenSkill) bool { return s.Path == declaration.Path }) {
				return nil, errors.New("skill surface must be explicitly selected by the worker")
			}
			surface.Path = declaration.Path
		} else {
			def := exactWorker(session, declaration.Agent)
			if def == nil {
				def = session.Agents[declaration.Agent]
			}
			if def == nil || def.Role == "coordinator" || def.Role == "orchestrator" || def.Name == "Helper" {
				return nil, fmt.Errorf("surface agent %q is not a worker", declaration.Agent)
			}
			source, exists := v.sources.Agents[def.FileAlias]
			if !exists || source.File == "" {
				return nil, errors.New("surface requires an authored worker, not a generated team preset role")
			}
			surface.Agent, surface.Path = def.Name, source.File
			switch declaration.Kind {
			case "markdown_body":
				if source.BodyField != "" || path.Ext(source.File) != ".md" {
					return nil, errors.New("markdown_body requires an authored Markdown worker")
				}
			case "inline_instructions":
				if (!strings.HasPrefix(source.FieldPath, "agents.") && !strings.HasPrefix(source.FieldPath, "spec.agents.")) ||
					(source.File != "team.yaml" && source.File != "team.yml") {
					return nil, errors.New("inline_instructions requires an inline declaration")
				}
				surface.Field = source.FieldPath
			default:
				return nil, errors.New("unsupported surface kind")
			}
		}
		key := surface.Kind + "/" + surface.Agent + "/" + surface.Path
		if seen[key] {
			return nil, errors.New("duplicate resolved surface")
		}
		seen[key] = true
		text, err := surfaceText(v.Tree, surface)
		if err != nil {
			return nil, err
		}
		if len(text) > paired.MaxTextBytes {
			return nil, errors.New("surface exceeds text limit")
		}
		surface.BeforeHash = paired.BytesDigest([]byte(text))
		surfaces = append(surfaces, surface)
	}
	slices.SortFunc(surfaces, func(a, b ResolvedSurface) int {
		return strings.Compare(a.Kind+"/"+a.Agent+"/"+a.Path, b.Kind+"/"+b.Agent+"/"+b.Path)
	})
	for i := range surfaces {
		surfaces[i].ID = fmt.Sprintf("surface_%03d", i+1)
	}
	return surfaces, nil
}

func inlineDeclaration(object map[string]any, surface ResolvedSurface) (map[string]any, error) {
	spec, err := manifestSpec(object)
	if err != nil {
		return nil, err
	}
	agents, ok := spec["agents"].(map[string]any)
	if !ok {
		return nil, errors.New("inline agents mapping missing")
	}
	// FieldPath uses the authored key; canonical names may differ in case.
	_, authoredName, found := strings.CutLast(surface.Field, ".")
	if !found {
		authoredName = surface.Agent
	}
	value, exists := agents[authoredName]
	if !exists {
		return nil, errors.New("inline declaration missing")
	}
	if preset, shorthand := value.(string); shorthand {
		declaration := map[string]any{"preset": preset}
		agents[authoredName] = declaration
		return declaration, nil
	}
	declaration, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("inline declaration must be mapping or preset shorthand")
	}
	return declaration, nil
}

func surfaceText(tree Tree, surface ResolvedSurface) (string, error) {
	raw, ok := tree.files[surface.Path]
	if !ok {
		return "", fmt.Errorf("surface file %q missing", surface.Path)
	}
	if surface.Kind != "inline_instructions" {
		_, body, err := splitMarkdown(raw)
		return string(body), err
	}
	object, err := yamlObject(raw)
	if err != nil {
		return "", err
	}
	declaration, err := inlineDeclaration(object, surface)
	if err != nil {
		return "", err
	}
	value, exists := declaration["instructions"]
	if !exists {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", errors.New("instructions must be a string")
	}
	return text, nil
}

func authoredProjection(tree Tree, surfaces []ResolvedSurface) (map[string]any, error) {
	projection := make(map[string]any, len(tree.files))
	for _, name := range slices.Sorted(maps.Keys(tree.files)) {
		raw := tree.files[name]
		var selected []ResolvedSurface
		for _, surface := range surfaces {
			if surface.Path == name {
				selected = append(selected, surface)
			}
		}
		if len(selected) == 0 {
			projection[name] = paired.BytesDigest(raw)
			continue
		}
		if selected[0].Kind != "inline_instructions" {
			header, _, err := splitMarkdown(raw)
			if err != nil {
				return nil, err
			}
			projection[name] = paired.BytesDigest(header)
			continue
		}
		object, err := yamlObject(raw)
		if err != nil {
			return nil, err
		}
		for _, surface := range selected {
			declaration, err := inlineDeclaration(object, surface)
			if err != nil {
				return nil, err
			}
			// A typed marker, not a string sentinel, cannot collide with valid
			// authored instructions. Decoding aliases first also catches shared
			// anchor changes to other workers or fields.
			declaration["instructions"] = map[string]bool{"paired_variable_surface": true}
		}
		projection[name] = object
	}
	return projection, nil
}
