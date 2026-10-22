package improver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aymanbagabas/go-udiff"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/utils"
)

func exactPatch(ctx context.Context, baseline, candidate Tree) ([]byte, error) {
	if len(baseline.files) != len(candidate.files) {
		return nil, errors.New("patch cannot add or remove protected files")
	}
	var patch bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(baseline.files)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before := baseline.files[name]
		after, exists := candidate.files[name]
		if !exists {
			return nil, errors.New("patch candidate inventory differs")
		}
		if bytes.Equal(before, after) {
			continue
		}
		if !paired.SafeRelativePath(name) || !utf8.Valid(before) || !utf8.Valid(after) {
			return nil, errors.New("patch requires normalized paths and UTF-8 changed files")
		}
		oldLabel, newLabel := quotePatchPath("a/"+name), quotePatchPath("b/"+name)
		diff, err := udiff.ToUnified(oldLabel, newLabel, string(before), udiff.Lines(string(before), string(after)), 3)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&patch, "diff --git %s %s\n%s", oldLabel, newLabel, diff)
		if patch.Len() > 2*paired.MaxTotalBytes {
			return nil, errors.New("patch exceeds byte limit")
		}
	}
	return patch.Bytes(), nil
}

// Git uses C quoting with octal byte escapes, not Go's Unicode escapes.
func quotePatchPath(path string) string {
	var output strings.Builder
	output.WriteByte('"')
	for i := range len(path) {
		value := path[i]
		switch {
		case value == '"' || value == '\\':
			output.WriteByte('\\')
			output.WriteByte(value)
		case value < 0x20 || value >= 0x7f:
			fmt.Fprintf(&output, "\\%03o", value)
		default:
			output.WriteByte(value)
		}
	}
	output.WriteByte('"')
	return output.String()
}

// applyPatchTree uses Git's patch parser in a new temporary baseline copy.
// The source team, source journal and candidate tree are never written.
func applyPatchTree(ctx context.Context, baseline Tree, patch []byte) (result Tree, retErr error) {
	parent, err := os.MkdirTemp("", "hufu-patch-check-")
	if err != nil {
		return Tree{}, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(parent)) }()
	dir := filepath.Join(parent, "baseline")
	if err := baseline.WriteNew(dir); err != nil {
		return Tree{}, err
	}
	if len(patch) != 0 {
		git, err := exec.LookPath("git")
		if err != nil {
			return Tree{}, errors.New("review export requires local git for patch round-trip verification")
		}
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for _, check := range []bool{true, false} {
			args := []string{"-c", "core.autocrlf=false", "-c", "core.attributesFile=" + os.DevNull,
				"apply", "--whitespace=nowarn", "-p1"}
			if check {
				args = append(args, "--check")
			}
			args = append(args, "--")
			command := exec.CommandContext(bounded, git, args...)
			command.Dir = dir
			command.Stdin = bytes.NewReader(patch)
			command.Env = []string{
				"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0",
				"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
				"GIT_ATTR_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES=" + parent,
			}
			output, err := command.CombinedOutput()
			if err != nil {
				return Tree{}, fmt.Errorf("patch round-trip: %w: %s", err, utils.TruncateString(utils.RedactSecrets(string(output)), 2048))
			}
		}
	}
	return CaptureTree(dir)
}

func verifyPatchRoundTrip(ctx context.Context, prepared Prepared, candidate Variant, patch []byte) error {
	tree, err := applyPatchTree(ctx, prepared.Variants["baseline"].Tree, patch)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(tree.Inventory(), candidate.Tree.Inventory()) {
		return errors.New("patch round-trip did not preserve exact candidate bytes")
	}
	loaded, _, err := materializeTree(tree, prepared.Config.Vars, prepared.Config.Worker,
		prepared.Preview.Build.BinaryDigest, prepared.Config.AllowedSurfaces)
	if err != nil {
		return err
	}
	if loaded.VariantDigest != candidate.VariantDigest {
		return errors.New("patch round-trip did not reproduce the tested variant")
	}
	return CompareCandidate(prepared.Variants["baseline"], loaded, prepared.Preview.Surfaces)
}

func bundleChanges(prepared Prepared, candidate Variant) ([]paired.Change, error) {
	var changes []paired.Change
	for _, surface := range prepared.Preview.Surfaces {
		before, err := surfaceText(prepared.Variants["baseline"].Tree, surface)
		if err != nil {
			return nil, err
		}
		after, err := surfaceText(candidate.Tree, surface)
		if err != nil {
			return nil, err
		}
		if before != after {
			changes = append(changes, paired.Change{SurfaceID: surface.ID,
				BeforeHash: paired.BytesDigest([]byte(before)), ReplacementText: after})
		}
	}
	return changes, nil
}
