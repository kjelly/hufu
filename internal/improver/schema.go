package improver

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

const schemaDialect = "https://json-schema.org/draft/2020-12/schema"
const resourceDefinitions = "hufu_paired_resources"

type schemaBundle struct {
	root      *os.Root
	files     map[string][]byte
	resources map[string]jsontext.Value
	active    map[string]bool
	total     int
}

// captureSchema resolves a bounded local closure, then rewrites it to one
// document for the existing runtime compiler. That compiler's ban on network
// and sibling-file loading remains unchanged.
func captureSchema(root *os.Root, name string) ([]byte, Tree, error) {
	bundle := schemaBundle{root: root, files: map[string][]byte{}, resources: map[string]jsontext.Value{}, active: map[string]bool{}}
	raw, err := bundle.document(name, "")
	if err != nil {
		return nil, Tree{}, err
	}
	var document map[string]jsontext.Value
	if err := json.Unmarshal(raw, &document); err != nil || document == nil {
		return nil, Tree{}, errors.New("result_schema: root must be an object")
	}
	if len(bundle.resources) > 0 {
		definitions := map[string]jsontext.Value{}
		if existing, ok := document["$defs"]; ok {
			if err := json.Unmarshal(existing, &definitions); err != nil || definitions == nil {
				return nil, Tree{}, errors.New("result_schema.$defs: expected object")
			}
		}
		if _, collision := definitions[resourceDefinitions]; collision {
			return nil, Tree{}, errors.New("result_schema: reserved bundled resource key")
		}
		resourceRaw, err := paired.CanonicalJSON(map[string]any{"$defs": bundle.resources})
		if err != nil {
			return nil, Tree{}, err
		}
		definitions[resourceDefinitions] = resourceRaw
		defRaw, err := paired.CanonicalJSON(definitions)
		if err != nil {
			return nil, Tree{}, err
		}
		document["$defs"] = defRaw
	}
	raw, err = paired.CanonicalJSON(document)
	if err != nil {
		return nil, Tree{}, err
	}
	if _, err := team.CompileResultContractDocument("paired.schema.json", raw); err != nil {
		return nil, Tree{}, err
	}
	return raw, Tree{files: bundle.files}, nil
}

func resourceName(name string) string { return "resource_" + paired.BytesDigest([]byte(name)) }

func resourcePrefix(name string) string {
	return "/$defs/" + resourceDefinitions + "/$defs/" + resourceName(name)
}

func (b *schemaBundle) document(name, prefix string) (jsontext.Value, error) {
	if b.active[name] {
		return nil, errors.New("result_schema: cyclic file reference")
	}
	if len(b.files) >= paired.MaxFiles {
		return nil, errors.New("result_schema: file count limit exceeded")
	}
	raw, err := readRegular(b.root, name, paired.MaxDataBytes)
	if err != nil {
		return nil, err
	}
	b.total += len(raw)
	if b.total > paired.MaxTotalBytes {
		return nil, errors.New("result_schema: closure size limit exceeded")
	}
	b.files[name] = raw
	b.active[name] = true
	defer delete(b.active, name)
	rewritten, err := b.rewrite(raw, name, prefix, 0)
	if err != nil {
		return nil, err
	}
	again, err := readRegular(b.root, name, paired.MaxDataBytes)
	if err != nil || paired.BytesDigest(again) != paired.BytesDigest(raw) {
		return nil, errors.New("source_drift: schema changed during capture")
	}
	return rewritten, nil
}

func (b *schemaBundle) rewrite(raw jsontext.Value, name, prefix string, depth int) (jsontext.Value, error) {
	if depth > 128 {
		return nil, errors.New("result_schema: nesting limit exceeded")
	}
	switch raw.Kind() {
	case '{':
		var object map[string]jsontext.Value
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
		if err := b.rewriteObject(object, name, prefix, depth); err != nil {
			return nil, err
		}
		return paired.CanonicalJSON(object)
	case '[':
		var values []jsontext.Value
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
		for i, value := range values {
			replacement, err := b.rewrite(value, name, prefix, depth+1)
			if err != nil {
				return nil, err
			}
			values[i] = replacement
		}
		return paired.CanonicalJSON(values)
	default:
		// Syntax validation, including duplicate keys, occurs through JSON v2.
		return paired.CanonicalJSON(raw)
	}
}

func (b *schemaBundle) rewriteObject(object map[string]jsontext.Value, name, prefix string, depth int) error {
	for key, value := range object {
		switch key {
		case "$id":
			return errors.New("result_schema: $id is not supported")
		case "$schema":
			var dialect string
			if err := json.Unmarshal(value, &dialect); err != nil || strings.TrimSuffix(dialect, "#") != schemaDialect || depth != 0 {
				return errors.New("result_schema: only root Draft 2020-12 dialect declaration is supported")
			}
			if prefix != "" {
				delete(object, key)
			}
		case "$ref", "$dynamicRef", "$recursiveRef":
			var ref string
			if err := json.Unmarshal(value, &ref); err != nil || ref == "" {
				return errors.New("result_schema: nonempty reference required")
			}
			if key != "$ref" && (!strings.HasPrefix(ref, "#") || prefix != "") {
				return errors.New("result_schema: cross-resource dynamic references are unsupported")
			}
			resolved, err := b.reference(ref, name, prefix)
			if err != nil {
				return err
			}
			object[key], err = json.Marshal(resolved)
			if err != nil {
				return err
			}
		case "$anchor", "$dynamicAnchor":
			if prefix != "" {
				var anchor string
				if err := json.Unmarshal(value, &anchor); err != nil {
					return err
				}
				if key == "$dynamicAnchor" {
					return errors.New("result_schema: bundled dynamic anchors are unsupported")
				}
				replacement, err := json.Marshal(resourceName(name) + "_" + anchor)
				if err != nil {
					return err
				}
				object[key] = replacement
			}
		default:
			replacement, err := b.rewrite(value, name, prefix, depth+1)
			if err != nil {
				return err
			}
			object[key] = replacement
		}
	}
	return nil
}

func (b *schemaBundle) reference(ref, name, prefix string) (string, error) {
	parsed, err := url.Parse(ref)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		strings.HasPrefix(parsed.Path, "/") || strings.Contains(parsed.Path, "\\") {
		return "", fmt.Errorf("result_schema: non-local reference %q", ref)
	}
	target := name
	if parsed.Path != "" {
		target = path.Join(path.Dir(name), parsed.Path)
		if !paired.SafeRelativePath(target) {
			return "", errors.New("result_schema: reference escapes fixture root")
		}
		if target != name {
			prefix = resourcePrefix(target)
			key := resourceName(target)
			if _, captured := b.resources[key]; !captured {
				raw, err := b.document(target, prefix)
				if err != nil {
					return "", err
				}
				b.resources[key] = raw
			}
		}
	}
	fragment := parsed.Fragment
	if prefix != "" {
		if fragment != "" && !strings.HasPrefix(fragment, "/") {
			fragment = resourceName(target) + "_" + fragment
		} else {
			fragment = prefix + fragment
		}
	}
	// Encode the decoded fragment once, preserving JSON pointer semantics.
	if fragment == "" {
		return "#", nil
	}
	return (&url.URL{Fragment: fragment}).String(), nil
}
