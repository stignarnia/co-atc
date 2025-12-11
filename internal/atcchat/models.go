package atcchat

import "time"

// SessionStatus represents the status of a chat session
type SessionStatus struct {
	ID           string         `json:"id"`
	Active       bool           `json:"active"`
	Connected    bool           `json:"connected"`
	LastActivity time.Time      `json:"last_activity"`
	ExpiresAt    time.Time      `json:"expires_at"`
	Type         string         `json:"type,omitempty"`
	SessionID    string         `json:"session_id,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
	Error        string         `json:"error,omitempty"`
}
