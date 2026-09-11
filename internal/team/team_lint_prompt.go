package team

import (
	"regexp"
	"strconv"
	"strings"
)

type promptDirectiveKind string

const (
	promptDirectiveTool  promptDirectiveKind = "tool"
	promptDirectiveSkill promptDirectiveKind = "skill"
)

type promptDirective struct {
	Kind      promptDirectiveKind
	Name      string
	Agent     string
	FieldPath string
	Location  TeamSourceLocation
}

const directiveNamePattern = `[A-Za-z][A-Za-z0-9_.:-]*`

var (
	toolDirectiveRE  = regexp.MustCompile(`(?i)(?:\b(?:use(?:\s+the)?|call|invoke)\s+\x60(` + directiveNamePattern + `)\x60\s+tool\b|\btool:\s+(?:\x60(` + directiveNamePattern + `)\x60|(` + directiveNamePattern + `)\b))`)
	skillDirectiveRE = regexp.MustCompile(`(?i)(?:\b(?:use(?:\s+the)?|load|invoke)\s+\x60(` + directiveNamePattern + `)\x60\s+skill\b|\bskill:\s+(?:\x60(` + directiveNamePattern + `)\x60|(` + directiveNamePattern + `)\b))`)
)

func scanTeamPromptDirectives(session *TeamSession, sources *TeamSourceIndex) []promptDirective {
	if session == nil || sources == nil {
		return nil
	}
	var directives []promptDirective
	for alias, source := range sources.Agents {
		def := session.Agents[normalizedName(alias)]
		if def == nil {
			continue
		}
		directives = append(directives, scanPromptText(source.Body, source.File, source.BodyLine, 1, source.BodyStatus, def.Name, "agents."+normalizedName(def.Name)+".system")...)
	}
	for index, task := range session.ContractTasks {
		for _, entry := range []struct {
			field string
			text  string
		}{{"goal", task.Goal}, {"constraints", task.Constraints}} {
			if strings.TrimSpace(entry.text) == "" {
				continue
			}
			field := taskSourceField(sources, index, entry.field)
			source := sourceTextForField(sources, field)
			line, column, status, file := source.Line, source.Column, source.Status, source.File
			if source.Block {
				line++
				column = 1
			}
			directives = append(directives, scanPromptText(entry.text, file, line, column, status, task.Agent, field)...)
		}
	}
	return directives
}

func taskSourceField(sources *TeamSourceIndex, index int, leaf string) string {
	base := "tasks"
	if sources.SchemaVersion == SchemaVersionV1Alpha1 {
		base = "spec.tasks"
	} else if _, ok := sources.Manifest["advanced.tasks"]; ok {
		base = "advanced.tasks"
	}
	return base + "[" + strconv.Itoa(index) + "]." + leaf
}

func sourceTextForField(sources *TeamSourceIndex, field string) TeamSourceText {
	if source, ok := sources.ManifestText[field]; ok {
		return source
	}
	loc := sources.Location(field)
	return TeamSourceText{File: loc.File, Line: loc.Line, Column: loc.Column, Status: loc.Status}
}

func scanPromptText(text, file string, baseLine, baseColumn int, status, agentName, field string) []promptDirective {
	var directives []promptDirective
	inFence := false
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		directives = append(directives, scanDirectiveLine(line, index, toolDirectiveRE, promptDirectiveTool, file, baseLine, baseColumn, status, agentName, field)...)
		directives = append(directives, scanDirectiveLine(line, index, skillDirectiveRE, promptDirectiveSkill, file, baseLine, baseColumn, status, agentName, field)...)
	}
	return directives
}

func scanDirectiveLine(line string, lineOffset int, re *regexp.Regexp, kind promptDirectiveKind, file string, baseLine, baseColumn int, status, agentName, field string) []promptDirective {
	var directives []promptDirective
	for _, match := range re.FindAllStringSubmatchIndex(line, -1) {
		name, start := "", 0
		for capture := 1; capture <= 3; capture++ {
			captureStart := match[capture*2]
			if captureStart >= 0 {
				name = line[captureStart:match[capture*2+1]]
				start = captureStart
				break
			}
		}
		column := start + 1
		if lineOffset == 0 && baseColumn > 1 {
			column += baseColumn - 1
		}
		directives = append(directives, promptDirective{
			Kind: kind, Name: strings.ToLower(name), Agent: agentName, FieldPath: field,
			Location: TeamSourceLocation{File: file, Line: baseLine + lineOffset, Column: column, Status: status},
		})
	}
	return directives
}
