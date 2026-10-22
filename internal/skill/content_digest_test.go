package skill

import (
	"testing"
	"time"
)

func TestContentDigestIdentity(t *testing.T) {
	def := &SkillDef{Name: "inspect", Description: "Inspect files", AllowedTools: "view", Content: "Read the requested file."}
	before, err := ContentDigest(def)
	if err != nil {
		t.Fatal(err)
	}
	def.Path = "/private/user/skills/inspect"
	def.Summary = "derived text"
	def.CreatedAt = time.Now()
	after, err := ContentDigest(def)
	if err != nil || before != after {
		t.Fatalf("non-content metadata changed digest: %q %q %v", before, after, err)
	}
	def.Content = "Write the requested file."
	changed, err := ContentDigest(def)
	if err != nil || changed == before {
		t.Fatalf("body change did not change digest: %q %v", changed, err)
	}
	if _, err := ContentDigest(nil); err == nil {
		t.Fatal("nil definition accepted")
	}
}
