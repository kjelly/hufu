package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/team"
)

type qualifiedRunInputAssignment struct {
	teamName   string
	assignment team.RunInputAssignment
}

func configureRunInputAssignments(loadedTeams map[string]*teamContext) error {
	assignments, err := readRunInputAssignments(opts.inputFlags, opts.inputFiles)
	if err != nil {
		return err
	}
	byTeam := make(map[string][]team.RunInputAssignment, len(loadedTeams))
	for _, qualified := range assignments {
		teamName := strings.ToLower(strings.TrimSpace(qualified.teamName))
		if teamName == "" {
			matches := make([]string, 0, 1)
			for name, context := range loadedTeams {
				if context == nil || context.session == nil {
					continue
				}
				for _, definition := range context.session.RunInputDefinitions {
					if definition.Name == qualified.assignment.Name {
						matches = append(matches, name)
						break
					}
				}
			}
			if len(matches) == 1 {
				teamName = matches[0]
			} else if len(loadedTeams) == 1 {
				for name := range loadedTeams {
					teamName = name
				}
			} else {
				return fmt.Errorf("input_team_ambiguous: unqualified input %q does not uniquely match one selected team", qualified.assignment.Name)
			}
		}
		if _, ok := loadedTeams[teamName]; !ok {
			return fmt.Errorf("input_team_unknown: input %q targets unloaded team %q", qualified.assignment.Name, teamName)
		}
		byTeam[teamName] = append(byTeam[teamName], qualified.assignment)
	}
	for name, context := range loadedTeams {
		if context != nil && context.coordinator != nil {
			context.coordinator.SetRunInputAssignments(byTeam[name])
		}
	}
	return nil
}

func readRunInputAssignments(flags, files []string) ([]qualifiedRunInputAssignment, error) {
	assignments := make([]qualifiedRunInputAssignment, 0, len(flags))
	for index, flag := range flags {
		name, value, ok := strings.Cut(flag, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid --input %q: expected name=value", flag)
		}
		teamName, inputName := splitQualifiedRunInputName(name)
		assignments = append(assignments, qualifiedRunInputAssignment{teamName: teamName, assignment: team.RunInputAssignment{
			Name: inputName, RawValue: []byte(value), Source: team.RunInputSourceCLI,
			Location: fmt.Sprintf("--input[%d]", index+1),
		}})
	}
	for _, path := range files {
		fileAssignments, err := readRunInputFile(path)
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, fileAssignments...)
	}
	return assignments, nil
}

func readRunInputFile(path string) ([]qualifiedRunInputAssignment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --input-file %q: %w", path, err)
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		if yamlErr := yaml.Unmarshal(data, &values); yamlErr != nil {
			return nil, fmt.Errorf("decode --input-file %q as JSON or YAML: %w", path, err)
		}
	} else {
		var trailing any
		if err := decoder.Decode(&trailing); err == nil {
			return nil, fmt.Errorf("decode --input-file %q: trailing JSON value is not allowed", path)
		} else if err != io.EOF {
			return nil, fmt.Errorf("decode --input-file %q trailing JSON: %w", path, err)
		}
	}
	if values == nil {
		return nil, fmt.Errorf("decode --input-file %q: top-level mapping is required", path)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	assignments := make([]qualifiedRunInputAssignment, 0, len(keys))
	for _, key := range keys {
		encoded, err := json.Marshal(values[key])
		if err != nil {
			return nil, fmt.Errorf("encode --input-file %q key %q: %w", path, key, err)
		}
		teamName, inputName := splitQualifiedRunInputName(key)
		assignments = append(assignments, qualifiedRunInputAssignment{teamName: teamName, assignment: team.RunInputAssignment{
			Name: inputName, RawValue: encoded, Source: team.RunInputSourceFile, Location: path + ":" + key,
		}})
	}
	return assignments, nil
}

func splitQualifiedRunInputName(value string) (string, string) {
	value = strings.TrimSpace(value)
	teamName, inputName, qualified := strings.Cut(value, "::")
	if !qualified {
		return "", value
	}
	return strings.ToLower(strings.TrimSpace(teamName)), strings.TrimSpace(inputName)
}
