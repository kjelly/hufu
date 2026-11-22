// Package main renders the verified review scope for Hufu's saved run report.
package main

import (
	"context"
	jsonv1 "encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

type reviewScope struct {
	Requested struct {
		Kind       string `json:"kind"`
		Head       string `json:"head"`
		History    string `json:"history"`
		Count      int    `json:"count"`
		Base       string `json:"base"`
		Since      string `json:"since"`
		CommitType string `json:"commit_type"`
		Batch      int    `json:"batch"`
	} `json:"requested"`
	Resolved struct {
		Base                 string `json:"base"`
		Head                 string `json:"head"`
		SelectedCommitCount  int    `json:"selected_commit_count"`
		AvailableCommitCount int    `json:"available_commit_count"`
		HistoryExhausted     bool   `json:"history_exhausted"`
		RepositoryShallow    bool   `json:"repository_shallow"`
	} `json:"resolved"`
	ObservedBudget struct {
		ChangedPaths int `json:"changed_paths"`
		WorksetItems int `json:"workset_items"`
	} `json:"observed_budget"`
	RepositorySnapshot struct {
		Batch      int `json:"batch"`
		BatchCount int `json:"batch_count"`
		FileCount  int `json:"file_count"`
	} `json:"repository_snapshot"`
	Satisfied          bool   `json:"satisfied"`
	RequestedInputHash string `json:"requested_input_hash"`
}

func Run(_ context.Context, in io.Reader, out io.Writer) error {
	var input struct {
		Version int         `json:"version"`
		Output  reviewScope `json:"output"`
	}
	if err := jsonv1.NewDecoder(in).Decode(&input); err != nil {
		return err
	}
	if input.Version != 1 {
		return errors.New("unsupported report input version")
	}
	scope := input.Output
	requested, resolved := scope.Requested, scope.Resolved
	if !scope.Satisfied || requested.Head == "" || resolved.Base == "" || resolved.Head == "" {
		return errors.New("incomplete review scope attestation")
	}

	var markdown strings.Builder
	switch requested.Kind {
	case "last_n":
		noun := "commits"
		if requested.Count == 1 {
			noun = "commit"
		}
		fmt.Fprintf(&markdown, "- **Requested:** last %d %s ending at %s", requested.Count, noun, code(requested.Head))
		if requested.CommitType != "" {
			fmt.Fprintf(&markdown, " with commit type %s", code(requested.CommitType))
		}
		markdown.WriteString("\n")
	case "revision_range":
		fmt.Fprintf(&markdown, "- **Requested:** revision range %s\n", code(requested.Base+".."+requested.Head))
	case "since":
		fmt.Fprintf(&markdown, "- **Requested:** commits since %s ending at %s\n", code(requested.Since), code(requested.Head))
	case "working_tree":
		fmt.Fprintf(&markdown, "- **Requested:** working-tree changes against %s\n", code(requested.Head))
	case "repository_snapshot":
		fmt.Fprintf(&markdown, "- **Requested:** repository snapshot at %s", code(requested.Head))
		if requested.Batch > 0 {
			fmt.Fprintf(&markdown, " (batch %d)", requested.Batch)
		}
		markdown.WriteString("\n")
	default:
		return errors.New("unsupported review scope kind")
	}

	switch requested.Kind {
	case "working_tree":
		fmt.Fprintf(&markdown, "- **Selected:** working-tree changes against %s\n", code(resolved.Base))
	case "repository_snapshot":
		fmt.Fprintf(&markdown, "- **Selected:** snapshot at %s", code(resolved.Head))
		if snapshot := scope.RepositorySnapshot; snapshot.BatchCount > 0 {
			if snapshot.Batch > 0 {
				fmt.Fprintf(&markdown, " (batch %d of %d)", snapshot.Batch, snapshot.BatchCount)
			}
			fmt.Fprintf(&markdown, " across %d tracked files", snapshot.FileCount)
		}
		markdown.WriteString("\n")
	default:
		fmt.Fprintf(&markdown, "- **Selected range:** %s\n", code(resolved.Base+".."+resolved.Head))
		fmt.Fprintf(&markdown, "- **Selected commits:** %d of %d available (%s history)\n", resolved.SelectedCommitCount, resolved.AvailableCommitCount, code(requested.History))
		if resolved.HistoryExhausted {
			markdown.WriteString("- Available history was exhausted.\n")
		}
	}
	if resolved.RepositoryShallow {
		markdown.WriteString("- The repository is shallow.\n")
	}
	fmt.Fprintf(&markdown, "- **Changed paths:** %d across %d workset items\n", scope.ObservedBudget.ChangedPaths, scope.ObservedBudget.WorksetItems)
	fmt.Fprintf(&markdown, "- **Verified input hash:** %s\n", code(scope.RequestedInputHash))
	return jsonv1.NewEncoder(out).Encode(map[string]string{"markdown": markdown.String()})
}

func code(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '`' {
			return ' '
		}
		return r
	}, value)
	return "`" + value + "`"
}
