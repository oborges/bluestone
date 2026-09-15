package buffer

import (
	"sync"
	"time"

	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// WriteSession represents an active write session for a file path
type WriteSession struct {
	Path       string
	Buffer     *WriteBuffer
	LastAccess time.Time
	RefCount   int
	Mu         sync.Mutex
	SessionID  string
}

// SessionManager manages write sessions across file handle open/close cycles
// This allows buffering to persist even when NFS clients repeatedly open/close files
type SessionManager struct {
	sessions      map[string]*WriteSession
	mu            sync.RWMutex
	bufferSize    int64
	sessionTimeout time.Duration
}

// NewSessionManager creates a new write session manager
func NewSessionManager(bufferSize int64, sessionTimeout time.Duration) *SessionManager {
	sm := &SessionManager{
		sessions:       make(map[string]*WriteSession),
		bufferSize:     bufferSize,
		sessionTimeout: sessionTimeout,
	}
	
	// Start cleanup goroutine for expired sessions
	go sm.cleanupExpiredSessions()
	
	return sm
}

// GetOrCreateSession gets an existing session or creates a new one for the given path
func (sm *SessionManager) GetOrCreateSession(path string) *WriteSession {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	
	session, exists := sm.sessions[path]
	if exists {
		session.Mu.Lock()
		session.RefCount++
		session.LastAccess = time.Now()
		session.Mu.Unlock()
		
		logging.Info("Reusing existing write session",
			zap.String("path", path),
			zap.String("session_id", session.SessionID),
			zap.Int("ref_count", session.RefCount),
			zap.Int64("buffer_size", session.Buffer.Size()))
		
		return session
	}
	
	// Create new session
	sessionID := generateSessionID(path)
	session = &WriteSession{
		Path:       path,
		Buffer:     NewWriteBuffer(sm.bufferSize),
		LastAccess: time.Now(),
		RefCount:   1,
		SessionID:  sessionID,
	}
	
	sm.sessions[path] = session
	
	logging.Info("Created new write session",
		zap.String("path", path),
		zap.String("session_id", sessionID),
		zap.Int64("buffer_size_bytes", sm.bufferSize),
		zap.Float64("buffer_size_mb", float64(sm.bufferSize)/(1024*1024)))
	
	return session
}

// ReleaseSession decrements the reference count for a session
// The session is kept alive for sessionTimeout after last release
func (sm *SessionManager) ReleaseSession(path string) {
	sm.mu.RLock()
	session, exists := sm.sessions[path]
	sm.mu.RUnlock()
	
	if !exists {
		return
	}
	
	session.Mu.Lock()
	session.RefCount--
	session.LastAccess = time.Now()
	refCount := session.RefCount
	session.Mu.Unlock()
	
	logging.Info("Released write session",
		zap.String("path", path),
		zap.String("session_id", session.SessionID),
		zap.Int("ref_count", refCount))
}

// CloseSession explicitly closes and removes a session
// This should be called after final flush
func (sm *SessionManager) CloseSession(path string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	
	session, exists := sm.sessions[path]
	if !exists {
		return
	}
	
	logging.Info("Closing write session",
		zap.String("path", path),
		zap.String("session_id", session.SessionID),
		zap.Int64("final_buffer_size", session.Buffer.Size()))
	
	delete(sm.sessions, path)
}

// GetSession returns an existing session without creating one
func (sm *SessionManager) GetSession(path string) (*WriteSession, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	
	session, exists := sm.sessions[path]
	return session, exists
}

// cleanupExpiredSessions removes sessions that haven't been accessed recently
func (sm *SessionManager) cleanupExpiredSessions() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		sm.mu.Lock()
		now := time.Now()
		
		for path, session := range sm.sessions {
			session.Mu.Lock()
			timeSinceAccess := now.Sub(session.LastAccess)
			refCount := session.RefCount
			bufferSize := session.Buffer.Size()
			session.Mu.Unlock()
			
			// Only cleanup sessions with no active references and expired timeout
			if refCount == 0 && timeSinceAccess > sm.sessionTimeout {
				// If buffer has data, warn about potential data loss
				if bufferSize > 0 {
					logging.Warn("Cleaning up expired session with unflushed data",
						zap.String("path", path),
						zap.String("session_id", session.SessionID),
						zap.Int64("unflushed_bytes", bufferSize),
						zap.Duration("idle_time", timeSinceAccess))
				} else {
					logging.Info("Cleaning up expired session",
						zap.String("path", path),
						zap.String("session_id", session.SessionID),
						zap.Duration("idle_time", timeSinceAccess))
				}
				
				delete(sm.sessions, path)
			}
		}
		
		sm.mu.Unlock()
	}
}

// Stats returns statistics about active sessions
func (sm *SessionManager) Stats() map[string]interface{} {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	
	totalSessions := len(sm.sessions)
	totalBufferSize := int64(0)
	activeSessions := 0
	
	for _, session := range sm.sessions {
		session.Mu.Lock()
		if session.RefCount > 0 {
			activeSessions++
		}
		totalBufferSize += session.Buffer.Size()
		session.Mu.Unlock()
	}
	
	return map[string]interface{}{
		"total_sessions":      totalSessions,
		"active_sessions":     activeSessions,
		"total_buffer_bytes":  totalBufferSize,
		"total_buffer_mb":     float64(totalBufferSize) / (1024 * 1024),
	}
}

// generateSessionID creates a unique session ID
func generateSessionID(path string) string {
	return path + "-" + time.Now().Format("20060102-150405.000")
}

// Made with Bob
