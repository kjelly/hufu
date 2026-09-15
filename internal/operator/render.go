package operator

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/utils"
)

func RenderArgv(argv []string, shell string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	for _, value := range argv {
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", false
		}
	}
	quoted := make([]string, len(argv))
	prefix := ""
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "bash", "sh", "zsh":
		for index, value := range argv {
			quoted[index] = quotePOSIX(value)
		}
	case "powershell", "pwsh":
		prefix = "& "
		for index, value := range argv {
			quoted[index] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
		}
	default:
		return "", false
	}
	rendered := prefix + strings.Join(quoted, " ")
	if utils.RedactSecrets(rendered) != rendered {
		return "", false
	}
	return rendered, true
}

func quotePOSIX(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_@%+=:,./-", r)
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func BuildSummary(snapshot OperatorSnapshot) OperatorSummary {
	return BuildSummaryForShell(snapshot, "")
}

func BuildSummaryForShell(snapshot OperatorSnapshot, shell string) OperatorSummary {
	primary := snapshot.PrimaryAction
	state := strings.ToUpper(snapshot.Activity.State)
	what := summaryWhat(snapshot)
	next := "No operator action is required."
	nextIsCommand := false
	if primary != nil {
		next, nextIsCommand = summaryNext(*primary, shell)
	}
	data := fmt.Sprintf("durable through event %s; live connection %s", valueOrUnknown(snapshot.Freshness.EventID), valueOrUnknown(snapshot.Freshness.LiveState))
	return OperatorSummary{
		State: SafeDisplayText(state, 80), What: SafeDisplayText(what, 320),
		Next: safeSummaryNext(next, nextIsCommand), Data: SafeDisplayText(data, 240),
	}
}

func safeSummaryNext(next string, isCommand bool) string {
	if isCommand {
		return next
	}
	return SafeDisplayText(next, 320)
}

func summaryWhat(snapshot OperatorSnapshot) string {
	if snapshot.PrimaryAction != nil && snapshot.PrimaryAction.ReasonCode == "external_effect_unknown" {
		return fmt.Sprintf("task %s has an unknown external-effect outcome; inspect persisted recovery evidence before any replay", valueOrUnknown(snapshot.PrimaryAction.Target.TaskID))
	}
	if len(snapshot.Blockers) > 0 {
		return fmt.Sprintf("%s; %d blocker(s) require review", snapshot.Activity.State, len(snapshot.Blockers))
	}
	if snapshot.Outcome.RunOutcome != "" {
		return fmt.Sprintf("run outcome is %s; activity is %s", snapshot.Outcome.RunOutcome, snapshot.Activity.State)
	}
	return "activity is " + snapshot.Activity.State
}

func summaryNext(action ActionSuggestion, shell string) (string, bool) {
	if action.Kind == "none" {
		return "No operator action is required.", false
	}
	if action.Kind == "wait" {
		return "Wait for the runtime; do not retry while execution is active.", false
	}
	if action.Availability != "available" {
		return fmt.Sprintf("%s is %s (%s); no executable command is available", action.ID, action.Availability, valueOrUnknown(action.ReasonCode)), false
	}
	if rendered, ok := RenderArgv(action.Argv, shell); ok {
		return rendered, true
	}
	if len(action.Argv) > 0 {
		return fmt.Sprintf("Use the typed %s argv from JSON output; this command cannot be rendered safely for the selected shell.", action.ID), false
	}
	return fmt.Sprintf("Use the typed %s action (%s).", action.ID, valueOrUnknown(action.ReasonCode)), false
}

// SafeDisplayText strips terminal controls, redacts recognizable credentials,
// collapses whitespace, and applies a rune bound before presentation.
func SafeDisplayText(value string, maxRunes int) string {
	value = stripTerminalControls(value)
	value = utils.RedactSecrets(value)
	value = strings.Join(strings.Fields(value), " ")
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		runes := []rune(value)
		value = string(runes[:maxRunes-1]) + "…"
	}
	return value
}

func stripTerminalControls(value string) string {
	var out strings.Builder
	for index := 0; index < len(value); {
		if value[index] == 0x1b {
			index++
			if index < len(value) && value[index] == ']' {
				index++
				for index < len(value) && value[index] != 0x07 {
					if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
						index += 2
						break
					}
					index++
				}
				if index < len(value) && value[index] == 0x07 {
					index++
				}
				continue
			}
			if index < len(value) && strings.ContainsRune("P_^X", rune(value[index])) {
				index++
				for index < len(value) {
					if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
						index += 2
						break
					}
					index++
				}
				continue
			}
			if index < len(value) && value[index] == '[' {
				index++
			}
			for index < len(value) {
				b := value[index]
				index++
				if b >= 0x40 && b <= 0x7e {
					break
				}
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(value[index:])
		index += size
		if r == utf8.RuneError && size == 1 {
			continue
		}
		if unicode.IsControl(r) {
			out.WriteByte(' ')
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
