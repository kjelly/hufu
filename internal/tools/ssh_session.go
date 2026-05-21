//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SSHSessionManager manages active SSH sessions in memory
type SSHSessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*SSHSession // key: host[:port]
}

// SSHSessionManagerKey is the context key for SSH session manager
type sshSessionManagerKey struct{}

var SSHSessionManagerKey = sshSessionManagerKey{}

// NewSSHSessionManager creates a new SSH session manager
func NewSSHSessionManager() *SSHSessionManager {
	return &SSHSessionManager{
		sessions: make(map[string]*SSHSession),
	}
}

// GetSessionKey generates a unique key for a session based on host and port
func GetSessionKey(host string, port int) string {
	if port <= 0 || port == 22 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Create creates a new SSH session and stores it in the manager
// Returns the created session or nil if session already exists
func (m *SSHSessionManager) Create(host string, user string, port int, taskID string) (*SSHSession, error) {
	if m == nil {
		return nil, nil
	}

	key := GetSessionKey(host, port)

	m.mu.Lock()
	defer m.mu.Unlock()

	if session, exists := m.sessions[key]; exists {
		return session, nil // Return existing session
	}

	session := &SSHSession{
		Host:      host,
		User:      user,
		Port:      port,
		TaskID:    taskID,
		CreatedAt: time.Now(),
	}
	m.sessions[key] = session
	return session, nil
}

// SetPassword caches a password for an SSH session with expiration
func (m *SSHSessionManager) SetPassword(host string, port int, password string, expiry time.Duration) bool {
	if m == nil {
		return false
	}

	key := GetSessionKey(host, port)

	m.mu.Lock()
	defer m.mu.Unlock()

	session, exists := m.sessions[key]
	if !exists {
		return false
	}

	session.Password = password
	session.PasswordExpiry = time.Now().Add(expiry)
	return true
}

// GetPassword retrieves a cached password if not expired
func (m *SSHSessionManager) GetPassword(host string, port int) (string, bool) {
	if m == nil {
		return "", false
	}

	key := GetSessionKey(host, port)

	m.mu.RLock()
	defer m.mu.RUnlock()

	session, exists := m.sessions[key]
	if !exists {
		return "", false
	}

	if !session.PasswordExpiry.IsZero() && time.Now().After(session.PasswordExpiry) {
		return "", false // Password expired
	}

	return session.Password, true
}

// ClearPassword removes cached password for a session
func (m *SSHSessionManager) ClearPassword(host string, port int) bool {
	if m == nil {
		return false
	}

	key := GetSessionKey(host, port)

	m.mu.Lock()
	defer m.mu.Unlock()

	session, exists := m.sessions[key]
	if !exists {
		return false
	}

	session.Password = ""
	session.PasswordExpiry = time.Time{}
	return true
}

// Get retrieves an SSH session by host and port
// Returns nil if session not found or manager is nil
func (m *SSHSessionManager) Get(host string, port int) *SSHSession {
	if m == nil {
		return nil
	}

	key := GetSessionKey(host, port)

	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.sessions[key]
}

// List returns all active SSH sessions
// Returns empty slice if manager is nil or no sessions exist
func (m *SSHSessionManager) List() []*SSHSession {
	if m == nil {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*SSHSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

// Close removes an SSH session from the manager
// Returns true if session was found and removed, false otherwise
func (m *SSHSessionManager) Close(host string, port int) bool {
	if m == nil {
		return false
	}

	key := GetSessionKey(host, port)

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[key]; !exists {
		return false
	}

	delete(m.sessions, key)
	return true
}

// Count returns the number of active SSH sessions
func (m *SSHSessionManager) Count() int {
	if m == nil {
		return 0
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.sessions)
}

// SetSSHSessionManager stores the session manager in context
func SetSSHSessionManager(ctx context.Context, manager *SSHSessionManager) context.Context {
	return context.WithValue(ctx, SSHSessionManagerKey, manager)
}

// GetSSHSessionManager retrieves the session manager from context
func GetSSHSessionManager(ctx context.Context) *SSHSessionManager {
	if v, ok := ctx.Value(SSHSessionManagerKey).(*SSHSessionManager); ok {
		return v
	}
	return nil
}

// ExtractUserFromHost extracts username from host string
// e.g., "user@host" -> user="user", host="host"
// If no @ symbol, returns empty user and original host
func ExtractUserFromHost(host string) (user string, cleanHost string) {
	parts := strings.SplitN(host, "@", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", host
}
