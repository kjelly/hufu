package tui

import (
	"encoding/hex"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

type TaskResultIdentityMsg struct {
	TodoID   string
	ResultID string
	Team     string
}

func (m Model) resultIdentityLines(taskID string) []string {
	identity, exists := m.resultIdentities[taskID]
	if !exists {
		return nil
	}
	width := max(m.width-2, 1)
	return []string{
		wrapText("Result ID: "+sanitizeTerminalText(identity.ResultID), width),
		wrapText("Team: "+sanitizeTerminalText(identity.Team), width),
		wrapText(team.HistoricalResultCommand(sanitizeTerminalText(identity.ResultID), sanitizeTerminalText(identity.Team)), width),
	}
}

func validResultIdentity(identity TaskResultIdentityMsg) bool {
	if identity.TodoID == "" || !strings.HasPrefix(identity.ResultID, "res-") || len(identity.ResultID) != 68 {
		return false
	}
	_, err := hex.DecodeString(identity.ResultID[4:])
	return err == nil
}
