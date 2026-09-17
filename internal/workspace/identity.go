package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

type IDKind string

const (
	ProjectIDKind   IDKind = "prj"
	WorkspaceIDKind IDKind = "ws"
	TrashIDKind     IDKind = "tr"
	OperationIDKind IDKind = "op"
)

type IDGenerator struct {
	Reader io.Reader
}

func NewIDGenerator(reader io.Reader) IDGenerator {
	return IDGenerator{Reader: reader}
}

func (g IDGenerator) New(kind IDKind) (string, error) {
	reader := g.Reader
	if reader == nil {
		reader = rand.Reader
	}
	value := make([]byte, 16)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", fmt.Errorf("generate %s id: %w", kind, err)
	}
	return string(kind) + "_" + hex.EncodeToString(value), nil
}

var aliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func NormalizeAlias(value string) (string, error) {
	alias := strings.ToLower(strings.TrimSpace(value))
	if !aliasPattern.MatchString(alias) {
		return "", fmt.Errorf("invalid project alias %q: use 1-64 lowercase letters, digits, dots, underscores, or hyphens and start with a letter or digit", value)
	}
	return alias, nil
}

func NormalizeTeamName(value string) (string, error) {
	team := strings.ToLower(strings.TrimSpace(value))
	if team == "" || team == "." || team == ".." || filepath.IsAbs(team) || strings.ContainsAny(team, `/\`) {
		return "", fmt.Errorf("invalid team name %q", value)
	}
	return team, nil
}

func projectSlug(subjectRoot string) string {
	base := strings.ToLower(filepathBase(subjectRoot))
	var builder strings.Builder
	previousDash := false
	for _, char := range base {
		valid := char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || strings.ContainsRune("._-", char)
		if valid {
			if char == '-' && previousDash {
				continue
			}
			builder.WriteRune(char)
			previousDash = char == '-'
			continue
		}
		if !previousDash {
			builder.WriteByte('-')
			previousDash = true
		}
	}
	slug := strings.Trim(builder.String(), "._-")
	if slug == "" {
		return "project"
	}
	return slug
}

func filepathBase(path string) string {
	path = strings.TrimRight(path, `/\`)
	if index := strings.LastIndexAny(path, `/\`); index >= 0 {
		return path[index+1:]
	}
	return path
}
