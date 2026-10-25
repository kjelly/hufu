package improver

import (
	"bytes"
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/kjelly/hufu/internal/improve/paired"
)

// ApplyProposal changes only registered surfaces in a private tree, then runs
// the same authored and effective comparison used for manual candidates.
func ApplyProposal(prepared Prepared, proposal paired.ChangeProposal) (Variant, error) {
	if err := proposal.Validate(); err != nil {
		return Variant{}, err
	}
	baseline, exists := prepared.Variants["baseline"]
	if !exists {
		return Variant{}, errors.New("proposal baseline is missing")
	}
	tree := Tree{files: make(map[string][]byte, len(baseline.Tree.files))}
	for name, raw := range baseline.Tree.files {
		tree.files[name] = bytes.Clone(raw)
	}
	surfaces := map[string]ResolvedSurface{}
	for _, surface := range prepared.Preview.Surfaces {
		surfaces[surface.ID] = surface
	}
	for _, change := range proposal.Changes {
		surface, exists := surfaces[change.SurfaceID]
		if !exists || change.BeforeHash != surface.BeforeHash {
			return Variant{}, errors.New("proposal surface is unregistered or its base hash changed")
		}
		before, err := surfaceText(baseline.Tree, surface)
		if err != nil || paired.BytesDigest([]byte(before)) != change.BeforeHash {
			return Variant{}, errors.New("proposal baseline surface differs from frozen registration")
		}
		if err := validateReplacement(change.ReplacementText); err != nil {
			return Variant{}, err
		}
		if err := replaceSurface(&tree, surface, change.ReplacementText); err != nil {
			return Variant{}, err
		}
	}
	variant, _, err := materializeTree(tree, prepared.Config.Vars, prepared.Config.Worker,
		baseline.BuildDigest, prepared.Config.AllowedSurfaces)
	if err != nil {
		return Variant{}, err
	}
	if err := CompareCandidate(baseline, variant, prepared.Preview.Surfaces); err != nil {
		return Variant{}, err
	}
	return variant, nil
}

func replaceSurface(tree *Tree, surface ResolvedSurface, replacement string) error {
	raw, exists := tree.files[surface.Path]
	if !exists {
		return errors.New("registered surface file is missing")
	}
	if surface.Kind == "inline_instructions" {
		object, err := yamlObject(raw)
		if err != nil {
			return err
		}
		declaration, err := inlineDeclaration(object, surface)
		if err != nil {
			return err
		}
		declaration["instructions"] = replacement
		raw, err = yaml.Marshal(object)
		if err != nil {
			return err
		}
	} else {
		header, _, err := splitMarkdown(raw)
		if err != nil {
			return err
		}
		raw = append(bytes.Clone(header), []byte(replacement)...)
	}
	if len(raw) > paired.MaxDataBytes {
		return errors.New("proposed file exceeds size limit")
	}
	if err := checkSanitized(surface.Path, raw); err != nil {
		return fmt.Errorf("proposal content: %w", err)
	}
	tree.files[surface.Path] = raw
	return nil
}
