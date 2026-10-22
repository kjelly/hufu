package improver

import (
	"strings"
	"testing"
)

func TestSchemaClosureBundlesLocalRefsWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	putFile(t, dir, "schema.json", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"category":{"$ref":"values.json#/$defs/category"}}}`)
	putFile(t, dir, "values.json", `{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"category":{"type":"string","enum":["allowed","denied"]}}}`)
	root, err := openSourceRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	raw, closure, err := captureSchema(root, "schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(closure.files) != 2 || strings.Contains(string(raw), `"values.json#`) || !strings.Contains(string(raw), resourceDefinitions) {
		t.Fatalf("closure not bundled: %s", raw)
	}
	for _, reference := range []string{"https://example.com/schema", "../outside.json", "file:///etc/passwd", "schema.json?query"} {
		putFile(t, dir, "schema.json", `{"$ref":"`+reference+`"}`)
		if _, _, err := captureSchema(root, "schema.json"); err == nil {
			t.Fatalf("nonlocal reference accepted: %q", reference)
		}
	}
}

func TestSchemaClosureRejectsCyclesAndRetainsIntegerPrecision(t *testing.T) {
	dir := t.TempDir()
	putFile(t, dir, "schema.json", `{"type":"integer","maximum":9223372036854775807}`)
	root, err := openSourceRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	raw, _, err := captureSchema(root, "schema.json")
	if err != nil || !strings.Contains(string(raw), "9223372036854775807") {
		t.Fatalf("integer rounded: %s, %v", raw, err)
	}
	putFile(t, dir, "schema.json", `{"$ref":"other.json"}`)
	putFile(t, dir, "other.json", `{"$ref":"schema.json"}`)
	if _, _, err := captureSchema(root, "schema.json"); err == nil {
		t.Fatal("file reference cycle accepted")
	}
}
