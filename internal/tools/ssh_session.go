//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
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

// UpdateLastUsed updates the last used timestamp for a session
func (m *SSHSessionManager) UpdateLastUsed(host string) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if session, exists := m.sessions[host]; exists {
		session.LastUsedAt = time.Now()
	}
}

// Create creates a new SSH session and stores it in the manager
// Returns the created session or nil if session already exists
func (m *SSHSessionManager) Create(host string, user string, port int, taskID string) (*SSHSession, error) {
	if m == nil {
		return nil, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[host]; exists {
		// Update last used time for existing session
		existing := m.sessions[host]
		existing.LastUsedAt = time.Now()
		return existing, nil
	}

	session := &SSHSession{
		Host:       host,
		User:       user,
		Port:       port,
		TaskID:     taskID,
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
	}
	m.sessions[host] = session
	return session, nil
}

// SetPassword caches a password for an SSH session with expiration
func (m *SSHSessionManager) SetPassword(host, password string, expiry time.Duration) bool {
	if m == nil {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	session, exists := m.sessions[host]
	if !exists {
		return false
	}

	session.Password = password
	session.PasswordExpiry = time.Now().Add(expiry)
	return true
}

// GetPassword retrieves a cached password if not expired
func (m *SSHSessionManager) GetPassword(host string) (string, bool) {
	if m == nil {
		return "", false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	session, exists := m.sessions[host]
	if !exists {
		return "", false
	}

	if time.Now().After(session.PasswordExpiry) {
		return "", false // Password expired
	}

	return session.Password, true
}

// ClearPassword removes cached password for a session
func (m *SSHSessionManager) ClearPassword(host string) bool {
	if m == nil {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	session, exists := m.sessions[host]
	if !exists {
		return false
	}

	session.Password = ""
	session.PasswordExpiry = time.Time{}
	return true
}

// Get retrieves an SSH session by host
// Returns nil if session not found or manager is nil
func (m *SSHSessionManager) Get(host string) *SSHSession {
	if m == nil {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.sessions[host]
}

// List returns all active SSH sessions
// Returns empty slice if manager is nil or no sessions exist
func (m *SSHSessionManager) List() []*SSHSession {
	if m == nil {
		return []*SSHSession{}
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
func (m *SSHSessionManager) Close(host string) bool {
	if m == nil {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[host]; !exists {
		return false
	}

	delete(m.sessions, host)
	return true
}

// CleanupIdle removes sessions idle for longer than timeout duration
// Returns the number of sessions removed
func (m *SSHSessionManager) CleanupIdle(timeout time.Duration) int {
	if m == nil {
		return 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-timeout)
	removed := 0

	for host, session := range m.sessions {
		if session.LastUsedAt.Before(cutoff) {
			delete(m.sessions, host)
			removed++
		}
	}

	return removed
}

// StartCleanupDaemon starts a background goroutine that periodically cleans up idle sessions
func (m *SSHSessionManager) StartCleanupDaemon(ctx context.Context, interval, timeout time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.CleanupIdle(timeout)
			}
		}
	}()
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
