package tui

const maxExpandedLogsPerTask = 16

func (m *Model) storeExpandedLog(todoID string, index int, line string) {
	if m.fullLogs == nil {
		m.fullLogs = make(map[string]map[int]string)
	}
	if m.fullLogs[todoID] == nil {
		m.fullLogs[todoID] = make(map[int]string)
	}
	runes := []rune(line)
	if len(runes) > maxExpandedToolRunes {
		line = string(runes[:maxExpandedToolRunes]) + "… [output limit reached]"
	}
	entries := m.fullLogs[todoID]
	if len(entries) >= maxExpandedLogsPerTask {
		oldest := index
		for i := range entries {
			oldest = min(oldest, i)
		}
		delete(entries, oldest)
	}
	entries[index] = line
}

func (m *Model) trimExpandedLogs(todoID string, dropped int) {
	entries := m.fullLogs[todoID]
	if len(entries) == 0 {
		return
	}
	shifted := make(map[int]string, len(entries))
	for index, line := range entries {
		if index >= dropped {
			shifted[index-dropped] = line
		}
	}
	m.fullLogs[todoID] = shifted
	if m.detailID == todoID && m.expandedLogIndex >= 0 {
		m.expandedLogIndex -= dropped
		if m.expandedLogIndex < 0 {
			m.expandedLogIndex = -1
		}
	}
}
