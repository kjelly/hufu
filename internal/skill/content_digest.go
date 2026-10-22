package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// ContentDigest fingerprints parsed behavioral metadata and body. Discovery
// paths, derived summaries and timestamps (including mtime fallback) are not
// content identity, and this digest is not an authenticity assertion.
func ContentDigest(definition *SkillDef) (string, error) {
	if definition == nil {
		return "", fmt.Errorf("skill definition is missing")
	}
	data, err := json.Marshal(struct {
		Version      int    `json:"version"`
		Name         string `json:"name"`
		Description  string `json:"description"`
		AllowedTools string `json:"allowed_tools"`
		Body         string `json:"body"`
	}{1, strings.TrimSpace(definition.Name), strings.TrimSpace(definition.Description),
		strings.TrimSpace(definition.AllowedTools), strings.TrimSpace(definition.Content)})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("hufu-skill-content-v1\x00"), data...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
