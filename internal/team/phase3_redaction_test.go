package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

func TestPhase3PromptAndSessionMarkdownRedactRegisteredSecret(t *testing.T) {
	const secret = "phase3-exact-prompt-secret-8a31"
	registry := tools.NewSecretRegistry()
	if err := registry.Register(tools.SecretRef{Name: "test.prompt", Source: "test", ExactValue: secret}); err != nil {
		t.Fatal(err)
	}
	utils.RegisterSecretRedactor(registry)

	session := NewSession()
	session.AddEntry("user", "prompt="+secret)
	if strings.Contains(session.Entries[0].Content, secret) || strings.Contains(session.ContextSummary(), secret) {
		t.Fatal("session context retained registered secret")
	}
	md := GenerateSessionMD(session, "team")
	if strings.Contains(md, secret) {
		t.Fatal("session markdown retained registered secret")
	}

	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	c.emitThinkPrompt("system prompt=" + secret)
	prompt, err := os.ReadFile(filepath.Join(workspace, "think-prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), secret) {
		t.Fatal("think prompt retained registered secret")
	}
}
