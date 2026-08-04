package team

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
)

// TerminalAttachment is a local client for the coordinator-owned PTY broker.
// One attachment owns one exclusive human lease until Detach or Close.
type TerminalAttachment struct {
	conn    *net.UnixConn
	encoder *json.Encoder
	decoder *json.Decoder
	mu      sync.Mutex
	leaseID string
	closed  bool
}

// TerminalAttachmentSnapshot is the latest normalized screen and lifecycle
// state returned by the broker.
type TerminalAttachmentSnapshot struct {
	Screen string
	EOF    bool
}

// DialTerminalBroker connects to the broker for a running hufu process.
func DialTerminalBroker(workspace string) (*TerminalAttachment, error) {
	path := filepath.Join(workspace, logsDir, terminalBrokerSocket)
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("connect terminal broker at %q: %w", path, err)
	}
	return &TerminalAttachment{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		decoder: json.NewDecoder(bufio.NewReader(conn)),
	}, nil
}

func (a *TerminalAttachment) Attach(sessionID string) (TerminalAttachmentSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return TerminalAttachmentSnapshot{}, fmt.Errorf("terminal attachment is closed")
	}
	if a.leaseID != "" {
		return TerminalAttachmentSnapshot{}, fmt.Errorf("terminal attachment is already attached")
	}
	response, err := a.requestLocked(terminalBrokerRequest{Action: "attach", SessionID: sessionID})
	if err != nil {
		return TerminalAttachmentSnapshot{}, err
	}
	if response.LeaseID == "" {
		return TerminalAttachmentSnapshot{}, fmt.Errorf("terminal broker returned an empty lease")
	}
	a.leaseID = response.LeaseID
	return terminalSnapshot(response), nil
}

func (a *TerminalAttachment) Write(data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.requestWithLeaseLocked("write", string(data), 0, 0)
	return err
}

func (a *TerminalAttachment) Read() (TerminalAttachmentSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	response, err := a.requestWithLeaseLocked("read", "", 0, 0)
	if err != nil {
		return TerminalAttachmentSnapshot{}, err
	}
	return terminalSnapshot(response), nil
}

func (a *TerminalAttachment) Resize(rows, cols uint16) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.requestWithLeaseLocked("resize", "", rows, cols)
	return err
}

// Transfer requests an explicit, operator-authorized handoff through the
// coordinator broker. The connection must not hold a human terminal lease.
func (a *TerminalAttachment) Transfer(sessionID, destinationTaskID string, acceptMode TerminalMode, reason, authorization string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return fmt.Errorf("terminal attachment is closed")
	}
	if a.leaseID != "" {
		return fmt.Errorf("detach before transferring a terminal session")
	}
	_, err := a.requestLocked(terminalBrokerRequest{
		Action: "transfer", SessionID: sessionID, DestinationTaskID: destinationTaskID,
		AcceptMode: acceptMode, Reason: reason, OperatorAuthorization: authorization,
	})
	return err
}

// Detach releases the user lease and allows the coordinator to resume the
// paused agent. It is safe to call more than once.
func (a *TerminalAttachment) Detach() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leaseID == "" || a.closed {
		return nil
	}
	_, err := a.requestWithLeaseLocked("detach", "", 0, 0)
	if err == nil {
		a.leaseID = ""
	}
	return err
}

func (a *TerminalAttachment) Close() error {
	_ = a.Detach()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	return a.conn.Close()
}

func (a *TerminalAttachment) requestWithLeaseLocked(action, data string, rows, cols uint16) (terminalBrokerResponse, error) {
	if a.closed {
		return terminalBrokerResponse{}, fmt.Errorf("terminal attachment is closed")
	}
	if a.leaseID == "" {
		return terminalBrokerResponse{}, fmt.Errorf("terminal attachment is not attached")
	}
	return a.requestLocked(terminalBrokerRequest{Action: action, LeaseID: a.leaseID, Data: data, Rows: rows, Cols: cols})
}

func (a *TerminalAttachment) requestLocked(request terminalBrokerRequest) (terminalBrokerResponse, error) {
	if err := a.encoder.Encode(request); err != nil {
		return terminalBrokerResponse{}, fmt.Errorf("send terminal broker request: %w", err)
	}
	var response terminalBrokerResponse
	if err := a.decoder.Decode(&response); err != nil {
		return terminalBrokerResponse{}, fmt.Errorf("read terminal broker response: %w", err)
	}
	if response.Error != "" {
		return terminalBrokerResponse{}, fmt.Errorf("terminal broker: %s", response.Error)
	}
	return response, nil
}

func terminalSnapshot(response terminalBrokerResponse) TerminalAttachmentSnapshot {
	return TerminalAttachmentSnapshot{Screen: response.Screen, EOF: response.EOF}
}
