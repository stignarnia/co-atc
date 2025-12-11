package ai

import (
	"context"
	"time"
)

// RealtimeSession represents an active realtime session
type RealtimeSession struct {
	ID           string
	ProviderID   string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	Active       bool
	LastActivity time.Time
	ClientSecret string
	SystemPrompt string
	Config       RealtimeSessionConfig
}

// RealtimeSessionConfig holds configuration for realtime sessions
type RealtimeSessionConfig struct {
	Model             string
	Voice             string
	Temperature       float64
	MaxResponseTokens int
	InputAudioFormat  string
	OutputAudioFormat string
	TurnDetection     string // "server_vad" or "none"
	SampleRate        int    // Audio sample rate in Hz
}

// RealtimeProvider defines the interface for realtime AI chat (WebSocket)
type RealtimeProvider interface {
	// CreateRealtimeSession creates a new realtime session
	CreateRealtimeSession(ctx context.Context, config RealtimeSessionConfig, systemPrompt string) (*RealtimeSession, error)

	// ConnectSession establishes the WebSocket connection
	// Returns the raw websocket connection and any error
	ConnectSession(ctx context.Context, session *RealtimeSession) (AIConnection, error)

	// UpdateSessionInstructions updates the system instructions for a session
	UpdateSessionInstructions(ctx context.Context, sessionID string, instructions string) error

	// EndSession terminates a session
	EndSession(ctx context.Context, sessionID string) error

	// ValidateSession checks if a session is valid
	ValidateSession(session *RealtimeSession) bool
}

// AIConnection represents a unified connection interface (WebSocket wrapper)
type AIConnection interface {
	// Send sends a message to the connection
	// Message type is implementation-specific (text/binary) but typically JSON text for these protocols
	Send(data []byte) error

	// Read reads a message from the connection
	// Returns message type (int), data ([]byte), and error
	// Message type matches websocket.TextMessage or websocket.BinaryMessage
	Read() (int, []byte, error)

	// Close closes the connection
	Close() error
}

// TranscriptionSession represents an active transcription session
type TranscriptionSession struct {
	ID           string
	ProviderID   string
	ClientSecret string
	Config       TranscriptionConfig
}

// TranscriptionProvider defines the interface for converting audio to text
type TranscriptionProvider interface {
	// CreateTranscriptionSession creates a transcription session (if required by provider protocol)
	CreateTranscriptionSession(ctx context.Context, config TranscriptionConfig) (*TranscriptionSession, error)

	// ConnectTranscriptionSession connects to the streaming transcription service
	ConnectTranscriptionSession(ctx context.Context, session *TranscriptionSession) (AIConnection, error)
}

// ChatMessage represents a message in a chat conversation
type ChatMessage struct {
	Role    string
	Content string
}

// ChatConfig holds configuration for chat completions
type ChatConfig struct {
	Model       string
	Temperature float64
	MaxTokens   int
}

// ChatProvider defines the interface for text-to-text chat completions (used for post-processing)
type ChatProvider interface {
	// Complete sends a conversation to the LLM and returns the text response
	ChatCompletion(ctx context.Context, messages []ChatMessage, config ChatConfig) (string, error)
}

// TranscriptionConfig holds configuration for transcription requests
type TranscriptionConfig struct {
	Language       string
	Prompt         string
	Model          string
	Temperature    float64
	NoiseReduction string
	SampleRate     int // Audio sample rate in Hz
}
