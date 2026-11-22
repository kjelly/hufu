package team

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/golangruntime"
)

const (
	reportSectionAssertion      = "passed-task-output-assertion"
	reportRendererTemplate      = "template"
	reportRendererGo            = "golang"
	reportRendererInputVersion  = 1
	maxReportSections           = 8
	maxReportSectionTimeout     = 5 // seconds
	maxReportSectionTemplate    = 16 << 10
	maxReportSectionMarkdown    = 64 << 10
	maxReportRendererOutput     = 6*maxReportSectionMarkdown + 256 // JSON escapes
	reportSectionSourceMissing  = "verified task output unavailable"
	reportSectionProgramMissing = "Go renderer source unavailable at run start"
)

var reportSectionIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// RunReportSection is saved verbatim in the terminal journal and projected
// into the immutable report. Run-start pins carry only its static fields.
type RunReportSection struct {
	ID                string                    `json:"id"`
	Title             string                    `json:"title"`
	Source            agent.ReportSectionSource `json:"source"`
	Renderer          string                    `json:"renderer"`
	RendererDigest    string                    `json:"renderer_digest,omitempty"`
	Provenance        *ReportSectionProvenance  `json:"provenance,omitempty"`
	Markdown          string                    `json:"markdown,omitempty"`
	UnavailableReason string                    `json:"unavailable_reason,omitempty"`
}

// ReportSectionProvenance identifies the exact output and assertion evaluated
// for this section, including the original action run for recovered tasks.
type ReportSectionProvenance struct {
	RunID                   string `json:"run_id"`
	TaskID                  string `json:"task_id"`
	OccurrenceRevision      int    `json:"occurrence_revision"`
	Attempt                 int    `json:"attempt"`
	ActionInvocationID      string `json:"action_invocation_id"`
	RuntimeOutputsHash      string `json:"runtime_outputs_hash"`
	RunInputSnapshotID      string `json:"run_input_snapshot_id"`
	RunInputSnapshotHash    string `json:"run_input_snapshot_hash"`
	VerificationFingerprint string `json:"verification_fingerprint,omitempty"`
}

type reportRendererInput struct {
	Version    int                     `json:"version"`
	RunID      string                  `json:"run_id"`
	SectionID  string                  `json:"section_id"`
	Output     jsontext.Value          `json:"output"`
	Provenance ReportSectionProvenance `json:"provenance"`
}

func validateExecutionReportConfig(config agent.ExecutionReportConfig) error {
	if len(config.Sections) > maxReportSections {
		return fmt.Errorf("execution-report supports at most %d sections", maxReportSections)
	}
	seen := make(map[string]bool, len(config.Sections))
	for _, section := range config.Sections {
		if !reportSectionIDPattern.MatchString(section.ID) || seen[section.ID] {
			return fmt.Errorf("execution-report has invalid or duplicate section id %q", section.ID)
		}
		seen[section.ID] = true
		if !validReportSectionLabel(section.Title) {
			return fmt.Errorf("execution-report section %q requires a single-line title", section.ID)
		}
		if !validReportSectionLabel(section.Source.Task) || !validReportSectionLabel(section.Source.Output) ||
			section.Source.Require != reportSectionAssertion {
			return fmt.Errorf("execution-report section %q requires a task, an output and require: %s", section.ID, reportSectionAssertion)
		}
		renderer := section.Renderer
		switch renderer.Runtime {
		case "", reportRendererTemplate:
			if strings.TrimSpace(renderer.Template) == "" || len(renderer.Template) > maxReportSectionTemplate ||
				renderer.Source != "" || renderer.Mode != "" || renderer.Timeout != 0 {
				return fmt.Errorf("execution-report section %q requires only a bounded Markdown template", section.ID)
			}
		case reportRendererGo:
			if !filepath.IsLocal(renderer.Source) || strings.TrimSpace(renderer.Source) != renderer.Source ||
				renderer.Mode != golangruntime.TrustedStaticMode || renderer.Template != "" ||
				renderer.Timeout < 0 || renderer.Timeout > maxReportSectionTimeout {
				return fmt.Errorf("execution-report section %q requires a team-relative Go source, trusted-static mode and timeout up to %d seconds", section.ID, maxReportSectionTimeout)
			}
		default:
			return fmt.Errorf("execution-report section %q has unknown renderer runtime %q", section.ID, renderer.Runtime)
		}
	}
	return nil
}

func validReportSectionLabel(value string) bool {
	return value != "" && len(value) <= 160 && strings.TrimSpace(value) == value &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func (c *Coordinator) pinRunReportSections() []RunReportSection {
	if c.session == nil {
		return nil
	}
	var pins []RunReportSection
	for _, section := range c.session.Config.ExecutionReport.Sections {
		pin := RunReportSection{ID: section.ID, Title: section.Title, Source: section.Source, Renderer: reportRendererTemplate}
		if section.Renderer.Runtime == reportRendererGo {
			pin.Renderer = reportRendererGo
			program, err := golangruntime.Prepare(c.session.Dir, section.Renderer.Source)
			if err != nil {
				pin.UnavailableReason = reportSectionProgramMissing
			} else {
				pin.RendererDigest = program.Digest
			}
		} else {
			pin.RendererDigest = runInputHash([]byte(section.Renderer.Template))
		}
		pins = append(pins, pin)
	}
	return pins
}

func reportSectionPins(events []RunEvent, runID string) []RunReportSection {
	for _, event := range events {
		if event.Type != string(EventRunStarted) || event.RunID != runID {
			continue
		}
		var start struct {
			Pins []RunReportSection `json:"report_section_pins"`
		}
		if json.Unmarshal(event.Payload, &start) == nil {
			return start.Pins
		}
	}
	return nil
}

// projectRunReportSections is pure. Recovery can still materialize unavailable
// sections from the start pins if it settles a run without rendered content.
func projectRunReportSections(pins, captured []RunReportSection) []RunReportSection {
	sections := slices.Clone(pins)
	for i, pin := range pins {
		if pin.UnavailableReason != "" {
			continue
		}
		sections[i].UnavailableReason = "renderer content was not captured"
		if i >= len(captured) {
			continue
		}
		section := captured[i]
		if section.ID == pin.ID && section.Title == pin.Title && section.Source == pin.Source &&
			section.Renderer == pin.Renderer && section.RendererDigest == pin.RendererDigest &&
			((section.Markdown != "" && len(section.Markdown) <= maxReportSectionMarkdown && section.Provenance != nil && section.UnavailableReason == "") ||
				(section.Markdown == "" && section.UnavailableReason != "")) {
			sections[i] = section
		}
	}
	return sections
}

func (c *Coordinator) renderRunReportSections(ctx context.Context, result *RunResult) []RunReportSection {
	if c.session == nil || len(c.session.Config.ExecutionReport.Sections) == 0 || c.eventStore == nil {
		return nil
	}
	// A failure to read report inputs is handled by the pure report projection.
	// Keep this read and all renderer processes outside the append transaction.
	readCtx, cancel := terminalFinalizationContext(ctx)
	defer cancel()
	tree, err := c.loadJournalSessionTree(readCtx, c.session.Workspace)
	if err != nil {
		return nil
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return nil
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, c.activeBranchID())
	if err != nil {
		return nil
	}
	pins := reportSectionPins(lineage, result.RunID)
	if len(pins) == 0 {
		return nil
	}
	for _, event := range lineage {
		if event.Type == string(EventRunStarted) && event.RunID == result.RunID {
			lineage = slices.DeleteFunc(slices.Clone(lineage), func(other RunEvent) bool { return other.SessionID != event.SessionID })
			break
		}
	}
	session := ReduceToSessionData(lineage)
	verifications := runReportVerifications(result, session.Tasks)
	snapshot := result.RunInputs
	hasFrozenInputs := snapshot != nil && slices.ContainsFunc(session.RunInputSnapshots, func(frozen RunInputSnapshot) bool {
		return frozen.ID == snapshot.ID && frozen.RunID == snapshot.RunID && frozen.SnapshotHash == snapshot.SnapshotHash
	})
	configs := c.session.Config.ExecutionReport.Sections
	// Independent time budgets keep a stalled section from consuming the
	// runtime allowance of another renderer.
	var workers sync.WaitGroup
	for i := range pins {
		if pins[i].UnavailableReason != "" {
			continue
		}
		if i >= len(configs) || pins[i].ID != configs[i].ID {
			pins[i].UnavailableReason = "renderer configuration changed"
			continue
		}
		if !hasFrozenInputs {
			pins[i].UnavailableReason = reportSectionSourceMissing
			continue
		}
		input, ok := verifiedReportRendererInput(result, session.Tasks, verifications, pins[i])
		if !ok {
			pins[i].UnavailableReason = reportSectionSourceMissing
			continue
		}
		workers.Go(func() {
			pins[i] = renderReportSection(ctx, c.session.Dir, configs[i].Renderer, pins[i], input)
		})
	}
	workers.Wait()
	return pins
}

func runReportVerifications(result *RunResult, tasks []*TodoItem) []*VerificationResult {
	var evidence []*VerificationResult
	if result.Acceptance != nil {
		evidence = slices.Clone(result.Acceptance.VerificationEvidence)
	}
	for _, item := range tasks {
		if item != nil && item.Status == TaskDone {
			if receipt := latestSuccessfulExecutionReceipt(item, result.RunID); receipt != nil && receipt.VerifyResult != nil {
				evidence = append(evidence, receipt.VerifyResult)
			}
		}
	}
	return evidence
}

func verifiedReportRendererInput(result *RunResult, tasks []*TodoItem, verifications []*VerificationResult, section RunReportSection) (reportRendererInput, bool) {
	snapshot := result.RunInputs
	if ValidateRunInputSnapshot(snapshot) != nil ||
		(snapshot.RunID != result.RunID && snapshot.RunID != result.RunInputOriginRunID) {
		return reportRendererInput{}, false
	}
	origins := make(map[string]string)
	if manifest := result.EvidenceManifest; manifest != nil {
		sealed := *manifest
		if manifest.RunID != result.RunID || sealed.Seal() != nil || sealed.ManifestHash != manifest.ManifestHash ||
			ValidateEvidenceRecoveryInputs(manifest, snapshot) != nil {
			return reportRendererInput{}, false
		}
		for _, item := range tasks {
			if item != nil {
				if binding, ok := manifest.VerifiedTaskBinding(item.ID); ok && binding.RunID != result.RunID {
					receipt := latestSuccessfulExecutionReceipt(item, binding.RunID)
					if receipt != nil && receipt.TaskID == binding.TaskID && receipt.Attempt == binding.Attempt &&
						receipt.ModelExecutionID == binding.ModelExecutionID && receipt.TranscriptRef == binding.TranscriptRef &&
						receipt.ActionInvocationID == binding.ActionInvocationID && receipt.RuntimeOutputsHash == binding.RuntimeOutputsHash {
						origins[item.ID] = binding.RunID
					}
				}
			}
		}
	}
	source, err := uniqueTaskOutputSource(tasks, section.Source.Task, result.RunID, origins)
	if err != nil {
		return reportRendererInput{}, false
	}
	for _, verification := range verifications {
		if !isVerifySuccess(verification) || verification.Overturned || verification.Spec == nil {
			continue
		}
		spec := verification.Spec
		if spec.Type != VerifyTaskOutputAssert || normalizeTaskReferenceID(spec.WorksetSourceTask) != normalizeTaskReferenceID(section.Source.Task) ||
			strings.TrimSpace(spec.TaskOutputName) != section.Source.Output {
			continue
		}
		evidence, err := ReplayTaskOutputAssertions(tasks, result.RunID, snapshot, *spec, origins)
		if err != nil || len(evidence) == 0 || !slices.Equal(evidence, verification.TaskOutputAssertions) {
			continue
		}
		receipt := latestSuccessfulExecutionReceipt(source, taskOutputOriginRun(result.RunID, source.ID, []map[string]string{origins}))
		output, err := jsonv1.Marshal(source.TypedResult.RuntimeOutputs[section.Source.Output])
		if err != nil || len(output) > maxRuntimeOutputBytes {
			return reportRendererInput{}, false
		}
		return reportRendererInput{
			Version: reportRendererInputVersion, RunID: result.RunID, SectionID: section.ID, Output: output,
			Provenance: ReportSectionProvenance{
				RunID: receipt.RunID, TaskID: source.ID, OccurrenceRevision: source.OccurrenceRevision,
				Attempt: receipt.Attempt, ActionInvocationID: receipt.ActionInvocationID, RuntimeOutputsHash: receipt.RuntimeOutputsHash,
				RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
				VerificationFingerprint: verification.Fingerprint,
			},
		}, true
	}
	return reportRendererInput{}, false
}

func renderReportSection(ctx context.Context, teamDir string, config agent.ReportRendererConfig, section RunReportSection, input reportRendererInput) RunReportSection {
	section.Provenance = &input.Provenance
	var markdown string
	var err error
	if section.Renderer == reportRendererGo {
		program, prepareErr := golangruntime.Prepare(teamDir, config.Source)
		if prepareErr != nil || program.Digest != section.RendererDigest || config.Mode != golangruntime.TrustedStaticMode {
			section.UnavailableReason = "Go renderer source changed or became unavailable"
			return section
		}
		markdown, err = renderGoReportSection(ctx, config, program, input)
	} else {
		if runInputHash([]byte(config.Template)) != section.RendererDigest {
			section.UnavailableReason = "Markdown template changed"
			return section
		}
		markdown, err = renderMarkdownReportTemplate(config.Template, input)
	}
	if err != nil {
		section.UnavailableReason = err.Error()
		return section
	}
	markdown = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ReplaceAll(markdown, "\r\n", "\n"))
	if strings.TrimSpace(markdown) == "" || len(markdown) > maxReportSectionMarkdown {
		section.UnavailableReason = "renderer returned empty or oversized Markdown"
		return section
	}
	section.Markdown = markdown
	return section
}

func renderGoReportSection(ctx context.Context, config agent.ReportRendererConfig, program golangruntime.Program, input reportRendererInput) (string, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return "", errors.New("go renderer input could not be encoded")
	}
	temp, err := os.MkdirTemp("", "hufu-report-")
	if err != nil {
		return "", errors.New("go renderer temporary directory unavailable")
	}
	defer func() { _ = os.RemoveAll(temp) }()
	env := []string{"TMPDIR=" + temp, "TEMP=" + temp, "TMP=" + temp}
	seconds := config.Timeout
	if seconds == 0 {
		seconds = maxReportSectionTimeout
	}
	renderCtx, cancel := detachedTimeoutContext(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	output, err := golangruntime.Execute(renderCtx, "", program, data, env, maxReportRendererOutput, 0)
	if errors.Is(err, context.DeadlineExceeded) {
		return "", errors.New("go renderer timed out")
	}
	if err != nil {
		return "", errors.New("go renderer failed")
	}
	var envelope struct {
		Markdown string `json:"markdown"`
	}
	if output.StdoutTruncated || json.Unmarshal(output.Stdout, &envelope, json.RejectUnknownMembers(true)) != nil {
		return "", errors.New("go renderer returned invalid or oversized JSON")
	}
	return envelope.Markdown, nil
}

func renderMarkdownReportTemplate(source string, input reportRendererInput) (string, error) {
	tmpl, err := template.New("report").Option("missingkey=error").Parse(source)
	if err != nil {
		return "", errors.New("markdown template is invalid")
	}
	var output any
	decoder := jsonv1.NewDecoder(bytes.NewReader(input.Output))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil {
		return "", errors.New("markdown template input could not be decoded")
	}
	data := map[string]any{"Version": input.Version, "RunID": input.RunID, "SectionID": input.SectionID, "Output": output, "Provenance": input.Provenance}
	var markdown reportMarkdownBuffer
	if err := tmpl.Execute(&markdown, data); err != nil {
		return "", errors.New("markdown template could not be rendered")
	}
	return markdown.buffer.String(), nil
}

type reportMarkdownBuffer struct {
	buffer bytes.Buffer
}

func (b *reportMarkdownBuffer) Write(data []byte) (int, error) {
	if len(data) > maxReportSectionMarkdown-b.buffer.Len() {
		return 0, errors.New("markdown template exceeded the output limit")
	}
	return b.buffer.Write(data)
}
