package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/resultquery"
)

func runResultsPick(command *cobra.Command, options *resultsCLIOptions, deps resultsDeps, request resultquery.Request, all []inspectpkg.HistoricalResult, response resultsEnvelope, query string) error {
	if options.pickView != "" && options.pickView != "final" && options.pickView != "report" {
		return resultsExit(2, errors.New("--view must be final or report"))
	}
	if options.pickTasks && options.pickView == "report" {
		return resultsExit(2, errors.New("--view report selects run reports; omit --tasks"))
	}
	if command.Flags().Changed("limit") {
		return resultsExit(2, errors.New("pick searches the whole selected scope; use --since, --run or --branch instead of --limit"))
	}
	if !options.pickTasks && (options.agent != "" || len(options.statuses) > 0) {
		return resultsExit(2, errors.New("--agent and --status require pick --tasks"))
	}
	if len([]rune(query)) > 256 {
		return resultsExit(2, errors.New("picker search text must not exceed 256 characters"))
	}
	var rows []inspectpkg.HistoricalResult
	var err error
	if options.pickTasks {
		statuses := options.statuses
		if len(statuses) == 0 {
			statuses = []string{"done"}
		}
		rows = resultquery.Retrieve(all, resultquery.Plan{Statuses: statuses, StatusBasis: "at_terminal", Order: "newest"}, request)
		rows = slices.DeleteFunc(rows, func(row inspectpkg.HistoricalResult) bool { return row.Kind != "task" })
	} else {
		rows, err = selectFinalResults(all, request, "")
		if err != nil {
			return resultsExit(2, err)
		}
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(command.ErrOrStderr(), "No saved results in this scope. Use --all-branches for older sessions, or --tasks for individual agent results.")
		return err
	}
	if len(rows) > 10_000 {
		return resultsExit(2, errors.New("picker scope exceeds 10000 results; narrow it with --team, --branch, --run or --since"))
	}
	pick := deps.pick
	if pick == nil {
		pick = pickHistoricalResult
	}
	index, err := pick(command.Context(), command.InOrStdin(), command.ErrOrStderr(), rows, query)
	if err != nil {
		return resultsExit(2, err)
	}
	if index == -1 {
		return nil
	}
	if index < 0 || index >= len(rows) {
		return resultsExit(2, errors.New("picker returned an invalid selection"))
	}
	response.Results, response.Matched = rows[index:index+1], 1
	if options.pickView == "report" {
		return presentRunReport(command, options, deps, response)
	}
	operation := "final"
	if options.pickTasks {
		operation = "show"
	}
	return presentResults(command, options, operation, response, true)
}

func pickHistoricalResult(ctx context.Context, input io.Reader, output io.Writer, rows []inspectpkg.HistoricalResult, query string) (int, error) {
	stdin, inputOK := input.(*os.File)
	stderr, outputOK := output.(*os.File)
	if !inputOK || !outputOK || !term.IsTerminal(stdin.Fd()) || !term.IsTerminal(stderr.Fd()) {
		return -1, errors.New("pick requires terminal stdin and stderr; use results runs/list/search for non-interactive reads")
	}
	model := newResultsPickerModel(rows, query)
	final, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return -1, err
	}
	return final.(resultsPickerModel).selected, nil
}

type resultsPickerModel struct {
	input                           textinput.Model
	rows                            []inspectpkg.HistoricalResult
	documents                       []string
	matches                         []int
	cursor, selected, width, height int
}

func newResultsPickerModel(rows []inspectpkg.HistoricalResult, query string) resultsPickerModel {
	input := textinput.New()
	input.Prompt, input.Placeholder, input.CharLimit = "> ", "Search original request, agent task, summary, branch or ID", 256
	input.SetStyles(textinput.Styles{})
	input.SetValue(query)
	input.Focus()
	model := resultsPickerModel{input: input, rows: rows, selected: -1, width: 80, height: 24}
	for _, row := range rows {
		model.documents = append(model.documents, strings.ToLower(strings.Join([]string{
			operatorpkg.SafeDisplayText(row.OriginalPrompt, 1000), operatorpkg.SafeDisplayText(row.Description, 1000),
			operatorpkg.SafeDisplayText(row.Summary, 1000), row.Team, row.Agent,
			operatorpkg.SafeDisplayText(strings.Join(row.Branches, " "), 1000), row.ResultID, row.RunID, row.TaskID,
		}, "\n")))
	}
	model.filter()
	return model
}

func (m resultsPickerModel) Init() tea.Cmd { return nil }

func (m resultsPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(msg.Width, 1), max(msg.Height, 1)
		m.input.SetWidth(max(m.width-3, 1))
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			return m, tea.Quit
		case "enter":
			if len(m.matches) > 0 {
				m.selected = m.matches[m.cursor]
				return m, tea.Quit
			}
			return m, nil
		case "up", "ctrl+p":
			m.cursor = max(m.cursor-1, 0)
			return m, nil
		case "down", "ctrl+n":
			m.cursor = max(min(m.cursor+1, len(m.matches)-1), 0)
			return m, nil
		}
	}
	previous := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != previous {
		m.filter()
	}
	return m, cmd
}

func (m *resultsPickerModel) filter() {
	type match struct{ index, score int }
	matched := []match{}
	for index, document := range m.documents {
		if score, ok := resultsFuzzyScore(m.input.Value(), document); ok {
			matched = append(matched, match{index, score})
		}
	}
	slices.SortStableFunc(matched, func(a, b match) int { return a.score - b.score })
	m.matches = make([]int, len(matched))
	for index, match := range matched {
		m.matches[index] = match.index
	}
	m.cursor = 0
}

// Literal, case-insensitive rune subsequences; whitespace-separated terms all
// have to match. This ranks text, without interpreting task prose as filters.
func resultsFuzzyScore(query, document string) (int, bool) {
	score := 0
	for term := range strings.FieldsSeq(strings.ToLower(query)) {
		wanted := []rune(term)
		next, start, end := 0, -1, 0
		for position, character := range document {
			if character == wanted[next] {
				if start < 0 {
					start = position
				}
				end = position
				next++
				if next == len(wanted) {
					break
				}
			}
		}
		if next != len(wanted) {
			return 0, false
		}
		score += end - start + start/4
	}
	return score, true
}

func (m resultsPickerModel) View() tea.View {
	lines := []string{"Search saved results", m.input.View(), fmt.Sprintf("%d / %d results · up/down select · enter read · esc cancel", len(m.matches), len(m.rows))}
	visible := max((m.height-9)/2, 1)
	start := max(m.cursor-visible+1, 0)
	for position := start; position < min(start+visible, len(m.matches)); position++ {
		row := m.rows[m.matches[position]]
		marker := "  "
		if position == m.cursor {
			marker = "> "
		}
		lines = append(lines, marker+operatorpkg.SafeDisplayText(firstNonEmpty(row.OriginalPrompt, row.Description, "(Original request unavailable)"), m.width))
		metadata := fmt.Sprintf("  %s · %s · %s (%s)", row.CompletedAt.Format("2006-01-02 15:04"), row.Team, row.Status, row.Validity)
		if row.Kind == "task" {
			metadata += fmt.Sprintf(" · %s · task %s", row.Agent, row.TaskID)
		}
		lines = append(lines, metadata+" · "+strings.Join(row.Branches, ", "))
	}
	if len(m.matches) == 0 {
		lines = append(lines, "No matching results. Edit the search text or press Esc.")
	} else {
		row := m.rows[m.matches[m.cursor]]
		lines = append(lines, "")
		if row.Description != "" {
			lines = append(lines, "Agent task: "+operatorpkg.SafeDisplayText(row.Description, 300))
		}
		lines = append(lines, "Result (excerpt): "+operatorpkg.SafeDisplayText(firstNonEmpty(row.Summary, row.Output), 300))
	}
	for index, line := range lines {
		lines[index] = ansi.Truncate(operatorpkg.SafeDisplayText(line, max(len([]rune(line)), 1)), m.width, "…")
	}
	view := tea.NewView(strings.Join(lines[:min(len(lines), m.height)], "\n"))
	view.AltScreen = true
	return view
}
