package transcription

import (
	"time"
)

// TranscriptionEvent represents a transcription event
type TranscriptionEvent struct {
	Type      string    // "delta" or "completed"
	Text      string    // The transcription text
	Timestamp time.Time // When the event occurred
}

// Config represents the configuration for the transcription service
type Config struct {
	// Authentication / endpoint configuration
	OpenAIAPIKey          string
	OpenAIBaseURL         string // Optional base URL for OpenAI API (e.g. proxy). If empty, the system will fall back to env/default.

	// OpenAI endpoint path overrides (optional)
	// These allow directing different API calls (session creation, websocket base, chat completions, etc.)
	// to custom paths on a proxy or alternative provider. If left empty, the application-level defaults will be used.
	RealtimeSessionPath        string // e.g. "/v1/realtime/sessions"
	RealtimeWebsocketPath      string // e.g. "/v1/realtime" (used to build the ws/wss base + path)
	TranscriptionSessionPath   string // e.g. "/v1/realtime/transcription_sessions"
	ChatCompletionsPath        string // e.g. "/v1/chat/completions"

	// Model and audio settings
	Model                 string
	Language              string
	NoiseReduction        string
	ChunkMs               int
	BufferSizeKB          int
	FFmpegPath            string
	FFmpegSampleRate      int
	FFmpegChannels        int
	FFmpegFormat          string
	ReconnectIntervalSec  int
	MaxRetries            int
	TurnDetectionType     string
	PrefixPaddingMs       int
	SilenceDurationMs     int
	VADThreshold          float64
	RetryMaxAttempts      int
	RetryInitialBackoffMs int
	RetryMaxBackoffMs     int
	PromptPath            string
	Prompt                string // Loaded from PromptPath
	TimeoutSeconds        int    // HTTP timeout for OpenAI API requests
}
