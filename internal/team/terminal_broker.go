package team

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

const terminalBrokerSocket = "terminal-broker.sock"

type terminalBrokerRequest struct {
	Action    string
	SessionID string
	LeaseID   string
	Data      string
	Rows      uint16
	Cols      uint16
}

type terminalBrokerResponse struct {
	Error   string
	LeaseID string
	Screen  string
	EOF     bool
}

type TerminalBroker struct {
	manager *TerminalSessionManager
	hooks   TerminalBrokerHooks
	path    string
	ln      *net.UnixListener
	once    sync.Once
	done    chan struct{}
}

// TerminalBrokerHooks bind a terminal handoff to coordinator task state.
// Hooks execute only after the lease transition has succeeded.
type TerminalBrokerHooks struct {
	OnAttach func(TerminalSession)
	OnDetach func(TerminalSession)
}

func StartTerminalBroker(workspace string, manager *TerminalSessionManager) (*TerminalBroker, error) {
	return StartTerminalBrokerWithHooks(workspace, manager, TerminalBrokerHooks{})
}

// StartTerminalBrokerWithHooks starts a same-user local socket broker.
func StartTerminalBrokerWithHooks(workspace string, manager *TerminalSessionManager, hooks TerminalBrokerHooks) (*TerminalBroker, error) {
	if manager == nil {
		return nil, fmt.Errorf("start terminal broker: manager is required")
	}
	logs := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logs, 0o700); err != nil {
		return nil, fmt.Errorf("create terminal broker directory: %w", err)
	}
	path := filepath.Join(logs, terminalBrokerSocket)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("terminal broker path %q is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale terminal broker socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect terminal broker socket: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen terminal broker: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("set terminal broker permissions: %w", err)
	}
	b := &TerminalBroker{manager: manager, hooks: hooks, path: path, ln: ln, done: make(chan struct{})}
	go b.serve()
	return b, nil
}

func (b *TerminalBroker) Close() error {
	var err error
	b.once.Do(func() {
		close(b.done)
		err = b.ln.Close()
		_ = os.Remove(b.path)
	})
	return err
}

func (b *TerminalBroker) serve() {
	for {
		conn, err := b.ln.AcceptUnix()
		if err != nil {
			select {
			case <-b.done:
				return
			default:
				continue
			}
		}
		go b.handle(conn)
	}
}

func (b *TerminalBroker) handle(conn *net.UnixConn) {
	defer conn.Close()
	if err := verifyTerminalBrokerPeer(conn); err != nil {
		return
	}
	dec := json.NewDecoder(bufio.NewReader(conn))
	enc := json.NewEncoder(conn)
	sessionID, leaseID := "", ""
	for {
		var req terminalBrokerRequest
		if err := dec.Decode(&req); err != nil {
			if sessionID != "" && leaseID != "" {
				b.release(sessionID, leaseID)
			}
			return
		}
		resp := b.request(&req, &sessionID, &leaseID)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (b *TerminalBroker) request(req *terminalBrokerRequest, sessionID, leaseID *string) terminalBrokerResponse {
	switch req.Action {
	case "attach":
		session, err := b.session(req.SessionID)
		if err != nil {
			return terminalBrokerResponse{Error: err.Error()}
		}
		if session.Mode != TerminalModePTY {
			return terminalBrokerResponse{Error: fmt.Sprintf("terminal session %q is not a PTY", req.SessionID)}
		}
		lease, err := b.manager.AcquireUserLease(req.SessionID)
		if err != nil {
			return terminalBrokerResponse{Error: err.Error()}
		}
		*sessionID, *leaseID = req.SessionID, lease.ID
		if b.hooks.OnAttach != nil {
			b.hooks.OnAttach(session)
		}
		read, err := b.read(req.SessionID)
		if err != nil {
			_ = b.manager.ReleaseUserLease(req.SessionID, lease.ID)
			return terminalBrokerResponse{Error: err.Error()}
		}
		return terminalBrokerResponse{LeaseID: lease.ID, Screen: read.Screen, EOF: read.EOF}
	case "write":
		if req.LeaseID != *leaseID {
			return terminalBrokerResponse{Error: "terminal broker lease mismatch"}
		}
		if err := b.manager.WriteUserLease(*sessionID, *leaseID, []byte(req.Data)); err != nil {
			return terminalBrokerResponse{Error: err.Error()}
		}
		return terminalBrokerResponse{}
	case "read":
		if req.LeaseID != *leaseID {
			return terminalBrokerResponse{Error: "terminal broker lease mismatch"}
		}
		read, err := b.read(*sessionID)
		if err != nil {
			return terminalBrokerResponse{Error: err.Error()}
		}
		return terminalBrokerResponse{Screen: read.Screen, EOF: read.EOF}
	case "resize":
		if req.LeaseID != *leaseID {
			return terminalBrokerResponse{Error: "terminal broker lease mismatch"}
		}
		if err := b.manager.Resize(nil, *sessionID, req.Rows, req.Cols); err != nil {
			return terminalBrokerResponse{Error: err.Error()}
		}
		return terminalBrokerResponse{}
	case "detach":
		if req.LeaseID != *leaseID {
			return terminalBrokerResponse{Error: "terminal broker lease mismatch"}
		}
		if err := b.release(*sessionID, *leaseID); err != nil {
			return terminalBrokerResponse{Error: err.Error()}
		}
		*sessionID, *leaseID = "", ""
		return terminalBrokerResponse{}
	default:
		return terminalBrokerResponse{Error: fmt.Sprintf("unknown terminal broker action %q", req.Action)}
	}
}

func (b *TerminalBroker) release(sessionID, leaseID string) error {
	session, err := b.session(sessionID)
	if err != nil {
		return err
	}
	if err := b.manager.ReleaseUserLease(sessionID, leaseID); err != nil {
		return err
	}
	if b.hooks.OnDetach != nil {
		b.hooks.OnDetach(session)
	}
	return nil
}

func (b *TerminalBroker) session(id string) (TerminalSession, error) {
	sessions, err := b.manager.List(context.Background(), "")
	if err != nil {
		return TerminalSession{}, err
	}
	for _, session := range sessions {
		if session.ID == id {
			return session, nil
		}
	}
	return TerminalSession{}, fmt.Errorf("terminal session %q not found", id)
}

func (b *TerminalBroker) read(id string) (TerminalReadResult, error) {
	session, err := b.session(id)
	if err != nil {
		return TerminalReadResult{}, err
	}
	return b.manager.Read(WithTerminalTaskID(context.Background(), session.OwnerTaskID), id)
}
