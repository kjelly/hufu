package main

import (
	"context"
	"fmt"
	"strings"
)

func resolveLastNByCommitType(ctx context.Context, repo, head, since string, count int, commitType string) (rangeResolution, error) {
	end, err := resolveCommit(ctx, repo, head)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("resolve review head %q: %w", head, err)
	}
	args := []string{"log", "--first-parent", "--format=%H%x00%s"}
	if since != "" {
		args = append(args, "--since="+since)
	}
	args = append(args, end)
	logText, err := git(ctx, repo, args...)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("list first-parent commits: %w", err)
	}

	selected := make([]string, 0, count)
	available := 0
	for _, record := range nonEmptyLines(logText) {
		commit, subject, ok := strings.Cut(record, "\x00")
		if !ok {
			return rangeResolution{}, fmt.Errorf("parse first-parent commit record %q", record)
		}
		if conventionalCommitType(subject) != commitType {
			continue
		}
		available++
		if len(selected) < count {
			selected = append(selected, strings.TrimSpace(commit))
		}
	}
	// git log returns newest first; manifests and patch artifacts use
	// chronological order so each selected commit is easy to follow.
	// This package also runs under Yaegi, whose current generic stdlib support
	// cannot load slices.Reverse.
	chronological := make([]string, 0, len(selected))
	for index := len(selected) - 1; index >= 0; index-- {
		chronological = append(chronological, selected[index])
	}
	selected = chronological

	resolution := rangeResolution{
		Range:                reviewRange{End: strings.TrimSpace(end), CommitCount: len(selected), Source: "selected_commits", Commits: selected},
		AvailableCommitCount: available,
		HistoryExhausted:     available < count,
	}
	if len(selected) == 0 {
		return resolution, nil
	}
	start, err := firstParentOrEmptyTree(ctx, repo, selected[0])
	if err != nil {
		return rangeResolution{}, err
	}
	resolution.Range.Start = start
	return resolution, nil
}

func conventionalCommitType(subject string) string {
	header, _, found := strings.Cut(subject, ":")
	if !found {
		return ""
	}
	header = strings.TrimSuffix(header, "!")
	commitType := header
	if scopeStart := strings.IndexByte(header, '('); scopeStart >= 0 {
		if scopeStart == 0 || !strings.HasSuffix(header, ")") || scopeStart == len(header)-2 {
			return ""
		}
		scope := header[scopeStart+1 : len(header)-1]
		if strings.ContainsAny(scope, "() \t") {
			return ""
		}
		commitType = header[:scopeStart]
	} else if strings.ContainsAny(header, "()") {
		return ""
	}
	if commitType == "" {
		return ""
	}
	for index, char := range commitType {
		if char >= 'a' && char <= 'z' || index > 0 && (char >= '0' && char <= '9' || char == '-') {
			continue
		}
		return ""
	}
	return commitType
}

func firstParentOrEmptyTree(ctx context.Context, repo, commit string) (string, error) {
	parent, err := git(ctx, repo, "rev-parse", "--verify", commit+"^")
	if err == nil {
		return strings.TrimSpace(parent), nil
	}
	root, rootErr := git(ctx, repo, "rev-list", "--max-parents=0", commit)
	if rootErr != nil || strings.TrimSpace(root) != commit {
		return "", fmt.Errorf("history_incomplete: resolve parent of selected commit %q: %w", commit, err)
	}
	emptyTree, err := gitWithInput(ctx, repo, nil, "hash-object", "-t", "tree", "--stdin")
	if err != nil {
		return "", fmt.Errorf("resolve empty tree for root commit %q: %w", commit, err)
	}
	return strings.TrimSpace(emptyTree), nil
}

func diffSelectedCommit(ctx context.Context, repo, commit string, options ...string) (string, error) {
	parent, err := firstParentOrEmptyTree(ctx, repo, commit)
	if err != nil {
		return "", err
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", parent, commit}
	args = append(args, options...)
	return git(ctx, repo, args...)
}
