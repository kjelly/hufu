package team

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"slices"
	"strings"
)

type codexNotificationKind uint8

const (
	codexIgnored codexNotificationKind = iota
	codexProgress
	codexDiagnostic
	codexTerminal
)

func (n CodexNotification) kind() codexNotificationKind {
	switch n.Method {
	case codexNotificationTurnCompleted:
		return codexTerminal
	case codexCommandOutputDelta, "item/started", "item/completed", "error", "turn/failed":
		return codexDiagnostic
	case "turn/started", "thread/status/changed", "turn/plan/updated":
		return codexProgress
	default:
		return codexIgnored
	}
}

type codexNotificationTarget struct {
	requestID    int64
	threadID     string
	turnID       string
	admissionErr error
	ctx          context.Context
}

type codexNotificationAddress struct {
	threadID string
	turnID   string
}

func (n CodexNotification) readAddress() (codexNotificationAddress, bool) {
	return n.routeAddress(nil)
}

func (n CodexNotification) routeAddress(terminalStatus *string) (codexNotificationAddress, bool) {
	if n.kind() == codexTerminal {
		var completion codexTurnCompletedParams
		if jsonv2.Unmarshal(n.Params, &completion) != nil {
			return codexNotificationAddress{}, false
		}
		if terminalStatus != nil {
			*terminalStatus = completion.Turn.Status
		}
		return codexAddressFromIDs(completion.ThreadID, completion.Turn.ID)
	}
	var fields struct {
		ThreadID json.RawMessage `json:"threadId"`
		TurnID   json.RawMessage `json:"turnId"`
		Turn     struct {
			ID json.RawMessage `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(n.Params, &fields); err != nil {
		// Advisory data can contain diagnostic strings without an object header.
		return codexNotificationAddress{}, true
	}
	if len(fields.TurnID) == 0 {
		fields.TurnID = fields.Turn.ID
	}
	return codexAddressFromIDs(fields.ThreadID, fields.TurnID)
}

func codexAddressFromIDs(thread, turn json.RawMessage) (codexNotificationAddress, bool) {
	threadID, threadOK := readOptionalCodexID(thread)
	turnID, turnOK := readOptionalCodexID(turn)
	return codexNotificationAddress{threadID, turnID}, threadOK && turnOK
}

func readOptionalCodexID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	var value string
	err := json.Unmarshal(raw, &value)
	return value, err == nil && value != ""
}

func (a codexNotificationAddress) matches(threadID, turnID string) bool {
	return (threadID == "" || a.threadID == "" || a.threadID == threadID) &&
		(turnID == "" || a.turnID == "" || a.turnID == turnID)
}

// Before the acknowledgment reveals the turn ID, reserve space for
// terminal candidates independently of the rolling advisory queue.
type codexPendingNotifications struct {
	frames         []codexPendingNotification
	terminals      []codexPendingNotification
	latestTerminal codexPendingNotification
	advisoryBytes  int64
	terminalBytes  int64
	sequence       uint64
	losses         codexPendingLosses
	terminalLosses codexPendingLosses
}

type codexPendingNotification struct {
	notification CodexNotification
	sequence     uint64
}

type codexPendingLosses struct {
	first, unassigned, overflow uint64
	turns                       map[[sha256.Size]byte]uint64
}

func (p *codexPendingLosses) lose(frame codexPendingNotification) {
	p.first = firstCodexLoss(p.first, frame.sequence)
	turnID := frame.notification.address.turnID
	if turnID == "" {
		p.unassigned = firstCodexLoss(p.unassigned, frame.sequence)
		return
	}
	if p.turns == nil {
		p.turns = make(map[[sha256.Size]byte]uint64)
	}
	key := sha256.Sum256([]byte(turnID))
	prior := p.turns[key]
	if prior == 0 && len(p.turns) >= 16*1024 {
		p.overflow = firstCodexLoss(p.overflow, frame.sequence)
		return
	}
	p.turns[key] = firstCodexLoss(prior, frame.sequence)
}

func firstCodexLoss(current, sequence uint64) uint64 {
	if current == 0 || sequence < current {
		return sequence
	}
	return current
}

func codexLossBefore(sequence, end uint64) bool { return sequence != 0 && sequence < end }

func (p *codexPendingLosses) lostBefore(turnID string, end uint64) bool {
	if turnID == "" {
		return codexLossBefore(p.first, end)
	}
	return codexLossBefore(p.unassigned, end) ||
		codexLossBefore(p.turns[sha256.Sum256([]byte(turnID))], end)
}

func (p *codexPendingNotifications) append(n CodexNotification, slots int, advisoryLimit, frameLimit int64) {
	p.sequence++
	frame := codexPendingNotification{notification: n, sequence: p.sequence}
	size := n.queuedBytes()
	if n.kind() == codexTerminal {
		for _, prior := range p.terminals {
			if prior.notification.address == n.address {
				return
			}
		}
		if p.latestTerminal.sequence != 0 && p.latestTerminal.notification.address == n.address {
			return
		}
		const terminalSlots = 1024
		limit := max(2*frameLimit, 8*resultCollectionResponseMaxBytes)
		if p.latestTerminal.sequence != 0 || len(p.terminals) >= terminalSlots || p.terminalBytes+size > limit {
			// Preserve the earliest candidates and reserve one frame for
			// the latest candidate. Record actual evictions for the ACK.
			if p.latestTerminal.sequence != 0 {
				p.terminalLosses.lose(p.latestTerminal)
			}
			p.latestTerminal = frame
			return
		}
		p.terminals = append(p.terminals, frame)
		p.terminalBytes += size
		return
	}
	if size > advisoryLimit {
		if n.kind() == codexDiagnostic {
			p.losses.lose(frame)
		}
		return
	}
	for len(p.frames) >= slots-1 || p.advisoryBytes+size > advisoryLimit {
		removed := p.frames[0]
		if removed.notification.kind() == codexDiagnostic {
			p.losses.lose(removed)
		}
		p.advisoryBytes -= removed.notification.queuedBytes()
		p.frames[0] = codexPendingNotification{}
		p.frames = p.frames[1:]
	}
	p.frames = append(p.frames, frame)
	p.advisoryBytes += size
}

func (c *CodexRPCClient) routeNotification(n CodexNotification, pending *codexPendingNotifications, frameLimit int, dropped *bool) bool {
	if n.kind() == codexIgnored {
		return true
	}
	target := c.notificationTarget.Load()
	if target == nil {
		// Idle traffic cannot supply evidence for a future turn.
		return !c.queueUnbound || c.queueNotification(n, frameLimit, dropped, nil)
	}
	var terminalStatus string
	address, valid := n.routeAddress(&terminalStatus)
	if !valid || !address.matches(target.threadID, target.turnID) {
		return true
	}
	n.terminalStatus = terminalStatus
	if target.turnID == "" {
		n.address = address
		pending.append(n, cap(c.notifications), c.advisoryQueueBytes, int64(frameLimit))
		return true
	}
	n.address = address
	return c.queueNotification(n, frameLimit, dropped, target)
}

func (c *CodexRPCClient) routePendingNotifications(target codexNotificationTarget, reply *jsonRPCResponse, pending *codexPendingNotifications, frameLimit int, dropped *bool) bool {
	defer func() { *pending = codexPendingNotifications{} }()
	terminals := pending.terminals
	if pending.latestTerminal.sequence != 0 {
		terminals = append(terminals, pending.latestTerminal)
	}
	if reply.Error != nil {
		// An RPC error has no turn ID. Attach the prefix directly so its
		// diagnostic volume cannot prevent delivery of the reply itself.
		frames := append(pending.frames, terminals...)
		slices.SortFunc(frames, func(a, b codexPendingNotification) int {
			return cmp.Compare(a.sequence, b.sequence)
		})
		reply.diagnosticPrefix = make([]CodexNotification, 0, len(frames))
		for _, frame := range frames {
			reply.diagnosticPrefix = append(reply.diagnosticPrefix, frame.notification)
		}
		reply.advisoryDropped = pending.losses.first != 0 || pending.terminalLosses.first != 0
		c.notificationTarget.Store(nil)
		return true
	}

	var start codexTurnStartResult
	if jsonv2.Unmarshal(reply.Result, &start) != nil || strings.TrimSpace(start.Turn.ID) == "" ||
		!codexOptionalIDMatches(start.ThreadID, target.threadID) || !c.providerIdentitySafe(start.Turn.ID) {
		return true
	}
	target.turnID, target.requestID = start.Turn.ID, 0
	end := pending.sequence + 1
	frames := pending.frames
	for _, frame := range terminals {
		if frame.notification.address.matches(target.threadID, target.turnID) {
			end = frame.sequence
			firstAfter, _ := slices.BinarySearchFunc(frames, end, func(frame codexPendingNotification, sequence uint64) int {
				return cmp.Compare(frame.sequence, sequence)
			})
			frames = append(frames[:firstAfter], frame)
			break
		}
	}
	switch {
	case codexLossBefore(pending.losses.overflow, end), codexLossBefore(pending.terminalLosses.overflow, end):
		target.admissionErr = errors.New("pre-acknowledgment notification loss index exceeded its capacity")
	case pending.terminalLosses.lostBefore(target.turnID, end):
		target.admissionErr = errors.New("pre-acknowledgment terminal notification capacity exceeded for the acknowledged turn")
	}
	c.notificationTarget.Store(&target)
	if target.admissionErr != nil {
		return true
	}

	*dropped = *dropped || pending.losses.lostBefore(target.turnID, end)
	for _, frame := range frames {
		n := frame.notification
		if n.address.matches(target.threadID, target.turnID) && !c.queueNotification(n, frameLimit, dropped, &target) {
			return false
		}
	}
	return true
}
