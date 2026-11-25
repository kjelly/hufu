package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	codexCommandOutputDelta       = "item/commandExecution/outputDelta"
	codexMaxDeferredActivityCount = 1024
)

type codexDeferredActivity struct {
	notification CodexNotification
	command      *strings.Builder
}

// A terminal response can reveal credentials that appeared earlier as
// opaque text. Delay provider-supplied activity detail until the whole turn
// is available.
type codexTurnDiagnostics struct {
	pendingCount  int
	pendingBytes  int
	pending       []codexDeferredActivity
	commandOutput map[string]*strings.Builder
	secrets       *utils.SecretRedactionScope
	knownSecrets  *utils.SecretRedactionScope
	unsafe        bool
}

func newCodexTurnDiagnostics(secrets *utils.SecretRedactionScope) *codexTurnDiagnostics {
	if secrets == nil {
		secrets = new(utils.SecretRedactionScope)
	}
	return &codexTurnDiagnostics{secrets: secrets, unsafe: !secrets.Complete()}
}

func (c *CodexRPCClient) newDiagnostics(secrets *utils.SecretRedactionScope) *codexTurnDiagnostics {
	diagnostics := newCodexTurnDiagnostics(secrets)
	diagnostics.knownSecrets = c.knownSecrets
	diagnostics.secrets.DiscoverEnvironment(c.diagnosticEnv)
	diagnostics.unsafe = !diagnostics.secrets.Complete()
	return diagnostics
}

func (d *codexTurnDiagnostics) learn(text string) {
	if d.unsafe || text == "" {
		return
	}
	d.secrets.DiscoverText(text)
	if learnCodexDiagnosticSecrets(text, d.secrets) != nil || !d.secrets.Complete() {
		d.discard()
	}
}

func (d *codexTurnDiagnostics) discard() {
	d.unsafe = true
	d.secrets.Discard()
	d.pending = nil
	d.commandOutput = nil
}

func (d *codexTurnDiagnostics) learnItem(item codexNotificationItem) {
	for _, text := range []string{item.Phase, item.Status, codexRawValue(item.Command), item.AggregatedOutput,
		item.FormattedOutput, item.Stdout, item.Stderr, item.Text, item.SummaryText} {
		d.learn(text)
	}
}

func (d *codexTurnDiagnostics) deferActivity(notif CodexNotification, detailed bool) bool {
	if d.unsafe {
		return false
	}
	switch notif.Method {
	case codexCommandOutputDelta:
		params := bytes.TrimSpace(notif.Params)
		if len(params) > 0 && !bytes.Equal(params, []byte("null")) && d.learnNotification(notif) {
			d.collectCommandOutput(notif.Params)
		}
		return true
	case "item/started", "item/completed":
		if !d.learnNotification(notif) {
			return false
		}
		item, ok := readCodexNotificationItem(notif.Params)
		if !ok {
			return false
		}
		d.learnItem(item)
	case "error", "turn/failed":
		if len(notif.Params) != 0 {
			d.learnNotification(notif)
		}
		return false
	default:
		return false
	}
	if detailed && !d.unsafe {
		d.pending = append(d.pending, codexDeferredActivity{notification: notif})
		return true
	}
	return false
}

func (d *codexTurnDiagnostics) learnNotification(notif CodexNotification) bool {
	if !d.reserveActivity(int(notif.queuedBytes())) {
		return false
	}
	d.learn(string(notif.Params))
	return !d.unsafe
}

func (d *codexTurnDiagnostics) reserveActivity(size int) bool {
	if d.unsafe {
		return false
	}
	if size > resultCollectionResponseMaxBytes-d.pendingBytes || d.pendingCount == codexMaxDeferredActivityCount {
		d.discard()
		return false
	}
	d.pendingBytes += size
	d.pendingCount++
	return true
}

func (d *codexTurnDiagnostics) collectCommandOutput(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var payload struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		d.discard()
		return
	}
	delta := payload.Delta
	if delta == "" {
		delta = payload.Text
	}
	if delta == "" {
		return
	}
	if payload.ItemID == "" {
		d.discard()
		return
	}
	stream := d.commandOutput[payload.ItemID]
	if stream == nil {
		if d.commandOutput == nil {
			d.commandOutput = make(map[string]*strings.Builder)
		}
		stream = new(strings.Builder)
		d.commandOutput[payload.ItemID] = stream
		d.pending = append(d.pending, codexDeferredActivity{command: stream})
	}
	stream.WriteString(delta)
}

func (a codexDeferredActivity) activity(secrets *utils.SecretRedactionScope) (codexTurnActivity, bool) {
	if a.command == nil {
		return codexActivityForNotification(a.notification, true, secrets)
	}
	text := codexActivityText(redactCodexProviderText(a.command.String(), secrets))
	return codexTurnActivity{Method: codexCommandOutputDelta, Message: "Codex command output: " + text}, text != ""
}

func (d *codexTurnDiagnostics) learnCommandStreams() {
	for _, pending := range d.pending {
		if pending.command != nil {
			d.learn(pending.command.String())
		}
	}
}

func (d *codexTurnDiagnostics) complete(turn *codexTurn, onActivity func(codexTurnActivity), elapsed time.Duration, detailed bool) {
	d.learnCommandStreams()
	for _, raw := range turn.Items {
		d.learn(string(raw))
		var item codexNotificationItem
		if err := json.Unmarshal(raw, &item); err != nil {
			d.discard()
			break
		}
		d.learnItem(item)
	}
	if turn.Error != nil {
		d.learn(string(turn.Error.Data))
		turn.Error.Data = nil
		d.learn(turn.Error.Message)
		if d.unsafe {
			turn.Error.Message = ""
		} else {
			turn.Error.Message = utils.TruncateRunes(redactCodexProviderText(turn.Error.Message, d.secrets), codexFailureEvidenceMaxRunes)
		}
	}
	turn.diagnosticsUnsafe = d.unsafe
	turn.diagnosticSecrets = d.secrets
	if !detailed || onActivity == nil {
		return
	}
	for _, pending := range d.pending {
		if activity, ok := pending.activity(d.secrets); ok {
			activity.Elapsed = elapsed
			onActivity(activity)
		}
	}
}

func failedCodexTurnStatus(status string) string {
	switch status {
	case "failed", "interrupted", "cancelled":
		return status
	default:
		return "unknown"
	}
}

func (r CodexTurnResult) diagnosticOutput() string {
	return r.redactedOutput
}

// A failed start carries its bounded diagnostic prefix with the RPC reply.
// There is no turn waiter or notification channel involved in its delivery.
func (c *CodexRPCClient) redactTurnStartError(reply jsonRPCResponse, diagnostics *codexTurnDiagnostics, threadID string) error {
	c.finishDiagnosticProcess(diagnostics)
	if reply.advisoryDropped {
		diagnostics.discard()
	}
	for _, notif := range reply.diagnosticPrefix {
		if address, valid := notif.readAddress(); !valid || !address.matches(threadID, "") {
			continue
		}
		if notif.Method == codexNotificationTurnCompleted {
			diagnostics.learnNotification(notif)
		} else {
			diagnostics.deferActivity(notif, false)
		}
	}
	diagnostics.learnCommandStreams()
	return reply.Error.redacted(diagnostics)
}

func (c *CodexRPCClient) finishDiagnosticProcess(diagnostics *codexTurnDiagnostics) {
	if c.finishProcess != nil && c.finishProcess() != nil {
		diagnostics.discard()
	}
	if c.stderr == nil {
		return
	}
	text, truncated := c.stderr.snapshot()
	if truncated {
		diagnostics.discard()
		return
	}
	diagnostics.learn(text)
}

func (c *CodexRPCClient) redactStderrError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	diagnostics := c.newDiagnostics(nil)
	c.finishDiagnosticProcess(diagnostics)
	return redactCodexError(err, diagnostics.secrets)
}

type codexDiagnosticError struct {
	message string
	cause   error
}

func (e *codexDiagnosticError) Error() string { return e.message }
func (e *codexDiagnosticError) Unwrap() error { return e.cause }

func redactCodexError(err error, secrets *utils.SecretRedactionScope) error {
	if err == nil {
		return nil
	}
	message := "provider result diagnostics unavailable"
	if secrets == nil || secrets.Complete() {
		message = secrets.RedactText(err.Error())
	}
	if message == err.Error() {
		return err
	}
	return &codexDiagnosticError{message: message, cause: err}
}

func rejectIncompleteCodexDiagnostics() error {
	return rejectResultSourceRead(errors.New("provider result credential discovery did not complete"))
}

func (r CodexTurnResult) canonicalize(ctx context.Context, request AttemptRequest, delta WorkspaceDelta, root string) (*TaskResult, *[]InvariantAssessmentClaim, error) {
	sealFanout := request.sealFanoutResult
	request.sealFanoutResult = false
	canonical, err := NewExternalResultCanonicalizer().Canonicalize(ctx, request, AttemptResult{ResultProposal: r.Proposal}, delta, root)
	if r.diagnosticsUnsafe && (err == nil || localResultRejectionReason(err) == RepairFailureInvalidSchema) {
		return nil, nil, rejectIncompleteCodexDiagnostics()
	}
	if err != nil {
		return nil, nil, redactCodexError(err, r.diagnosticSecrets)
	}
	canonical, err = redactExternalResultCandidate(canonical, request.resultContractSchema, r.diagnosticSecrets)
	if err != nil {
		return nil, nil, redactCodexError(err, r.diagnosticSecrets)
	}
	claims := cloneInvariantAssessmentClaims(r.Proposal.InvariantAssessments)
	if claims != nil {
		for i := range *claims {
			claim := &(*claims)[i]
			if r.diagnosticSecrets.RedactText(claim.InvariantID) != claim.InvariantID {
				return nil, nil, rejectResultSourceRead(errors.New("redaction would alter an invariant identity"))
			}
			claim.Summary = r.diagnosticSecrets.RedactText(claim.Summary)
			for i, missing := range claim.MissingEvidence {
				claim.MissingEvidence[i] = r.diagnosticSecrets.RedactText(missing)
			}
		}
	}
	if sealFanout && taskResultStatusIsSuccessful(canonical.Status) && len(canonical.Artifacts) != 0 {
		if err := stageExternalResultArtifacts(ctx, request, root, canonical.Artifacts); err != nil {
			return nil, nil, redactCodexError(err, r.diagnosticSecrets)
		}
	}
	return canonical, claims, nil
}
